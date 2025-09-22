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
# TODO: reach here in this file

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
    ignore_affinity     = bool(instance.get("ignore_affinity", True))  # TODO_HC: consider to use this
    workers             = _available_cpus() # set number of workers to the amount available
    use_hints           = bool(instance.get("use_hints", False))
    hints               = instance.get("hints") if use_hints else None
    log_progress        = bool(instance.get("log_progress", False))
    log_subsolvers      = bool(instance.get("log_progress", False))

    # --- De-dup by UID, prefer entries that have 'node' (running) ---
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
            old_has = bool((old.get("node") or "").strip())
            new_has = bool((p.get("node")  or "").strip())
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
                "req_cpu_m": int(pre.get("req_cpu_m", 0)),
                "req_mem_bytes": int(pre.get("req_mem_bytes", 0)),
                "priority": int(pre.get("priority", 0)),
                "protected": bool(pre.get("protected", False)),
                "node": "",  # pending
            }

    pods = list(by_uid.values())
    num_nodes = len(nodes)
    num_pods  = len(pods)
    if num_pods == 0:
        return {"status": "OK", "placements": [], "evictions": []}

    node_idx = {n["name"]: j for j, n in enumerate(nodes)}

    # field accessors
    def n_cap_cpu_m(j):     return int(nodes[j]["cap_cpu_m"])
    def n_cap_mem_bytes(j): return int(nodes[j]["cap_mem_bytes"])

    def p_uid(i):       return pods[i]["uid"]
    def p_ns(i):        return pods[i].get("namespace","default")
    def p_name(i):      return pods[i].get("name","")
    def p_req_cpu_m(i):     return int(pods[i]["req_cpu_m"])
    def p_req_mem_bytes(i): return int(pods[i]["req_mem_bytes"])
    def p_pri(i):       return int(pods[i].get("priority",0))
    def p_prot(i):      return bool(pods[i].get("protected", False))
    def p_node_j(i):
        w = pods[i].get("node") or ""
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
    running_idxs = [i for i in range(num_pods) if p_node_j(i) is not None]
    pending_idxs = [i for i in range(num_pods) if p_node_j(i) is None]

    # --------------------- Build pruned eligibility ---------------------
    # Only keep nodes that can host the pod alone (quick screen).
    eligible = []
    for i in range(num_pods):
        cpu_i, mem_i = p_req_cpu_m(i), p_req_mem_bytes(i)
        lst = []
        for j in range(num_nodes):
            if n_cap_cpu_m(j) >= cpu_i and n_cap_mem_bytes(j) >= mem_i:
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
        # sum over i node j in eligible[i]
        cap_cpu_terms = []
        cap_mem_terms = []
        for i in range(num_pods):
            if j in eligible[i]:
                idx = eligible[i].index(j)
                cap_cpu_terms.append(x[i][idx] * p_req_cpu_m(i))
                cap_mem_terms.append(x[i][idx] * p_req_mem_bytes(i))
        if cap_cpu_terms:
            m.Add(sum(cap_cpu_terms) <= n_cap_cpu_m(j))
            m.Add(sum(cap_mem_terms) <= n_cap_mem_bytes(j))
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
        orig = p_node_j(i)
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
        # preemptor must be placed and never evicted
        m.Add(placed[pre_idx] == 1)
        m.Add(evict[pre_idx]  == 0)
        pre_pr = p_pri(pre_idx)

        for i in running_idxs:
            if p_prot(i) or p_pri(i) > pre_pr:
                # Protected OR higher-priority than preemptor:
                # must stay put (no evict, no move)
                m.Add(placed[i] == 1)
                m.Add(evict[i] == 0)
                if move[i] is not None:
                    m.Add(move[i] == 0)

            elif p_pri(i) == pre_pr:
                # Equal priority to preemptor:
                # no evict, but moves ARE allowed
                m.Add(placed[i] == 1)
                m.Add(evict[i] == 0)

            else:
                # Lower-priority than preemptor:
                # no extra guard — solver may move or evict as needed
                pass
    else:
        # In batch mode (no single preemptor):
        # Protected pods must stay put (no evict, no move)
        # Compared to single-preemptor mode, we allow to move all already running pods no matter their priority.
        # Also we do not restrict any evictions, however, the objective will try to minimize them, so in practice it should be ok.
        for i in running_idxs:
            if p_prot(i):
                m.Add(evict[i] == 0)
                m.Add(placed[i] == 1)
                if move[i] is not None:
                    m.Add(move[i] == 0)
        m.Add(sum(placed[i] for i in pending_idxs) >= 1)

    # ---------------------- constraints from Go hints (min placed by priority only) -------------------
    if use_hints and isinstance(hints, dict):
        hp_raw = (hints.get("placed_by_priority") or {})
        if hp_raw:
            priorities_desc = sorted({p_pri(i) for i in range(num_pods)}, reverse=True)

            # normalize hint keys -> int
            hinted_exact = {int(k): int(v) for k, v in hp_raw.items()
                            if str(k).lstrip("-").isdigit() and int(v) > 0}

            # precompute indices + placeable cap + current baseline (running)
            idxs_ge_map, cap_ge_map, baseline_ge_map = {}, {}, {}
            for pr in priorities_desc:
                idxs_ge = [i for i in range(num_pods) if p_pri(i) >= pr]
                idxs_ge_map[pr] = idxs_ge
                cap_ge_map[pr]  = sum(1 for i in idxs_ge if eligible[i])  # upper bound feasibility guard
                baseline_ge_map[pr] = sum(1 for i in idxs_ge if i in running_idxs)

            # cumulative ≥pr from exact-tier hints
            need_cum = 0
            for pr in priorities_desc:
                need_cum += hinted_exact.get(pr, 0)

                idxs_ge = idxs_ge_map[pr]
                if not idxs_ge:
                    continue

                # Clamp to what’s placeable; also never below current baseline
                need_clamped = min(need_cum, cap_ge_map[pr])
                lb = max(baseline_ge_map[pr], need_clamped)

                # In single-preemptor mode, don't let hints over-constrain tiers at/under the preemptor's priority
                if single_preemptor_mode and pre_idx is not None and pr <= pre_pr:
                    lb = baseline_ge_map[pr]  # keep non-degradation only

                if lb > 0:
                    m.Add(sum(placed[i] for i in idxs_ge) >= lb)
    else:
        # Non-degradation on cumulative tiers (safe for single-preemptor too)
        priorities_desc = sorted({p_pri(i) for i in range(num_pods)}, reverse=True)
        for pr in priorities_desc:
            idxs_ge = [i for i in range(num_pods) if p_pri(i) >= pr]
            baseline = sum(1 for i in idxs_ge if i in running_idxs)
            if idxs_ge:
                m.Add(sum(placed[i] for i in idxs_ge) >= baseline)
    
    # ------------------------ solve (two modes) -------------------------
    solver = cp_model.CpSolver()
    solver.parameters.max_time_in_seconds = max(1, timeout_ms / 1000.0)
    solver.parameters.num_search_workers  = max(1, workers)
    solver.parameters.log_search_progress       = log_progress
    solver.parameters.log_subsolver_statistics  = log_subsolvers
    solver.parameters.log_to_stdout = False  # KEEP False → logs go to stderr
    solver.log_callback = lambda line: (
        print(line, file=sys.stderr, flush=True) if line else None
    )
    
    st = _solve_lexi(m, solver, placed, evict, move, running_idxs, p_pri)

    if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
        return _encode_status(st)

    # ---------------------- extract plan ----------------------
    placements = []
    evictions  = []

    for i in range(num_pods):
        if int(solver.Value(evict[i])) == 1:
            evictions.append({
                "pod": {"uid": p_uid(i), "namespace": p_ns(i), "name": p_name(i)},
                "node": nodes[p_node_j(i)]["name"],
            })
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

            orig_j = p_node_j(i)  # None for pending pods
            # Emit placement only if this pod is pending OR it actually moved
            if orig_j is None or (move[i] is not None and int(solver.Value(move[i])) == 1):
                placements.append({
                    "pod": {
                        "uid": p_uid(i),
                        "namespace": p_ns(i),
                        "name": p_name(i),
                    },
                    "fromNode": nodes[orig_j]["name"] if orig_j is not None else "",
                    "toNode": nodes[chosen_j]["name"],
                })

    return {
        "status": _status_str(st),
        "placements": placements,
        "evictions": evictions,
    }

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
