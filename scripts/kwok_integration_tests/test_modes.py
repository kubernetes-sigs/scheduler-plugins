# test_modes.py
#!/usr/bin/env python3

import argparse, csv
import json
import logging
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, Any, Tuple, Optional, List

import yaml

# pytest is optional: used only when running via pytest
try:
    import pytest  # type: ignore
except ImportError:  # pragma: no cover
    pytest = None  # type: ignore

from scripts.helpers.general_helpers import (
    setup_logging,
    make_header_footer,
    qty_to_mcpu_int,
    qty_to_mcpu_str,
    qty_to_bytes_int,
    qty_to_bytes_str,
    solver_trigger_http,
    get_solver_active_status_http,
)
from scripts.helpers.kubectl_helpers import (
    kubectl_apply_yaml,
    ensure_namespace,
    ensure_priority_classes,
    wait_rs_pods,
    get_json_ctx,
)
from scripts.helpers.kwok_helpers import (
    ensure_kwok_cluster,
    create_kwok_nodes,
    kwok_pods_cap,
    merge_kwokctl_envs,
    yaml_kwok_rs,
)

# ---------------------------------------------------------------------------
# Data model for workloads
# ---------------------------------------------------------------------------

@dataclass
class WorkloadPod:
    """
    Logical test pod specification expressed as fractions of node capacity.

    cpu, mem are fractions of a *single node's* capacity (0.0–1.0+).
    priority is an integer PriorityClass index (1..NUM_PRIORITIES).
    expected_assignment:
      - True  => we assert the ReplicaSet ends up scheduled on some node
      - False => we assert the ReplicaSet ends up NOT scheduled
      - None  => we don't assert anything for this ReplicaSet
    """
    id: int
    cpu: float
    mem: float
    priority: int
    replicas: int = 1
    expected_assignment: Optional[bool] = None


@dataclass
class WorkloadStep:
    """
    A step in a workload scenario: a batch of ReplicaSets applied together,
    and a policy for how long we wait after creating them.
    """
    name: str
    pods: List[WorkloadPod]
    wait_mode: str = "exist"  # "exist", "running", or "none"
    wait_timeout_s: int = 5


@dataclass
class WorkloadScenario:
    """
    Complete workload scenario composed of one or more steps.
    """
    id: str
    description: str
    steps: List[WorkloadStep]


# ---------------------------------------------------------------------------
# Constants (adjust if needed)
# ---------------------------------------------------------------------------

DEFAULT_CLUSTER_NAME = "kwok1"
DEFAULT_KWOK_RUNTIME = "binary"
DEFAULT_KWOKCTL_CONFIG = "data/configs-kwokctl/test/solver-default.yaml"

TEST_NAMESPACE = "mpo-itest"
NUM_NODES = 2
NODE_CPU = "1000m"
NODE_MEM = "1Gi"
NUM_PRIORITIES = 3

PLAN_NAMESPACE = "kube-system"

# Must match your plugin's ConfigMap label & data key
PLAN_LABEL_KEY = "plan"
PLAN_DATA_KEY = PLAN_LABEL_KEY + ".json"

# Plugin readiness ConfigMap (created once the plugin is fully ready)
PLUGIN_CFG_NAMESPACE = PLAN_NAMESPACE
PLUGIN_CFG_NAME = "plugin-config"
PLUGIN_CFG_DATA_KEY = "plugin-config.json"
PLUGIN_CFG_TIMEOUT_S = 10  # timeout for waiting for plugin readiness

POD_TIMEOUT_S = 5

# Timing model:
# 1) After creating workload & pods exist -> wait WORKLOAD_SETTLE_TIME_S
# 2) Then wait up to CONFIG_MAP_TIMEOUT_S for a plan ConfigMap to appear
# 3) Once plan is present -> wait PLAN_EXECUTION_TIME_S for evictions/new pods
WORKLOAD_SETTLE_TIME_S = 2
CONFIG_MAP_TIMEOUT_S = 10
PLAN_EXECUTION_TIME_S = 10

# Manual HTTP trigger (same style as test_runner)
SOLVER_TRIGGER_URL = "http://localhost:18080/solve"
SOLVER_TRIGGER_TIMEOUT_S = 60

# NEW: /active endpoint (solver status)
SOLVER_ACTIVE_URL = "http://localhost:18080/active"
SOLVER_ACTIVE_TIMEOUT_S = 3.0

# Valid optimization modes for this integration test
VALID_OPT_MODES = {"per_pod", "periodic", "interlude", "manual"}

# Node names KWOK normally uses for this cluster size.
# Used only for documentation / expectations; we don't *assert* on them globally.
EXPECTED_NODE_NAMES = [f"kwok-node-{i+1}" for i in range(NUM_NODES)]

# Mode combinations to exercise in pytest (central place to tweak)
MODE_CASES: List[Tuple[str, str, bool]] = [
    # ("per_pod", "preenqueue", False),
    # ("per_pod", "postfilter", False),
    # ("per_pod", "postfilter", True),
    # ("periodic", "preenqueue", False),
    # ("periodic", "preenqueue", True),
    # ("periodic", "postfilter", False),
    # ("periodic", "postfilter", True),
    # ("interlude", "preenqueue", False),
    # ("interlude", "preenqueue", True),
    # ("interlude", "postfilter", False),
    ("interlude", "postfilter", True),
    ("manual", "preenqueue", False),
    ("manual", "postfilter", False),
    ("manual", "postfilter", True),
]

# ---------------------------------------------------------------------------
# Workload scenario definitions
# ---------------------------------------------------------------------------

def _rs_name_for_pod(scenario: WorkloadScenario, pod: WorkloadPod) -> str:
    """
    Stable ReplicaSet name derived from (scenario.id, pod.id).
    """
    return f"{scenario.id}-pod-{pod.id:03d}"


def _scenario_same_priority() -> WorkloadScenario:
    big_step = WorkloadStep(
        name="big-first",
        pods=[
            WorkloadPod(id=101, cpu=0.8, mem=0.1, priority=3, expected_assignment=False),
            WorkloadPod(id=102, cpu=0.7, mem=0.1, priority=3, expected_assignment=True),
        ],
        wait_mode="running",
        wait_timeout_s=POD_TIMEOUT_S,
    )
    small_step = WorkloadStep(
        name="small-later",
        pods=[
            WorkloadPod(id=201, cpu=0.4, mem=0.1, priority=3, expected_assignment=True),
            WorkloadPod(id=202, cpu=0.3, mem=0.1, priority=3, expected_assignment=True),
            WorkloadPod(id=203, cpu=0.3, mem=0.1, priority=3, expected_assignment=True),
            WorkloadPod(id=204, cpu=0.3, mem=0.1, priority=3, expected_assignment=True),
        ],
        wait_mode="exist",
        wait_timeout_s=POD_TIMEOUT_S,
    )
    return WorkloadScenario(
        id="sameprio",
        description=(
            "Equal-priority repacking: two big p3 pods first, four smaller "
            "p3 pods later; default will not evict reschedule equal priorities, optimizer can."
        ),
        steps=[big_step, small_step],
    )


def _scenario_different_priority() -> WorkloadScenario:
    """
    Scenario 2: Mixed priorities.

    Cluster: 2 nodes, each 1.0 CPU.

    - Low-priority p1 pods (0.5 CPU each) fill the cluster.
    - Later, high-priority p3 pods (0.7 CPU each) arrive.

    We assert that all high-priority pods end up scheduled (expected=True),
    while we don't assert anything for the low-priority background pods.
    """
    low_step = WorkloadStep(
        name="low-background",
        pods=[
            WorkloadPod(id=101, cpu=0.5, mem=0.1, priority=1, expected_assignment=None),
            WorkloadPod(id=102, cpu=0.5, mem=0.1, priority=1, expected_assignment=None),
            WorkloadPod(id=103, cpu=0.5, mem=0.1, priority=1, expected_assignment=None),
            WorkloadPod(id=104, cpu=0.5, mem=0.1, priority=1, expected_assignment=None),
        ],
        wait_mode="running",
        wait_timeout_s=POD_TIMEOUT_S,
    )
    high_step = WorkloadStep(
        name="high-arrival",
        pods=[
            WorkloadPod(id=201, cpu=0.7, mem=0.1, priority=3, expected_assignment=True),
            WorkloadPod(id=202, cpu=0.7, mem=0.1, priority=3, expected_assignment=True),
        ],
        wait_mode="exist",
        wait_timeout_s=POD_TIMEOUT_S,
    )
    return WorkloadScenario(
        id="prioaware",
        description=(
            "Priority-aware scheduling: low-priority p1 pods fill the cluster; "
            "later high-priority p3 pods should be scheduled, evicting some p1 pods."
        ),
        steps=[low_step, high_step],
    )

def _scenario_high_arrival() -> WorkloadScenario:
    steps1 = [
        WorkloadStep(
            name=f"step{100+i}",
            pods=[WorkloadPod(
                id=100+i,
                cpu=0.3,
                mem=0.1,
                priority=1,
                expected_assignment=None,
            )],
            wait_mode="exist",
            wait_timeout_s=POD_TIMEOUT_S,
        )
        for i in range(1, 7)
    ]
    steps2 = [
        WorkloadStep(
            name=f"step{200+i}",
            pods=[WorkloadPod(
                id=200+i,
                cpu=0.1,
                mem=0.1,
                priority=1,
                expected_assignment=None,
            )],
            wait_mode="exist",
            wait_timeout_s=POD_TIMEOUT_S,
        )
        for i in range(1, 5)
    ]
    return WorkloadScenario(
        id="higharrival",
        description=(
            "High-arrival to check mode interlude behavior."
        ),
        steps=steps1 + steps2,
    )

WORKLOAD_SCENARIOS: Dict[str, WorkloadScenario] = {
    "sameprio": _scenario_same_priority(),
    "prioaware": _scenario_different_priority(),
    "higharrival": _scenario_high_arrival(),
}

DEFAULT_WORKLOAD_ID = "sameprio"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def load_kwokctl_config(path: str | Path) -> Dict[str, Any]:
    """Load a KwokctlConfiguration YAML as a dict."""
    p = Path(path)
    if not p.exists():
        raise SystemExit(f"kwokctl config not found: {p}")
    with p.open("r", encoding="utf-8") as f:
        doc = yaml.safe_load(f) or {}
    if not isinstance(doc, dict):
        raise SystemExit(f"{p}: expected KwokctlConfiguration mapping")
    return doc


def build_kwokctl_config_for_mode(
    base_doc: Dict[str, Any],
    opt_mode: str,
    hook_stage: str,
    opt_sync: bool,
) -> Dict[str, Any]:
    """
    Return a copy of base_doc that injects OPTIMIZE_MODE / OPTIMIZE_HOOK_STAGE
    envs into the kube-scheduler component.
    """
    envs = [
        {"name": "OPTIMIZE_MODE", "value": opt_mode},
        {"name": "OPTIMIZE_HOOK_STAGE", "value": hook_stage},
        {"name": "OPTIMIZE_SOLVE_SYNCH", "value": "true" if opt_sync else "false"},
    ]
    return merge_kwokctl_envs(base_doc, envs, component="kube-scheduler")


def wait_for_plugin_configmap(
    ctx: str,
    logger: logging.Logger,
    *,
    timeout_s: int = PLUGIN_CFG_TIMEOUT_S,
) -> Optional[Dict[str, Any]]:
    """
    Wait until the plugin readiness ConfigMap appears.

    The plugin is considered 'ready' once it has created the ConfigMap:
      - namespace: PLUGIN_CFG_NAMESPACE
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
                    "-n", PLUGIN_CFG_NAMESPACE,
                    "get", "cm", PLUGIN_CFG_NAME,
                    "-o", "json",
                ],
            )
            logger.info(
                "plugin readiness ConfigMap %s/%s found; plugin is ready.",
                PLUGIN_CFG_NAMESPACE,
                PLUGIN_CFG_NAME,
            )
            return cm
        except RuntimeError as e:
            last_err = e
            logger.info(
                "plugin not ready yet; waiting for ConfigMap %s/%s (error: %s)",
                PLUGIN_CFG_NAMESPACE,
                PLUGIN_CFG_NAME,
                e,
            )
            time.sleep(1.0)

    logger.warning(
        "plugin ConfigMap %s/%s did not appear within %.1fs (last error: %s)",
        PLUGIN_CFG_NAMESPACE,
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
    Poll for the latest plan ConfigMap in PLAN_NAMESPACE, filtered by PLAN_LABEL_KEY=true.
    Returns the newest ConfigMap (by creationTimestamp/resourceVersion) or None.
    """
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        try:
            data = get_json_ctx(
                ctx,
                [
                    "-n", PLAN_NAMESPACE,
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

    # 2) Evicts: remove from mapping (no final node for evicted pods)
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

def _scenario_max_priority(scenario: WorkloadScenario) -> int:
    m = 0
    for step in scenario.steps:
        for pod in step.pods:
            m = max(m, pod.priority)
    return m


def _scenario_total_replicas(scenario: WorkloadScenario) -> int:
    total = 0
    for step in scenario.steps:
        for pod in step.pods:
            total += int(pod.replicas)
    return total


def _build_expected_assignment(
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
            rs_name = _rs_name_for_pod(scenario, pod)
            mapping[(namespace, rs_name)] = bool(pod.expected_assignment)
    return mapping


def _apply_workload_step(
    logger: logging.Logger,
    ctx: str,
    namespace: str,
    scenario: WorkloadScenario,
    step: WorkloadStep,
    node_cpu_m: int,
    node_mem_b: int,
) -> None:
    """
    Create ReplicaSets for a single workload step and apply them via kubectl.
    """
    yaml_chunks: List[str] = []
    for pod in step.pods:
        rs_name = _rs_name_for_pod(scenario, pod)
        cpu_m = max(1, int(round(pod.cpu * node_cpu_m)))
        mem_b = max(1, int(round(pod.mem * node_mem_b)))
        cpu_str = qty_to_mcpu_str(cpu_m)
        mem_str = qty_to_bytes_str(mem_b)
        pc_name = f"p{pod.priority}"
        logger.info(
            "Workload step %s: RS %s (pod-id=%d) replicas=%d cpu=%s mem=%s prio=p%d",
            step.name,
            rs_name,
            pod.id,
            pod.replicas,
            cpu_str,
            mem_str,
            pod.priority,
        )
        yaml_chunks.append(
            yaml_kwok_rs(
                namespace,
                rs_name,
                int(pod.replicas),
                cpu_str,
                mem_str,
                pc_name,
            )
        )

    if not yaml_chunks:
        logger.info("Workload step %s has no pods; skipping apply.", step.name)
        return

    yaml_text = "".join(yaml_chunks)
    kubectl_apply_yaml(logger, ctx, yaml_text)


def _wait_for_workload_step(
    logger: logging.Logger,
    ctx: str,
    namespace: str,
    scenario: WorkloadScenario,
    step: WorkloadStep,
) -> None:
    """
    Wait for ReplicaSets of a workload step according to step.wait_mode.
    """
    if step.wait_mode not in {"running", "exist", "none"}:
        logger.warning("Unknown wait_mode=%r for step %s; skipping waits.", step.wait_mode, step.name)
        return
    if step.wait_mode == "none":
        logger.info("Workload step %s: wait_mode=none; not waiting for pods.", step.name)
        return

    for pod in step.pods:
        rs_name = _rs_name_for_pod(scenario, pod)
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


def _write_plan_debug_files(
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

        # Raw plan: exactly what was in the ConfigMap (or pretty-printed)
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
    actual cluster state, and compare:

      1) planned placements vs actual node assignments
      2) expected_assignment (ReplicaSet-level) vs actual assignment
    """
    # Convert StoredPlan to final planned placement per pod
    plan_map = plan_placements_by_pod(stored_plan)

    # 3) Wait for the plan to actually be executed (evictions + new pods)
    logger.info("Sleeping %.1fs to allow plan execution (evictions / new placements).", PLAN_EXECUTION_TIME_S)
    time.sleep(PLAN_EXECUTION_TIME_S)

    # 4) Actual node assignments (AFTER plan execution window)
    pods_json = get_json_ctx(
        ctx,
        ["-n", namespace, "get", "pods", "-o", "json"],
    )

    # actual_nodes: real pod (ns, name) -> nodeName (for plan vs actual)
    actual_nodes: Dict[Tuple[str, str], str] = {}
    # pod_to_rs: real pod (ns, name) -> logical ReplicaSet name (label "app")
    pod_to_rs: Dict[Tuple[str, str], str] = {}
    # rs_seen: which logical ReplicaSets actually have at least one pod object
    rs_seen = set()
    # rs_assigned: per (ns, rs_name) whether ANY replica is scheduled on a node
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
            # mark RS as assigned if *any* of its replicas has a nodeName
            if node:
                rs_assigned[key_rs] = rs_assigned.get(key_rs, False) or True
            else:
                rs_assigned.setdefault(key_rs, False)

    # Sanity: make sure we saw at least one pod for each logical ReplicaSet
    for step in scenario.steps:
        for pod in step.pods:
            rs_name = _rs_name_for_pod(scenario, pod)
            key_rs = (namespace, rs_name)
            if key_rs not in rs_seen:
                logger.error(
                    "no pods found for ReplicaSet %s/%s (label app=%s) via kubectl",
                    namespace,
                    rs_name,
                    rs_name,
                )
                return False

    # ------------------------------------------------------------------
    # Write debug artifacts
    # ------------------------------------------------------------------
    try:
        out_dir = Path(__file__).resolve().parent
        raw_plan = json.dumps(stored_plan, indent=2)
        _write_plan_debug_files(
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

    # Check 2: expected assignment vs actual assignment (derived from nodeName)
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

    Returns True if everything looks OK (status 200, JSON with active==False),
    False otherwise.
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

    # Contract: { "active": <bool>, ... }
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
    hook_stage: str,
    opt_sync: bool,
    *,
    scenario: WorkloadScenario,
    cluster_name: str = DEFAULT_CLUSTER_NAME,
    kwok_runtime: str = DEFAULT_KWOK_RUNTIME,
    kwokctl_config_file: str | Path = DEFAULT_KWOKCTL_CONFIG,
) -> bool:
    """
    End-to-end integration test for a given (opt_mode, hook_stage, scenario):

    1. Recreate KWOK cluster with scheduler env set to opt_mode/hook_stage.
    2. Create KWOK nodes.
    3. Ensure test namespace + PriorityClasses.
    4. Apply workload steps for the scenario in order, with per-step waits.
       After each *intermediate* step we assert there is no active plan yet
       via GET /active.
    5. Sleep WORKLOAD_SETTLE_TIME_S to let default scheduler / queues settle.
       - If opt_mode == "manual", trigger the solver via HTTP here.
    6. Wait (up to CONFIG_MAP_TIMEOUT_S) for a StoredPlan ConfigMap to appear.
    7. Evaluate plan vs cluster state and expected_assignment for the scenario.
    """
    if opt_mode not in VALID_OPT_MODES:
        raise ValueError(f"Invalid opt_mode={opt_mode!r}; expected one of {sorted(VALID_OPT_MODES)}")

    LOG = setup_logging(
        name=f"mpo-itest-{scenario.id}-{opt_mode}-{hook_stage}-{opt_sync}",
        prefix=f"[mpo-itest scenario={scenario.id} mode={opt_mode} hook={hook_stage} sync={opt_sync}] ",
        level="INFO",
    )
    header, footer = make_header_footer(
        f"MPOptimizer KWOK integration: scenario={scenario.id}, mode={opt_mode}, hook={hook_stage}, sync={opt_sync}"
    )
    LOG.info("\n%s\ncluster=%s\n%s", header, cluster_name, footer)
    LOG.info("Scenario description: %s", scenario.description)

    ctx = f"kwok-{cluster_name}"

    # Pre-compute node capacities as integers (per-node capacity)
    node_cpu_m = qty_to_mcpu_int(NODE_CPU)
    node_mem_b = qty_to_bytes_int(NODE_MEM)

    # --- KWOK cluster (always recreate) ---
    base_cfg = load_kwokctl_config(kwokctl_config_file)
    cfg_for_mode = build_kwokctl_config_for_mode(base_cfg, opt_mode, hook_stage, opt_sync)

    ensure_kwok_cluster(
        LOG,
        cluster_name,
        kwok_runtime,
        cfg_for_mode,
        recreate=True,
    )

    # --- Nodes ---
    total_pods = _scenario_total_replicas(scenario)
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
    num_prios = max(NUM_PRIORITIES, _scenario_max_priority(scenario))
    ensure_priority_classes(LOG, ctx, num_prios, prefix="p", start=1)

    # --- Plugin readiness: wait for plugin_config before applying workloads ---
    LOG.info(
        "Waiting up to %.1fs for plugin readiness ConfigMap %s/%s before applying workloads.",
        PLUGIN_CFG_TIMEOUT_S,
        PLUGIN_CFG_NAMESPACE,
        PLUGIN_CFG_NAME,
    )
    plugin_cfg_cm = wait_for_plugin_configmap(ctx, LOG, timeout_s=PLUGIN_CFG_TIMEOUT_S)
    if plugin_cfg_cm is None:
        LOG.error(
            "Plugin readiness ConfigMap %s/%s not found within %.1fs; aborting integration.",
            PLUGIN_CFG_NAMESPACE,
            PLUGIN_CFG_NAME,
            PLUGIN_CFG_TIMEOUT_S,
        )
        return False  # abort integration test

    # Save plugin config map + decoded inner config for debugging / inspection
    try:
        out_dir = Path(__file__).resolve().parent
        inner_raw = (plugin_cfg_cm.get("data") or {}).get(PLUGIN_CFG_DATA_KEY, "")
        cfg_path = out_dir / "latest_plugin_config.json"
        try:
            inner_obj = json.loads(inner_raw)
            cfg_path.write_text(
                json.dumps(inner_obj, indent=2),
                encoding="utf-8",
            )
            LOG.info("Saved decoded plugin config snapshot to %s", cfg_path)
        except Exception as e:
            # Fallback: write the raw string so we don't lose information
            cfg_path.write_text(inner_raw, encoding="utf-8")
            LOG.warning(
                "Failed to parse inner plugin config JSON; wrote raw string to %s: %s",
                cfg_path,
                e,
            )

    except Exception as e:
        LOG.warning("Failed to write plugin config debug files: %s", e)

    # --- Apply workload steps ---
    num_steps = len(scenario.steps)
    for idx, step in enumerate(scenario.steps):
        LOG.info("=== Applying workload step: %s ===", step.name)
        _apply_workload_step(LOG, ctx, TEST_NAMESPACE, scenario, step, node_cpu_m, node_mem_b)
        _wait_for_workload_step(LOG, ctx, TEST_NAMESPACE, scenario, step)

        # Between workload steps, assert that the solver has no active plan yet.
        # We don't do this after the last step, because that is exactly when we
        # expect a plan to be produced later in the test.
        if idx < num_steps - 1:
            when = f"after workload step {idx+1}/{num_steps} ({step.name})"
            if not assert_no_active_plan_http(LOG, when=when):
                LOG.error("Active-plan invariant violated %s; failing integration test.", when)
                return False

    # 1) Workload settle time
    LOG.info("Sleeping %.1fs for workload to settle before expecting a plan.", WORKLOAD_SETTLE_TIME_S)
    time.sleep(WORKLOAD_SETTLE_TIME_S)

    # Manual modes: trigger optimization via HTTP (same idea as test_runner)
    if opt_mode == "manual":
        LOG.info("Manual mode: triggering solver via HTTP: %s", SOLVER_TRIGGER_URL)
        code, body = solver_trigger_http(LOG, SOLVER_TRIGGER_URL, SOLVER_TRIGGER_TIMEOUT_S)
        body_compact = (body or "").replace("\n", "\\n")
        if len(body_compact) > 600:
            body_compact = body_compact[:600] + "...(truncated)"
        LOG.info("solver_response code=%s body=%s", code, body_compact)

    # 2) Wait for plan ConfigMap (with timeout)
    cm = get_latest_plan_configmap(ctx, LOG, timeout_s=CONFIG_MAP_TIMEOUT_S)
    if cm is None:
        LOG.warning("No plan ConfigMap found within %.1fs; treating as integration failure.", CONFIG_MAP_TIMEOUT_S)
        return False
    raw_plan = (cm.get("data") or {}).get(PLAN_DATA_KEY)
    if not raw_plan:
        LOG.error("StoredPlan ConfigMap data[%s] is empty or missing", PLAN_DATA_KEY)
        return False
    try:
        stored_plan = json.loads(raw_plan)
    except Exception as e:
        LOG.error("failed to parse StoredPlan JSON from ConfigMap: %s", e)
        return False

    expected_assignment = _build_expected_assignment(scenario, TEST_NAMESPACE)

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
    # Workload IDs we want to exercise under pytest
    WORKLOAD_IDS_FOR_TEST = ["sameprio", "prioaware", "higharrival"]

    @pytest.mark.parametrize(
        "workload_id,opt_mode,opt_hook,opt_sync",
        [
            (wid, opt_mode, opt_hook, opt_sync)
            for wid in WORKLOAD_IDS_FOR_TEST
            for (opt_mode, opt_hook, opt_sync) in MODE_CASES
        ],
    )
    def test_mpo_modes_end_to_end(workload_id: str, opt_mode: str, opt_hook: str, opt_sync: bool):
        scenario = WORKLOAD_SCENARIOS[workload_id]
        assert run_mode_integration(
            opt_mode,
            opt_hook,
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
    ap.add_argument("--optimize-hook-stage", default="postfilter",
                    help="OPTIMIZE_HOOK_STAGE value (preenqueue or postfilter; default: postfilter)")
    ap.add_argument("--optimize-sync", action="store_true",
                    help="Set OPTIMIZE_SYNC=true (default: false)")
    ap.add_argument("--workload-id", default=DEFAULT_WORKLOAD_ID,
                    choices=sorted(WORKLOAD_SCENARIOS.keys()),
                    help=f"Workload scenario id (default: {DEFAULT_WORKLOAD_ID})")
    return ap


def main() -> None:
    ap = build_argparser()
    args = ap.parse_args()

    scenario = WORKLOAD_SCENARIOS[args.workload_id]

    ok = run_mode_integration(
        opt_mode=args.optimize_mode,
        hook_stage=args.optimize_hook_stage,
        opt_sync=args.optimize_sync,
        scenario=scenario,
        cluster_name=args.cluster_name,
        kwok_runtime=args.kwok_runtime,
        kwokctl_config_file=args.kwokctl_config_file,
    )
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
