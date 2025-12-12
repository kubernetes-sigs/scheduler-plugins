#!/usr/bin/env python3
# main.py

import time, sys, json
from dataclasses import dataclass
from typing import Optional, Union
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


@dataclass(frozen=True)
class SolverOptions:
    """Parsed solver options.

    Keeping this as a dataclass makes the solver's public `solve()` entrypoint
    much easier to test: tests can build options explicitly and call the
    internal methods directly.
    """

    timeout_ms: int
    ignore_affinity: bool
    log_progress: bool
    guaranteed_tier_fraction: float
    move_fraction_of_tier: float
    gap_limit: float


@dataclass(frozen=True)
class Problem:
    """A frozen, index-based view of the input.

    The original code used a large set of nested accessor functions inside
    `solve()`. This refactor turns that into explicit arrays + indices so that
    individual pieces are easier to unit-test.
    """

    # Raw payload
    nodes: list[dict]
    pods: list[dict]

    # Node lookup
    node_idx: dict[str, int]

    # Sizes
    num_nodes: int
    num_pods: int

    # Frozen nodes
    node_names: list[str]
    node_cap_cpu_m: list[int]
    node_cap_mem_bytes: list[int]

    # Frozen pods
    pod_uid: list[str]
    pod_namespace: list[str]
    pod_name: list[str]
    pod_req_cpu_m: list[int]
    pod_req_mem_bytes: list[int]
    pod_priority: list[int]
    pod_protected: list[bool]
    pod_node_j: list[Optional[int]]  # None => pending

    # Convenience index sets
    running_idxs: list[int]
    pending_idxs: list[int]

    # Eligible nodes per pod
    eligible_nodes: list[list[int]]
    eligible_pos: list[dict[int, int]]

    # Preemptor mode
    single_preemptor_mode: bool
    preemptor_uid: Optional[str]
    preemptor_idx: Optional[int]


@dataclass(frozen=True)
class DecisionVars:
    """Decision variables for the CP-SAT model."""

    placed: list[cp_model.IntVar]
    assign: list[list[cp_model.IntVar]]

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
            - guaranteed_tier_fraction: float in [0.0, 1.0], fraction of time guaranteed for all tiers (default 0.6)
            - disr_fraction_of_tier: float in [0.0, 1.0], fraction of tier time for moves (default 0.5)
            - gap_limit: float in [0.0, 1.0], relative gap limit (default 0.00)
            - num_lower_priorities: int >= 0, number of *lowest* distinct priorities to optimize over (0 = all priorities, default)
        Returns a dict with keys:
            - placements: list of placements (dicts with pod {uid, namespace, name}, from_node, to_node)
            - evictions: list of evictions (dicts with pod {uid, namespace, name}, node)
            - phases: list of phases (dicts with tier, stage ("place" or "moves"), status, duration_ms, relative_gap)
            - duration_ms: total duration in milliseconds
            - status: overall status string
        """
        
        # Record start time
        started_at = time.monotonic()

        #################################################
        # --- Unwrap Go payload -------------------------
        #################################################
        solver_input, solver_options = self._unwrap_go_payload(instance)

        #################################################
        # --- Read Input -------------------------------
        #################################################
        nodes, pods, preemptor = self._read_input(solver_input)

        #################################################
        # --- Options/Parameters ------------------------
        #################################################
        options = self._parse_options(solver_input, solver_options)

        #################################################
        # --- Solver setup ------------------------------
        #################################################
        solver = self._init_solver(solver_options, options)

        #################################################
        # --- Ensure Pods from Input --------------------
        #################################################
        # We may have multiple records for the same pod UID.
        # Therefore we keep only one record per UID, preferring
        # the first one that is currently assigned to a node (if any).
        pod_by_uid = self._dedupe_pods_by_uid(pods)

        #################################################
        # --- Preemptor (per-pod) setup -----------------
        #################################################
        # If preemptor (per-pod) mode and preemptor
        # not already in pods, add it as pending.
        pod_by_uid, single_preemptor_mode, preemptor_uid = self._apply_preemptor_to_pods(
            pod_by_uid, preemptor
        )

        #################################################
        # --- Freeze nodes and pods and quick checks ----
        #################################################
        pods = list(pod_by_uid.values())
        frozen_or_err = self._freeze_problem(
            nodes=nodes,
            pods=pods,
            single_preemptor_mode=single_preemptor_mode,
            preemptor_uid=preemptor_uid,
        )
        if isinstance(frozen_or_err, dict):
            return frozen_or_err
        problem: Problem = frozen_or_err

        #################################################
        # --- Initiate Solver Model ---------------------
        #################################################
        model = cp_model.CpModel()

        #################################################
        # --- Decision Variables ------------------------
        #################################################
        # placed (bool) - whether model places pod i somewhere
        # assign (bool) - whether model places pod i on node j (only for eligible nodes)
        vars = self._build_decision_vars(model, problem)

        #################################################
        # --- Common Constraints ------------------------
        #################################################
        # --- Node constraints (bin-packing constraints) - capacity per node --------
        # Ensure that total assigned resources do not exceed node capacities
        self._add_capacity_constraints(model, problem, vars)

        # --- Assign constraints - exactly one assignment per pod ----
        # In the paper we say at most one, but here we use exactly one,
        # since we have the placed[i] variable to indicate whether pod i is placed.
        self._add_assign_constraints(model, problem, vars)

        # --- Stay constraints - only for running protected pods ---
        # If protected & running: force stay if possible
        protected_err = self._add_protected_stay_constraints(model, problem, vars)
        if protected_err is not None:
            return protected_err

        #################################################
        # --- Mode Specific Constraints -----------------
        #################################################
        self._add_mode_specific_constraints(model, problem, vars)

        #################################################
        # Lexicographic optimization:
        # Order per tier (≥priority):
        #   1) maximize placed
        #   2) maximize stays (already-running, priority ≥ tier)
        #################################################
        st, phases = self._solve_lexicographically(model, solver, problem, vars, options)

        #########################
        # Extract and return plan
        #########################
        placements, evictions = self._extract_plan(problem, vars, solver)
        total_time = max(0.0, time.monotonic() - started_at)

        # Get overall status by checking all phases. Priority:
        # MODEL_INVALID > UNKNOWN > INFEASIBLE > FEASIBLE > OPTIMAL
        overall_status = self._compute_overall_status(phases, st)

        # Return the plan
        return {
            "placements": placements,
            "evictions": evictions,
            "phases": phases,
            "duration_ms": round(total_time * 1000),
            "status": overall_status,
        }

    #################################################
    # --- Input parsing / freezing --------------------
    #################################################
    def _unwrap_go_payload(self, instance: dict) -> tuple[dict, dict]:
        """Unwrap Go payload wrapper."""
        solver_input = (instance or {}).get("solver_input") or {}
        solver_options = (instance or {}).get("solver_options") or {}
        return solver_input, solver_options

    def _read_input(self, solver_input: dict) -> tuple[list[dict], list[dict], Optional[dict]]:
        """Read the main solver input payload."""
        nodes = solver_input.get("nodes") or []
        pods = solver_input.get("pods") or []
        preemptor = solver_input.get("preemptor") or None  # preemptor (per-pod) mode
        return nodes, pods, preemptor

    def _parse_options(self, solver_input: dict, solver_options: dict) -> SolverOptions:
        """Parse the option fields.

        Note: we subtract a small safety pad from timeout_ms because the Go side
        typically wraps the Python solver call and needs some headroom.
        """
        timeout_ms = int(solver_input.get("timeout_ms", 3000)) - 200
        ignore_affinity = bool(solver_input.get("ignore_affinity", True))  # TODO: consider to use this
        log_progress = bool(solver_options.get("log_progress", False))
        guaranteed_tier_fraction = float(solver_options.get("guaranteed_tier_fraction", 0.6))
        move_fraction_of_tier = float(solver_options.get("move_fraction_of_tier", 0.5))
        gap_limit = float(solver_options.get("gap_limit", 0.00))
        return SolverOptions(
            timeout_ms=timeout_ms,
            ignore_affinity=ignore_affinity,
            log_progress=log_progress,
            guaranteed_tier_fraction=guaranteed_tier_fraction,
            move_fraction_of_tier=move_fraction_of_tier,
            gap_limit=gap_limit,
        )

    def _init_solver(self, solver_options: dict, options: SolverOptions) -> cp_model.CpSolver:
        """Configure CpSolver from options."""
        solver = cp_model.CpSolver()
        solver.parameters.num_search_workers = 0  # 0 = all available cores
        solver.parameters.relative_gap_limit = options.gap_limit  # allowed relative gap (0.0 = exact, 0.05 = 5% of optimum)
        solver.parameters.log_search_progress = options.log_progress
        solver.parameters.log_subsolver_statistics = options.log_progress
        solver.parameters.log_to_stdout = False  # KEEP False → logs go to stderr
        # log_callback only if we want to; can slow down the solver quite a bit
        if options.log_progress:
            solver.log_callback = lambda line: (print(line, file=sys.stderr, flush=True) if line else None)

        # Other parameters we may consider to tune:
        # solver.parameters.cp_model_presolve # default True
        return solver

    def _dedupe_pods_by_uid(self, pods: list[dict]) -> dict[str, dict]:
        """Deduplicate pod records by UID.

        We may have multiple records for the same pod UID. Therefore we keep
        only one record per UID, preferring the first one that is currently
        assigned to a node (if any).
        """
        pod_by_uid: dict[str, dict] = {}
        for p in pods or []:
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
            new_has_node = bool((p.get("node") or "").strip())
            # Only upgrade from "no node" -> "has node".
            if new_has_node and not old_has_node:
                pod_by_uid[uid] = p
        return pod_by_uid

    def _apply_preemptor_to_pods(
        self,
        pod_by_uid: dict[str, dict],
        preemptor: Optional[dict],
    ) -> tuple[dict[str, dict], bool, Optional[str]]:
        """Preemptor (per-pod) mode setup."""
        single_preemptor_mode = False
        preemptor_uid: Optional[str] = None
        if isinstance(preemptor, dict) and preemptor.get("uid"):
            preemptor_uid = preemptor["uid"]
            single_preemptor_mode = True
            if preemptor_uid not in pod_by_uid:
                pod_by_uid[preemptor_uid] = {
                    "uid": preemptor["uid"],
                    "namespace": preemptor.get("namespace", "default"),
                    "name": preemptor.get("name", "preemptor"),
                    "req_cpu_m": int(preemptor.get("req_cpu_m", 0)),
                    "req_mem_bytes": int(preemptor.get("req_mem_bytes", 0)),
                    "priority": int(preemptor.get("priority", 0)),
                    "protected": bool(preemptor.get("protected", False)),
                    "node": "",  # add it as pending
                }
        return pod_by_uid, single_preemptor_mode, preemptor_uid

    def _freeze_problem(
        self,
        *,
        nodes: list[dict],
        pods: list[dict],
        single_preemptor_mode: bool,
        preemptor_uid: Optional[str],
    ) -> Problem | dict:
        """Freeze nodes/pods into a compact indexed representation.

        Returns:
          - FrozenProblem on success
          - dict with {"status": ...} for quick exits
        """
        num_nodes = len(nodes)
        num_pods = len(pods)
        if num_nodes == 0:
            return {"status": self._status_str(NO_NODES)}
        if num_pods == 0:
            return {"status": self._status_str(NO_PODS)}

        node_idx = {n["name"]: j for j, n in enumerate(nodes)}

        # Freeze nodes into arrays for fast indexed access
        node_names = [n.get("name", "") for n in nodes]
        node_cap_cpu_m = [int(n.get("cap_cpu_m", 0)) for n in nodes]
        node_cap_mem_bytes = [int(n.get("cap_mem_bytes", 0)) for n in nodes]

        # Freeze pods into arrays
        pod_uid = [p.get("uid", "") for p in pods]
        pod_namespace = [p.get("namespace", "default") for p in pods]
        pod_name = [p.get("name", "") for p in pods]
        pod_req_cpu_m = [int(p.get("req_cpu_m", 0)) for p in pods]
        pod_req_mem_bytes = [int(p.get("req_mem_bytes", 0)) for p in pods]
        pod_priority = [int(p.get("priority", 0)) for p in pods]
        pod_protected = [bool(p.get("protected", False)) for p in pods]
        pod_node_j: list[Optional[int]] = []
        for p in pods:
            w = (p.get("node") or "").strip()
            pod_node_j.append(node_idx.get(w) if w else None)

        # Indices of running vs pending pods
        running_idxs = [i for i in range(num_pods) if pod_node_j[i] is not None]
        pending_idxs = [i for i in range(num_pods) if pod_node_j[i] is None]

        # Index of preemptor
        preemptor_idx: Optional[int] = None
        if single_preemptor_mode and preemptor_uid is not None:
            for i in range(num_pods):
                if pod_uid[i] == preemptor_uid:
                    preemptor_idx = i
                    break
            if preemptor_idx is None:
                single_preemptor_mode = False  # fallback if not found

        #################################################
        # --- Build Eligible Nodes ----------------------
        #################################################
        # Only consider nodes that can host the pod alone.
        eligible_nodes: list[list[int]] = []
        for i in range(num_pods):
            cpu_i, mem_i = pod_req_cpu_m[i], pod_req_mem_bytes[i]
            lst: list[int] = []
            for j in range(num_nodes):
                if node_cap_cpu_m[j] >= cpu_i and node_cap_mem_bytes[j] >= mem_i:
                    lst.append(j)
            eligible_nodes.append(lst)

        eligible_pos: list[dict[int, int]] = [
            {j: pos for pos, j in enumerate(eligible_nodes[i])} for i in range(num_pods)
        ]

        return Problem(
            nodes=nodes,
            pods=pods,
            node_idx=node_idx,
            num_nodes=num_nodes,
            num_pods=num_pods,
            node_names=node_names,
            node_cap_cpu_m=node_cap_cpu_m,
            node_cap_mem_bytes=node_cap_mem_bytes,
            pod_uid=pod_uid,
            pod_namespace=pod_namespace,
            pod_name=pod_name,
            pod_req_cpu_m=pod_req_cpu_m,
            pod_req_mem_bytes=pod_req_mem_bytes,
            pod_priority=pod_priority,
            pod_protected=pod_protected,
            pod_node_j=pod_node_j,
            running_idxs=running_idxs,
            pending_idxs=pending_idxs,
            eligible_nodes=eligible_nodes,
            eligible_pos=eligible_pos,
            single_preemptor_mode=single_preemptor_mode,
            preemptor_uid=preemptor_uid,
            preemptor_idx=preemptor_idx,
        )

    #################################################
    # --- Model building -----------------------------
    #################################################
    def _build_decision_vars(self, model: cp_model.CpModel, problem: Problem) -> DecisionVars:
        placed = [model.NewBoolVar(f"placed_{i}") for i in range(problem.num_pods)]
        assign = [
            [
                # equals to the x_i,j in the paper, except that we only consider eligible nodes for each pod
                model.NewBoolVar(f"assign_{i}_{j}")
                for j in problem.eligible_nodes[i]
            ]
            for i in range(problem.num_pods)
        ]
        return DecisionVars(placed=placed, assign=assign)

    def _add_capacity_constraints(self, model: cp_model.CpModel, problem: Problem, vars: DecisionVars) -> None:
        for j in range(problem.num_nodes):
            cap_cpu_terms, cap_mem_terms = [], []
            for i in range(problem.num_pods):
                pos = problem.eligible_pos[i].get(j)
                if pos is not None:
                    cap_cpu_terms.append(vars.assign[i][pos] * problem.pod_req_cpu_m[i])
                    cap_mem_terms.append(vars.assign[i][pos] * problem.pod_req_mem_bytes[i])
            if cap_cpu_terms:
                model.Add(sum(cap_cpu_terms) <= problem.node_cap_cpu_m[j])
                model.Add(sum(cap_mem_terms) <= problem.node_cap_mem_bytes[j])

    def _add_assign_constraints(self, model: cp_model.CpModel, problem: Problem, vars: DecisionVars) -> None:
        for i in range(problem.num_pods):
            if problem.eligible_nodes[i]:
                model.Add(sum(vars.assign[i]) == vars.placed[i])
            else:
                model.Add(vars.placed[i] == 0)

    def _add_protected_stay_constraints(
        self,
        model: cp_model.CpModel,
        problem: Problem,
        vars: DecisionVars,
    ) -> Optional[dict]:
        for i in problem.running_idxs:
            if problem.pod_protected[i]:
                orig = problem.pod_node_j[i]
                pos = problem.eligible_pos[i].get(orig)
                if pos is not None:
                    model.Add(vars.assign[i][pos] == 1)  # must stay
                else:
                    # protected but cannot stay: infeasible → early return?
                    return {"status": "MODEL_INVALID"}
        return None

    def _add_mode_specific_constraints(self, model: cp_model.CpModel, problem: Problem, vars: DecisionVars) -> None:
        # Preemptor (per-pod) mode:
        if problem.single_preemptor_mode and problem.preemptor_idx is not None:
            # Single-preemptor must be placed.
            # However, possibly, it cannot be placed anywhere.
            # In that case, the model is infeasible.
            model.Add(sum(vars.assign[problem.preemptor_idx]) == 1)
        # Background modes
        else:
            # There must be at least one placement of pending pods
            # (weak constraint to avoid trivial empty solution and solutions that only move running pods or evict.)
            if problem.pending_idxs:
                model.Add(sum(vars.placed[i] for i in problem.pending_idxs) >= 1)

    #################################################
    # --- Optimization ------------------------------
    #################################################
    def _solve_lexicographically(
        self,
        model: cp_model.CpModel,
        solver: cp_model.CpSolver,
        problem: Problem,
        vars: DecisionVars,
        options: SolverOptions,
    ) -> tuple[int, list[dict]]:
        """Run the tiered lexicographic optimization.
        """

        #########################
        # Solver helpers
        #########################
        total_sec = max(0.0, options.timeout_ms / 1000.0)  # total time we have in seconds
        usable_sec = max(0.0, total_sec)  # time we can actually use
        deadline = time.monotonic() + total_sec  # absolute deadline time

        def remaining_wall() -> float:
            """Remaining wall time in seconds."""
            # wall guard with safety pad
            return max(0.0, deadline - time.monotonic())

        def relative_gap(sense: str, obj: float, bound: float) -> Optional[float]:
            """Compute the relative gap between the objective and the bound.

            For max: gap = (bound - obj) / max(1, |obj|)
            For min: gap = (obj   - bound) / max(1, |obj|)
            """
            if obj is None or bound is None:
                return None
            denom = max(1.0, abs(obj))
            if sense == "max":
                return max(0.0, (bound - obj) / denom)
            else:  # min
                return max(0.0, (obj - bound) / denom)

        def orig_node(i: int):
            """1 if pod i is placed on its same/original node, 0 otherwise."""
            orig = problem.pod_node_j[i]
            pos = problem.eligible_pos[i].get(orig)
            return vars.assign[i][pos] if pos is not None else 0

        def apply_current_placement_as_hint() -> None:
            """Seed solver hints from the current cluster placement.

            For each pod that is currently running on an eligible node, we hint:
            - placed[i] == 1
            - assign[i][pos(orig_j)] == 1
            """
            for i in range(problem.num_pods):
                orig_j = problem.pod_node_j[i]
                if orig_j is None:  # pending in the snapshot: no preferred node
                    continue
                pos = problem.eligible_pos[i].get(orig_j)
                if pos is None:  # original node not in eligible_nodes (e.g. filtered out/unusable)
                    continue
                # "Stay where you are" is the starting suggestion
                model.AddHint(vars.placed[i], 1)
                model.AddHint(vars.assign[i][pos], 1)

        def apply_solution_as_hint() -> None:
            """Apply current solution (how pods are placed) as hints for next stage.

            1) Clear previous hints
            2) Push only the assign[] decisions
            """
            model.ClearHints()
            for i in range(problem.num_pods):
                for local, _ in enumerate(problem.eligible_nodes[i]):
                    model.AddHint(vars.assign[i][local], int(solver.Value(vars.assign[i][local])))

        def run_stage(obj_expr, sense: str, cap_sec: float) -> dict:
            """Run a single optimization stage."""
            result = {
                "status": cp_model.UNKNOWN,
                "time_spent": 0.0,
                "relative_gap": None,  # ratio of (bound - obj) / max(1, |obj|)
            }
            if cap_sec <= 1e-3:
                return result

            if sense == "max":
                model.Maximize(obj_expr)
            else:
                model.Minimize(obj_expr)

            budget = min(cap_sec, remaining_wall())
            if budget <= 1e-3:
                return result

            t0 = time.monotonic()
            solver.parameters.max_time_in_seconds = budget
            st = solver.Solve(model)
            time_spent = min(budget, max(0.0, time.monotonic() - t0))

            result["status"] = st
            result["time_spent"] = time_spent

            # Get objective value and bound when the model had an objective
            try:
                obj = solver.ObjectiveValue()
                bnd = solver.BestObjectiveBound()
                result["relative_gap"] = relative_gap(sense, obj, bnd)
            except Exception:
                pass

            return result

        def gap_str(g: Optional[float]) -> str:
            # Keep same output shape as before (string), but avoid crashing
            # if OR-Tools does not provide a bound for some reason.
            return f"{g:.4f}" if isinstance(g, (int, float)) else ""

        #########################
        # Time management
        #########################
        # Time allocation pools for priorities
        reserved_total = usable_sec * max(0.0, min(1.0, options.guaranteed_tier_fraction))  # guaranteed pool for all priorities
        unreserved_pool = max(0.0, usable_sec - reserved_total)  # free pool, first priority can burn it
        floor_left = reserved_total  # remaining guaranteed pool

        #########################
        # Hint current placements
        #########################
        # Seed hints from the current cluster placement before the first solve.
        # This gives CP-SAT a good starting point.
        apply_current_placement_as_hint()

        #########################
        # Build priorities and tiers.
        # a tier is a group of pods of equal priority.
        #########################
        priorities = sorted({problem.pod_priority[i] for i in range(problem.num_pods)}, reverse=True)
        tiers = [p for p in priorities if any(problem.pod_priority[i] >= p for i in range(problem.num_pods))]
        remaining_tiers = max(1, len(tiers))

        #########################
        # Solve per priority
        #########################
        # TODO: add total_solver_time to output, as total_time also includes model building time and I/O time
        st = cp_model.UNKNOWN
        phases: list[dict] = []
        for p in tiers:
            # Indices of pods with priority ≥ p, both running and pending
            idxs_ge = [i for i in range(problem.num_pods) if problem.pod_priority[i] >= p]

            # --- Time budget calculation ----------
            if not idxs_ge:  # no pods for this priority -> skip
                remaining_tiers = max(0, remaining_tiers - 1)
                continue
            rem_wall = remaining_wall()
            if rem_wall <= 0.0:  # no time left -> break
                break
            tier_min = floor_left / max(1, remaining_tiers)  # minimum guaranteed for this tier = equal share of what's left in the guaranteed pool
            tier_cap = min(rem_wall, tier_min + unreserved_pool)  # this tier can use: its guaranteed minimum + the whole unreserved pool
            if tier_cap <= 1e-3:
                remaining_tiers = max(0, remaining_tiers - 1)
                continue
            place_cap = tier_cap * (1.0 - options.move_fraction_of_tier)  # split priority budget into place/moves by MOVES_SHARE

            # --- PLACEMENT stage ---
            # Objective:
            #   Maximize the number of placed pods among those with priority ≥ p.
            # Term:
            #   place := Σ_{i : prio(i) ≥ p} Σ_{j ∈ B} x_{i,j}
            # Implementation detail:
            #   We use `placed[i] ∈ {0,1}` with the constraint Σ_j assign[i][j] == placed[i],
            #   so the objective is equivalent to:
            #     placed_expr = Σ_{i : prio(i) ≥ p} placed[i]
            #   After solving:
            #     - If OPTIMAL:     Add( placed_expr == best_value )
            #     - If FEASIBLE:    Add( placed_expr ≥  best_value )
            #   This prevents later (lower) priorities from reducing the total number of
            #   placed pods already achieved for ≥ p, while still allowing improvements.
            # Warm start:
            #   Apply the current assignment values as hints to help the next stage.
            placed_expr = sum(vars.placed[i] for i in idxs_ge)  # sum of placed pods (priority ≥ pr) both running and pending
            place_result = run_stage(placed_expr, "max", place_cap)
            st = place_result["status"]
            time_spent_place = place_result["time_spent"]
            phases.append(
                {
                    "tier": p,
                    "stage": "place",
                    "status": self._status_str(st),
                    "duration_ms": round(time_spent_place * 1000),
                    "relative_gap": gap_str(place_result["relative_gap"]),
                }
            )
            if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
                break
            placed_p = solver.Value(placed_expr)
            # If not optimal, allow feasible or better solutions in later priorities to increase placement
            model.Add(placed_expr == placed_p if st == cp_model.OPTIMAL else placed_expr >= placed_p)
            apply_solution_as_hint()

            # pools deduction for placement
            use_unreserve = min(unreserved_pool, time_spent_place)
            unreserved_pool -= use_unreserve
            floor_left = max(0.0, floor_left - (time_spent_place - use_unreserve))

            # --- DISRUPTION (evictions + moves) stage ---
            # Objective:
            #   Minimize total disruption for already-running pods with priority ≥ p.
            # For a running pod i with original node orig(i), define:
            #   x_orig(i) := 1 if pod i is assigned to orig(i), else 0
            #   placed[i] := 1 if pod i is assigned anywhere, else 0
            # Per-pod disruption:
            #   disr_i = (eviction_term) + (move_term)
            #          = (1 - x_orig(i)) + (placed[i] - x_orig(i))
            #          = 1 + placed[i] - 2 * x_orig(i)
            #          where we can remove the 1+ since it's constant across all pods, so
            #          = placed[i] - 2 * x_orig(i)
            # Therefore:
            #   - stayed (placed on original node):   placed=1, x_orig=1 → disr_i = 0
            #   - eviction (not placed anywhere):     placed=0, x_orig=0 → disr_i = 1
            #   - move (placed on different node):    placed=1, x_orig=0 → disr_i = 2
            # Term:
            #   disr := Σ_{i ∈ running, prio(i) ≥ p} (1 + placed[i] - 2 * x_orig(i))
            # Implementation detail:
            #   x_orig(i) is computed as assign[i][pos] if the original node is eligible,
            #   otherwise 0 (pod cannot stay on an ineligible node in this model).
            #   After solving:
            #     - If OPTIMAL:     Add( disr_expr == best_value )
            #     - If FEASIBLE:    Add( disr_expr ≤  best_value )
            #   This prevents later (lower) priorities from increasing the total disruption
            #   already achieved for ≥ p, while still allowing improvements.
            # Warm start:
            #   Apply the current assignment values as hints to help subsequent tiers.
            rem_tiers = max(0.0, tier_cap - time_spent_place)
            if remaining_wall() > 1e-3 and rem_tiers > 1e-3:
                running_ge = [i for i in problem.running_idxs if problem.pod_priority[i] >= p]  # running pods with priority ≥ p
                if running_ge:
                    disr_expr = sum(vars.placed[i] - 2 * orig_node(i) for i in running_ge)
                    disr_result = run_stage(disr_expr, "min", min(rem_tiers, remaining_wall()))
                    st = disr_result["status"]
                    time_spent_disr = disr_result["time_spent"]
                    phases.append(
                        {
                            "tier": p,
                            "stage": "disruption",
                            "status": self._status_str(st),
                            "duration_ms": round(time_spent_disr * 1000),
                            "relative_gap": gap_str(disr_result["relative_gap"]),
                        }
                    )
                    if st not in (cp_model.OPTIMAL, cp_model.FEASIBLE):
                        break
                    best_disr = solver.Value(disr_expr)
                    model.Add(disr_expr == best_disr if st == cp_model.OPTIMAL else disr_expr <= best_disr)
                    apply_solution_as_hint()

                    # pools deduction for disruption
                    use_unreserve = min(unreserved_pool, time_spent_disr)
                    unreserved_pool -= use_unreserve
                    floor_left = max(0.0, floor_left - (time_spent_disr - use_unreserve))

            # decrement priorities remaining
            remaining_tiers = max(0, remaining_tiers - 1)

        return st, phases

    #################################################
    # --- Output extraction -------------------------
    #################################################
    def _extract_plan(
        self,
        problem: Problem,
        vars: DecisionVars,
        solver: cp_model.CpSolver,
    ) -> tuple[list[dict], list[dict]]:
        """Extract placements/evictions from the final solver assignment."""

        def is_pod_placed_solver(i: int) -> bool:
            """Return whether pod i is currently placed."""
            return bool(int(solver.Value(vars.placed[i])) == 1)

        def chosen_node_for_pod_solver(i: int) -> int | None:
            """Return the chosen node index for pod i, or None if not placed."""
            for local, j in enumerate(problem.eligible_nodes[i]):
                if int(solver.Value(vars.assign[i][local])) == 1:
                    return j
            return None

        def stayed_on_same_node_solver(i: int) -> bool:
            orig = problem.pod_node_j[i]
            pos = problem.eligible_pos[i].get(orig)
            return bool(pos is not None and solver.Value(vars.assign[i][pos]) == 1)

        placements: list[dict] = []
        evictions: list[dict] = []
        for i in range(problem.num_pods):
            was_running = problem.pod_node_j[i] is not None
            is_placed = is_pod_placed_solver(i)
            if was_running and not is_placed:
                evictions.append(
                    {
                        "uid": problem.pod_uid[i],
                        "name": problem.pod_name[i],
                        "namespace": problem.pod_namespace[i],
                        "node": problem.node_names[problem.pod_node_j[i]],
                    }
                )
                continue
            if is_placed:
                if was_running and stayed_on_same_node_solver(i):
                    # stayed — no placement entry
                    continue
                # either moved (was_running and not stayed), or newly placed (was not running)
                chosen_j = chosen_node_for_pod_solver(i)
                if chosen_j is None:
                    continue
                orig_j = problem.pod_node_j[i]
                moved = was_running and not stayed_on_same_node_solver(i)
                if moved or orig_j is None:
                    placements.append(
                        {
                            "uid": problem.pod_uid[i],
                            "name": problem.pod_name[i],
                            "namespace": problem.pod_namespace[i],
                            "old_node": problem.node_names[orig_j] if orig_j is not None else "",
                            "node": problem.node_names[chosen_j],
                        }
                    )

        return placements, evictions

    def _compute_overall_status(self, phases: list[dict], st: int) -> str:
        """Compute the overall status string.

        Get overall status by checking all phases. Priority:
        MODEL_INVALID > UNKNOWN > INFEASIBLE > FEASIBLE > OPTIMAL
        """
        seen = {s.get("status") for s in phases or []}
        if "MODEL_INVALID" in seen:
            return "MODEL_INVALID"
        if "UNKNOWN" in seen:
            return "UNKNOWN"
        if "INFEASIBLE" in seen:
            return "INFEASIBLE"
        if "FEASIBLE" in seen:
            return "FEASIBLE"
        if seen:  # all OPTIMAL
            return "OPTIMAL"
        return self._status_str(st)

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
