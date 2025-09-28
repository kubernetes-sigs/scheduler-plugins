#!/usr/bin/env python3

# kwok_stats.py

import argparse
from typing import Callable
from tabulate import tabulate

from kwok_helpers import stat_snapshot, qty_to_mcpu_str

class KwokStats:
    def __init__(self, ctx: str, ns: str, expected: int, settle_timeout: float,
                 printer: Callable[[str], None] = print):
        self.printer = printer
        self.ctx = ctx
        self.ns = ns
        self.expected = expected
        self.settle_timeout = settle_timeout

    def _bytes_formatter(self, b: int) -> str:
        """
        Simple formatter: bytes → KiB/MiB/GiB/TiB with 1 decimal
        """
        units = ["B","KiB","MiB","GiB","TiB","PiB","EiB"]
        x = float(max(0, b))
        u = 0
        while x >= 1024.0 and u < len(units)-1:
            x /= 1024.0
            u += 1
        return f"{x:.1f}{units[u]}"

    def _node_table(
        self,
        pods_run_by_node: dict[str, int],
        cpu_alloc_by_node: dict[str, int],
        mem_alloc_by_node: dict[str, int],
        cpu_req_by_node: dict[str, int],
        mem_req_by_node: dict[str, int],
    ) -> str:
        """
        Show pods running per node + free CPU/MEM and running utilization per node.
        """
        headers = ["NODE", "CPU UTIL", "MEM UTIL", "CPU FREE", "MEM FREE", "PODS RUN"]
        rows = []
        for node in sorted(pods_run_by_node.keys()):
            alloc_cpu_m = int(cpu_alloc_by_node.get(node, 0))
            alloc_mem_b = int(mem_alloc_by_node.get(node, 0))
            req_cpu_m   = int(cpu_req_by_node.get(node, 0))
            req_mem_b   = int(mem_req_by_node.get(node, 0))

            free_cpu_m = max(0, alloc_cpu_m - req_cpu_m)
            free_mem_b = max(0, alloc_mem_b - req_mem_b)

            cpu_util = (req_cpu_m / alloc_cpu_m) if alloc_cpu_m > 0 else 0.0
            mem_util = (req_mem_b / alloc_mem_b) if alloc_mem_b > 0 else 0.0

            rows.append([
                node,
                f"{cpu_util*100:.1f}%",
                f"{mem_util*100:.1f}%",
                qty_to_mcpu_str(free_cpu_m),
                self._bytes_formatter(free_mem_b),
                pods_run_by_node.get(node, 0),
            ])
        return tabulate(rows, headers=headers, tablefmt="fancy_grid", stralign="right")

    def _totals_table(self, cpu_run_util: float, mem_run_util: float,
                      total_running: int, total_not_running: int,
                      cpu_free_m: int, mem_free_b: int) -> str:
        """
        Cluster running utilization, pod totals, and aggregate free resources.
        """
        rows = [[
            f"{cpu_run_util*100:.1f}%",
            f"{mem_run_util*100:.1f}%",
            qty_to_mcpu_str(cpu_free_m),
            self._bytes_formatter(mem_free_b),
            f"{total_running}/{total_running+total_not_running}",
        ]]
        headers = ["CPU UTIL", "MEM UTIL", "CPU FREE", "MEM FREE", "PODS RUN/TOTAL"]
        return tabulate(rows, headers=headers, tablefmt="fancy_grid", stralign="right")

    def run(self) -> None:
        s = stat_snapshot(self.ctx, ns=self.ns, expected=self.expected)

        # cluster totals for FREE = alloc − running
        tot_cpu_alloc_m = sum(s.cpu_alloc_by_node.values())
        tot_mem_alloc_b = sum(s.mem_alloc_by_node.values())
        tot_cpu_req_m   = sum(s.cpu_req_by_node.values())
        tot_mem_req_b   = sum(s.mem_req_by_node.values())
        cpu_free_m = max(0, tot_cpu_alloc_m - tot_cpu_req_m)
        mem_free_b = max(0, tot_mem_alloc_b - tot_mem_req_b)

        self.printer(f"[kwok-stats] context={self.ctx} namespace={self.ns} expected={self.expected}")
        self.printer("")
        self.printer("Pods running and per-node utilization/free resources")
        self.printer(self._node_table(
            s.pods_run_by_node,
            s.cpu_alloc_by_node,
            s.mem_alloc_by_node,
            s.cpu_req_by_node,
            s.mem_req_by_node,
        ))
        self.printer("")
        self.printer("Cluster utilization, pod totals, and free resources")
        self.printer(self._totals_table(
            s.cpu_run_util, s.mem_run_util,
            len(s.pods_running), len(s.pods_unscheduled),
            cpu_free_m, mem_free_b,
        ))

def main():
    ap = argparse.ArgumentParser(description="Show KWOK stats: running/total utilization and pods per node.")
    ap.add_argument("cluster-name", dest="cluster_name", help="Cluster short name (context will be kwok-<cluster_name>)", default="kwok1")
    ap.add_argument("--namespace", "-n", default="test", help="Namespace to inspect")
    ap.add_argument("--expected", type=int, default=0,
                    help="Expected pod count for waiting logic in stat_snapshot; 0 means don't wait")
    ap.add_argument("--settle-timeout", type=float, default=0,
                    help="Seconds to wait in stat_snapshot; 0 means immediate snapshot")

    args = ap.parse_args()
    ctx = f"kwok-{args.cluster_name}"
    KwokStats(ctx, args.namespace, args.expected, args.settle_timeout).run()

if __name__ == "__main__":
    main()
