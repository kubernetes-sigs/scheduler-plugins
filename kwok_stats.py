#!/usr/bin/env python3

# kwok_stats.py

import argparse
from typing import Callable
from tabulate import tabulate

from kwok_shared import (bytes_to_mib, stat_snapshot, compute_stat_totals)

class KwokStats:
    def __init__(self,
                 ctx: str,
                 printer: Callable[[str], None] = print):
        self._printer = printer
        self.ctx = ctx

    def _node_table(self, alloc, cpu_req_by_node, mem_req_by_node, pods_run_by_node) -> str:
        pods_run_by_node = pods_run_by_node or {n: 0 for n in alloc}

        headers = [
            "NODE",
            "CPU\nALLOC(m)", "CPU\nREQ(m)", "CPU\nUTIL(%)", "CPU\nFREE(m)",
            "MEM\nALLOC(Mi)", "MEM\nREQ(Mi)", "MEM\nUTIL(%)", "MEM\nFREE(Mi)",
            "PODS\nRUN",
        ]
        rows = []
        for node in sorted(alloc.keys()):
            cpu_alloc, mem_alloc_b = alloc[node]
            cpu_req   = cpu_req_by_node.get(node, 0)
            mem_req_b = mem_req_by_node.get(node, 0)

            free_cpu   = cpu_alloc - cpu_req
            free_mem_b = mem_alloc_b - mem_req_b

            cpu_req_pct = (cpu_req / cpu_alloc * 100.0) if cpu_alloc > 0 else 0.0
            mem_req_pct = (mem_req_b / mem_alloc_b * 100.0) if mem_alloc_b > 0 else 0.0

            rows.append([
                node,
                f"{cpu_alloc}",
                f"{cpu_req}",
                f"{cpu_req_pct:.1f}%",
                f"{free_cpu}",
                f"{bytes_to_mib(mem_alloc_b)}",
                f"{bytes_to_mib(mem_req_b)}",
                f"{mem_req_pct:.1f}%",
                f"{bytes_to_mib(free_mem_b)}",
                pods_run_by_node.get(node, 0),
            ])

        return tabulate(rows, headers=headers, tablefmt="fancy_grid", stralign="right")

    def _totals_tables(self, alloc, cpu_req_by_node, mem_req_by_node,
                       all_run:int, all_notrun:int, cpu_req_all:int, mem_req_all:int):
        tot_cpu_alloc, tot_mem_alloc_b, tot_cpu_req_run, tot_mem_req_run_b = compute_stat_totals(
            alloc, cpu_req_by_node, mem_req_by_node
        )
        cpu_util_run = (tot_cpu_req_run / tot_cpu_alloc * 100.0) if tot_cpu_alloc else 0.0
        mem_util_run = (tot_mem_req_run_b / tot_mem_alloc_b * 100.0) if tot_mem_alloc_b else 0.0

        cpu_util_all = (cpu_req_all / tot_cpu_alloc * 100.0) if tot_cpu_alloc else 0.0
        mem_util_all = (mem_req_all / tot_mem_alloc_b * 100.0) if tot_mem_alloc_b else 0.0

        # Running-only summary
        tbl_run = tabulate([[
            tot_cpu_req_run, f"{cpu_util_run:.1f}",
            bytes_to_mib(tot_mem_req_run_b), f"{mem_util_run:.1f}",
        ]], headers=[
            "CPU\nREQ(m)", "CPU\nUTIL(%)",
            "MEM\nREQ(Mi)", "MEM\nUTIL(%)",
        ], tablefmt="fancy_grid", stralign="right")

        # All-pods summary (incl. unscheduled)
        tbl_all = tabulate([[
            cpu_req_all, f"{cpu_util_all:.1f}",
            bytes_to_mib(mem_req_all), f"{mem_util_all:.1f}",
            tot_cpu_alloc, bytes_to_mib(tot_mem_alloc_b),
            all_run, all_notrun
        ]], headers=[
            "CPU\nREQ(m)", "CPU\nUTIL(%)",
            "MEM\nREQ(Mi)", "MEM\nUTIL(%)",
            "CPU\nALLOC(m)", "MEM\nALLOC(Mi)",
            "PODS\nRUN", "PODS\nNOTRUN",
        ], tablefmt="fancy_grid", stralign="right")

        return tbl_run, tbl_all

    def run(self):
        s = stat_snapshot(self.ctx)
        self._printer(f"[kwok-stats] context: {self.ctx}")
        self._printer(self._node_table(s.alloc, s.cpu_req_by_node, s.mem_req_by_node, s.pods_run_by_node))
        self._printer("")
        self._printer("Stats (Running Pods Only)")
        run_only, totals = self._totals_tables(s.alloc, s.cpu_req_by_node, s.mem_req_by_node,
                                               s.all_run, s.all_notrun, s.cpu_req_all, s.mem_req_all)
        self._printer(run_only)
        self._printer("")
        self._printer("Stats (All Pods, incl. unscheduled)")
        self._printer(totals)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cluster_name")
    args = ap.parse_args()
    ctx = f"kwok-{args.cluster_name}"
    KwokStats(ctx).run()

if __name__ == "__main__":
    main()
