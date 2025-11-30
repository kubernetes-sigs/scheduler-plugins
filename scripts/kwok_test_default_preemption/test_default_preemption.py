#!/usr/bin/env python3
# test_default_preemption.py

"""
Simple KWOK case to see the weakness of DefaultPreemption in the Default Scheduler:

We assume a single KWOK node with:
    CPU: 100m
    RAM: 100M   (≈ 100 MB)

Scenario:
- First, create 1 pod:
    PriorityClass: p2 (value=2)
    CPU: 90m
    RAM: 90M
- Then, create 4 more pods:
    PriorityClass: p2 (value=2)
    CPU: 20m
    RAM: 20M each

The test shows how the Default Scheduler + DefaultPreemption behaves under pressure.
"""

import argparse, logging, time, yaml
from pathlib import Path
from scripts.helpers.kwok_helpers import (
    yaml_kwok_pod,
    ensure_kwok_cluster,
    create_kwok_nodes,
)
from scripts.helpers.kubectl_helpers import (
    run_kubectl_logged,
    kubectl_apply_yaml,
    ensure_namespace,
    ensure_priority_classes,
    wait_pod,
)
from scripts.helpers.general_helpers import (
    setup_logging,
    make_header_footer,
)

LOG = logging.getLogger("default-preemption-test")


def create_pods(ctx: str, namespace: str) -> None:
    """
    Create one 90m/90M pod and four 20m/20M pods, all with PriorityClass p2.
    """
    header, footer = make_header_footer("PODS")
    LOG.info("\n%s\nCreating pods\n%s", header, footer)

    # 1) First pod: high-priority p2, 90m/90M
    pod_yaml = yaml_kwok_pod(namespace, "p1", "90m", "90M", "p2")
    kubectl_apply_yaml(LOG, ctx, pod_yaml)
    # Wait until it is Running
    wait_pod(LOG, ctx, "p1", namespace, timeout_sec=60, mode="running")

    # 2) Remaining pods (p2..p5), high-priority p2, 20m/20M
    for i in range(1, 5):
        name = f"p{i+1}"
        pod_yaml = yaml_kwok_pod(namespace, name, "20m", "20M", "p2")
        kubectl_apply_yaml(LOG, ctx, pod_yaml)

    LOG.info("Waiting a few seconds for the Default Scheduler to react...")
    time.sleep(5)

    LOG.info("Final pod state (kubectl get pods -o wide):")
    run_kubectl_logged(LOG, ctx, "-n", namespace, "get", "pods", "-o", "wide", check=False)


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description="Simple KWOK Default Scheduler test")
    ap.add_argument("--cluster-name", default="kwok1", help="KWOK cluster name (default: kwok1)")
    ap.add_argument(
        "--kwok-runtime",
        default="binary",
        choices=["binary", "docker"],
        help="KWOK runtime for kwokctl create cluster (default: binary)",
    )
    ap.add_argument(
        "--kwokctl-config-file",
        default="default.yaml",
        help=(
            "KwokctlConfiguration YAML used to create the KWOK cluster "
            "(API server, scheduler, etc.). Nodes can either be defined "
            "here or created by this script."
        ),
    )
    ap.add_argument("--namespace", default="prio-test", help="Namespace to use (default: prio-test)")
    ap.add_argument(
        "--log-level",
        default="INFO",
        help="Logging level (DEBUG, INFO, WARNING, ERROR) (default: INFO)",
    )
    args = ap.parse_args(argv)

    ctx = f"kwok-{args.cluster_name}"

    setup_logging(
        name="default-preemption-test",
        prefix="[default-preemption-test] ",
        level=args.log_level,
    )

    header, footer = make_header_footer("DEFAULT PREEMPTION TEST")
    LOG.info(
        "\n%s\ncluster_name=%s  context=%s  namespace=%s\n%s",
        header,
        args.cluster_name,
        ctx,
        args.namespace,
        footer,
    )

    # ------------------------------------------------------------------
    # 1) Recreate KWOK cluster using the shared helper + config file
    # ------------------------------------------------------------------
    cfg_path = Path(args.kwokctl_config_file).resolve()
    with open(cfg_path, "r", encoding="utf-8") as f:
        config_doc = yaml.safe_load(f) or {}

    ensure_kwok_cluster(
        logger=LOG,
        cluster_name=args.cluster_name,
        kwok_runtime=args.kwok_runtime,
        config_doc=config_doc,
        recreate=True,
    )

    # ------------------------------------------------------------------
    # 2) Ensure a single KWOK node with 100m CPU / 100M MEM
    # ------------------------------------------------------------------
    # Create exactly one node kwok-node-1
    create_kwok_nodes(
        logger=LOG,
        ctx=ctx,
        num_nodes=1,
        node_cpu="100m",
        node_mem="100M",
        pods_cap=50,
    )

    # ------------------------------------------------------------------
    # 3) Namespace + PriorityClasses + pods
    # ------------------------------------------------------------------
    ensure_namespace(LOG, ctx, args.namespace)

    # This will create p1, p2 with values 1..2; we only use p2 here.
    ensure_priority_classes(LOG, ctx, num_priorities=2, prefix="p", start=1)

    create_pods(ctx, args.namespace)

    LOG.info("Setup complete. KWOK and all resources are left running for inspection.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
