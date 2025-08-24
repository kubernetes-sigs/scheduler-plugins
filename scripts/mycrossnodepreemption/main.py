#!/usr/bin/env python3
import sys, json
from ortools.sat.python import cp_model

def solve(instance: dict) -> dict:
    nodes = instance.get("nodes") or []
    pods  = instance.get("pods")  or []
    pre   = instance.get("preemptor") or None

    if not nodes:
        return {"status": "ERROR", "message": "no nodes provided"}

    timeout_ms = int(instance.get("timeout_ms", 3000))
    ignore_affinity = bool(instance.get("ignore_affinity", True))

    # --- De-dup by UID, prefer entries that have 'where' (running) ---
    by_uid = {}
    for p in pods:
        uid = p.get("uid")
        if not uid: 
            continue
        keep = False
        old = by_uid.get(uid)
        if old is None:
            keep = True
        else:
            old_has = bool((old.get("where") or "").strip())
            new_has = bool((p.get("where")  or "").strip())
            if new_has and not old_has:
                keep = True
        if keep:
            by_uid[uid] = p

    # If single-preemptor mode and preemptor not already in pods → add it as pending
    single_preemptor_mode = False
    pre_uid = None
    if isinstance(pre, dict) and pre.get("uid"):
        pre_uid = pre["uid"]
        single_preemptor_mode = True
        if pre_uid not in by_uid:
            by_uid[pre_uid] = {
                "uid": pre["uid"],
                "namespace": pre.get("namespace","default"),
                "name": pre.get("name","preemptor"),
                "cpu_m": int(pre.get("cpu_m", 0)),
                "mem_bytes": int(pre.get("mem_bytes", 0)),
                "priority": int(pre.get("priority", 0)),
                "protected": bool(pre.get("protected", False)),
                "where": "",  # pending
            }

    pods = list(by_uid.values())
    num_nodes = len(nodes)
    num_pods  = len(pods)

    node_idx = {n["name"]: j for j, n in enumerate(nodes)}

    # helpers
    STATUS_MAP = {
        cp_model.OPTIMAL:       "OPTIMAL",
        cp_model.FEASIBLE:      "FEASIBLE",
        cp_model.INFEASIBLE:    "INFEASIBLE",
        cp_model.MODEL_INVALID: "MODEL_INVALID",
        cp_model.UNKNOWN:       "UNKNOWN",
    }
    def status_str(st: int) -> str:
        return STATUS_MAP.get(st, "UNKNOWN")
    def encode_status(st: int) -> dict:
        return {"status": status_str(st)}

    def n_cpu_m(j):     return int(nodes[j]["cpu_m"])
    def n_mem_bytes(j): return int(nodes[j]["mem_bytes"])
    def p_uid(i):       return pods[i]["uid"]
    def p_ns(i):        return pods[i].get("namespace","default")
    def p_name(i):      return pods[i].get("name","")
    def p_cpu_m(i):     return int(pods[i]["cpu_m"])
    def p_mem_bytes(i): return int(pods[i]["mem_bytes"])
    def p_pri(i):       return int(pods[i].get("priority",0))
    def p_prot(i):      return bool(pods[i].get("protected", False))
    def p_where_j(i):
        w = pods[i].get("where") or ""
        return node_idx.get(w) if w else None
    def p_eligible(i, j):
        if ignore_affinity:
            return True
        return True

    if num_pods == 0:
        return {"status": "OK", "nominatedNode": "", "placements": {}, "evictions": []}

    # Index of preemptor if present
    pre_idx = None
    pre_pr  = None
    if single_preemptor_mode:
        for i in range(num_pods):
            if p_uid(i) == pre_uid:
                pre_idx = i
                pre_pr  = p_pri(i)
                break
        if pre_idx is None:
            single_preemptor_mode = False  # fallback if not found for any reason

    # Identify which pods are "running now" vs "pending"
    running_idxs = [i for i in range(num_pods) if p_where_j(i) is not None]
    pending_idxs = [i for i in range(num_pods) if p_where_j(i) is None]

    m = cp_model.CpModel()

    # Decision variables
    x = [[m.NewBoolVar(f"x_{i}_{j}") for j in range(num_nodes)] for i in range(num_pods)]
    placed = [m.NewBoolVar(f"placed_{i}") for i in range(num_pods)]
    evict  = [m.NewBoolVar(f"evict_{i}")  for i in range(num_pods)]
    kept   = [m.NewBoolVar(f"kept_{i}")   for i in range(num_pods)]
    move   = [m.NewBoolVar(f"move_{i}")   for i in range(num_pods)]

    # Assignment & eligibility
    for i in range(num_pods):
        for j in range(num_nodes):
            if not p_eligible(i, j):
                m.Add(x[i][j] == 0)
        m.Add(sum(x[i][j] for j in range(num_nodes)) == placed[i])

    # Capacity
    for j in range(num_nodes):
        m.Add(sum(x[i][j] * p_cpu_m(i) for i in range(num_pods)) <= n_cpu_m(j))
        m.Add(sum(x[i][j] * p_mem_bytes(i) for i in range(num_pods)) <= n_mem_bytes(j))

    # Keep/move indicators
    for i in range(num_pods):
        orig = p_where_j(i)
        if orig is None:  # pending
            m.Add(kept[i] == 0)
            m.Add(move[i] == 0)
        else:
            m.Add(kept[i] <= placed[i])
            m.Add(kept[i] <= x[i][orig])
            m.Add(kept[i] >= placed[i] + x[i][orig] - 1)
            m.Add(move[i] >= placed[i] - x[i][orig])
            m.Add(move[i] <= placed[i])
            m.Add(move[i] <= 1 - x[i][orig])

    # Running pods: either remain placed somewhere or get evicted (pending pods cannot be evicted)
    for i in range(num_pods):
        if p_where_j(i) is not None:
            m.Add(placed[i] + evict[i] == 1)
        else:
            m.Add(evict[i] == 0)
            m.Add(move[i] == 0)

    # ---------------- Mode-specific guards ----------------
    if single_preemptor_mode:
        # Preemptor must be placed
        m.Add(placed[pre_idx] == 1)
        m.Add(evict[pre_idx]  == 0)

        # Do not evict or move protected / >= preemptor priority
        for i in running_idxs:
            if p_prot(i) or p_pri(i) >= pre_pr:
                m.Add(evict[i] == 0)
                m.Add(move[i]  == 0)
                m.Add(placed[i] == 1)
    else:
        for i in running_idxs:
            if p_prot(i):
                m.Add(evict[i] == 0)
                m.Add(move[i]  == 0)
                m.Add(placed[i] == 1)

    # ---------------- Symmetry-breaking ----------------
    # TODO: Check if we need this
    # for j1 in range(num_nodes - 1):
    #     for j2 in range(j1 + 1, num_nodes):
    #         if n_cpu(j1) == n_cpu(j2) and n_ram(j1) == n_ram(j2):
    #             m.Add(
    #                 sum(x[i][j1] * p_ram(i) for i in range(num_pods))
    #                 <=
    #                 sum(x[i][j2] * p_ram(i) for i in range(num_pods))
    #             )

    # if num_pods > 0:
    #     node_indices = list(range(num_nodes))
    #     for i1 in range(num_pods - 1):
    #         for i2 in range(i1 + 1, num_pods):
    #             if (p_cpu(i1), p_ram(i1), p_pri(i1)) == (p_cpu(i2), p_ram(i2), p_pri(i2)):
    #                 lhs = sum(node_indices[j] * x[i1][j] for j in range(num_nodes))
    #                 rhs = sum(node_indices[j] * x[i2][j] for j in range(num_nodes))
    #                 m.Add(lhs <= rhs)

    # ---------------- Lexicographic optimization ----------------
    solver = cp_model.CpSolver()
    solver.parameters.max_time_in_seconds = max(0.1, timeout_ms / 1000.0)
    solver.parameters.num_search_workers = 8

    # Stage 1: maximize placed by priority tier (higher first)
    priorities = sorted({p_pri(i) for i in range(num_pods)}, reverse=True)
    for pr in priorities:
        idxs = [i for i in range(num_pods) if p_pri(i) == pr]
        placed_count = m.NewIntVar(0, len(idxs), f"placed_count_pr{pr}")
        m.Add(placed_count == sum(placed[i] for i in idxs))
        m.Maximize(placed_count)

        st = solver.Solve(m)
        if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
            return encode_status(st)
        m.Add(placed_count == int(solver.Value(placed_count)))

    # Stage 2: minimize evictions of currently running pods ONLY
    total_evict = m.NewIntVar(0, len(running_idxs), "total_evict_running")
    m.Add(total_evict == sum(evict[i] for i in running_idxs))
    m.Minimize(total_evict)

    st = solver.Solve(m)
    if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
        return encode_status(st)
    m.Add(total_evict == int(solver.Value(total_evict)))

    # Stage 3: minimize moves of currently running pods ONLY
    total_moves = m.NewIntVar(0, len(running_idxs), "total_moves_running")
    m.Add(total_moves == sum(move[i] for i in running_idxs))
    m.Minimize(total_moves)

    st = solver.Solve(m)
    if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
        return encode_status(st)

    # ---------------- Extract plan ----------------
    placements = {}
    evictions  = []

    for i in range(num_pods):
        if int(solver.Value(evict[i])) == 1:
            evictions.append({"uid": p_uid(i), "namespace": p_ns(i), "name": p_name(i)})
            continue
        if int(solver.Value(placed[i])) == 1:
            for j in range(num_nodes):
                if int(solver.Value(x[i][j])) == 1:
                    placements[p_uid(i)] = nodes[j]["name"]
                    break

    nominated = ""
    if single_preemptor_mode and pre_idx is not None:
        nominated = placements.get(p_uid(pre_idx), "")

    return {
        "status": status_str(st), # every call to Solve(m) re-solves the whole model (including the constraints frozen so far).
        "nominatedNode": nominated,
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
