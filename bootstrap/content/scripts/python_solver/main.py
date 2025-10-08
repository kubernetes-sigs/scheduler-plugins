#!/usr/bin/env python3

# main.py

import os, time, sys, json
from typing import Union
from ortools.sat.python import cp_model

#################################################
# --- Constants ---------------------------------
#################################################
STATUS_MAP = {
    cp_model.OPTIMAL:       "OPTIMAL",
    cp_model.FEASIBLE:      "FEASIBLE",
    cp_model.INFEASIBLE:    "INFEASIBLE",
    cp_model.MODEL_INVALID: "MODEL_INVALID",
    cp_model.UNKNOWN:       "UNKNOWN",
}
NO_NODES = "NO_NODES"
NO_PODS = "NO_PODS"

class CPSATSolver:
    #################################################
    # --- Helpers -----------------------------------
    #################################################
    @staticmethod
    def _available_cpus() -> int:
        # Honor Linux cgroup/affinity limits when inside containers
        try:
            return max(1, len(os.sched_getaffinity(0)))
        except Exception:
            return max(1, os.cpu_count() or 1)

    @classmethod
    def _status_str(cls, st: Union[int, str]) -> str:
        if isinstance(st, str):
            return st
        return STATUS_MAP.get(st, "UNKNOWN")

    def solve(self, instance: dict) -> dict:
        _started_at = time.monotonic()
        
        #################################################
        # --- Read Input --------------------------------
        #################################################
        nodes       = instance.get("nodes") or []
        pods        = instance.get("pods")  or []
        preemptor   = instance.get("preemptor") or None

        if not nodes:
            return {"status": self._status_str(NO_NODES)}

        #################################################
        # --- Options/Parameters ------------------------
        #################################################
        timeout_ms              = int(instance.get("timeout_ms", 3000))
        ignore_affinity         = bool(instance.get("ignore_affinity", True)) # TODO: consider to use this
        workers                 = self._available_cpus() # set number of workers to the amount available
        use_hints               = bool(instance.get("use_hints", False))
        # If use_strictly_improving is true, accept a plan only if it is strictly better than the current state under the objective. 
        # This prevents no-op plans but may reject globally optimal plans whose objective ties the baseline. Set False to allow equal-score solutions.
        use_strictly_improving  = bool(instance.get("use_strictly_improving", False)) # TODO: Input interface doesn't implement this yet, however we enforce it
        use_symmetry_breaking   = bool(instance.get("symmetry_breaking", False))
        hints                   = instance.get("hints") if use_hints else None
        log_progress            = bool(instance.get("log_progress", False))
        log_subsolvers          = bool(instance.get("log_progress", False))

        #################################################
        # --- Solver Options ----------------------------
        #################################################
        solver = cp_model.CpSolver()
        solver.parameters.max_time_in_seconds       = max(1, timeout_ms / 1000.0)
        solver.parameters.num_search_workers        = max(1, workers)
        solver.parameters.log_search_progress       = log_progress
        solver.parameters.log_subsolver_statistics  = log_subsolvers
        solver.parameters.log_to_stdout             = False  # KEEP False → logs go to stderr
        solver.parameters.relative_gap_limit        = 0.05 # allowed relative gap (0.0 = exact, 0.05 = 5% of optimum)
        solver.log_callback = lambda line: (
            print(line, file=sys.stderr, flush=True) if line else None
        )

        # Single global deadline (timeout_ms <= 0 means "no limit")
        _deadline_s = (_started_at + (timeout_ms / 1000.0)) if timeout_ms and timeout_ms > 0 else None
        def _remaining_seconds() -> float | None:
            """Return remaining seconds until the global deadline, or None if unlimited."""
            if _deadline_s is None:
                return None
            return max(0.0, _deadline_s - time.monotonic())

        #################################################
        # --- Ensure Pods from Input --------------------
        #################################################
        # We may have multiple records for the same pod UID.
        # Therefore we keep only one record per UID, preferring
        # the first one that is currently assigned to a node (if any).
        by_uid = {}
        for p in pods:
            uid = p.get("uid")
            if not uid:
                continue
            old = by_uid.get(uid)
            if old is None:
                # First time we see this UID: store it.
                by_uid[uid] = p
                continue
            # Prefer a record that shows the pod is assigned to a node.
            old_has_node = bool((old.get("node") or "").strip())
            new_has_node = bool((p.get("node")  or "").strip())
            # Only upgrade from "no node" -> "has node".
            if new_has_node and not old_has_node:
                by_uid[uid] = p

        #################################################
        # --- Single-Preemptor Setup --------------------
        #################################################
        # If single-preemptor mode and preemptor 
        # not already in pods, add it as pending.
        single_preemptor_mode = False
        preemptor_uid = None
        if isinstance(preemptor, dict) and preemptor.get("uid"):
            preemptor_uid = preemptor["uid"]
            single_preemptor_mode = True
            if preemptor_uid not in by_uid:
                by_uid[preemptor_uid] = {
                    "uid": preemptor["uid"],
                    "namespace": preemptor.get("namespace","default"),
                    "name": preemptor.get("name","preemptor"),
                    "req_cpu_m": int(preemptor.get("req_cpu_m", 0)),
                    "req_mem_bytes": int(preemptor.get("req_mem_bytes", 0)),
                    "priority": int(preemptor.get("priority", 0)),
                    "protected": bool(preemptor.get("protected", False)),
                    "node": "",  # add it as pending
                }

        #################################################
        # --- Freeze nodes and pods and quick checks ----
        #################################################
        pods = list(by_uid.values())
        num_nodes = len(nodes)
        num_pods  = len(pods)
        if num_pods == 0:
            return {"status": self._status_str(NO_PODS)}
        node_idx = {n["name"]: j for j, n in enumerate(nodes)}

        #################################################
        # --- Field Accessors ---------------------------
        #################################################
        def n_cap_cpu_m(j):     return int(nodes[j]["cap_cpu_m"])
        def n_cap_mem_bytes(j): return int(nodes[j]["cap_mem_bytes"])
        def p_uid(i):           return pods[i]["uid"]
        def p_namespace(i):     return pods[i].get("namespace","default")
        def p_name(i):          return pods[i].get("name","")
        def p_req_cpu_m(i):     return int(pods[i]["req_cpu_m"])
        def p_req_mem_bytes(i): return int(pods[i]["req_mem_bytes"])
        def p_priority(i):      return int(pods[i].get("priority",0))
        def p_protected(i):     return bool(pods[i].get("protected", False))
        def p_node_j(i):
            w = pods[i].get("node") or ""
            return node_idx.get(w) if w else None

        # Index of preemptor if present
        preemptor_idx = None
        preemptor_priority  = None
        if single_preemptor_mode:
            for i in range(num_pods):
                if p_uid(i) == preemptor_uid:
                    preemptor_idx = i
                    preemptor_priority = p_priority(i)
                    break
            if preemptor_idx is None:
                single_preemptor_mode = False # fallback if not found

        # Indices of "running now" vs "pending"
        running_idxs = [i for i in range(num_pods) if p_node_j(i) is not None]
        pending_idxs = [i for i in range(num_pods) if p_node_j(i) is None]

        #################################################
        # --- Build Eligible Nodes ----------------------
        #################################################
        # Only consider nodes that can host the pod alone.
        eligible_nodes = []
        for i in range(num_pods):
            cpu_i, mem_i = p_req_cpu_m(i), p_req_mem_bytes(i)
            lst = []
            for j in range(num_nodes):
                if n_cap_cpu_m(j) >= cpu_i and n_cap_mem_bytes(j) >= mem_i:
                    lst.append(j)
            eligible_nodes.append(lst)

        #################################################
        # --- Initiate Model ----------------------------
        #################################################
        model = cp_model.CpModel()

        #################################################
        # --- Decision Variables ------------------------
        #################################################
        # placed (bool) - whether model places pod i somewhere
        # assign (bool) - whether model places pod i on node j (only for eligible nodes)
        # move   (bool) - whether model moves pod i (only for running pods)
        placed = [model.NewBoolVar(f"placed_{i}") for i in range(num_pods)]
        assign = [[model.NewBoolVar(f"assign_{i}_{j}") for j in eligible_nodes[i]] for i in range(num_pods)]
        move = [model.NewBoolVar(f"move_{i}") if i in running_idxs else None for i in range(num_pods)]
        
        #################################################
        # --- Common Constraints ------------------------
        #################################################
        ### Node constraints - capacity per node
        for j in range(num_nodes):
            cap_cpu_terms = []
            cap_mem_terms = []
            # Sum over i node j in eligible[i]
            for i in range(num_pods):
                if j in eligible_nodes[i]:
                    idx = eligible_nodes[i].index(j)
                    cap_cpu_terms.append(assign[i][idx] * p_req_cpu_m(i))
                    cap_mem_terms.append(assign[i][idx] * p_req_mem_bytes(i))
            if cap_cpu_terms:
                model.Add(sum(cap_cpu_terms) <= n_cap_cpu_m(j))
                model.Add(sum(cap_mem_terms) <= n_cap_mem_bytes(j))

        ### Assign constraints - exactly one assignment if placed
        for i in range(num_pods):
            if eligible_nodes[i]:
                model.Add(sum(assign[i]) == placed[i])
            else: # cannot be placed
                model.Add(placed[i] == 0)

        ### Move constraints - only for running pods
        for i in running_idxs:
            orig = p_node_j(i)
            model.Add(move[i] <= placed[i])
            if orig in eligible_nodes[i]: # if place, either moved or stayed on orig
                idx = eligible_nodes[i].index(orig)
                model.Add(move[i] + assign[i][idx] <= 1)
                model.Add(placed[i] <= move[i] + assign[i][idx])
            else: # can't stay on original -> moving iff place
                model.Add(move[i] == placed[i])
            # Protected pods must stay put (no move)
            if p_protected(i):
                model.Add(placed[i] == 1)
                if move[i] is not None:
                    model.Add(move[i] == 0)

        #################################################
        # --- Mode Specific Constraints -----------------
        #################################################
        # Single-preemptor mode:
        if single_preemptor_mode and preemptor_idx is not None:
            # Preemptor must be place
            model.Add(placed[preemptor_idx] == 1)
        # Batch mode
        else:
            # There must be at least one placement of pending pods
            # (weak constraint to avoid trivial empty solution)
            # Below we will enforce non-degradation on priorities
            model.Add(sum(placed[i] for i in pending_idxs) >= 1)

        #################################################
        # --- Placed by Priority Constraints ------------
        #################################################
        priorities = sorted({p_priority(i) for i in range(num_pods)}, reverse=True) # priorities: high->low
        
        # External hints (placed_by_priority) constraints
        if use_hints and isinstance(hints, dict):
            raw = hints.get("placed_by_priority") or {}
            # normalize to int keys/values, ignore non-positive
            exact = {int(k): int(v) for k, v in raw.items()
                    if str(k).lstrip("-").isdigit() and int(v) > 0}
            if exact:
                # build cumulative >=pr demand from exact-tier hints
                cum_need = {}
                acc = 0
                for priority in priorities:
                    acc += exact.get(priority, 0)
                    cum_need[priority] = acc
                for priority in priorities:
                    idxs_ge = [i for i in range(num_pods) if p_priority(i) >= priority]
                    if not idxs_ge:
                        continue
                    # feasibility guards
                    cap_ge  = sum(1 for i in idxs_ge if eligible_nodes[i]) # placeable upper bound
                    base_ge = sum(1 for i in idxs_ge if i in running_idxs) # non-degradation baseline
                    need_ge = cum_need.get(priority, 0)
                    # lower bound for this tier
                    lower_bound = max(base_ge, min(need_ge, cap_ge))
                    # in single-preemptor mode, don't push tiers at/under the preemptor
                    if single_preemptor_mode and preemptor_idx is not None and priority <= preemptor_priority:
                        lower_bound = base_ge
                    if lower_bound > 0:
                        model.Add(sum(placed[i] for i in idxs_ge) >= lower_bound)
        
        # If no hints, enforce non-degradation per priority tier
        else:
            for priority in priorities:
                idxs_ge = [i for i in range(num_pods) if p_priority(i) >= priority]
                if not idxs_ge:
                    continue
                base_ge = sum(1 for i in idxs_ge if i in running_idxs)
                # weak constraint: non-degradation at ≥priority
                # to avoid trivial empty solution
                model.Add(sum(placed[i] for i in idxs_ge) >= base_ge)
        
        # Enforce at least one tier to strictly improve
        if use_strictly_improving:
            # Require at least one strict improvement on some priority tier
            # To avoid trivial solutions that do not improve anything
            improved_at = [] # improved_at[priority] == 1 means strictly improved at ≥pr
            for priority in priorities:
                # If you have single-preemptor rules and want to forbid improving ≥ preemptor tier:
                if single_preemptor_mode and preemptor_idx is not None and priority >= preemptor_priority:
                    continue
                # Get indices of pods at this tier or above
                idxs_ge = [i for i in range(num_pods) if p_priority(i) >= priority]
                if not idxs_ge: # no pods at this tier
                    continue
                # Current baseline at this tier (running count)
                base_ge = sum(1 for i in idxs_ge if i in running_idxs)
                # Strictly improved at ≥pr"
                improved = model.NewBoolVar(f"strict_improve_ge_{priority}")
                # If imp==1, enforce ≥ base+1 at that tier
                # +1 means one more placement than current running count at that tier
                model.Add(sum(placed[i] for i in idxs_ge) >= base_ge + 1).OnlyEnforceIf(improved)
                # If imp==0, no extra requirement for that tier (non-degradation already applies)
                improved_at.append(improved)
            if improved_at:
                model.AddBoolOr(improved_at) # at least one tier must strictly improve.

        #################################################
        # --- Symmetry breaking (optional, safe)---------
        #################################################
        if use_symmetry_breaking:
            # Signatures for equivalence
            def _pod_signature(i):
                return (
                    p_req_cpu_m(i),
                    p_req_mem_bytes(i),
                    p_priority(i),
                    bool(p_protected(i)),
                )

            def _node_signature(j):
                return (
                    n_cap_cpu_m(j),
                    n_cap_mem_bytes(j),
                    # If you add labels: tuple(sorted(nodes[j].get("labels", {}).items()))
                )

            # Precompute a stable eligible list tuple for comparison
            eligible_tuple = [tuple(eligible_nodes[i]) for i in range(num_pods)]

            # ---------- (A) Equivalent nodes: non-decreasing loads ----------
            # This one is generally safe.
            node_cpu_load = [None] * num_nodes
            node_mem_load = [None] * num_nodes
            for j in range(num_nodes):
                cpu_terms, mem_terms = [], []
                for i in range(num_pods):
                    if j in eligible_nodes[i]:
                        idx = eligible_nodes[i].index(j)
                        cpu_terms.append(assign[i][idx] * p_req_cpu_m(i))
                        mem_terms.append(assign[i][idx] * p_req_mem_bytes(i))
                node_cpu_load[j] = model.NewIntVar(0, n_cap_cpu_m(j), f"cpu_load_node_{j}")
                node_mem_load[j] = model.NewIntVar(0, n_cap_mem_bytes(j), f"mem_load_node_{j}")
                model.Add(node_cpu_load[j] == (sum(cpu_terms) if cpu_terms else 0))
                model.Add(node_mem_load[j] == (sum(mem_terms) if mem_terms else 0))

            for j in range(num_nodes):
                sigj = _node_signature(j)
                for k in range(j + 1, num_nodes):
                    if _node_signature(k) == sigj:
                        model.Add(node_cpu_load[j] <= node_cpu_load[k])
                        model.Add(node_mem_load[j] <= node_mem_load[k])

            # ---------- (B) Equivalent pods: guarded lex ordering on one-hots ----------
            # Apply only to: pending, non-protected, same priority, identical eligibility lists.
            # This avoids conflicts with fixed placements and domain differences.
            # For p1 < p2, enforce that p1 picks an equal/earlier node than p2 (lex).
            for p1 in range(num_pods):
                if p_protected(p1) or (p_node_j(p1) is not None):
                    continue  # skip fixed/protected/running
                sig1 = _pod_signature(p1)
                elig1 = eligible_tuple[p1]
                if not elig1:
                    continue  # unplaceable → nothing to order
                for p2 in range(p1 + 1, num_pods):
                    if p_protected(p2) or (p_node_j(p2) is not None):
                        continue
                    if _pod_signature(p2) != sig1:
                        continue
                    if eligible_tuple[p2] != elig1:
                        continue  # domains differ → do not couple

                    # Guard by both placed
                    both_placed = model.NewBoolVar(f"both_placed_{p1}_{p2}")
                    # both_placed ⇔ (placed[p1] & placed[p2])
                    # Implement as both_placed <= placed[p1], both_placed <= placed[p2], and
                    # placed[p1] + placed[p2] - 1 <= both_placed
                    model.Add(both_placed <= placed[p1])
                    model.Add(both_placed <= placed[p2])
                    model.Add(placed[p1] + placed[p2] - 1 <= both_placed)

                    # Lex (prefix) ordering on their assignment vectors
                    # Let a1[t] = assign[p1][t], a2[t] = assign[p2][t] aligned over identical eligible lists
                    # For each prefix k: sum_{t<=k} a1[t] >= sum_{t<=k} a2[t]  (only if both placed)
                    for k in range(len(elig1)):
                        lhs = sum(assign[p1][t] for t in range(k + 1))
                        rhs = sum(assign[p2][t] for t in range(k + 1))
                        # lhs >= rhs when both placed; otherwise no restriction
                        model.Add(lhs >= rhs).OnlyEnforceIf(both_placed)

                
        #################################################
        # --- Lexicographic optimization (sequential) ---
        # Order per tier (≥priority):
        #   1) maximize placed
        #   2) minimize moves (running only)
        #################################################
        
        # Helper to set objective and solve one sub-stage.
        def _solve_stage(obj_expr, sense: str) -> int:
            # set objective
            if sense == "max":
                model.Maximize(obj_expr)
            else:
                model.Minimize(obj_expr)
            # give only the remaining time to this stage
            rem = _remaining_seconds()
            if rem is not None:
                # CP-SAT accepts fractional seconds; keep a tiny epsilon if almost zero
                solver.parameters.max_time_in_seconds = max(1e-3, rem-0.05)
            # if rem is None, we leave the param unchanged => unlimited
            return solver.Solve(model)
        
        # Build cumulative placed count vars: placed_ge[pr] = #placed with priority ≥ pr
        placed_ge: dict[int, cp_model.IntVar] = {}
        for pr in priorities:
            idxs_ge = [i for i in range(num_pods) if p_priority(i) >= pr]
            if not idxs_ge:
                continue
            v = model.NewIntVar(0, len(idxs_ge), f"placed_ge_{pr}")
            model.Add(v == sum(placed[i] for i in idxs_ge))
            placed_ge[pr] = v

        # Build cumulative moves-by-priority: moves_ge[pr] = #moves among running pods with priority ≥ pr
        moves_ge: dict[int, cp_model.IntVar] = {}
        for pr in priorities:
            idxs_ge_running = [i for i in running_idxs if p_priority(i) >= pr]
            v = model.NewIntVar(0, len(idxs_ge_running), f"moves_ge_{pr}")
            if idxs_ge_running:
                model.Add(v == sum(move[i] for i in idxs_ge_running))
            else:
                model.Add(v == 0)
            moves_ge[pr] = v
        move_terms = [move[i] for i in running_idxs if move[i] is not None]
        total_moves = model.NewIntVar(0, len(move_terms), "total_moves")
        if move_terms:
            model.Add(total_moves == sum(move_terms))
        else:
            model.Add(total_moves == 0)

        active_tiers = [pr for pr in priorities if pr in placed_ge]
        if not active_tiers:
            return {"status": self._status_str("NO_TIERS")}

        # (A) Maximize cumulative placements at ≥ priority, one tier at a time
        # Lock each tier's plateau before moving to the next tier.
        placement_all_optimal = True
        
        for pr in active_tiers:
            # 1) Maximize placements at ≥pr
            status = _solve_stage(placed_ge[pr], "max")
            if status not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
                return {"status": self._status_str(status)}
            if status != cp_model.OPTIMAL:
                placement_all_optimal = False
            best_placed = solver.Value(placed_ge[pr])
            model.Add(placed_ge[pr] == best_placed)  # lock plateau

            # 2) Minimize moves at ≥pr (protect high-priority moves first)
            status = _solve_stage(moves_ge[pr], "min")
            if status not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
                return {"status": self._status_str(status)}
            best_moves = solver.Value(moves_ge[pr])
            model.Add(moves_ge[pr] == best_moves)  # lock moves at this tier

        # compute final status per your rule
        if placement_all_optimal and status in (cp_model.OPTIMAL, cp_model.FEASIBLE):
            final_status = cp_model.OPTIMAL
        else:
            final_status = status

        #################################################
        # --- Extract and Return Plan -------------------
        #################################################
        placements = []
        evictions  = []

        for i in range(num_pods):
            was_running = (p_node_j(i) is not None)
            now_placed  = int(solver.Value(placed[i])) == 1
            if was_running and not now_placed:
                evictions.append({
                    "pod": {"uid": p_uid(i), "namespace": p_namespace(i), "name": p_name(i)},
                    "node": nodes[p_node_j(i)]["name"],
                })
                continue
            # Placed pods
            if int(solver.Value(placed[i])) == 1 and eligible_nodes[i]:
                # find the chosen node among eligible list
                chosen_j = None
                for local, j in enumerate(eligible_nodes[i]):
                    if int(solver.Value(assign[i][local])) == 1:
                        chosen_j = j
                        break
                if chosen_j is None:
                    continue
                orig_j = p_node_j(i) # None for pending pods
                # Emit placement only if this pod is pending OR it actually moved
                if orig_j is None or (move[i] is not None and int(solver.Value(move[i])) == 1):
                    placements.append({
                        "pod": {
                            "uid": p_uid(i),
                            "namespace": p_namespace(i),
                            "name": p_name(i),
                        },
                        "from_node": nodes[orig_j]["name"] if orig_j is not None else "",
                        "to_node": nodes[chosen_j]["name"],
                    })

        # Return status, placements, evictions
        return {
            "status": self._status_str(final_status),
            "placements": placements,
            "evictions": evictions,
        }

#################################################
# --- Main --------------------------------------
#################################################
def main():
    try:
        raw = sys.stdin.read()
        inst = json.loads(raw or "{}")
        solver = CPSATSolver()
        out = solver.solve(inst if isinstance(inst, dict) else {})
        print(json.dumps(out))
    except Exception as e:
        # Always return a JSON object and exit 0 so Go doesn't see "status 1"
        err = {"status": "PYTHON_EXCEPTION", "error": str(e)}
        try:
            print(json.dumps(err))
        except Exception:
            # last resort
            print('{"status":"PYTHON_EXCEPTION","error":"unserializable error"}')

if __name__ == "__main__":
    main()
