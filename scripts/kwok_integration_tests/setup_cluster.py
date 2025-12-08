# setup_cluster.py

import argparse

from scripts.helpers.general_helpers import (
    setup_logging,
    make_header_footer,
)
from scripts.helpers.kubectl_helpers import (
    ensure_namespace,
    ensure_priority_classes,
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
    VALID_HOOK_STAGES,
    WORKLOAD_SCENARIOS,
    DEFAULT_WORKLOAD_ID,
    DEFAULT_DISABLE_WAIT_AND_ACTIVE_CHECKS,
    load_kwokctl_config,
    build_kwokctl_config_for_mode,
    scenario_max_priority,
    scenario_total_replicas,
    apply_workload_step,
    wait_for_workload_step,
)

# Namespace is specific to this script (tests use a different one)
TEST_NAMESPACE = "test"

# ---------------------------------------------------------------------------
# Core setup function
# ---------------------------------------------------------------------------

def setup_cluster(
    *,
    opt_mode: str,
    hook_stage: str,
    opt_sync: bool,
    scenario,
    cluster_name: str = DEFAULT_CLUSTER_NAME,
    kwok_runtime: str = DEFAULT_KWOK_RUNTIME,
    kwokctl_config_file: str = DEFAULT_KWOKCTL_CONFIG,
    disable_wait_and_active_checks: bool = DEFAULT_DISABLE_WAIT_AND_ACTIVE_CHECKS,
) -> None:
    """
    Recreate a KWOK cluster with the given plugin mode and apply a small workload.

    If disable_wait_and_active_checks is True, no waiting on pods is performed.
    """
    if opt_mode not in VALID_OPT_MODES:
        raise ValueError(f"Invalid opt_mode={opt_mode!r}; expected one of {sorted(VALID_OPT_MODES)}")
    if hook_stage not in VALID_HOOK_STAGES:
        raise ValueError(f"Invalid hook_stage={hook_stage!r}; expected one of {sorted(VALID_HOOK_STAGES)}")

    LOG = setup_logging(
        name=f"setup-{scenario.id}-{opt_mode}-{hook_stage}-{opt_sync}",
        prefix=f"[setup scenario={scenario.id} mode={opt_mode} hook={hook_stage} sync={opt_sync}] ",
        level="INFO",
    )
    header, footer = make_header_footer(
        f"Setup KWOK cluster: scenario={scenario.id}, mode={opt_mode}, hook={hook_stage}, sync={opt_sync}"
    )
    LOG.info("\n%s\ncluster=%s\n%s", header, cluster_name, footer)
    LOG.info("Scenario description: %s", scenario.description)

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

    # --- Apply workload steps ---
    from scripts.helpers.general_helpers import qty_to_mcpu_int, qty_to_bytes_int

    node_cpu_m = qty_to_mcpu_int(NODE_CPU)
    node_mem_b = qty_to_bytes_int(NODE_MEM)

    for step in scenario.steps:
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
        wait_for_workload_step(
            LOG,
            ctx,
            TEST_NAMESPACE,
            scenario,
            step,
            hook_stage=hook_stage,
            disable_wait_and_active_checks=disable_wait_and_active_checks,
        )

    LOG.info(
        "Cluster %s is ready in context %s with workload '%s' applied.",
        cluster_name,
        ctx,
        scenario.id,
    )
    LOG.info(
        "Inspect with: kubectl --context %s -n %s get pods -o wide",
        ctx,
        TEST_NAMESPACE,
    )


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def build_argparser() -> argparse.ArgumentParser:
    ap = argparse.ArgumentParser(
        description="Create a KWOK cluster and apply a small MyPriorityOptimizer workload.",
    )
    ap.add_argument(
        "--cluster-name",
        default=DEFAULT_CLUSTER_NAME,
        help=f"KWOK cluster name (default: {DEFAULT_CLUSTER_NAME})",
    )
    ap.add_argument(
        "--kwok-runtime",
        default=DEFAULT_KWOK_RUNTIME,
        choices=["binary", "docker"],
        help=f"KWOK runtime (default: {DEFAULT_KWOK_RUNTIME})",
    )
    ap.add_argument(
        "--kwokctl-config-file",
        default=DEFAULT_KWOKCTL_CONFIG,
        help=f"KwokctlConfiguration YAML (default: {DEFAULT_KWOKCTL_CONFIG})",
    )
    ap.add_argument(
        "--optimize-mode",
        required=True,
        choices=sorted(VALID_OPT_MODES),
        help="OPTIMIZE_MODE value for the scheduler plugin",
    )
    ap.add_argument(
        "--optimize-hook-stage",
        default="postfilter",
        choices=sorted(VALID_HOOK_STAGES),
        help="OPTIMIZE_HOOK_STAGE value (default: postfilter)",
    )
    ap.add_argument(
        "--optimize-sync",
        action="store_true",
        help="Set OPTIMIZE_SOLVE_SYNCH=true (default: false)",
    )
    ap.add_argument(
        "--workload-id",
        default=DEFAULT_WORKLOAD_ID,
        choices=sorted(WORKLOAD_SCENARIOS.keys()),
        help=f"Workload scenario id (default: {DEFAULT_WORKLOAD_ID})",
    )
    ap.add_argument(
        "--disable-wait-and-active-checks",
        action="store_true",
        help="Disable waiting for pods (useful for quick manual setup).",
    )
    return ap


def main() -> None:
    ap = build_argparser()
    args = ap.parse_args()

    scenario = WORKLOAD_SCENARIOS[args.workload_id]

    setup_cluster(
        opt_mode=args.optimize_mode,
        hook_stage=args.optimize_hook_stage,
        opt_sync=args.optimize_sync,
        scenario=scenario,
        cluster_name=args.cluster_name,
        kwok_runtime=args.kwok_runtime,
        kwokctl_config_file=args.kwokctl_config_file,
        disable_wait_and_active_checks=args.disable_wait_and_active_checks,
    )


if __name__ == "__main__":
    main()
