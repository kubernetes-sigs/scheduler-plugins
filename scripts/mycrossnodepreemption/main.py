#!/usr/bin/env python3

# main.py

import os
import sys, json
from ortools.sat.python import cp_model

def _available_cpus() -> int:
    # Honor Linux cgroup/affinity limits when inside containers
    try:
        return max(1, len(os.sched_getaffinity(0)))
    except Exception:
        return max(1, os.cpu_count() or 1)

# ----------------------------- helpers ---------------------------------

STATUS_MAP = {
    cp_model.OPTIMAL:       "OPTIMAL",
    cp_model.FEASIBLE:      "FEASIBLE",
    cp_model.INFEASIBLE:    "INFEASIBLE",
    cp_model.MODEL_INVALID: "MODEL_INVALID",
    cp_model.UNKNOWN:       "UNKNOWN",
}
def _status_str(st: int) -> str:
    return STATUS_MAP.get(st, "UNKNOWN")
def _encode_status(st: int) -> dict:
    return {"status": _status_str(st)}

def solve(instance: dict) -> dict:
    nodes = instance.get("nodes") or []
    pods  = instance.get("pods")  or []
    pre   = instance.get("preemptor") or None

    if not nodes:
        return {"status": "ERROR", "message": "no nodes provided"}

    # ---- options / params ----
    timeout_ms          = int(instance.get("timeout_ms", 3000))
    ignore_affinity     = bool(instance.get("ignore_affinity", True))  # TODO: use this
    mode                = str(instance.get("mode", "lexi")).strip().lower()   # "weighted" or "lexi"
    workers             = _available_cpus() # set number of workers to the amount available
    use_decision_order  = False   # TODO: not sure if needed
    use_hints           = False  # TODO: not sure if needed; but seems to improve EveryPreemptor and BatchPostFilter modes
    log_progress        = bool(instance.get("log_progress", False))
    log_subsolvers      = bool(instance.get("log_progress", False))

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
    if num_pods == 0:
        return {"status": "OK", "nominatedNode": "", "placements": {}, "evictions": []}

    node_idx = {n["name"]: j for j, n in enumerate(nodes)}

    # field accessors
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
            single_preemptor_mode = False  # fallback if not found

    # Identify which pods are "running now" vs "pending"
    running_idxs = [i for i in range(num_pods) if p_where_j(i) is not None]
    pending_idxs = [i for i in range(num_pods) if p_where_j(i) is None]

    # --------------------- Build pruned eligibility ---------------------
    # Only keep nodes that can host the pod alone (quick screen).
    eligible = []
    for i in range(num_pods):
        cpu_i, mem_i = p_cpu_m(i), p_mem_bytes(i)
        lst = []
        for j in range(num_nodes):
            if n_cpu_m(j) >= cpu_i and n_mem_bytes(j) >= mem_i:
                # TODO: add affinity/anti-affinity checks here when you enable them
                lst.append(j)
        eligible.append(lst)

    # --------------------- Model + common constraints -------------------
    m = cp_model.CpModel()

    # Decision variables
    placed = [m.NewBoolVar(f"placed_{i}") for i in range(num_pods)]
    evict  = [m.NewBoolVar(f"evict_{i}")  for i in range(num_pods)]
    # x[i] only for eligible nodes
    x = [[m.NewBoolVar(f"x_{i}_{j}") for j in eligible[i]] for i in range(num_pods)]

    # placed <=> sum over eligible x == 1 (or 0)
    for i in range(num_pods):
        if eligible[i]:
            m.Add(sum(x[i]) == placed[i])
        else:
            # cannot be placed anywhere in the current snapshot
            m.Add(placed[i] == 0)

    # capacity
    for j in range(num_nodes):
        # sum over i where j in eligible[i]
        cap_cpu_terms = []
        cap_mem_terms = []
        for i in range(num_pods):
            if j in eligible[i]:
                idx = eligible[i].index(j)
                cap_cpu_terms.append(x[i][idx] * p_cpu_m(i))
                cap_mem_terms.append(x[i][idx] * p_mem_bytes(i))
        if cap_cpu_terms:
            m.Add(sum(cap_cpu_terms) <= n_cpu_m(j))
            m.Add(sum(cap_mem_terms) <= n_mem_bytes(j))
        else:
            # no pods eligible for this node => trivially satisfied
            pass

    # running vs pending / eviction logic
    for i in running_idxs:
        # either placed somewhere or evicted
        m.Add(placed[i] + evict[i] == 1)
    for i in pending_idxs:
        # pending pods cannot be evicted
        m.Add(evict[i] == 0)

    # move[i] only for running pods
    move = [None] * num_pods
    for i in running_idxs:
        move[i] = m.NewBoolVar(f"move_{i}")
        orig = p_where_j(i)
        m.Add(move[i] <= placed[i])
        if orig in eligible[i]:
            idx = eligible[i].index(orig)
            # if placed, either moved or stayed on orig
            m.Add(move[i] + x[i][idx] <= 1)
            m.Add(placed[i] <= move[i] + x[i][idx])
        else:
            # can't stay on original => moving iff placed
            m.Add(move[i] == placed[i])

    # Mode-specific guards
    if single_preemptor_mode and pre_idx is not None:
        # preemptor must not evict
        m.Add(evict[pre_idx]  == 0)
        pre_pr = p_pri(pre_idx)
        for i in running_idxs:
            if p_prot(i) or p_pri(i) >= pre_pr:
                # protected pods and higher priority: no evict, no move
                # equal priority: no evict, move allowed
                m.Add(placed[i] == 1)
                m.Add(evict[i] == 0)
                if move[i] is not None:
                    m.Add(move[i] == 0)
                if p_pri(i) == pre_pr:
                    # same priority: no evict, move allowed
                    m.Add(evict[i] == 0)
                    m.Add(placed[i] == 1)
    else:
        for i in running_idxs:
            if p_prot(i):
                m.Add(evict[i] == 0)
                m.Add(placed[i] == 1)
                if move[i] is not None:
                    m.Add(move[i] == 0)

    # ---------------------- decision strategies -------------------------
    # Order pods: sort by (CPU, MEM, PRI desc)
    if use_decision_order:
        order = list(range(num_pods))
        def key(i):
            return (
                p_cpu_m(i),
                p_mem_bytes(i),
                p_pri(i),
                -len(eligible[i]),
            )
        order.sort(key=key, reverse=True)
        for i in order:
            if x[i]:
                m.AddDecisionStrategy(x[i], cp_model.CHOOSE_FIRST, cp_model.SELECT_MAX_VALUE)

    # ---------------------- optional hints (both modes) -----------------
    # Add hints to keep running pods where they are if possible
    if use_hints:
        for i in running_idxs:
            orig = p_where_j(i)
            if orig in eligible[i]:
                idx = eligible[i].index(orig)
                m.AddHint(x[i][idx], 1)
                m.AddHint(placed[i], 1)
    
    # ------------------------ solve (two modes) -------------------------
    solver = cp_model.CpSolver()
    solver.parameters.max_time_in_seconds = max(0.05, timeout_ms / 1000.0)
    solver.parameters.num_search_workers  = max(1, workers)
    solver.parameters.log_search_progress       = log_progress
    solver.parameters.log_subsolver_statistics  = log_subsolvers
    solver.parameters.log_to_stdout = False  # KEEP False → logs go to stderr
    solver.log_callback = lambda line: (
        print(line, file=sys.stderr, flush=True) if line else None
    )
    
    if mode == "weighted":
        st = _solve_weighted(m, solver, placed, evict, move, running_idxs, p_pri, single_preemptor_mode, pre_idx)
    elif mode == "lexi":
        st = _solve_lexi(m, solver, placed, evict, move, running_idxs, p_pri)
    else:
        return {"status": "ERROR", "message": f"unknown mode '{mode}', expected 'weighted' or 'lexi'"}

    if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
        return _encode_status(st)
    

    # ---------------- metrics for comparison ----------------
    placed_by_priority = {}
    evicted = 0
    moved = 0
    for i in running_idxs:
        if int(solver.Value(evict[i])) == 1:
            evicted += 1
        else:
            if move[i] is not None and int(solver.Value(move[i])) == 1:
                moved += 1
    for i in range(num_pods):
        if int(solver.Value(placed[i])) == 1:
            pr = str(p_pri(i))
            placed_by_priority[pr] = placed_by_priority.get(pr, 0) + 1
    score = {
        "placed_by_priority": placed_by_priority,
        "evicted": evicted,
        "moved": moved,
    }

    # ---------------------- extract plan ----------------------
    placements = {}
    evictions  = []

    for i in range(num_pods):
        if int(solver.Value(evict[i])) == 1:
            evictions.append({"uid": p_uid(i), "namespace": p_ns(i), "name": p_name(i)})
            continue

        if int(solver.Value(placed[i])) == 1 and eligible[i]:
            # find the chosen node among eligible list
            chosen_j = None
            for local, j in enumerate(eligible[i]):
                if int(solver.Value(x[i][local])) == 1:
                    chosen_j = j
                    break
            if chosen_j is None:
                continue

            orig_j = p_where_j(i)  # None for pending pods
            # Emit placement only if this pod is pending OR it actually moved
            if orig_j is None or (move[i] is not None and int(solver.Value(move[i])) == 1):
                placements[p_uid(i)] = nodes[chosen_j]["name"]

    nominated = ""
    if single_preemptor_mode and pre_idx is not None:
        nominated = placements.get(p_uid(pre_idx), "")

    return {
        "status": _status_str(st),
        "nominatedNode": nominated,
        "placements": placements,
        "evictions": evictions,
        "score": score,
    }

# ---------------------- optimization modes -----------------------------
def _solve_weighted(m, solver, placed, evict, move, running_idxs, p_pri, single_preemptor_mode, pre_idx):
    """
    Single-solve big-M objective:
      Max sum(placed_i * W_PRIO * priority_i) - W_EVICT * sum(evict_running) - W_MOVE * sum(move_running)
    """
    W_PRIO  = 10_000_000
    W_EVICT = 10_000
    W_MOVE  = 1

    reward_terms = [placed[i] * (W_PRIO * p_pri(i)) for i in range(len(placed))]
    evict_terms  = [evict[i] for i in running_idxs]
    move_terms   = [move[i] for i in running_idxs if move[i] is not None]

    m.Maximize(sum(reward_terms) - W_EVICT * sum(evict_terms) - W_MOVE * sum(move_terms))
    return solver.Solve(m)

def _solve_lexi(m, solver, placed, evict, move, running_idxs, p_pri):
    """
    Strict lexicographic objective:
      (1) maximize placed count per priority tier (high→low)
      (2) minimize evictions of running pods
      (3) minimize moves of running pods
    """
    # Stage 1: maximize placed by priority tier (higher first)
    num_pods = len(placed)
    priorities = sorted({p_pri(i) for i in range(num_pods)}, reverse=True)
    for pr in priorities:
        idxs = [i for i in range(num_pods) if p_pri(i) == pr]
        if not idxs:
            continue
        placed_count = m.NewIntVar(0, len(idxs), f"placed_count_pr{pr}")
        m.Add(placed_count == sum(placed[i] for i in idxs))
        m.Maximize(placed_count)
        st = solver.Solve(m)
        if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
            return st
        # Freeze that tier’s optimum
        m.Add(placed_count == int(solver.Value(placed_count)))

    # Stage 2: minimize evictions (running only)
    total_evict = m.NewIntVar(0, len(running_idxs), "total_evict_running")
    m.Add(total_evict == sum(evict[i] for i in running_idxs))
    m.Minimize(total_evict)
    st = solver.Solve(m)
    if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
        return st
    m.Add(total_evict == int(solver.Value(total_evict)))

    # Stage 3: minimize moves (running only)
    move_terms = [move[i] for i in running_idxs if move[i] is not None]
    max_moves  = len(move_terms)
    total_moves = m.NewIntVar(0, max_moves, "total_moves_running")
    if move_terms:
        m.Add(total_moves == sum(move_terms))
    else:
        m.Add(total_moves == 0)
    m.Minimize(total_moves)
    return solver.Solve(m)

# ------------------------------- main -----------------------------------

def main():
    raw = sys.stdin.read()
    inst = json.loads(raw or "{}")
    out = solve(inst if isinstance(inst, dict) else {})
    print(json.dumps(out))

if __name__ == "__main__":
    main()
