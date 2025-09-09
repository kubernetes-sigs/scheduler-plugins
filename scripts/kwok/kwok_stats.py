#!/usr/bin/env python3

# kwok_stats.py

import argparse
from typing import Callable
from tabulate import tabulate

from kwok_shared import stat_snapshot

class KwokStats:
    def __init__(self, ctx: str, ns: str, expected: int, settle_timeout: float,
                 printer: Callable[[str], None] = print):
        self._printer = printer
        self.ctx = ctx
        self.ns = ns
        self.expected = expected
        self.settle_timeout = settle_timeout

    def _node_table(self, pods_run_by_node: dict[str, int]) -> str:
        """
        Show only pods running per node.
        """
        headers = ["NODE", "PODS RUN"]
        rows = []
        for node in sorted(pods_run_by_node.keys()):
            rows.append([node, pods_run_by_node.get(node, 0)])
        return tabulate(rows, headers=headers, tablefmt="fancy_grid", stralign="right")

    def _totals_table(self, cpu_run_util: float, mem_run_util: float,
                      cpu_total_util: float, mem_total_util: float,
                      total_running: int, total_not_running: int) -> str:
        """
        Show running and total utilizations, plus pod counts.
        """
        rows = [[
            f"{cpu_run_util*100:.1f}%/{cpu_total_util*100:.1f}%",
            f"{mem_run_util*100:.1f}%/{mem_total_util*100:.1f}%",
            f"{total_running}/{total_running+total_not_running}"
        ]]
        headers = [
            "CPU UTIL", "MEM UTIL", "PODS"
        ]
        return tabulate(rows, headers=headers, tablefmt="fancy_grid", stralign="right")

    def run(self) -> None:
        """
        Collect and print snapshot stats.
        """
        s = stat_snapshot(self.ctx, ns=self.ns, expected=self.expected, settle_timeout=self.settle_timeout)

        self._printer(f"[kwok-stats] context={self.ctx} namespace={self.ns} expected={self.expected} settle_timeout={self.settle_timeout}s")
        self._printer("")
        self._printer("Cluster utilization and pod totals")
        self._printer(self._totals_table(
            s.cpu_run_util, s.mem_run_util,     # running-only util
            s.cpu_total_util, s.mem_total_util, # total util (capped by alloc)
            len(s.pods_scheduled), len(s.pods_unscheduled)
        ))
        self._printer("Pods running per node")
        self._printer(self._node_table(s.pods_run_by_node))
        self._printer("")

def main():
    ap = argparse.ArgumentParser(description="Show KWOK stats: running/total utilization and pods per node.")
    ap.add_argument("cluster_name", help="Cluster short name (context will be kwok-<cluster_name>)")
    ap.add_argument("--namespace", "-n", default="crossnode-test", help="Namespace to inspect")
    ap.add_argument("--expected", type=int, default=0,
                    help="Expected pod count for waiting logic in stat_snapshot; 0 means don't wait")
    ap.add_argument("--settle-timeout", type=float, default=0.0,
                    help="Seconds to wait in stat_snapshot; 0 means immediate snapshot")

    args = ap.parse_args()
    ctx = f"kwok-{args.cluster_name}"
    KwokStats(ctx, args.namespace, args.expected, args.settle_timeout).run()

if __name__ == "__main__":
    main()
