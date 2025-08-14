#!/usr/bin/env python3
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
    i_pre    = pod_idx[pre["uid"]]
    pre_pr   = int(pods[i_pre]["priority"])

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
        # TODO: honor affinity/anti-affinity if you re-enable it
        return True

    m = cp_model.CpModel()
    x = [[m.NewBoolVar(f"x_{i}_{j}") for j in range(J)] for i in range(P)]
    placed = [m.NewBoolVar(f"placed_{i}") for i in range(P)]
    evict  = [m.NewBoolVar(f"evict_{i}")  for i in range(P)]
    kept   = [m.NewBoolVar(f"kept_{i}")   for i in range(P)]
    used   = [m.NewBoolVar(f"used_{j}")   for j in range(J)]  # node used

    # assignment
    for i in range(P):
        for j in range(J):
            if not p_eligible(i, j):
                m.Add(x[i][j] == 0)
        m.Add(sum(x[i][j] for j in range(J)) == placed[i])

    # capacity
    for j in range(J):
        m.Add(sum(x[i][j] * p_cpu(i) for i in range(P)) <= n_cpu(j))
        m.Add(sum(x[i][j] * p_ram(i) for i in range(P)) <= n_ram(j))
        # link used[j]
        for i in range(P):
            m.Add(used[j] >= x[i][j])

    # preemptor must be placed; never evicted
    m.Add(placed[i_pre] == 1)
    m.Add(evict[i_pre] == 0)

    # eviction policy
    for i in range(P):
        if i == i_pre:
            continue
        if p_prot(i) or p_pri(i) >= pre_pr:
            m.Add(evict[i] == 0)
            m.Add(placed[i] == 1)
        else:
            # low-priority pod either stays scheduled somewhere or is evicted
            m.Add(placed[i] + evict[i] == 1)

    # kept[i] <=> placed[i] AND (stays on original node)
    for i in range(P):
        orig = p_where_j(i)
        if orig is None:
            m.Add(kept[i] == 0)
        else:
            m.Add(kept[i] <= placed[i])
            m.Add(kept[i] <= x[i][orig])
            # if both placed and x[i,orig] then kept=1
            # kept >= placed + x[i,orig] - 1
            m.Add(kept[i] >= placed[i] + x[i][orig] - 1)

    # Objective with clear priority:
    # 1) Minimize evictions (huge weight)
    # 2) Minimize moves (i.e., maximize kept)
    # 3) Maximize placed pods with higher priority getting slightly more weight
    # 4) Minimize the number of nodes used
    W_EVICT = 10**9
    W_KEPT  = 10**6
    W_NODE  = 10**3   # small compared to KEPT
    pr_levels = sorted({p_pri(i) for i in range(P)}, reverse=True)
    pr_rank = {pr: (len(pr_levels) - k) for k, pr in enumerate(pr_levels)}

    m.Maximize(
        sum(W_EVICT * (1 - evict[i]) for i in range(P)) +                 # fewer evictions
        sum(W_KEPT  * kept[i]         for i in range(P)) +                 # fewer moves
        sum(pr_rank[p_pri(i)] * placed[i] for i in range(P)) -            # place more, biasing high prio
        sum(W_NODE  * used[j] for j in range(J))                           # fewer nodes used
    )

    solver = cp_model.CpSolver()
    solver.parameters.max_time_in_seconds = max(0.1, timeout_ms / 1000.0)
    solver.parameters.num_search_workers = 8

    st = solver.Solve(m)
    if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
        return {"status": "INFEASIBLE" if st == cp_model.INFEASIBLE else "TIMEOUT"}

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
