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
        #solver.parameters.search_branching = cp_model.PORTFOLIO_SEARCH
        #solver.parameters.cp_model_presolve = False
        #solver.parameters.cp_model_probing_level = 1  # 0=off, 1=basic, 2=aggressive
        #solver.parameters.max_presolve_iterations = 1
        #solver.parameters.linearization_level = 2
        #solver.parameters.keep_all_feasible_solutions_in_presolve = True
        solver.parameters.log_subsolver_statistics  = log_subsolvers
        solver.parameters.log_to_stdout             = False  # KEEP False → logs go to stderr
        solver.parameters.relative_gap_limit        = 0.00 # allowed relative gap (0.0 = exact, 0.05 = 5% of optimum)
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
        pod_by_uid = {}
        for p in pods:
            uid = p.get("uid")
            if not uid:
                continue
            old = pod_by_uid.get(uid)
            if old is None:
                # First time we see this UID: store it.
                pod_by_uid[uid] = p
                continue
            # Prefer a record that shows the pod is assigned to a node.
            old_has_node = bool((old.get("node") or "").strip())
            new_has_node = bool((p.get("node")  or "").strip())
            # Only upgrade from "no node" -> "has node".
            if new_has_node and not old_has_node:
                pod_by_uid[uid] = p

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
            if preemptor_uid not in pod_by_uid:
                pod_by_uid[preemptor_uid] = {
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
        pods = list(pod_by_uid.values())
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
        placed = [model.NewBoolVar(f"placed_{i}") for i in range(num_pods)]
        assign = [[model.NewBoolVar(f"assign_{i}_{j}") for j in eligible_nodes[i]]
            for i in range(num_pods)]
        
        #################################################
        # --- Common Constraints ------------------------
        #################################################
        ### Node constraints - capacity per node
        eligible_pos = [{j: pos for pos, j in enumerate(eligible_nodes[i])}
                for i in range(num_pods)]
        for j in range(num_nodes):
            cap_cpu_terms, cap_mem_terms = [], []
            for i in range(num_pods):
                pos = eligible_pos[i].get(j)
                if pos is not None:
                    cap_cpu_terms.append(assign[i][pos] * p_req_cpu_m(i))
                    cap_mem_terms.append(assign[i][pos] * p_req_mem_bytes(i))
            if cap_cpu_terms:
                model.Add(sum(cap_cpu_terms) <= n_cap_cpu_m(j))
                model.Add(sum(cap_mem_terms) <= n_cap_mem_bytes(j))

        ### Assign constraints - exactly one assignment if placed
        for i in range(num_pods):
            if eligible_nodes[i]:
                model.Add(sum(assign[i]) == placed[i])
            else:
                model.Add(placed[i] == 0)

        ### Move constraints - only for running pods
        for i in running_idxs:
            orig = p_node_j(i)
            pos  = eligible_pos[i].get(orig)
            # "stay or move" coupling is implicit with <=1 assignment.
            # If protected & running: force stay if possible
            if p_protected(i):
                if pos is not None:
                    model.Add(assign[i][pos] == 1)   # must stay
                else:
                    # protected but cannot stay: infeasible → early return?
                    return {"status": "MODEL_INVALID"}

        #################################################
        # --- Mode Specific Constraints -----------------
        #################################################
        # Single-preemptor mode:
        if single_preemptor_mode and preemptor_idx is not None:
            # Single-preemptor must be placed
            model.Add(sum(assign[preemptor_idx]) == 1)
        # Batch mode
        else:
            # There must be at least one placement of pending pods
            # (weak constraint to avoid trivial empty solution)
            # Below we will enforce non-degradation on priorities
            if pending_idxs:
                model.Add(sum(placed[i] for i in pending_idxs) >= 1)

        #################################################
        # --- Placed by Priority Constraints ------------
        #################################################
        priorities = sorted({p_priority(i) for i in range(num_pods)}, reverse=True)

        # Hints path that used placed[i] → switch to sums of assign
        if use_hints and isinstance(hints, dict):
            raw = hints.get("placed_by_priority") or {}
            exact = {int(k): int(v) for k, v in raw.items()
                    if str(k).lstrip("-").isdigit() and int(v) > 0}
            if exact:
                cum_need, acc = {}, 0
                for pr in priorities:
                    acc += exact.get(pr, 0)
                    cum_need[pr] = acc
                for pr in priorities:
                    idxs_ge = [i for i in range(num_pods) if p_priority(i) >= pr]
                    if not idxs_ge: continue
                    cap_ge  = sum(1 for i in idxs_ge if eligible_nodes[i])
                    base_ge = sum(1 for i in idxs_ge if i in running_idxs)
                    need_ge = cum_need.get(pr, 0)
                    lower_bound = max(base_ge, min(need_ge, cap_ge))
                    if single_preemptor_mode and preemptor_idx is not None and pr <= preemptor_priority:
                        lower_bound = base_ge
                    if lower_bound > 0:
                        model.Add(sum(placed[i] for i in idxs_ge) >= lower_bound)
        else:
            # Non-degradation (no hints)
            for pr in priorities:
                idxs_ge = [i for i in range(num_pods) if p_priority(i) >= pr]
                if not idxs_ge: 
                    continue
                base_ge = sum(1 for i in idxs_ge if i in running_idxs)
                model.Add(sum(placed[i] for i in idxs_ge) >= base_ge)   # <- placed_b

        # Strict improvement
        if use_strictly_improving:
            improved_at = []
            for pr in priorities:
                if single_preemptor_mode and preemptor_idx is not None and pr >= preemptor_priority:
                    continue
                idxs_ge = [i for i in range(num_pods) if p_priority(i) >= pr]
                if not idxs_ge: 
                    continue
                base_ge = sum(1 for i in idxs_ge if i in running_idxs)
                improved = model.NewBoolVar(f"strict_improve_ge_{pr}")
                model.Add(sum(placed[i] for i in idxs_ge) >= base_ge + 1).OnlyEnforceIf(improved)  # <- placed_b
                improved_at.append(improved)
            if improved_at:
                model.AddBoolOr(improved_at)

        #################################################
        # --- Symmetry breaking (optional, safe)---------
        #################################################
        for i in range(num_pods):
            model.AddDecisionStrategy(assign[i], cp_model.CHOOSE_FIRST, cp_model.SELECT_MAX_VALUE)

        if use_symmetry_breaking:
            # node loads from assignments
            node_cpu_load = []
            for j in range(num_nodes):
                terms = []
                for i in range(num_pods):
                    pos = eligible_pos[i].get(j)
                    if pos is not None:
                        terms.append(assign[i][pos] * p_req_cpu_m(i))
                v = model.NewIntVar(0, n_cap_cpu_m(j), f"cpu_load_node_{j}")
                model.Add(v == (sum(terms) if terms else 0))
                node_cpu_load.append(v)

            # baseline: protected-staying + singleton-domain pods
            base_cpu = [0] * num_nodes
            for i in range(num_pods):
                dom = eligible_nodes[i]
                if p_protected(i) and p_node_j(i) in dom:
                    base_cpu[p_node_j(i)] += p_req_cpu_m(i)
                elif len(dom) == 1:
                    base_cpu[dom[0]] += p_req_cpu_m(i)

            # free load per node and chain within equivalent groups
            free_cpu = []
            for j in range(num_nodes):
                v = model.NewIntVar(0, n_cap_cpu_m(j), f"free_cpu_{j}")
                model.Add(v == node_cpu_load[j] - base_cpu[j])
                free_cpu.append(v)

            def node_signature_for_sb(j):
                domain_mask = tuple(eligible_pos[i].get(j) is not None for i in range(num_pods))
                return (n_cap_cpu_m(j), n_cap_mem_bytes(j), base_cpu[j], domain_mask)

            groups = {}
            for j in range(num_nodes):
                groups.setdefault(node_signature_for_sb(j), []).append(j)

            for group in groups.values():
                group.sort()
                for a, b in zip(group, group[1:]):
                    model.Add(free_cpu[a] <= free_cpu[b])
            
            # Within equivalent pods, prefer lower-indexed to get assigned first
            chosen_idx = []
            for i in range(num_pods):
                L = len(eligible_nodes[i])
                if L == 0:
                    chosen_idx.append(None)
                    continue
                ci = model.NewIntVar(0, L, f"chosen_idx_{i}")
                chosen_idx.append(ci)
                model.Add(ci == 0).OnlyEnforceIf(placed[i].Not())
                for t in range(L):
                    model.Add(ci == t + 1).OnlyEnforceIf(assign[i][t])

            def pod_sig(i):
                return (p_req_cpu_m(i), p_req_mem_bytes(i), p_priority(i), tuple(eligible_nodes[i]))

            classes = {}
            for i in range(num_pods):
                if p_protected(i) or p_node_j(i) is not None or not eligible_nodes[i]:
                    continue
                classes.setdefault(pod_sig(i), []).append(i)

            for idxs in classes.values():
                idxs.sort()
                for a, b in zip(idxs, idxs[1:]):
                    model.Add(chosen_idx[a] <= chosen_idx[b]).OnlyEnforceIf([placed[a], placed[b]])
        
        #################################################
        # --- Lexicographic optimization (sequential) ---
        # Order per tier (≥priority):
        #   1) maximize placed
        #   2) minimize moves (running only)
        #################################################
        def move_term(i):
            pos = eligible_pos[i].get(p_node_j(i))
            return placed[i] if pos is None else placed[i] - assign[i][pos]

        def tier_moves_expr(pr):
            idxs = [i for i in running_idxs if p_priority(i) >= pr]
            return sum(move_term(i) for i in idxs)

        def _apply_solution_as_hint():
            # Clear previous hints and push only the assign[] decisions
            model.ClearHints()
            for i in range(num_pods):
                for local, _ in enumerate(eligible_nodes[i]):
                    model.AddHint(assign[i][local], int(solver.Value(assign[i][local])))
        
        # Helper to set objective and solve one sub-stage.
        def _solve_stage(obj_expr, sense: str) -> int:
            if sense == "max": model.Maximize(obj_expr)
            else:              model.Minimize(obj_expr)
            rem = _remaining_seconds()
            if rem is not None:
                solver.parameters.max_time_in_seconds = max(1e-3, rem - 0.05)
            return solver.Solve(model)

        placement_all_optimal = True
        for pr in priorities:
            idxs_ge = [i for i in range(num_pods) if p_priority(i) >= pr]
            if not idxs_ge: 
                continue

            placed_expr = sum(placed[i] for i in idxs_ge)
            status = _solve_stage(placed_expr, "max")
            if status not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
                return {"status": self._status_str(status)}
            if status != cp_model.OPTIMAL:
                placement_all_optimal = False
            best_placed = solver.Value(placed_expr)
            model.Add(placed_expr == best_placed)
            _apply_solution_as_hint()

            moves_expr = tier_moves_expr(pr)
            status = _solve_stage(moves_expr, "min")
            if status not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
                return {"status": self._status_str(status)}
            best_moves = solver.Value(moves_expr)
            model.Add(moves_expr == best_moves)
            _apply_solution_as_hint()

        # compute final status per your rule
        if placement_all_optimal and status in (cp_model.OPTIMAL, cp_model.FEASIBLE):
            final_status = cp_model.OPTIMAL
        else:
            final_status = status

        #################################################
        # --- Extract and Return Plan -------------------
        #################################################
        placements, evictions = [], []
        for i in range(num_pods):
            was_running = (p_node_j(i) is not None)
            placed_now = (solver.Value(placed[i]) == 1)
            if was_running and not placed_now:
                evictions.append({
                    "pod": {"uid": p_uid(i), "namespace": p_namespace(i), "name": p_name(i)},
                    "node": nodes[p_node_j(i)]["name"],
                })
                continue

            if placed_now and eligible_nodes[i]:
                chosen_j = None
                for local, j in enumerate(eligible_nodes[i]):
                    if int(solver.Value(assign[i][local])) == 1:
                        chosen_j = j
                        break
                if chosen_j is None:
                    continue
                orig_j = p_node_j(i)
                moved = (orig_j is None) or (eligible_pos[i].get(orig_j) is None) or (chosen_j != orig_j)
                if moved:
                    placements.append({
                        "pod": {"uid": p_uid(i), "namespace": p_namespace(i), "name": p_name(i)},
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
