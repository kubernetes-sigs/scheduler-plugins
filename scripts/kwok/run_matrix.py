#!/usr/bin/env python3
"""
Run a KWOK matrix and append results to one CSV per full combo:
  runs_n<NN>_ppn<PPN>_util<UTIL>.csv  (e.g. runs_n4_ppn8_util0.90.csv)

Defaults match the Bash version but are configurable via CLI flags or env vars.
Assumes this file sits alongside kwok_test_generator.py and kwok_shared.py.
"""

import os
import sys
import csv
import time
import argparse
import subprocess
from pathlib import Path
from kwok_shared import stat_snapshot, parse_timeout_s, ensure_cluster, env_default, env_bool, parse_int_list, parse_float_list, ensure_dir

def ensure_header(path: Path) -> None:
    if not path.exists():
        with path.open("w", newline="") as f:
            w = csv.writer(f)
            w.writerow([
                "timestamp","cluster","num_nodes","pods_per_node","util","repeat",
                "scheduled","unscheduled","cpu_run_util","mem_run_util"
            ])

def build_parser() -> argparse.ArgumentParser:
    here = Path(__file__).resolve().parent

    p = argparse.ArgumentParser(description="Run KWOK matrix and write per-(n,ppn,util) CSVs")
    p.add_argument("--kwok-config", default=env_default("KWOK_CONFIG", ""))
    p.add_argument("--cluster-name", default=env_default("CLUSTER_NAME", "kwok1"))
    p.add_argument("--namespace", default=env_default("NS", "crossnode-test"))
    p.add_argument("--repeats", type=int, default=int(env_default("REPEATS", "5")))

    p.add_argument("--node-cpu", default=env_default("NODE_CPU", "24"))
    p.add_argument("--node-mem", default=env_default("NODE_MEM", "32Gi"))
    p.add_argument("--max-pods-per-node", type=int, default=int(env_default("MAX_PODS_PER_NODE", "64")))
    p.add_argument("--util-tol", type=float, default=float(env_default("UTIL_TOL", "0.00")))
    p.add_argument("--wait-mode", choices=["exist","ready","running"], default=env_default("WAIT_MODE", "running"))
    p.add_argument("--wait-each", action="store_true", default=env_bool("WAIT_EACH", False))
    p.add_argument("--num-replicaset", type=int, default=int(env_default("NUM_REPLICASET", "0")))
    p.add_argument("--num-prios", type=int, default=int(env_default("NUM_PRIOS", "4")))
    p.add_argument("--cpu-interval", default=env_default("CPU_INT", "50m,500m"))
    p.add_argument("--mem-interval", default=env_default("MEM_INT", "64Mi,1024Mi"))
    p.add_argument("--dist-mode", choices=["random","even"], default=env_default("DIST_MODE", "random"))
    p.add_argument("--variance", type=int, default=int(env_default("VARIANCE", "50")))

    p.add_argument("--pods-per-node-list", default=os.environ.get("PODS_PER_NODE_LIST", "4,8"),
                   help="e.g. '4,8'")
    p.add_argument("--num-nodes-list", default=os.environ.get("NUM_NODES_LIST", "4,8,16,32"),
                   help="e.g. '4,8,16,32'")
    p.add_argument("--util-list", default=os.environ.get("UTIL_LIST", "0.90,0.95,1.00,1.05"),
                   help="e.g. '0.90,0.95,1.00,1.05'")

    p.add_argument("--out-dir", default=env_default("OUT_DIR", "./scripts/kwok/out"))
    p.add_argument("--generator", default=str(here / "kwok_test_generator.py"),
                   help="Path to kwok_test_generator.py")
    p.add_argument("--kwok-shared", default=str(here / "kwok_shared.py"),
                   help="Path to kwok_shared.py")

    p.add_argument("--wait-timeout", default="5s")
    p.add_argument("--settle-timeout", default="3s")
    return p

def main():
    args = build_parser().parse_args()

    out_dir = Path(args.out_dir).resolve()
    ensure_dir(out_dir)

    kwok_config = Path(args.kwok_config) if args.kwok_config != "" else None
    gen_path = Path(args.generator).resolve()

    cluster = args.cluster_name
    ctx = f"kwok-{cluster}"

    ensure_cluster(cluster, kwok_config, recreate=True)

    pods_per_node_list = parse_int_list(args.pods_per_node_list)
    num_nodes_list = parse_int_list(args.num_nodes_list)
    util_list = parse_float_list(args.util_list)

    if not pods_per_node_list or not num_nodes_list or not util_list:
        print("[error] One or more matrix lists are empty. Check --pods-per-node-list, --num-nodes-list, --util-list.")
        sys.exit(2)

    for util in util_list:
        for nn in num_nodes_list:
            for ppn in pods_per_node_list:
                test_case = f"n{nn}_ppn{ppn}_util{util:.2f}"
                file_name = out_dir / f"{test_case}.csv"
                ensure_header(file_name)

                for rep in range(1, args.repeats + 1):
                    test_case_rep = f"{test_case}_r{rep}/{args.repeats}"
                    print(f"\n===== test: {test_case_rep} =====")

                    gen_args = [
                        sys.executable, str(gen_path),
                        cluster, str(nn), str(ppn), str(args.num_replicaset),
                        "--namespace", args.namespace,
                        "--max-pods-per-node", str(args.max_pods_per_node),
                        "--util", f"{util}",
                        "--util-tolerance", f"{args.util_tol}",
                        "--node-cpu", args.node_cpu,
                        "--node-mem", args.node_mem,
                        "--dist-mode", args.dist_mode,
                        "--variance", str(args.variance),
                        "--num-priorities", str(args.num_prios),
                        "--cpu-interval", args.cpu_interval,
                        "--mem-interval", args.mem_interval,
                        "--wait-timeout", args.wait_timeout,
                        "--settle-timeout", args.settle_timeout,
                        "--wait-mode", args.wait_mode,
                    ]
                    if args.wait_each:
                        gen_args.append("--wait-each")
                    
                    subprocess.run(gen_args, check=True)
                    
                    s = stat_snapshot(ctx, args.namespace, expected=nn*ppn, settle_timeout=parse_timeout_s(args.settle_timeout))
                    ts = int(time.time())
                    with file_name.open("a", newline="") as f:
                        w = csv.writer(f)
                        w.writerow([
                            ts, cluster, nn, ppn, f"{util:.2f}", rep,
                            len(s.pods_scheduled), len(s.pods_unscheduled), f"{s.cpu_run_util:.6f}", f"{s.mem_run_util:.6f}"
                        ])

                    print(f"[run][n={nn} ppn={ppn} util={util:.2f}] "
                          f"scheduled={len(s.pods_scheduled)}/{len(s.pods_scheduled)+len(s.pods_unscheduled)} "
                          f"cpu_util={s.cpu_run_util*100:.2f}%/{s.cpu_total_util*100:.2f}% mem_util={s.mem_run_util*100:.2f}%/{s.mem_total_util*100:.2f}%")

                print(f"[done] {test_case} all {args.repeats} repeats done")

    print(f"[done] all tests done")

if __name__ == "__main__":
    main()
