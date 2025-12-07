#!/usr/bin/env python3

import argparse, csv
import json
import logging
import sys
import time
from pathlib import Path
from typing import Dict, Any, Tuple, Optional

import yaml

# pytest is optional: used only when running via pytest
try:
    import pytest  # type: ignore
except ImportError:  # pragma: no cover
    pytest = None  # type: ignore

from scripts.helpers.general_helpers import (
    setup_logging,
    make_header_footer,
)
from scripts.helpers.kubectl_helpers import (
    kubectl_apply_yaml,
    ensure_namespace,
    ensure_priority_classes,
    wait_pod,
    get_json_ctx,
)
from scripts.helpers.kwok_helpers import (
    ensure_kwok_cluster,
    create_kwok_nodes,
    kwok_pods_cap,
    merge_kwokctl_envs,
    yaml_kwok_pod,
)

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

POD_TIMEOUT_S = 60

# Timing model:
# 1) After creating workload & pods exist -> wait WORKLOAD_SETTLE_TIME_S
# 2) Then wait up to CONFIG_MAP_TIMEOUT_S for a plan ConfigMap to appear
# 3) Once plan is present -> wait PLAN_EXECUTION_TIME_S for evictions/new pods
WORKLOAD_SETTLE_TIME_S = 2
CONFIG_MAP_TIMEOUT_S = 30
PLAN_EXECUTION_TIME_S = 10

# Node names KWOK normally uses for this cluster size.
# Used only for documentation / expectations; we don't *assert* on them globally.
EXPECTED_NODE_NAMES = [f"kwok-node-{i+1}" for i in range(NUM_NODES)]

# Test scenario:
# - Two large "background" pods (each ~fills a node)
# - Four smaller pods that can all fit if we evict/repack.
#
# expected_assignment:
#   - mpo-big-1: expected to be evicted (False)
#   - others: expected to be assigned after the plan (True)
TEST_PODS = [
    {
        "name": "mpo-big-1",
        "cpu": "700m",
        "mem": "700Mi",
        "pc": "p1",
        "expected_assignment": False,
    },
    {
        "name": "mpo-big-2",
        "cpu": "700m",
        "mem": "700Mi",
        "pc": "p2",
        "expected_assignment": True,
    },
    {
        "name": "mpo-small-1",
        "cpu": "400m",
        "mem": "400Mi",
        "pc": "p3",
        "expected_assignment": True,
    },
    {
        "name": "mpo-small-2",
        "cpu": "400m",
        "mem": "400Mi",
        "pc": "p3",
        "expected_assignment": True,
    },
    {
        "name": "mpo-small-3",
        "cpu": "100m",
        "mem": "100Mi",
        "pc": "p3",
        "expected_assignment": True,
    },
    {
        "name": "mpo-small-4",
        "cpu": "100m",
        "mem": "100Mi",
        "pc": "p3",
        "expected_assignment": True,
    },
]

TEST_POD_NAMES = [p["name"] for p in TEST_PODS]

# Expected final running/non-running state after the plan is applied.
EXPECTED_ASSIGNMENT: Dict[Tuple[str, str], bool] = {
    (TEST_NAMESPACE, p["name"]): p["expected_assignment"] for p in TEST_PODS
}

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
        {"name": "OPTIMIZE_SYNCH", "value": "true" if opt_sync else "false"},
    ]
    return merge_kwokctl_envs(base_doc, envs, component="kube-scheduler")


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


# ---------------------------------------------------------------------------
# Core integration function
# ---------------------------------------------------------------------------

def run_mode_integration(
    opt_mode: str,
    hook_stage: str,
    opt_sync: bool,
    *,
    cluster_name: str = DEFAULT_CLUSTER_NAME,
    kwok_runtime: str = DEFAULT_KWOK_RUNTIME,
    kwokctl_config_file: str | Path = DEFAULT_KWOKCTL_CONFIG,
) -> bool:
    """
    End-to-end integration test for a given (opt_mode, hook_stage):

    1. Recreate KWOK cluster with scheduler env set to opt_mode/hook_stage.
    2. Create KWOK nodes.
    3. Ensure test namespace + PriorityClasses.
    4. Apply a small workload meant to trigger optimizer.
    5. Wait for pods to exist.
    6. Sleep WORKLOAD_SETTLE_TIME_S to let default scheduler / queues settle.
    7. Wait (up to CONFIG_MAP_TIMEOUT_S) for a StoredPlan ConfigMap to appear.
    8. Sleep PLAN_EXECUTION_TIME_S to allow evictions / re-scheduling.
    9. Fetch actual cluster state and compare vs plan + EXPECTED_ASSIGNMENT.
    """
    LOG = setup_logging(
        name=f"mpo-itest-{opt_mode}-{hook_stage}-{opt_sync}",
        prefix=f"[mpo-itest mode={opt_mode} hook={hook_stage} sync={opt_sync}] ",
        level="INFO",
    )
    header, footer = make_header_footer(
        f"MPOptimizer KWOK integration: mode={opt_mode}, hook={hook_stage}, sync={opt_sync}"
    )
    LOG.info("\n%s\ncluster=%s\n%s", header, cluster_name, footer)
    LOG.info("Test pods (expected_assignment): %s", EXPECTED_ASSIGNMENT)

    ctx = f"kwok-{cluster_name}"

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
    total_pods = len(TEST_PODS)
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
    ensure_priority_classes(LOG, ctx, NUM_PRIORITIES, prefix="p", start=1)

    # --- Workload ---
    yaml_text = "".join(
        [
            yaml_kwok_pod(
                TEST_NAMESPACE,
                p["name"],
                p["cpu"],
                p["mem"],
                p["pc"],
            )
            for p in TEST_PODS
        ]
    )
    kubectl_apply_yaml(LOG, ctx, yaml_text)

    # Wait for pods to exist in the API
    for name in TEST_POD_NAMES:
        _ = wait_pod(
            LOG,
            ctx,
            name,
            TEST_NAMESPACE,
            POD_TIMEOUT_S,
            mode="exist",
        )

    # 1) Workload settle time
    LOG.info("Sleeping %.1fs for workload to settle before expecting a plan.", WORKLOAD_SETTLE_TIME_S)
    time.sleep(WORKLOAD_SETTLE_TIME_S)

    # 2) Wait for plan ConfigMap (with timeout)
    cm = get_latest_plan_configmap(ctx, LOG, timeout_s=CONFIG_MAP_TIMEOUT_S)
    if cm is None:
        LOG.warning("No plan ConfigMap found within %.1fs; treating as integration failure.", CONFIG_MAP_TIMEOUT_S)
        return False

    raw_plan = (cm.get("data") or {}).get(PLAN_DATA_KEY)
    if not raw_plan:
        LOG.error("plan ConfigMap %s missing data[%r]", cm.get("metadata", {}).get("name"), PLAN_DATA_KEY)
        return False

    try:
        stored_plan = json.loads(raw_plan)
    except Exception as e:
        LOG.error("failed to parse StoredPlan JSON from ConfigMap: %s", e)
        return False

    plan_map = plan_placements_by_pod(stored_plan)

    # 3) Wait for the plan to actually be executed (evictions + new pods)
    LOG.info("Sleeping %.1fs to allow plan execution (evictions / new placements).", PLAN_EXECUTION_TIME_S)
    time.sleep(PLAN_EXECUTION_TIME_S)

    # 4) Actual node assignments (AFTER plan execution window)
    pods_json = get_json_ctx(
        ctx,
        ["-n", TEST_NAMESPACE, "get", "pods", "-o", "json"],
    )
    actual_nodes: Dict[Tuple[str, str], str] = {}
    for item in pods_json.get("items", []):
        name = (item.get("metadata") or {}).get("name", "")
        ns = (item.get("metadata") or {}).get("namespace", TEST_NAMESPACE)
        node = (item.get("spec") or {}).get("nodeName", "")
        if name:
            actual_nodes[(str(ns), str(name))] = str(node or "")

    # Sanity: make sure we at least saw our pods in the API
    for name in TEST_POD_NAMES:
        key = (TEST_NAMESPACE, name)
        if key not in actual_nodes:
            LOG.error("pod %s/%s not found via kubectl", TEST_NAMESPACE, name)
            return False

    # ------------------------------------------------------------------
    # Write debug artifacts:
    #   - latest_plan_raw.json: raw StoredPlan JSON from ConfigMap
    #   - latest_plan_vs_actual.csv: planned vs actual node placements
    # ------------------------------------------------------------------
    try:
        out_dir = Path(__file__).resolve().parent
        raw_plan_path = out_dir / "latest_plan_raw.json"
        csv_path = out_dir / "latest_plan_vs_actual.csv"

        # Raw plan: exactly what was in the ConfigMap
        raw_plan_path.write_text(raw_plan, encoding="utf-8")

        # CSV: merged view of plan placements, actual state, and expected_assignment
        all_keys = sorted(set(plan_map.keys()) | set(actual_nodes.keys()))
        with csv_path.open("w", newline="", encoding="utf-8") as f:
            writer = csv.writer(f)
            writer.writerow(
                [
                    "pod",
                    "planned_node",
                    "actual_node",
                    "expected_assignment",
                ]
            )
            for ns, name in all_keys:
                planned_node = plan_map.get((ns, name), "")
                actual_node = actual_nodes.get((ns, name), "")
                should_run = EXPECTED_ASSIGNMENT.get((ns, name))
                expected_assignment = (
                    "" if should_run is None else ("true" if should_run else "false")
                )
                writer.writerow(
                    [
                        name,
                        planned_node,
                        actual_node,
                        expected_assignment,
                    ]
                )

        LOG.info(
            "wrote plan debug files: %s, %s",
            raw_plan_path,
            csv_path,
        )
    except Exception as e:
        LOG.warning("failed to write plan debug files: %s", e)

    # Check 1: plan vs actual placements
    mismatches = []
    for key, planned_node in plan_map.items():
        actual_node = actual_nodes.get(key, "")
        if planned_node and actual_node and planned_node != actual_node:
            mismatches.append((key, planned_node, actual_node))

    if mismatches:
        LOG.error("plan vs actual node mismatches: %s", mismatches)
        return False

    # Check 2: expected assignment vs actual assignment (derived from nodeName)
    assignment_mismatches = []
    for key, should_run in EXPECTED_ASSIGNMENT.items():
        actual_node = actual_nodes.get(key, "")
        is_assigned = bool(actual_node)
        if is_assigned != should_run:
            assignment_mismatches.append((key, should_run, is_assigned, actual_node))

    if assignment_mismatches:
        LOG.error("expected assignment mismatches: %s", assignment_mismatches)
        return False

    LOG.info(
        "Mode %s / hook %s: plan placements are consistent with actual pod "
        "assignments and expected assignment.",
        opt_mode,
        hook_stage,
    )
    return True


# ---------------------------------------------------------------------------
# Pytest entrypoint
# ---------------------------------------------------------------------------

if pytest is not None:
    @pytest.mark.kwok
    @pytest.mark.integration
    @pytest.mark.parametrize(
        "opt_mode,opt_hook,opt_sync",
        [
            # ("per_pod", "preenqueue", False),
            # ("per_pod", "postfilter", False),
            # ("per_pod", "postfilter", True),
            ("periodic", "preenqueue", False),
            ("periodic", "preenqueue", True),
            ("periodic", "postfilter", False),
            ("periodic", "postfilter", True),
            ("interlude", "preenqueue", False),
            ("interlude", "preenqueue", True),
            ("interlude", "postfilter", False),
            ("interlude", "postfilter", True),
            ("manual", "preenqueue", False),
            ("manual", "postfilter", False),
            ("manual", "postfilter", True),
            # add more modes if you like:
        ],
    )
    def test_mpo_modes_end_to_end(opt_mode: str, opt_hook: str, opt_sync: bool):
        assert run_mode_integration(
            opt_mode,
            opt_hook,
            opt_sync,
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
                    choices=["per_pod", "periodic", "interlude", "manual"],
                    help="OPTIMIZE_MODE value")
    ap.add_argument("--optimize-hook-stage", default="postfilter",
                    help="OPTIMIZE_HOOK_STAGE value (preenqueue or postfilter; default: postfilter)")
    ap.add_argument("--optimize-sync", action="store_true",
                    help="Set OPTIMIZE_SYNC=true (default: false)")
    return ap


def main() -> None:
    ap = build_argparser()
    args = ap.parse_args()

    ok = run_mode_integration(
        opt_mode=args.optimize_mode,
        hook_stage=args.optimize_hook_stage,
        opt_sync=args.optimize_sync,
        cluster_name=args.cluster_name,
        kwok_runtime=args.kwok_runtime,
        kwokctl_config_file=args.kwokctl_config_file,
    )
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
