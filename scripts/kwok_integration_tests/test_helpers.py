# test_helpers.py
import logging
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, Any, List, Optional

import yaml

from scripts.helpers.general_helpers import (
    qty_to_mcpu_str,
    qty_to_bytes_str,
)
from scripts.helpers.kubectl_helpers import (
    kubectl_apply_yaml,
    wait_rs_pods,
)
from scripts.helpers.kwok_helpers import (
    merge_kwokctl_envs,
    yaml_kwok_rs,
)

# ---------------------------------------------------------------------------
# Shared constants
# ---------------------------------------------------------------------------

DEFAULT_CLUSTER_NAME = "kwok1"
DEFAULT_KWOK_RUNTIME = "binary"
DEFAULT_KWOKCTL_CONFIG = "data/configs-kwokctl/test/test.yaml"

NUM_NODES = 2
NODE_CPU = "1000m"
NODE_MEM = "1Gi"
NUM_PRIORITIES = 3

# In practice 10s is still fast but a bit more forgiving.
POD_TIMEOUT_S = 10

# Valid optimization modes (just for CLI/pytest validation)
VALID_OPT_MODES = {"per_pod", "periodic", "interlude", "manual", "manual_blocking"}

# Global default: by default we DO NOT disable waits & active checks.
DEFAULT_DISABLE_WAIT_AND_ACTIVE_CHECKS = False

# ---------------------------------------------------------------------------
# Data model
# ---------------------------------------------------------------------------

@dataclass
class Workload:
    id: int
    cpu: float
    mem: float
    priority: int
    replicas: int = 1
    # Only used by tests; setup_cluster ignores this.
    expected_assignment: Optional[bool] = None


@dataclass
class WorkloadStep:
    name: str
    pods: List[Workload]
    wait_mode: str = "exist"  # "exist", "running", or "none"
    wait_timeout_s: int = POD_TIMEOUT_S
    # Only used by tests; setup_cluster ignores this.
    active_plan_check_mode: str = "none" # "each_pod", "after_step", "none"


@dataclass
class WorkloadScenario:
    id: str
    description: str
    steps: List[WorkloadStep]


# ---------------------------------------------------------------------------
# Scenario helpers + definitions
# ---------------------------------------------------------------------------

def rs_name_for_pod(scenario: WorkloadScenario, pod: Workload) -> str:
    """Stable ReplicaSet name derived from (scenario.id, pod.id)."""
    return f"{scenario.id}-pod-{pod.id:03d}"


def _scenario_all_scheduled_by_default() -> WorkloadScenario:
    step = WorkloadStep(
        name="all-scheduled",
        pods=[
            Workload(id=1, cpu=0.4, mem=0.1, priority=3, expected_assignment=True),
            Workload(id=2, cpu=0.3, mem=0.1, priority=3, expected_assignment=True),
            Workload(id=3, cpu=0.2, mem=0.1, priority=3, expected_assignment=True),
            Workload(id=4, cpu=0.1, mem=0.1, priority=3, expected_assignment=True),
        ],
        wait_mode="running",
        wait_timeout_s=POD_TIMEOUT_S,
        active_plan_check_mode="after_step",
    )
    return WorkloadScenario(
        id="allscheduled",
        description=(
            "All pods fit without eviction: four p2 pods that fit in the cluster; "
            "all should be scheduled regardless of mode."
        ),
        steps=[step],
    )


def _scenario_same_priority() -> WorkloadScenario:
    big_step = WorkloadStep(
        name="big-first",
        pods=[
            Workload(id=101, cpu=0.8, mem=0.1, priority=3, expected_assignment=False),
            Workload(id=102, cpu=0.7, mem=0.1, priority=3, expected_assignment=True),
        ],
        wait_mode="running",
        wait_timeout_s=POD_TIMEOUT_S,
        active_plan_check_mode="none",
    )
    small_step = WorkloadStep(
        name="small-later",
        pods=[
            Workload(id=201, cpu=0.4, mem=0.1, priority=3, expected_assignment=True),
            Workload(id=202, cpu=0.3, mem=0.1, priority=3, expected_assignment=True),
            Workload(id=203, cpu=0.3, mem=0.1, priority=3, expected_assignment=True),
            Workload(id=204, cpu=0.3, mem=0.1, priority=3, expected_assignment=True),
        ],
        wait_mode="exist",
        wait_timeout_s=POD_TIMEOUT_S,
        active_plan_check_mode="none",
    )
    return WorkloadScenario(
        id="sameprio",
        description=(
            "Equal-priority repacking: two big p3 pods first, four smaller p3 "
            "pods later; good for sanity-checking the optimizer and default preemption."
        ),
        steps=[big_step, small_step],
    )


def _scenario_different_priority() -> WorkloadScenario:
    """
    Mixed priorities:

    - Low-priority p1 pods (0.5 CPU each) fill the cluster.
    - Later, high-priority p3 pods (0.7 CPU each) arrive.
    """
    low_step = WorkloadStep(
        name="low-background",
        pods=[
            Workload(id=101, cpu=0.5, mem=0.1, priority=1, expected_assignment=None),
            Workload(id=102, cpu=0.5, mem=0.1, priority=1, expected_assignment=None),
            Workload(id=103, cpu=0.5, mem=0.1, priority=1, expected_assignment=None),
            Workload(id=104, cpu=0.5, mem=0.1, priority=1, expected_assignment=None),
        ],
        wait_mode="running",
        wait_timeout_s=POD_TIMEOUT_S,
        active_plan_check_mode="none",
    )
    high_step = WorkloadStep(
        name="high-arrival",
        pods=[
            Workload(id=201, cpu=0.7, mem=0.1, priority=3, expected_assignment=True),
            Workload(id=202, cpu=0.7, mem=0.1, priority=3, expected_assignment=True),
        ],
        wait_mode="exist",
        wait_timeout_s=POD_TIMEOUT_S,
        active_plan_check_mode="none",
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
            name=f"step-{100+i}",
            pods=[
                Workload(
                    id=100+i,
                    cpu=0.3,
                    mem=0.1,
                    priority=3,
                    expected_assignment=None,
                )
            ],
            wait_mode="exist",
            wait_timeout_s=POD_TIMEOUT_S,
            active_plan_check_mode="each_pod",
        )
        for i in range(1, 6)
    ]
    steps2 = [
        WorkloadStep(
            name=f"step-{200+i}",
            pods=[
                Workload(
                    id=200+i,
                    cpu=0.2,
                    mem=0.1,
                    priority=3,
                    expected_assignment=None,
                )
            ],
            wait_mode="exist",
            wait_timeout_s=POD_TIMEOUT_S,
            active_plan_check_mode="each_pod",
        )
        for i in range(1, 6)
    ]
    return WorkloadScenario(
        id="higharrival",
        description=(
            "High-arrival scenario: many small p1 pods in quick succession, "
            "useful for testing 'interlude' behavior."
        ),
        steps=steps1 + steps2,
    )


WORKLOAD_SCENARIOS: Dict[str, WorkloadScenario] = {
    "allscheduled": _scenario_all_scheduled_by_default(),
    "sameprio": _scenario_same_priority(),
    "prioaware": _scenario_different_priority(),
    "higharrival": _scenario_high_arrival(),
}

DEFAULT_WORKLOAD_ID = "sameprio"


def scenario_max_priority(scenario: WorkloadScenario) -> int:
    m = 0
    for step in scenario.steps:
        for pod in step.pods:
            m = max(m, pod.priority)
    return m


def scenario_total_replicas(scenario: WorkloadScenario) -> int:
    total = 0
    for step in scenario.steps:
        for pod in step.pods:
            total += int(pod.replicas)
    return total


# ---------------------------------------------------------------------------
# KWOK / kube-scheduler helpers
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
    opt_sync: bool,
) -> Dict[str, Any]:
    """
    Return a copy of base_doc that injects OPTIMIZE_MODE
    envs into the kube-scheduler component.
    """
    envs = [
        {"name": "OPTIMIZE_MODE", "value": opt_mode},
        {"name": "OPTIMIZE_SOLVE_SYNCH", "value": "true" if opt_sync else "false"},
    ]
    return merge_kwokctl_envs(base_doc, envs, component="kube-scheduler")


def apply_workload_step(
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
        rs_name = rs_name_for_pod(scenario, pod)
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


def wait_for_workload_step_simple(
    logger: logging.Logger,
    ctx: str,
    namespace: str,
    scenario: WorkloadScenario,
    step: WorkloadStep,
    *,
    disable_wait_and_active_checks: bool = DEFAULT_DISABLE_WAIT_AND_ACTIVE_CHECKS,
) -> None:
    """
    Simple waiting logic used by setup_cluster:
    wait for ReplicaSets of a workload step according to step.wait_mode.

    If disable_wait_and_active_checks is True, this returns immediately.
    """
    if disable_wait_and_active_checks:
        logger.info(
            "WAIT DISABLED: skipping pod waits for step %s in namespace %s",
            step.name,
            namespace,
        )
        return

    if step.wait_mode not in {"running", "exist", "none"}:
        logger.warning(
            "Unknown wait_mode=%r for step %s; skipping waits.",
            step.wait_mode,
            step.name,
        )
        return
    if step.wait_mode == "none":
        logger.info(
            "Workload step %s: wait_mode=none; not waiting for pods.",
            step.name,
        )
        return

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
