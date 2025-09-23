#!/usr/bin/env python3

# main.py

import os
import sys, json
from ortools.sat.python import cp_model

class CPSATSolver:
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
    def _status_str(cls, st: int) -> str:
        return cls.STATUS_MAP.get(st, "UNKNOWN")

    @classmethod
    def _encode_status(cls, st: int) -> dict:
        return {"status": cls._status_str(st)}

    def solve(self, instance: dict) -> dict:
        #################################################
        # --- Read Input --------------------------------
        #################################################
        nodes       = instance.get("nodes") or []
        pods        = instance.get("pods")  or []
        preemptor   = instance.get("preemptor") or None

        if not nodes:
            return {"status": "no nodes", "placements": [], "evictions": []}

        #################################################
        # --- Options/Parameters ------------------------
        #################################################
        timeout_ms          = int(instance.get("timeout_ms", 3000))
        ignore_affinity     = bool(instance.get("ignore_affinity", True)) # TODO: consider to use this
        workers             = self._available_cpus() # set number of workers to the amount available
        use_hints           = bool(instance.get("use_hints", False))
        hints               = instance.get("hints") if use_hints else None
        log_progress        = bool(instance.get("log_progress", False))
        log_subsolvers      = bool(instance.get("log_progress", False))

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
            return {"status": "no pods", "placements": [], "evictions": []}

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
        # evict  (bool) - whether model evicts pod i
        # assign (bool) - whether model places pod i on node j (only for eligible nodes)
        # move   (bool) - whether model moves pod i (only for running pods)
        placed = [model.NewBoolVar(f"placed_{i}") for i in range(num_pods)]
        evict  = [model.NewBoolVar(f"evict_{i}")  for i in range(num_pods)]
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

        ### Running and pending pods constraints
        for i in running_idxs:
            # either placed somewhere or evicted
            model.Add(placed[i] + evict[i] == 1)
        for i in pending_idxs:
            # pending pods cannot be evicted
            model.Add(evict[i] == 0)

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

        #################################################
        # --- Mode Specific Constraints -----------------
        #################################################
        # Single-preemptor mode:
        if single_preemptor_mode and preemptor_idx is not None:
            # Preemptor must be place and never evicted
            model.Add(placed[preemptor_idx] == 1)
            model.Add(evict[preemptor_idx]  == 0)
            preemptor_priority = p_priority(preemptor_idx)
            for i in running_idxs:
                # Higher priority than the preemptor (or explicitly protected):
                # Cannot be evicted, cannot be moved
                if p_protected(i) or p_priority(i) > preemptor_priority:
                    model.Add(placed[i] == 1)
                    model.Add(evict[i] == 0)
                    if move[i] is not None:
                        model.Add(move[i] == 0) # cannot be moved
                # Equal priority to the preemptor:
                # Cannot be evicted, but can be moved
                elif p_priority(i) == preemptor_priority:
                    model.Add(placed[i] == 1)
                    model.Add(evict[i] == 0)

                else:
                    # Lower-priority than preemptor:
                    # no extra guard — solver may move or evict as needed
                    pass
        # Batch mode (no single preemptor)
        else:
            for i in running_idxs:
                # Protected pods must stay put (no evict, no move)
                # Allow move all running pods no matter their priority
                # Note: lexi_solve will ensure few moves and evicts.
                if p_protected(i):
                    model.Add(evict[i] == 0)
                    model.Add(placed[i] == 1)
                    if move[i] is not None:
                        model.Add(move[i] == 0)
            # There must be at least one placement of pending pods
            # (weak constraint to avoid trivial empty solution)
            # Below we will enforce non-degradation on priorities
            model.Add(sum(placed[i] for i in pending_idxs) >= 1)

        #################################################
        # --- Hints (placed_by_priority) Constraints ----
        #################################################
        # External hints (placed_by_priority) constraints
        if use_hints and isinstance(hints, dict):
            raw = hints.get("placed_by_priority") or {}
            # normalize to int keys/values, ignore non-positive
            exact = {int(k): int(v) for k, v in raw.items()
                    if str(k).lstrip("-").isdigit() and int(v) > 0}
            if exact:
                # priorities present in the instance, high -> low
                prios = sorted({p_priority(i) for i in range(num_pods)}, reverse=True)
                # build cumulative >=pr demand from exact-tier hints
                cum_need = {}
                acc = 0
                for pr in prios:
                    acc += exact.get(pr, 0)
                    cum_need[pr] = acc
                for pr in prios:
                    idxs_ge = [i for i in range(num_pods) if p_priority(i) >= pr]
                    if not idxs_ge:
                        continue
                    # feasibility guards
                    cap_ge  = sum(1 for i in idxs_ge if eligible_nodes[i]) # placeable upper bound
                    base_ge = sum(1 for i in idxs_ge if i in running_idxs) # non-degradation baseline
                    need_ge = cum_need.get(pr, 0)
                    # lower bound for this tier
                    lower_bound = max(base_ge, min(need_ge, cap_ge))
                    # in single-preemptor mode, don't push tiers at/under the preemptor
                    if single_preemptor_mode and preemptor_idx is not None and pr <= preemptor_priority:
                        lower_bound = base_ge
                    if lower_bound > 0:
                        model.Add(sum(placed[i] for i in idxs_ge) >= lower_bound)
        # If no hints, enforce non-degradation per priority tier
        else:
            prios = sorted({p_priority(i) for i in range(num_pods)}, reverse=True)
            for pr in prios:
                idxs_ge = [i for i in range(num_pods) if p_priority(i) >= pr]
                if not idxs_ge:
                    continue
                baseline = sum(1 for i in idxs_ge if i in running_idxs)
                model.Add(sum(placed[i] for i in idxs_ge) >= baseline)
        
        #################################################
        # --- Solve -------------------------------------
        #################################################
        solver = cp_model.CpSolver()
        solver.parameters.max_time_in_seconds = max(1, timeout_ms / 1000.0)
        solver.parameters.num_search_workers  = max(1, workers)
        solver.parameters.log_search_progress       = log_progress
        solver.parameters.log_subsolver_statistics  = log_subsolvers
        solver.parameters.log_to_stdout = False  # KEEP False → logs go to stderr
        solver.log_callback = lambda line: (
            print(line, file=sys.stderr, flush=True) if line else None
        )
        
        # More solvers could be tried here if needed
        st = self._solve_lexi(model, solver, placed, evict, move, running_idxs, p_priority)
        if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
            return self._encode_status(st)

        #################################################
        # --- Extract and Return Plan -------------------
        #################################################
        placements = []
        evictions  = []

        for i in range(num_pods):
            # Evicted pods
            if int(solver.Value(evict[i])) == 1:
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
                        "fromNode": nodes[orig_j]["name"] if orig_j is not None else "",
                        "toNode": nodes[chosen_j]["name"],
                    })

        # Return status, placements, evictions
        return {
            "status": self._status_str(st),
            "placements": placements,
            "evictions": evictions,
        }

    #################################################
    # --- Lexicographic Objective -------------------
    #################################################
    def _solve_lexi(self, model, solver, placed, evict, move, running_idxs, p_pri):
        """
        Lexicographic objective:
          1) maximize placed count per priority tier (high->low)
          2) minimize evictions of running pods
          3) minimize moves of running pods
        """
        # Stage 1: maximize placed by priority tier (higher first)
        num_pods = len(placed)
        priorities = sorted({p_pri(i) for i in range(num_pods)}, reverse=True)
        for priority in priorities:
            idxs = [i for i in range(num_pods) if p_pri(i) == priority]
            if not idxs:
                continue
            placed_count = model.NewIntVar(0, len(idxs), f"placed_count_priority{priority}")
            model.Add(placed_count == sum(placed[i] for i in idxs))
            model.Maximize(placed_count)
            status = solver.Solve(model)
            if status not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
                return status
            # Freeze that tier’s optimum
            model.Add(placed_count == int(solver.Value(placed_count)))

        # Stage 2: minimize evictions (running only)
        total_evict = model.NewIntVar(0, len(running_idxs), "total_evict_running")
        model.Add(total_evict == sum(evict[i] for i in running_idxs))
        model.Minimize(total_evict)
        status = solver.Solve(model)
        if status not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
            return status
        model.Add(total_evict == int(solver.Value(total_evict)))

        # Stage 3: minimize moves (running only)
        move_terms = [move[i] for i in running_idxs if move[i] is not None]
        max_moves  = len(move_terms)
        total_moves = model.NewIntVar(0, max_moves, "total_moves_running")
        if move_terms:
            model.Add(total_moves == sum(move_terms))
        else:
            model.Add(total_moves == 0)
        model.Minimize(total_moves)
        return solver.Solve(model)


#################################################
# --- Main --------------------------------------
#################################################
def main():
    raw = sys.stdin.read()
    inst = json.loads(raw or "{}")
    solver = CPSATSolver()
    out = solver.solve(inst if isinstance(inst, dict) else {})
    print(json.dumps(out))

if __name__ == "__main__":
    main()
