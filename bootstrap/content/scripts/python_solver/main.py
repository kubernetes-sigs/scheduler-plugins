#!/usr/bin/env python3

# main.py

import time, sys, json
from typing import Union
from ortools.sat.python import cp_model

#################################################
# --- Constants ---------------------------------
#################################################
STATUS_MAP = {
    cp_model.UNKNOWN:       "UNKNOWN",
    cp_model.MODEL_INVALID: "MODEL_INVALID",
    cp_model.INFEASIBLE:    "INFEASIBLE",
    cp_model.FEASIBLE:      "FEASIBLE",
    cp_model.OPTIMAL:       "OPTIMAL",
}
NO_NODES = "NO_NODES"
NO_PODS = "NO_PODS"

class CPSATSolver:
    #################################################
    # --- Helpers -----------------------------------
    #################################################
    @classmethod
    def _status_str(cls, st: Union[int, str]) -> str:
        """
        Convert solver status to string.
        """
        if isinstance(st, str):
            return st
        return STATUS_MAP.get(st, "UNKNOWN")

    @staticmethod
    def _relative_gap(sense: str, obj: float, bound: float) -> float:
        """
        Compute the relative gap between the objective and the bound.
        For max: gap = (bound - obj) / max(1, |obj|)
        For min: gap = (obj   - bound) / max(1, |obj|)
        """
        if obj is None or bound is None:
            return None
        denom = max(1.0, abs(obj))
        if sense == "max":
            return max(0.0, (bound - obj) / denom)
        else:
            return max(0.0, (obj - bound) / denom)

    def solve(self, instance: dict) -> dict:
        """
        Solve the given instance and return the plan.
        The instance is a dict with keys:
            - nodes: list of nodes (dicts with name, cap_cpu_m, cap_mem_bytes)
            - pods: list of pods (dicts with uid, namespace, name, req_cpu_m, req_mem_bytes, priority, protected, node)
            - preemptor: optional dict with uid, namespace, name, req_cpu_m, req_mem_bytes, priority, protected
            - timeout_ms: int, total timeout in milliseconds (default 3000)
            - ignore_affinity: bool, whether to ignore affinity constraints (default True)
            - log_progress: bool, whether to log progress (default False)
            - log_subsolvers: bool, whether to log subsolver statistics (default False)
            - guaranteed_tier_fraction: float in [0.0, 1.0], fraction of time guaranteed for all tiers (default 0.4)
            - move_fraction_of_tier: float in [0.0, 1.0], fraction of tier time for moves (default 0.3)
            - gap_limit: float in [0.0, 1.0], relative gap limit (default 0.00)
            - use_strictly_improving: bool, whether to enforce strictly improving solutions (default False)
        Returns a dict with keys:
            - placements: list of placements (dicts with pod {uid, namespace, name}, from_node, to_node)
            - evictions: list of evictions (dicts with pod {uid, namespace, name}, node)
            - stages: list of stages (dicts with tier, stage ("place" or "moves"), status, duration_ms, relative_gap)
            - duration_ms: total duration in milliseconds
            - status: overall status string
        """
        
        # Record start time
        _started_at = time.monotonic()
        
        #################################################
        # --- Read Input -------------------------------
        #################################################
        nodes       = instance.get("nodes") or []
        pods        = instance.get("pods")  or []
        preemptor   = instance.get("preemptor") or None

        if not nodes:
            return {"status": self._status_str(NO_NODES)}
        
        #################################################
        # --- Constants ---------------------------------
        #################################################
        SAFETY_PADDING_SEC  = 0.05 # amount of seconds to keep as safety padding to avoid overshooting the timeout

        #################################################
        # --- Options/Parameters ------------------------
        #################################################
        timeout_ms               = int(instance.get("timeout_ms", 3000)) - 200
        ignore_affinity          = bool(instance.get("ignore_affinity", True)) # TODO: consider to use this
        # If use_strictly_improving is true, accept a plan only if it is strictly better than the current state under the objective. 
        # This prevents no-op plans but may reject globally optimal plans whose objective ties the baseline. Set False to allow equal-score solutions.
        use_strictly_improving   = bool(instance.get("use_strictly_improving", False)) # TODO: Input interface doesn't implement this yet, however we enforce it
        log_progress             = bool(instance.get("log_progress", False))
        guaranteed_tier_fraction = float(instance.get("guaranteed_tier_fraction", 0.4)) # guranteed fraction of total time for all tiers. 0.50 means 50% of total time is guaranteed for all tiers (divided equally), the rest is unreserved and can be used greedily by higher tiers.
        move_fraction_of_tier    = float(instance.get("move_fraction_of_tier", 0.3)) # of a tier's budget: 30% moves, 70% placement. If you want to let placement stage use all time, set to 0.0 -- the rest is then for moves.

        #################################################
        # --- Solver setup ------------------------------
        #################################################
        solver = cp_model.CpSolver()
        solver.parameters.relative_gap_limit        = float(instance.get("gap_limit", 0.00)) # allowed relative gap (0.0 = exact, 0.05 = 5% of optimum)
        solver.parameters.log_search_progress       = log_progress
        solver.parameters.log_subsolver_statistics  = log_progress
        solver.parameters.log_to_stdout             = False  # KEEP False → logs go to stderr
        # log_callback only if we want to; can slow down the solver quite a bit
        if log_progress:
            solver.log_callback = lambda line: (
                print(line, file=sys.stderr, flush=True) if line else None
            )

        # Other parameters you may consider to tune:
        # solver.parameters.num_search_workers # default: all available. Can sometimes be beneficial to limit this.
        # solver.parameters.cp_model_presolve # default True

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
        # --- Single-Preemptor setup --------------------
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

        #################################################
        # --- Get preemptor, running/pending indices ----
        #################################################
        # Index of preemptor
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

        # Indices of running vs pending pods
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
        # --- Initiate Solver Model ---------------------
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

        def _is_placeable(i: int) -> bool:
            return len(eligible_nodes[i]) > 0

        # Non-degradation
        for pr in priorities:
            idxs_ge = [i for i in range(num_pods) if p_priority(i) >= pr]
            if not idxs_ge: 
                continue
            base_ge = sum(1 for i in idxs_ge if i in running_idxs and _is_placeable(i))
            model.Add(sum(placed[i] for i in idxs_ge) >= base_ge)   # <- placed_b

        # Strict improvement. Enforce at least one strictly improving priority tier
        if use_strictly_improving:
            improved_at = []
            for pr in priorities:
                if single_preemptor_mode and preemptor_idx is not None and pr >= preemptor_priority:
                    continue
                idxs_ge = [i for i in range(num_pods) if p_priority(i) >= pr]
                if not idxs_ge: 
                    continue
                base_ge = sum(1 for i in idxs_ge if i in running_idxs and _is_placeable(i))
                improved = model.NewBoolVar(f"strict_improve_ge_{pr}")
                model.Add(sum(placed[i] for i in idxs_ge) >= base_ge + 1).OnlyEnforceIf(improved)  # <- placed_b
                improved_at.append(improved)
            if improved_at:
                model.AddBoolOr(improved_at)

        #################################################
        # --- Decision strategy ---------
        #################################################
        # TODO: Don't think this is needed
        for i in range(num_pods):
            model.AddDecisionStrategy(assign[i], cp_model.CHOOSE_FIRST, cp_model.SELECT_MAX_VALUE)
        
        #################################################
        # Lexicographic optimization:
        # Order per tier (≥priority):
        #   1) maximize placed
        #   2) minimize moves (running only)
        #################################################
        
        #########################
        # Solver helpers
        #########################
        def _move_term(i):
            """
            Expression for whether pod i is moved (1) or not (0).
            """
            pos = eligible_pos[i].get(p_node_j(i))
            return placed[i] if pos is None else placed[i] - assign[i][pos]

        def _apply_solution_as_hint():
            """
            Apply current solution as hints for next stage.
            1) Clear previous hints
            2) Push only the assign[] decisions
            """
            model.ClearHints()
            for i in range(num_pods):
                for local, _ in enumerate(eligible_nodes[i]):
                    model.AddHint(assign[i][local], int(solver.Value(assign[i][local])))

        def _remaining_wall() -> float:
            """
            Remaining wall time in seconds, with safety padding.
            """
            # wall guard with safety pad
            return max(0.0, deadline - time.monotonic() - SAFETY_PADDING_SEC)

        def _chosen_node(i) -> int | None:
            """
            Return the chosen node index for pod i, or None if not placed.
            """
            for local, j in enumerate(eligible_nodes[i]):
                if int(solver.Value(assign[i][local])) == 1:
                    return j
            return None

        def _placed_now(i) -> bool:
            """
            Return whether pod i is currently placed.
            """
            return bool(int(solver.Value(placed[i])) == 1)

        def run_stage(obj_expr, sense: str, cap_sec: float) -> dict:
            """
            Run a single optimization stage with given objective expression,
            sense ("max" or "min"), and time cap in seconds.
            """
            result = {
                "status": cp_model.UNKNOWN,
                "time_spent": 0.0,
                "relative_gap": None, # ratio of (bound - obj) / max(1, |obj|)
            }
            if cap_sec <= 1e-3:
                return result

            if sense == "max":
                model.Maximize(obj_expr)
            else:
                model.Minimize(obj_expr)

            budget = min(cap_sec, _remaining_wall())
            if budget <= 1e-3:
                return result

            t0 = time.monotonic()
            solver.parameters.max_time_in_seconds = budget
            st = solver.Solve(model)
            time_spent = min(budget, max(0.0, time.monotonic() - t0))

            result["status"] = st
            result["time_spent"] = time_spent

            # Pull objective/ bound when the model had an objective.
            try:
                obj = solver.ObjectiveValue()
                bnd = solver.BestObjectiveBound()
                result["relative_gap"] = self._relative_gap(sense, obj, bnd)
            except Exception:
                pass

            return result

        #########################
        # Time management
        #########################
        total_sec  = max(0.0, timeout_ms / 1000.0) # total time we have in seconds
        usable_sec = max(0.0, total_sec - SAFETY_PADDING_SEC) # time we can actually use, keeping a safety padding
        deadline   = time.monotonic() + total_sec # absolute deadline time

        # Build effective_tiers. An effective tier is one that has at least one pod that can be placed.
        effective_tiers = [pr for pr in priorities if any(p_priority(i) >= pr for i in range(num_pods))]
        tiers_remaining = max(1, len(effective_tiers))

        # Time allocation pools for tiers
        reserved_total   = usable_sec * max(0.0, min(1.0, guaranteed_tier_fraction))  # guaranteed pool for all tiers
        unreserved_pool  = max(0.0, usable_sec - reserved_total)                      # free pool, first tier can burn it
        floor_left       = reserved_total                                             # remaining guaranteed pool

        #########################
        # Solve per tier
        #########################
        st = cp_model.UNKNOWN
        stages: list[dict] = []
        for pr in effective_tiers:
            idxs_ge = [i for i in range(num_pods) if p_priority(i) >= pr]
            if not idxs_ge:
                tiers_remaining = max(0, tiers_remaining - 1)
                continue
            rem_wall = _remaining_wall()
            if rem_wall <= 0.0:
                break
            # Minimum guaranteed for this tier = equal share of what's left in the guaranteed pool
            tier_min = floor_left / max(1, tiers_remaining)
            # This tier can use: its guaranteed minimum + the whole unreserved pool
            tier_cap = min(rem_wall, tier_min + unreserved_pool)
            if tier_cap <= 1e-3:
                tiers_remaining = max(0, tiers_remaining - 1)
                continue
            # Split tier budget into place/moves by MOVES_SHARE
            place_cap = tier_cap * (1.0 - move_fraction_of_tier)
            
            # --- PLACEMENT stage ----------
            placed_expr = sum(placed[i] for i in idxs_ge)
            reserve_place = run_stage(placed_expr, "max", place_cap)
            st = reserve_place["status"]
            time_spent_place = reserve_place["time_spent"]
            stages.append({
                "tier": pr,
                "stage": "place",
                "status": self._status_str(st),
                "duration_ms": round(time_spent_place * 1000),
                "relative_gap": f"{reserve_place['relative_gap']:.4f}",
            })
            if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
                break
            best_placed = solver.Value(placed_expr)
            model.Add(placed_expr == best_placed if st == cp_model.OPTIMAL else placed_expr >= best_placed)
            _apply_solution_as_hint()

            # pools deduction for placement
            use_unreserve = min(unreserved_pool, time_spent_place)
            unreserved_pool -= use_unreserve
            floor_left      = max(0.0, floor_left - (time_spent_place - use_unreserve))

            # --- MOVES stage ----------
            # Let moves use whatever remains of this tier's cap (including any unused "placement" share)
            rem_tier = max(0.0, tier_cap - time_spent_place)
            if _remaining_wall() > 1e-3 and rem_tier > 1e-3:
                def _move_term(i):
                    pos = eligible_pos[i].get(p_node_j(i))
                    return placed[i] if pos is None else placed[i] - assign[i][pos]
                running_ge = [i for i in running_idxs if p_priority(i) >= pr]
                moves_expr = sum(_move_term(i) for i in running_ge) if running_ge else 0
                
                # cap moves by both remaining wall and remaining tier budget
                reserve_moves = run_stage(moves_expr, "min", min(rem_tier, _remaining_wall()))
                st = reserve_moves["status"]
                time_spent_moves = reserve_moves["time_spent"]
                stages.append({
                    "tier": pr,
                    "stage": "moves",
                    "status": self._status_str(st),
                    "duration_ms": round(time_spent_moves * 1000),
                    "relative_gap": f"{reserve_moves['relative_gap']:.4f}",
                })
                if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
                    break
                model.Add(moves_expr == solver.Value(moves_expr))
                _apply_solution_as_hint()

                # pools deduction for moves
                use_unreserve = min(unreserved_pool, time_spent_moves)
                unreserved_pool -= use_unreserve
                floor_left      = max(0.0, floor_left - (time_spent_moves - use_unreserve))

            # decrement tiers remaining
            tiers_remaining = max(0, tiers_remaining - 1)

        #########################
        # Extract and return plan
        #########################
        placements, evictions = [], []
        for i in range(num_pods):
            was_running = (p_node_j(i) is not None)
            placed_now  = _placed_now(i)
            if was_running and not placed_now:
                evictions.append({
                    "pod": {"uid": p_uid(i), "namespace": p_namespace(i), "name": p_name(i)},
                    "node": nodes[p_node_j(i)]["name"],
                })
                continue
            if placed_now and eligible_nodes[i]:
                chosen_j = _chosen_node(i)
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
        total_time = max(0.0, time.monotonic() - _started_at)
        
        # Get overall status by checking all stages. Priority:
        # MODEL_INVALID > UNKNOWN > INFEASIBLE > FEASIBLE > OPTIMAL
        seen = {s["status"] for s in stages}
        if "MODEL_INVALID" in seen:
            overall_status = "MODEL_INVALID"
        elif "UNKNOWN" in seen:
            overall_status = "UNKNOWN"
        elif "INFEASIBLE" in seen:
            overall_status = "INFEASIBLE"
        elif "FEASIBLE" in seen:
            overall_status = "FEASIBLE"
        elif seen:  # all OPTIMAL
            overall_status = "OPTIMAL"
        else:
            overall_status = self._status_str(st)
        
        # Return the plan
        return {
            "placements": placements,
            "evictions": evictions,
            "stages": stages,
            "duration_ms": round(total_time * 1000),
            "status": overall_status,
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
