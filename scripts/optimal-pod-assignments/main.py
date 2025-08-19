#!/usr/bin/env python3

# main.py

import sys, json
from ortools.sat.python import cp_model


def solve(instance: dict) -> dict:
    nodes = instance["nodes"]
    pods  = instance["pods"]
    pre   = instance["preemptor"]

    timeout_ms = int(instance.get("timeout_ms", 3000))
    ignore_affinity = bool(instance.get("ignore_affinity", True))

    J = len(nodes); P = len(pods)
    node_idx = {n["name"]: j for j, n in enumerate(nodes)}
    pod_idx  = {p["uid"]:  i for i, p in enumerate(pods)}

    # helpers
    def n_cpu(j): return int(nodes[j]["cpu"])
    def n_ram(j): return int(nodes[j]["ram"])
    def p_uid(i): return pods[i]["uid"]
    def p_ns(i):  return pods[i]["namespace"]
    def p_name(i):return pods[i]["name"]
    def p_cpu(i): return int(pods[i]["cpu"])
    def p_ram(i): return int(pods[i]["ram"])
    def p_pri(i): return int(pods[i]["priority"])
    def p_prot(i):return bool(pods[i].get("protected", False))
    def p_where_j(i):
        w = pods[i].get("where") or ""
        return node_idx.get(w) if w else None
    def p_eligible(i, j):
        if ignore_affinity:
            return True
        # TODO: honor affinity/anti-affinity if desired
        return True

    if pre["uid"] not in pod_idx:
        return {"status": "ERROR", "message": "preemptor uid not in pods"}

    i_pre  = pod_idx[pre["uid"]]
    pre_pr = p_pri(i_pre)

    m = cp_model.CpModel()

    # Decision variables
    x = [[m.NewBoolVar(f"x_{i}_{j}") for j in range(J)] for i in range(P)]  # pod i on node j
    placed = [m.NewBoolVar(f"placed_{i}") for i in range(P)]                # pod i is scheduled somewhere
    evict  = [m.NewBoolVar(f"evict_{i}")  for i in range(P)]                # pod i is evicted (unscheduled)
    kept   = [m.NewBoolVar(f"kept_{i}")   for i in range(P)]                # pod i kept on its original node (if any)
    move   = [m.NewBoolVar(f"move_{i}")   for i in range(P)]                # pod i is moved off its original node (if any)

    # Assignment & eligibility
    for i in range(P):
        for j in range(J):
            if not p_eligible(i, j):
                m.Add(x[i][j] == 0)
        # placed[i] == OR_j x[i][j]
        m.Add(sum(x[i][j] for j in range(J)) == placed[i])

    # Capacity
    for j in range(J):
        m.Add(sum(x[i][j] * p_cpu(i) for i in range(P)) <= n_cpu(j))
        m.Add(sum(x[i][j] * p_ram(i) for i in range(P)) <= n_ram(j))

    # Keep/move indicators (only meaningful if pod had an original node)
    for i in range(P):
        orig = p_where_j(i)
        if orig is None:
            m.Add(kept[i] == 0)
            m.Add(move[i] == 0)
        else:
            # kept[i] <-> placed[i] AND x[i,orig]
            m.Add(kept[i] <= placed[i])
            m.Add(kept[i] <= x[i][orig])
            m.Add(kept[i] >= placed[i] + x[i][orig] - 1)
            # move[i] is 1 if placed and NOT kept
            m.Add(move[i] >= placed[i] - x[i][orig])
            m.Add(move[i] <= placed[i])
            m.Add(move[i] <= 1 - x[i][orig])

    # Preemptor must be placed; never evicted
    m.Add(placed[i_pre] == 1)
    m.Add(evict[i_pre] == 0)

    # Moving policy: Only pods with strictly lower priority than preemptor can be moved (pods with equal/higher priority cannot be moved)
    # This also prevents a potential race condition, if a replica set is being scaled up, then this new pending pod
    # will be deleted immediately if the execution plan needs to move some of the same replica sets, as pending pods
    # are always deleted first, no matter what pod-deletion-cost is, see https://kubernetes.io/docs/concepts/workloads/controllers/replicaset/#scaling-a-replicaset.
    for i in range(P):
        if i == i_pre:
            continue
        if p_prot(i) or p_pri(i) > pre_pr:
            m.Add(move[i] == 0)

    # Eviction policy: only strictly-lower priority pods than preemptor can be evicted.
    for i in range(P):
        if i == i_pre:
            continue
        if p_prot(i) or p_pri(i) >= pre_pr:
            # must remain scheduled (possibly moved), cannot be evicted
            m.Add(evict[i] == 0)
            m.Add(placed[i] == 1)
        else:
            # either placed somewhere OR evicted (but not both)
            m.Add(placed[i] + evict[i] == 1)

    # ---------------- Symmetry-breaking ----------------
    # 1) For equal-capacity nodes, enforce non-decreasing packed RAM
    for j1 in range(J - 1):
        for j2 in range(j1 + 1, J):
            if n_cpu(j1) == n_cpu(j2) and n_ram(j1) == n_ram(j2):
                m.Add(
                    sum(x[i][j1] * p_ram(i) for i in range(P))
                    <=
                    sum(x[i][j2] * p_ram(i) for i in range(P))
                )

    # 2) For identical pods (cpu, ram, priority), enforce node index monotonicity
    if P > 0:
        node_indices = list(range(J))
        for i1 in range(P - 1):
            for i2 in range(i1 + 1, P):
                if (p_cpu(i1), p_ram(i1), p_pri(i1)) == (p_cpu(i2), p_ram(i2), p_pri(i2)):
                    lhs = sum(node_indices[j] * x[i1][j] for j in range(J))
                    rhs = sum(node_indices[j] * x[i2][j] for j in range(J))
                    m.Add(lhs <= rhs)

    # ---------------- Lexicographic optimization ----------------
    solver = cp_model.CpSolver()
    solver.parameters.max_time_in_seconds = max(0.1, timeout_ms / 1000.0)
    solver.parameters.num_search_workers = 8

    # Stage 1: for each priority (high->low), maximize placed at that priority, freeze each optimum
    priorities = sorted({p_pri(i) for i in range(P)}, reverse=True)
    for pr in priorities:
        idxs = [i for i in range(P) if p_pri(i) == pr]
        placed_count = m.NewIntVar(0, len(idxs), f"placed_count_pr{pr}")
        m.Add(placed_count == sum(placed[i] for i in idxs))
        m.Maximize(placed_count)

        st = solver.Solve(m)
        if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
            return {"status": "INFEASIBLE" if st == cp_model.INFEASIBLE else "TIMEOUT"}

        best_placed = int(solver.Value(placed_count))
        m.Add(placed_count == best_placed)  # freeze for this tier

    # Stage 2: minimize total evictions, freeze
    total_evict = m.NewIntVar(0, P, "total_evict")
    m.Add(total_evict == sum(evict[i] for i in range(P)))
    m.Minimize(total_evict)

    st = solver.Solve(m)
    if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
        return {"status": "INFEASIBLE" if st == cp_model.INFEASIBLE else "TIMEOUT"}
    m.Add(total_evict == int(solver.Value(total_evict)))  # freeze

    # Stage 3: minimize total pod moves among pods allowed to move (prio <= pre_pr)
    move_idxs = [i for i in range(P) if p_pri(i) <= pre_pr]
    total_moves = m.NewIntVar(0, len(move_idxs), "total_moves_allowed")
    m.Add(total_moves == sum(move[i] for i in move_idxs))
    m.Minimize(total_moves)

    st = solver.Solve(m)
    if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
        return {"status": "INFEASIBLE" if st == cp_model.INFEASIBLE else "TIMEOUT"}

    # --------------- Extract final plan ---------------
    placements = {}
    evictions  = []

    for i in range(P):
        if int(solver.Value(evict[i])) == 1:
            evictions.append({"uid": p_uid(i), "namespace": p_ns(i), "name": p_name(i)})
            continue
        if int(solver.Value(placed[i])) == 1:
            for j in range(J):
                if int(solver.Value(x[i][j])) == 1:
                    placements[p_uid(i)] = nodes[j]["name"]
                    break

    nominated = placements.get(p_uid(i_pre), "")

    return {
        "status": "OK",
        "nominatedNode": nominated,
        "placements": placements,
        "evictions": evictions,
    }


def main():
    raw = sys.stdin.read()
    inst = json.loads(raw)
    out = solve(inst)
    print(json.dumps(out))


if __name__ == "__main__":
    main()
