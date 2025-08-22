#!/usr/bin/env python3
# main.py

import sys, json
from ortools.sat.python import cp_model


def solve(instance: dict) -> dict:
    # Be defensive: treat missing or null arrays as empty lists.
    nodes = instance.get("nodes") or []
    pods  = instance.get("pods")  or []

    # If there are no nodes, we cannot produce a plan.
    if not nodes:
        return {"status": "ERROR", "message": "no nodes provided"}

    # --- multi/single-preemptor handling (safe access) ---
    preemptors = list(instance.get("preemptors") or [])
    legacy_pre = instance.get("preemptor")
    if legacy_pre and not preemptors:
        preemptors = [legacy_pre]

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
        return True

    # Identify indices of declared preemptors (if any). If preemptor not present in pods,
    # just ignore it (more forgiving than erroring out).
    pre_idxs = []
    for pre in preemptors:
        uid = pre.get("uid")
        if uid is None:
            continue
        if uid in pod_idx:
            pre_idxs.append(pod_idx[uid])

    single_preemptor_mode = (len(pre_idxs) == 1)

    # If there are truly no pods at all, return a vacuous OK.
    if P == 0:
        nominated_legacy = ""
        if single_preemptor_mode:
            # preemptor exists but not in pods[]; with no pods we can only return empty plan
            nominated_legacy = ""
        return {"status": "OK", "nominatedNode": nominated_legacy, "placements": {}, "evictions": []}

    pre_ref_pri = max((p_pri(i) for i in pre_idxs), default=max(p_pri(i) for i in range(P)))

    m = cp_model.CpModel()

    # Decision variables
    x = [[m.NewBoolVar(f"x_{i}_{j}") for j in range(J)] for i in range(P)]
    placed = [m.NewBoolVar(f"placed_{i}") for i in range(P)]
    evict  = [m.NewBoolVar(f"evict_{i}")  for i in range(P)]
    kept   = [m.NewBoolVar(f"kept_{i}")   for i in range(P)]
    move   = [m.NewBoolVar(f"move_{i}")   for i in range(P)]

    # Assignment & eligibility
    for i in range(P):
        for j in range(J):
            if not p_eligible(i, j):
                m.Add(x[i][j] == 0)
        m.Add(sum(x[i][j] for j in range(J)) == placed[i])

    # Capacity
    for j in range(J):
        m.Add(sum(x[i][j] * p_cpu(i) for i in range(P)) <= n_cpu(j))
        m.Add(sum(x[i][j] * p_ram(i) for i in range(P)) <= n_ram(j))

    # Keep/move indicators
    for i in range(P):
        orig = p_where_j(i)
        if orig is None:
            m.Add(kept[i] == 0)
            m.Add(move[i] == 0)
        else:
            m.Add(kept[i] <= placed[i])
            m.Add(kept[i] <= x[i][orig])
            m.Add(kept[i] >= placed[i] + x[i][orig] - 1)
            m.Add(move[i] >= placed[i] - x[i][orig])
            m.Add(move[i] <= placed[i])
            m.Add(move[i] <= 1 - x[i][orig])

    # Mode-specific rules
    if single_preemptor_mode:
        i_pre = pre_idxs[0]
        m.Add(placed[i_pre] == 1)
        m.Add(evict[i_pre] == 0)
        for i in range(P):
            if i == i_pre:
                continue
            if p_prot(i) or p_pri(i) >= p_pri(i_pre):
                m.Add(evict[i] == 0)
                m.Add(placed[i] == 1)
                m.Add(move[i] == 0)
            else:
                m.Add(placed[i] + evict[i] == 1)
    else:
        for i in range(P):
            if p_prot(i):
                m.Add(evict[i] == 0)
                m.Add(placed[i] == 1)

    # Symmetry-breaking (unchanged)
    for j1 in range(J - 1):
        for j2 in range(j1 + 1, J):
            if n_cpu(j1) == n_cpu(j2) and n_ram(j1) == n_ram(j2):
                m.Add(
                    sum(x[i][j1] * p_ram(i) for i in range(P))
                    <=
                    sum(x[i][j2] * p_ram(i) for i in range(P))
                )

    if P > 0:
        node_indices = list(range(J))
        for i1 in range(P - 1):
            for i2 in range(i1 + 1, P):
                if (p_cpu(i1), p_ram(i1), p_pri(i1)) == (p_cpu(i2), p_ram(i2), p_pri(i2)):
                    lhs = sum(node_indices[j] * x[i1][j] for j in range(J))
                    rhs = sum(node_indices[j] * x[i2][j] for j in range(J))
                    m.Add(lhs <= rhs)

    # Lexicographic optimization
    solver = cp_model.CpSolver()
    solver.parameters.max_time_in_seconds = max(0.1, timeout_ms / 1000.0)
    solver.parameters.num_search_workers = 8

    # Stage 1: maximize placed by priority tier
    priorities = sorted({p_pri(i) for i in range(P)}, reverse=True)
    for pr in priorities:
        idxs = [i for i in range(P) if p_pri(i) == pr]
        placed_count = m.NewIntVar(0, len(idxs), f"placed_count_pr{pr}")
        m.Add(placed_count == sum(placed[i] for i in idxs))
        m.Maximize(placed_count)
        st = solver.Solve(m)
        if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
            return {"status": "INFEASIBLE" if st == cp_model.INFEASIBLE else "TIMEOUT"}
        m.Add(placed_count == int(solver.Value(placed_count)))

    # Stage 2: minimize evictions
    total_evict = m.NewIntVar(0, P, "total_evict")
    m.Add(total_evict == sum(evict[i] for i in range(P)))
    m.Minimize(total_evict)
    st = solver.Solve(m)
    if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
        return {"status": "INFEASIBLE" if st == cp_model.INFEASIBLE else "TIMEOUT"}
    m.Add(total_evict == int(solver.Value(total_evict)))

    # Stage 3: minimize moves
    total_moves = m.NewIntVar(0, P, "total_moves")
    m.Add(total_moves == sum(move[i] for i in range(P)))
    m.Minimize(total_moves)
    st = solver.Solve(m)
    if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
        return {"status": "INFEASIBLE" if st == cp_model.INFEASIBLE else "TIMEOUT"}

    # Extract plan
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

    nominated_legacy = ""
    if single_preemptor_mode:
        i_pre = pre_idxs[0]
        nominated_legacy = placements.get(pods[i_pre]["uid"], "")

    return {
        "status": "OK",
        "nominatedNode": nominated_legacy,
        "placements": placements,
        "evictions": evictions,
    }


def main():
    raw = sys.stdin.read()
    inst = json.loads(raw or "{}")
    out = solve(inst if isinstance(inst, dict) else {})
    print(json.dumps(out))


if __name__ == "__main__":
    main()
