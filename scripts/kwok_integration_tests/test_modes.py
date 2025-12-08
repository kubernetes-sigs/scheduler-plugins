# test_modes.py

import argparse, csv, json, logging, sys, time
from pathlib import Path
from typing import Dict, Any, Tuple, Optional, List

# pytest is optional: used only when running via pytest
try:
    import pytest  # type: ignore
except ImportError:  # pragma: no cover
    pytest = None  # type: ignore

from scripts.helpers.general_helpers import (
    setup_logging,
    make_header_footer,
    qty_to_mcpu_int,
    qty_to_bytes_int,
    solver_trigger_http,
    get_solver_active_status_http,
)
from scripts.helpers.kubectl_helpers import (
    ensure_namespace,
    ensure_priority_classes,
    wait_rs_pods,
    get_json_ctx,
)
from scripts.helpers.kwok_helpers import (
    ensure_kwok_cluster,
    create_kwok_nodes,
    kwok_pods_cap,
)
from scripts.kwok_integration_tests.test_helpers import (
    DEFAULT_CLUSTER_NAME,
    DEFAULT_KWOK_RUNTIME,
    DEFAULT_KWOKCTL_CONFIG,
    NUM_NODES,
    NODE_CPU,
    NODE_MEM,
    NUM_PRIORITIES,
    VALID_OPT_MODES,
    WorkloadStep,
    WorkloadScenario,
    WORKLOAD_SCENARIOS,
    DEFAULT_WORKLOAD_ID,
    DEFAULT_DISABLE_WAIT_AND_ACTIVE_CHECKS,
    rs_name_for_pod,
    load_kwokctl_config,
    build_kwokctl_config_for_mode,
    scenario_max_priority,
    scenario_total_replicas,
    apply_workload_step,
)


# ---------------------------------------------------------------------------
# Constants specific to the integration tests
# ---------------------------------------------------------------------------

TEST_NAMESPACE = "integration-test"
SYSTEM_NAMESPACE = "kube-system"

# Must match your plugin's ConfigMap label & data key
PLAN_LABEL_KEY = "plan"
PLAN_DATA_KEY = PLAN_LABEL_KEY + ".json"

# Plugin readiness ConfigMap (created once the plugin is fully ready)
PLUGIN_CFG_NAME = "plugin-config"
PLUGIN_CFG_DATA_KEY = "plugin-config.json"
PLUGIN_CFG_TIMEOUT_S = 10  # timeout for waiting for plugin readiness

# Pod wait timeout per step (scenario definitions already use POD_TIMEOUT_S via shared module)

# Timing model:
# 1) After creating workload & pods -> wait WORKLOAD_SETTLE_TIME_S
# 2) Then wait up to CONFIG_MAP_TIMEOUT_S for a plan ConfigMap to appear
# 3) Once plan is present -> wait PLAN_EXECUTION_TIME_S to be sure evictions/new pods are done
WORKLOAD_SETTLE_TIME_S = 2
CONFIG_MAP_TIMEOUT_S = 10
PLAN_EXECUTION_MAX_WAIT_S = 10
PLAN_EXECUTION_MIN_WAIT_S = 1
PLAN_EXECUTION_POLL_INTERVAL_S = 1

# Manual HTTP trigger (same style as test_runner)
SOLVER_TRIGGER_URL = "http://localhost:18080/solve"
SOLVER_TRIGGER_TIMEOUT_S = 60

# NEW: /active endpoint (solver status)
SOLVER_ACTIVE_URL = "http://localhost:18080/active"
SOLVER_ACTIVE_TIMEOUT_S = 5.0

# KWOK node names
NODE_NAMES = [f"kwok-node-{i+1}" for i in range(NUM_NODES)]

# Mode combinations to exercise in pytest (central place to tweak)
# Each entry: (opt_mode, opt_sync, workload_ids)
PYTEST_MODE_CASES: List[Tuple[str, bool, List[str]]] = [
    ("manual_blocking", True, ["prioaware"]),
    ("manual_blocking", False, ["prioaware"]),
    ("manual", True, ["sameprio"]),
    # ("manual", False, ["sameprio"]), #TODO: find out why Async fails and per_pod fails
    # ("per_pod", True, ["sameprio"]),
    ("periodic", True, ["sameprio"]),
    # ("periodic", False, ["sameprio"]),
    ("interlude", True, ["sameprio", "higharrival"]),
    # ("interlude", False, ["sameprio", "higharrival"]),
]


# ---------------------------------------------------------------------------
# Helpers reused from setup_cluster + extra test-only helpers
# ---------------------------------------------------------------------------

def wait_for_plugin_configmap(
    ctx: str,
    logger: logging.Logger,
    *,
    timeout_s: int = PLUGIN_CFG_TIMEOUT_S,
) -> Optional[Dict[str, Any]]:
    """
    Wait until the plugin readiness ConfigMap appears.

    The plugin is considered 'ready' once it has created the ConfigMap:
      - namespace: SYSTEM_NAMESPACE
      - name:      PLUGIN_CFG_NAME

    Returns the ConfigMap JSON dict, or None on timeout.
    """
    deadline = time.time() + timeout_s
    last_err: Optional[Exception] = None

    while time.time() < deadline:
        try:
            cm = get_json_ctx(
                ctx,
                [
                    "-n", SYSTEM_NAMESPACE,
                    "get", "cm", PLUGIN_CFG_NAME,
                    "-o", "json",
                ],
            )
            logger.info(
                "plugin readiness ConfigMap %s/%s found; plugin is ready.",
                SYSTEM_NAMESPACE,
                PLUGIN_CFG_NAME,
            )
            return cm
        except RuntimeError as e:
            last_err = e
            logger.info(
                "plugin not ready yet; waiting for ConfigMap %s/%s (error: %s)",
                SYSTEM_NAMESPACE,
                PLUGIN_CFG_NAME,
                e,
            )
            time.sleep(1.0)

    logger.warning(
        "plugin ConfigMap %s/%s did not appear within %.1fs (last error: %s)",
        SYSTEM_NAMESPACE,
        PLUGIN_CFG_NAME,
        timeout_s,
        last_err,
    )
    return None


def get_latest_plan_configmap(
    ctx: str,
    logger: logging.Logger,
    *,
    timeout_s: int = CONFIG_MAP_TIMEOUT_S,
) -> Optional[Dict[str, Any]]:
    """
    Poll for the latest plan ConfigMap in SYSTEM_NAMESPACE, filtered by PLAN_LABEL_KEY=true.
    Returns the newest ConfigMap (by creationTimestamp/resourceVersion) or None.
    """
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            data = get_json_ctx(
                ctx,
                [
                    "-n", SYSTEM_NAMESPACE,
                    "get", "cm",
                    "-l", f"{PLAN_LABEL_KEY}=true",
                    "-o", "json",
                ],
            )
        except RuntimeError as e:
            logger.info("waiting for plan ConfigMap (kubectl failed): %s", e)
            time.sleep(1.0)
            continue

        items = data.get("items", [])
        if items:
            items.sort(
                key=lambda cm: (
                    (cm.get("metadata") or {}).get("creationTimestamp", ""),
                    (cm.get("metadata") or {}).get("resourceVersion", "0"),
                )
            )
            return items[-1]

        logger.info("no plan ConfigMap yet; sleeping...")
        time.sleep(1.0)

    logger.warning("no plan ConfigMap found within %ss", timeout_s)
    return None


def stored_plan_is_done(sp: Dict[str, Any]) -> bool:
    """
    Return True if the StoredPlan JSON reports done.
    """
    status = sp.get("plan_status")
    if isinstance(status, str):
        return status.lower() == "completed"
    return False


def wait_for_plan_done(
    logger: logging.Logger,
    *,
    ctx: str,
    cm_name: str,
    min_wait_s: float = PLAN_EXECUTION_MIN_WAIT_S,
    max_wait_s: float = PLAN_EXECUTION_MAX_WAIT_S,
    poll_interval_s: float = PLAN_EXECUTION_POLL_INTERVAL_S,
) -> Optional[Dict[str, Any]]:
    """
    Poll the already-existing plan ConfigMap until its StoredPlan.status is 'done',
    or until max_wait_s is exceeded.
    """
    start = time.time()
    deadline = start + max_wait_s

    if min_wait_s > 0:
        logger.info(
            "Waiting %.1fs minimum before checking plan status in ConfigMap %s/%s.",
            min_wait_s,
            SYSTEM_NAMESPACE,
            cm_name,
        )
        time.sleep(min_wait_s)

    attempt = 0
    last_err: Optional[Exception] = None

    while time.time() < deadline:
        attempt += 1
        try:
            cm = get_json_ctx(
                ctx,
                [
                    "-n", SYSTEM_NAMESPACE,
                    "get", "cm", cm_name,
                    "-o", "json",
                ],
            )
            raw_plan = (cm.get("data") or {}).get(PLAN_DATA_KEY)
            if not raw_plan:
                logger.warning(
                    "Plan ConfigMap %s/%s has no data[%s] on attempt %d; retrying...",
                    SYSTEM_NAMESPACE,
                    cm_name,
                    PLAN_DATA_KEY,
                    attempt,
                )
            else:
                try:
                    sp = json.loads(raw_plan)
                except Exception as e:
                    last_err = e
                    logger.warning(
                        "Failed to parse StoredPlan JSON from %s/%s on attempt %d: %s",
                        SYSTEM_NAMESPACE,
                        cm_name,
                        attempt,
                        e,
                    )
                else:
                    if stored_plan_is_done(sp):
                        elapsed = time.time() - start
                        logger.info(
                            "Plan status reached 'done' in ConfigMap %s/%s after %.2fs (attempt %d).",
                            SYSTEM_NAMESPACE,
                            cm_name,
                            elapsed,
                            attempt,
                        )
                        return sp

                    logger.info(
                        "Plan status not done yet in ConfigMap %s/%s (attempt %d); polling again...",
                        SYSTEM_NAMESPACE,
                        cm_name,
                        attempt,
                    )

        except RuntimeError as e:
            last_err = e
            logger.warning(
                "Failed to read plan ConfigMap %s/%s on attempt %d: %s",
                SYSTEM_NAMESPACE,
                cm_name,
                attempt,
                e,
            )

        remaining = deadline - time.time()
        if remaining <= 0:
            break
        time.sleep(min(poll_interval_s, max(0.0, remaining)))

    logger.error(
        "Plan status did not reach 'done' within %.1fs for ConfigMap %s/%s (last_err=%r)",
        max_wait_s,
        SYSTEM_NAMESPACE,
        cm_name,
        last_err,
    )
    return None


def plan_placements_by_pod(sp: Dict[str, Any]) -> Dict[Tuple[str, str], str]:
    """
    Given a StoredPlan dict, compute the FINAL planned placement:

      - Start from plan.old_placements as baseline.
      - Remove pods listed in plan.evicts.
      - Override/add pods from plan.new_placements.

    Returns mapping: (namespace, name) -> final planned node.
    Evicted pods are intentionally omitted (no final node).
    """
    plan = sp.get("plan") or {}

    mapping: Dict[Tuple[str, str], str] = {}

    # 1) Baseline: old placements
    for op in plan.get("old_placements") or []:
        pod = op.get("pod") or {}
        ns = pod.get("namespace") or TEST_NAMESPACE
        name = pod.get("name")
        node = op.get("node")
        if ns and name and node:
            mapping[(str(ns), str(name))] = str(node)

    # 2) Evicts: remove from mapping
    for ev in plan.get("evicts") or []:
        pod = ev.get("pod") or {}
        ns = pod.get("namespace") or TEST_NAMESPACE
        name = pod.get("name")
        if ns and name:
            mapping.pop((str(ns), str(name)), None)

    # 3) New placements: override / add
    for pl in plan.get("new_placements") or []:
        pod = pl.get("pod") or {}
        ns = pod.get("namespace") or TEST_NAMESPACE
        name = pod.get("name")
        to_node = pl.get("to_node")
        if ns and name and to_node:
            mapping[(str(ns), str(name))] = str(to_node)

    return mapping


def build_expected_assignment(
    scenario: WorkloadScenario,
    namespace: str,
) -> Dict[Tuple[str, str], bool]:
    """
    Build expected assignment mapping at ReplicaSet level:
      (namespace, rs_name) -> should_run
    Only pods with expected_assignment != None are included.
    """
    mapping: Dict[Tuple[str, str], bool] = {}
    for step in scenario.steps:
        for pod in step.pods:
            if pod.expected_assignment is None:
                continue
            rs_name = rs_name_for_pod(scenario, pod)
            mapping[(namespace, rs_name)] = bool(pod.expected_assignment)
    return mapping


def wait_for_workload_step(
    logger: logging.Logger,
    ctx: str,
    namespace: str,
    scenario: WorkloadScenario,
    step: WorkloadStep,
    *,
    step_index: int,
    num_steps: int,
    disable_wait_and_active_checks: bool = DEFAULT_DISABLE_WAIT_AND_ACTIVE_CHECKS,
) -> bool:
    """
    Wait for ReplicaSets of a workload step according to step.wait_mode
    and enforce the /active invariants according to step.active_plan_check_mode.

    If disable_wait_and_active_checks is True, this returns immediately
    without waiting or calling /active.
    """
    if disable_wait_and_active_checks:
        logger.info(
            "WAIT & ACTIVE-CHECK DISABLED: skipping pod waits and /active checks "
            "for step %s (%d/%d) in namespace %s",
            step.name,
            step_index + 1,
            num_steps,
            namespace,
        )
        return True

    # Normalize active_plan_check_mode
    raw_mode = step.active_plan_check_mode
    mode = (raw_mode or "").lower()
    if mode not in {"each_pod", "after_step", "never", ""}:
        logger.warning(
            "Unknown active_plan_check_mode=%r for step %s; treating as 'after_step'.",
            raw_mode,
            step.name,
        )
        mode = "after_step"

    # Do not enforce /active invariants on the final step
    is_last_step = (step_index == num_steps - 1)
    should_check_any = (not is_last_step) and (mode in {"each_pod", "after_step"})

    if step.wait_mode not in {"running", "exist", "none"}:
        logger.warning(
            "Unknown wait_mode=%r for step %s; skipping waits and /active checks.",
            step.wait_mode,
            step.name,
        )
        return True

    # No wait for pods; only possible after-step check
    if step.wait_mode == "none":
        logger.info(
            "Workload step %s: wait_mode=none; not waiting for pods.",
            step.name,
        )
        if should_check_any and mode == "after_step":
            when = (
                f"after workload step {step_index + 1}/{num_steps} "
                f"({step.name})"
            )
            if not assert_no_active_plan_http(logger, when=when):
                return False
        return True

    # Normal wait: per-pod waits, optional per-pod /active checks
    for pod in step.pods:
        rs_name = rs_name_for_pod(scenario, pod)
        logger.info(
            "Waiting for RS %s/%s to be %s (timeout=%.1fs)",
            namespace,
            rs_name,
            step.wait_mode,
            float(step.wait_timeout_s),
        )
        _ = wait_rs_pods(
            logger,
            ctx,
            rs_name,
            namespace,
            step.wait_timeout_s,
            mode=step.wait_mode,
        )

        # Per-pod invariant, if requested
        if should_check_any and mode == "each_pod":
            when = (
                f"after pod {pod.id} in step {step_index + 1}/{num_steps} "
                f"({step.name})"
            )
            if not assert_no_active_plan_http(logger, when=when):
                return False

    # After-step invariant
    if should_check_any and mode == "after_step":
        when = (
            f"after workload step {step_index + 1}/{num_steps} "
            f"({step.name})"
        )
        if not assert_no_active_plan_http(logger, when=when):
            return False

    return True


def write_plan_debug_files(
    logger: logging.Logger,
    out_dir: Path,
    raw_plan: str,
    plan_map: Dict[Tuple[str, str], str],
    actual_nodes: Dict[Tuple[str, str], str],
    pod_to_rs: Dict[Tuple[str, str], str],
    expected_assignment: Dict[Tuple[str, str], bool],
    scenario: WorkloadScenario,
) -> None:
    """
    Write debug artifacts for StoredPlan vs actual cluster state.
    """
    try:
        raw_plan_path = out_dir / f"latest_plan_raw_{scenario.id}.json"
        csv_path = out_dir / f"latest_plan_vs_actual_{scenario.id}.csv"

        # Raw plan
        raw_plan_path.write_text(raw_plan, encoding="utf-8")

        # CSV: merged view of plan placements, actual state, and expected_assignment
        all_keys = sorted(set(plan_map.keys()) | set(actual_nodes.keys()))
        with csv_path.open("w", newline="", encoding="utf-8") as f:
            writer = csv.writer(f)
            writer.writerow(
                [
                    "pod",              # real pod name
                    "planned_node",
                    "actual_node",
                    "expected_assignment",  # from owning ReplicaSet (if any)
                ]
            )
            for ns, name in all_keys:
                planned_node = plan_map.get((ns, name), "")
                actual_node = actual_nodes.get((ns, name), "")
                rs_name = pod_to_rs.get((ns, name))
                should_run = (
                    expected_assignment.get((ns, rs_name))
                    if rs_name is not None
                    else None
                )
                expected_str = (
                    "" if should_run is None else ("true" if should_run else "false")
                )
                writer.writerow(
                    [
                        name,
                        planned_node,
                        actual_node,
                        expected_str,
                    ]
                )

        logger.info(
            "wrote plan debug files: %s, %s",
            raw_plan_path,
            csv_path,
        )
    except Exception as e:
        logger.warning("failed to write plan debug files: %s", e)


def evaluate_plan_and_cluster_state(
    logger: logging.Logger,
    *,
    ctx: str,
    namespace: str,
    stored_plan: Dict[str, Any],
    expected_assignment: Dict[Tuple[str, str], bool],
    scenario: WorkloadScenario,
) -> bool:
    """
    After a StoredPlan has been published, wait for plan execution, fetch the
    actual cluster state, and compare plan vs actual + expectations.
    """
    # Convert StoredPlan to final planned placement per pod
    plan_map = plan_placements_by_pod(stored_plan)

    # Actual node assignments
    pods_json = get_json_ctx(
        ctx,
        ["-n", namespace, "get", "pods", "-o", "json"],
    )

    actual_nodes: Dict[Tuple[str, str], str] = {}
    pod_to_rs: Dict[Tuple[str, str], str] = {}
    rs_seen = set()
    rs_assigned: Dict[Tuple[str, str], bool] = {}

    for item in pods_json.get("items", []):
        meta = (item.get("metadata") or {})
        spec = (item.get("spec") or {})
        name = meta.get("name", "")
        ns = meta.get("namespace", namespace)
        node = spec.get("nodeName", "") or ""
        labels = meta.get("labels") or {}

        if not name:
            continue

        key_pod = (str(ns), str(name))
        actual_nodes[key_pod] = node

        rs_name = labels.get("app")
        if rs_name:
            key_rs = (str(ns), str(rs_name))
            rs_seen.add(key_rs)
            pod_to_rs[key_pod] = str(rs_name)
            if node:
                rs_assigned[key_rs] = rs_assigned.get(key_rs, False) or True
            else:
                rs_assigned.setdefault(key_rs, False)

    # Sanity: make sure we saw at least one pod for each logical ReplicaSet
    for step in scenario.steps:
        for pod in step.pods:
            rs_name = rs_name_for_pod(scenario, pod)
            key_rs = (namespace, rs_name)
            if key_rs not in rs_seen:
                logger.error(
                    "no pods found for ReplicaSet %s/%s (label app=%s) via kubectl",
                    namespace,
                    rs_name,
                    rs_name,
                )
                return False

    # Debug artifacts
    try:
        out_dir = Path(__file__).resolve().parent
        raw_plan = json.dumps(stored_plan, indent=2)
        write_plan_debug_files(
            logger,
            out_dir,
            raw_plan,
            plan_map,
            actual_nodes,
            pod_to_rs,
            expected_assignment,
            scenario,
        )
    except Exception as e:
        logger.warning("failed to write plan debug files wrapper: %s", e)

    # Check 1: plan vs actual placements
    mismatches = []
    for key, planned_node in plan_map.items():
        actual_node = actual_nodes.get(key, "")
        if planned_node and actual_node and planned_node != actual_node:
            mismatches.append((key, planned_node, actual_node))

    if mismatches:
        logger.error("plan vs actual node mismatches: %s", mismatches)
        return False

    # Check 2: expected assignment vs actual assignment
    assignment_mismatches = []
    for key_rs, should_run in expected_assignment.items():
        is_assigned = rs_assigned.get(key_rs, False)
        if is_assigned != should_run:
            assignment_mismatches.append((key_rs, should_run, is_assigned))

    if assignment_mismatches:
        logger.error(
            "expected assignment mismatches (ReplicaSet-level): %s",
            assignment_mismatches,
        )
        return False

    logger.info(
        "Scenario %s: plan placements are consistent with actual pod "
        "assignments and expected assignment.",
        scenario.id,
    )
    return True


def assert_no_active_plan_http(logger: logging.Logger, *, when: str) -> bool:
    """
    Call GET /active via the shared helper and assert that no active plan is reported.
    """
    code, body = get_solver_active_status_http(SOLVER_ACTIVE_URL, timeout=SOLVER_ACTIVE_TIMEOUT_S)
    body_compact = (body or "").replace("\n", "\\n")
    if len(body_compact) > 600:
        body_compact = body_compact[:600] + "...(truncated)"

    logger.info("solver /active %s: code=%s body=%s", when, code, body_compact)

    if code != 200:
        logger.error("Unexpected /active status=%s %s", code, when)
        return False

    try:
        payload = json.loads(body)
    except Exception as e:
        logger.error("Failed to parse /active JSON %s: %s body=%r", when, e, body)
        return False

    active = bool(payload.get("active", False))
    if active:
        logger.error("Expected no active plan %s, but /active reported active=true: %s", when, payload)
        return False

    return True


# ---------------------------------------------------------------------------
# Core integration function
# ---------------------------------------------------------------------------

def run_mode_integration(
    opt_mode: str,
    opt_sync: bool,
    *,
    scenario: WorkloadScenario,
    cluster_name: str = DEFAULT_CLUSTER_NAME,
    kwok_runtime: str = DEFAULT_KWOK_RUNTIME,
    kwokctl_config_file: str = DEFAULT_KWOKCTL_CONFIG,
    disable_wait_and_active_checks: bool = DEFAULT_DISABLE_WAIT_AND_ACTIVE_CHECKS,
) -> bool:
    """
    End-to-end integration test for a given (opt_mode, scenario).

    If disable_wait_and_active_checks is True, pod waits and /active checks
    during workload application are skipped.
    """
    if opt_mode not in VALID_OPT_MODES:
        raise ValueError(f"Invalid opt_mode={opt_mode!r}; expected one of {sorted(VALID_OPT_MODES)}")

    LOG = setup_logging(
        name=f"mpo-itest-{scenario.id}-{opt_mode}-{opt_sync}",
        prefix=f"[mpo-itest scenario={scenario.id} mode={opt_mode} sync={opt_sync}] ",
        level="INFO",
    )
    header, footer = make_header_footer(
        f"MPOptimizer KWOK integration: scenario={scenario.id}, mode={opt_mode}, sync={opt_sync}"
    )
    LOG.info("\n%s\ncluster=%s\n%s", header, cluster_name, footer)
    LOG.info("Scenario description: %s", scenario.description)

    ctx = f"kwok-{cluster_name}"

    # Pre-compute node capacities as integers (per-node capacity)
    node_cpu_m = qty_to_mcpu_int(NODE_CPU)
    node_mem_b = qty_to_bytes_int(NODE_MEM)

    # --- KWOK cluster (always recreate) ---
    base_cfg = load_kwokctl_config(kwokctl_config_file)
    cfg_for_mode = build_kwokctl_config_for_mode(base_cfg, opt_mode, opt_sync)

    ensure_kwok_cluster(
        LOG,
        cluster_name,
        kwok_runtime,
        cfg_for_mode,
        recreate=True,
    )

    # --- Nodes ---
    total_pods = scenario_total_replicas(scenario)
    create_kwok_nodes(
        LOG,
        ctx,
        NUM_NODES,
        NODE_CPU,
        NODE_MEM,
        pods_cap=kwok_pods_cap(total_pods),
    )

    # --- Namespace + PCs ---
    ensure_namespace(LOG, ctx, TEST_NAMESPACE)
    num_prios = max(NUM_PRIORITIES, scenario_max_priority(scenario))
    ensure_priority_classes(LOG, ctx, num_prios, prefix="p", start=1)

    # --- Plugin readiness: wait for plugin_config before applying workloads ---
    LOG.info(
        "Waiting up to %.1fs for plugin readiness ConfigMap %s/%s before applying workloads.",
        PLUGIN_CFG_TIMEOUT_S,
        SYSTEM_NAMESPACE,
        PLUGIN_CFG_NAME,
    )
    plugin_cfg_cm = wait_for_plugin_configmap(ctx, LOG, timeout_s=PLUGIN_CFG_TIMEOUT_S)
    if plugin_cfg_cm is None:
        LOG.error(
            "Plugin readiness ConfigMap %s/%s not found within %.1fs; aborting integration.",
            SYSTEM_NAMESPACE,
            PLUGIN_CFG_NAME,
            PLUGIN_CFG_TIMEOUT_S,
        )
        return False

    # --- Apply workload steps ---
    num_steps = len(scenario.steps)
    for idx, step in enumerate(scenario.steps):
        LOG.info("=== Applying workload step: %s ===", step.name)
        apply_workload_step(
            LOG,
            ctx,
            TEST_NAMESPACE,
            scenario,
            step,
            node_cpu_m,
            node_mem_b,
        )
        if not wait_for_workload_step(
            LOG,
            ctx,
            TEST_NAMESPACE,
            scenario,
            step,
            step_index=idx,
            num_steps=num_steps,
            disable_wait_and_active_checks=disable_wait_and_active_checks,
        ):
            LOG.error(
                "Workload step %s failed (wait or /active invariant).",
                step.name,
            )
            return False

    # 1) Workload settle time
    LOG.info("Sleeping %.1fs for workload to settle before expecting a plan.", WORKLOAD_SETTLE_TIME_S)
    time.sleep(WORKLOAD_SETTLE_TIME_S)

    # Manual modes: trigger optimization via HTTP
    if opt_mode.startswith("manual"):
        LOG.info("Manual mode: triggering solver via HTTP: %s", SOLVER_TRIGGER_URL)
        code, body = solver_trigger_http(LOG, SOLVER_TRIGGER_URL, SOLVER_TRIGGER_TIMEOUT_S)
        body_compact = (body or "").replace("\n", "\\n")
        if len(body_compact) > 600:
            body_compact = body_compact[:600] + "...(truncated)"
        LOG.info("solver_response code=%s body=%s", code, body_compact)

    # 2) Wait for plan ConfigMap (with timeout)
    cm = get_latest_plan_configmap(ctx, LOG, timeout_s=CONFIG_MAP_TIMEOUT_S)
    if cm is None:
        LOG.warning(
            "No plan ConfigMap found within %.1fs; treating as integration failure.",
            CONFIG_MAP_TIMEOUT_S,
        )
        return False

    cm_name = (cm.get("metadata") or {}).get("name")
    if not cm_name:
        LOG.error("StoredPlan ConfigMap is missing metadata.name; cannot poll for completion.")
        return False

    # Initial parse
    raw_plan = (cm.get("data") or {}).get(PLAN_DATA_KEY)
    if not raw_plan:
        LOG.error("StoredPlan ConfigMap data[%s] is empty or missing", PLAN_DATA_KEY)
        return False
    try:
        _ = json.loads(raw_plan)
    except Exception as e:
        LOG.error("Failed initial parse of StoredPlan JSON from ConfigMap: %s", e)
        return False

    # Now poll the same ConfigMap until status=='done' (or timeout)
    stored_plan = wait_for_plan_done(
        LOG,
        ctx=ctx,
        cm_name=cm_name,
        min_wait_s=PLAN_EXECUTION_MIN_WAIT_S,
        max_wait_s=PLAN_EXECUTION_MAX_WAIT_S,
        poll_interval_s=PLAN_EXECUTION_POLL_INTERVAL_S,
    )
    if stored_plan is None:
        return False

    expected_assignment = build_expected_assignment(scenario, TEST_NAMESPACE)

    ok = evaluate_plan_and_cluster_state(
        LOG,
        ctx=ctx,
        namespace=TEST_NAMESPACE,
        stored_plan=stored_plan,
        expected_assignment=expected_assignment,
        scenario=scenario,
    )
    return ok


# ---------------------------------------------------------------------------
# Pytest entrypoint
# ---------------------------------------------------------------------------

if pytest is not None:
    # Flatten (workload_ids, mode, sync) -> (workload_id, mode, sync)
    PARAM_CASES: List[Tuple[str, bool, str]] = [
        (opt_mode, opt_sync, wid)
        for (opt_mode, opt_sync, workload_ids) in PYTEST_MODE_CASES
        for wid in workload_ids
    ]

    @pytest.mark.parametrize(
        "opt_mode,opt_sync,workload_id",
        PARAM_CASES,
    )
    def test_modes_end_to_end(opt_mode: str, opt_sync: bool, workload_id: str):
        scenario = WORKLOAD_SCENARIOS[workload_id]
        assert run_mode_integration(
            opt_mode,
            opt_sync,
            scenario=scenario,
            cluster_name=DEFAULT_CLUSTER_NAME,
            kwok_runtime=DEFAULT_KWOK_RUNTIME,
            kwokctl_config_file=DEFAULT_KWOKCTL_CONFIG,
        )


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def build_argparser() -> argparse.ArgumentParser:
    ap = argparse.ArgumentParser(
        description="KWOK-based integration test for MyPriorityOptimizer modes.",
    )
    ap.add_argument("--cluster-name", default=DEFAULT_CLUSTER_NAME,
                    help=f"KWOK cluster name (default: {DEFAULT_CLUSTER_NAME})")
    ap.add_argument("--kwok-runtime", default=DEFAULT_KWOK_RUNTIME,
                    choices=["binary", "docker"],
                    help=f"KWOK runtime (default: {DEFAULT_KWOK_RUNTIME})")
    ap.add_argument("--kwokctl-config-file", default=DEFAULT_KWOKCTL_CONFIG,
                    help=f"KwokctlConfiguration YAML (default: {DEFAULT_KWOKCTL_CONFIG})")
    ap.add_argument("--optimize-mode", required=True,
                    choices=sorted(VALID_OPT_MODES),
                    help="OPTIMIZE_MODE value")
    ap.add_argument("--optimize-sync", action="store_true",
                    help="Set OPTIMIZE_SYNC=true (default: false)")
    ap.add_argument("--workload-id", default=DEFAULT_WORKLOAD_ID,
                    choices=sorted(WORKLOAD_SCENARIOS.keys()),
                    help=f"Workload scenario id (default: {DEFAULT_WORKLOAD_ID})")
    ap.add_argument(
        "--disable-wait-and-active-checks",
        action="store_true",
        help="Disable waiting for pods and /active invariants while applying workloads.",
    )
    return ap


def main() -> None:
    ap = build_argparser()
    args = ap.parse_args()

    scenario = WORKLOAD_SCENARIOS[args.workload_id]

    ok = run_mode_integration(
        opt_mode=args.optimize_mode,
        opt_sync=args.optimize_sync,
        scenario=scenario,
        cluster_name=args.cluster_name,
        kwok_runtime=args.kwok_runtime,
        kwokctl_config_file=args.kwokctl_config_file,
        disable_wait_and_active_checks=args.disable_wait_and_active_checks,
    )
    sys.exit(0 if ok else 1)

if __name__ == "__main__":
    main()