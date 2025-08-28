#!/usr/bin/env python3

# kwok_stats.py

from typing import Tuple, Dict, Callable, Optional
from dataclasses import dataclass
from tabulate import tabulate

import os, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from kwok_shared import (
    get_json,
    cpu_to_m, mem_to_bytes, bytes_to_mib,
)

# ---------- helpers ----------
def sum_requests(pod: dict) -> tuple[int, int]:
    cpu_sum = 0
    mem_sum_b = 0
    spec = pod.get("spec", {}) or {}

    for c in spec.get("containers", []) or []:
        req = (c.get("resources",{}) or {}).get("requests",{}) or {}
        cpu_sum += cpu_to_m(req.get("cpu","0"))
        mem_sum_b += mem_to_bytes(req.get("memory","0"))

    init_cpu_max = 0
    init_mem_max_b = 0
    for c in spec.get("initContainers", []) or []:
        req = (c.get("resources",{}) or {}).get("requests",{}) or {}
        init_cpu_max = max(init_cpu_max, cpu_to_m(req.get("cpu","0")))
        init_mem_max_b = max(init_mem_max_b, mem_to_bytes(req.get("memory","0")))

    return cpu_sum + init_cpu_max, mem_sum_b + init_mem_max_b


@dataclass
class Snapshot:
    alloc: Dict[str, Tuple[int,int]]          # node -> (cpu m, mem bytes)
    cpu_req_by_node: Dict[str,int]            # node -> m (Running & assigned only)
    mem_req_by_node: Dict[str,int]            # node -> bytes (Running & assigned only)
    pods_run_by_node: Dict[str,int]           # node -> running pods count
    all_run: int
    all_notrun: int
    cpu_req_all: int                          # all pods (incl. unscheduled)
    mem_req_all: int                          # all pods (incl. unscheduled)


class KwokStats:
    def __init__(self,
                 fetch_json: Callable[[list], dict] = get_json,
                 printer: Callable[[str], None] = print):
        self.fetch_json = fetch_json
        self.printer = printer
        self.snap: Optional[Snapshot] = None
        
    def _collect(self) -> Snapshot:
        nodes = self.fetch_json(["kubectl","get","nodes","-o","json"])
        pods  = self.fetch_json(["kubectl","get","pods","--all-namespaces","-o","json"])

        alloc: Dict[str,Tuple[int,int]] = {}
        for n in nodes["items"]:
            name = n["metadata"]["name"]
            a = n.get("status",{}).get("allocatable",{}) or {}
            alloc[name] = (cpu_to_m(a.get("cpu","0")), mem_to_bytes(a.get("memory","0")))

        cpu_req = {n:0 for n in alloc}
        mem_req = {n:0 for n in alloc}
        pods_run_by_node = {n:0 for n in alloc}

        all_run = 0
        all_notrun = 0
        cpu_req_all = 0
        mem_req_all = 0

        for p in pods["items"]:
            phase = (p.get("status",{}) or {}).get("phase","")
            node = (p.get("spec",{}) or {}).get("nodeName","")

            if phase == "Running":
                all_run += 1
                if node in pods_run_by_node:
                    pods_run_by_node[node] += 1
            elif phase:
                all_notrun += 1

            rcpu, rmem = sum_requests(p)
            cpu_req_all += rcpu
            mem_req_all += rmem

            # Per-node attribution: running & assigned only (your current policy)
            if node and node in alloc and phase == "Running":
                cpu_req[node] += rcpu
                mem_req[node] += rmem

        self.snap = Snapshot(
            alloc=alloc,
            cpu_req_by_node=cpu_req,
            mem_req_by_node=mem_req,
            pods_run_by_node=pods_run_by_node,
            all_run=all_run,
            all_notrun=all_notrun,
            cpu_req_all=cpu_req_all,
            mem_req_all=mem_req_all,
        )
        return self.snap

    def _compute_totals(self,
                       alloc: Dict[str,Tuple[int,int]],
                       cpu_req_by_node: Dict[str,int],
                       mem_req_by_node: Dict[str,int]):
        tot_cpu_alloc = sum(v[0] for v in alloc.values())          # mCPU
        tot_mem_alloc_b = sum(v[1] for v in alloc.values())        # bytes
        tot_cpu_req_run   = sum(cpu_req_by_node.values())          # mCPU
        tot_mem_req_run_b = sum(mem_req_by_node.values())          # bytes
        return tot_cpu_alloc, tot_mem_alloc_b, tot_cpu_req_run, tot_mem_req_run_b


    def _node_table(self, alloc, cpu_req_by_node, mem_req_by_node, pods_run_by_node) -> str:
        pods_run_by_node = pods_run_by_node or {n: 0 for n in alloc}
        headers = [
            "NODE", "CPU_ALLOC(m)", "CPU_REQ(m)", "CPU_UTIL(%)", "CPU_FREE(m)",
            "MEM_ALLOC(Mi)", "MEM_REQ(Mi)", "MEM_UTIL(%)", "MEM_FREE(Mi)", "PODS_RUN"
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

    def _totals_tables(self,
                       alloc,
                       cpu_req_by_node, mem_req_by_node,
                       all_run:int, all_notrun:int,
                       cpu_req_all:int, mem_req_all:int) -> str:
        tot_cpu_alloc, tot_mem_alloc_b, tot_cpu_req_run, tot_mem_req_run_b = self._compute_totals(
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
            "RUN_CPU_REQ(m)", "RUN_CPU_UTIL(%)",
            "RUN_MEM_REQ(Mi)", "RUN_MEM_UTIL(%)",
        ], tablefmt="fancy_grid", stralign="right")

        # Cluster allocation summary
        tbl_alloc = tabulate([[
            tot_cpu_alloc, bytes_to_mib(tot_mem_alloc_b),
            all_run, all_notrun
        ]], headers=[
            "TOTAL_CPU_ALLOC(m)", "TOTAL_MEM_ALLOC(Mi)",
            "TOTAL_PODS_RUN", "TOTAL_PODS_NOTRUN",
        ], tablefmt="fancy_grid", stralign="right")

        # All-pods request summary (incl. unscheduled)
        tbl_all = tabulate([[
            cpu_req_all, f"{cpu_util_all:.1f}",
            bytes_to_mib(mem_req_all), f"{mem_util_all:.1f}",
        ]], headers=[
            "TOTAL_CPU_REQ_ALL(m)", "TOTAL_CPU_UTIL_ALL(%)",
            "TOTAL_MEM_REQ_ALL(Mi)", "TOTAL_MEM_UTIL_ALL(%)",
        ], tablefmt="fancy_grid", stralign="right")

        return "\n".join([tbl_run, tbl_alloc, tbl_all])

    def run(self):
        s = self._collect()
        self.printer(self._node_table(s.alloc, s.cpu_req_by_node, s.mem_req_by_node, s.pods_run_by_node))
        self.printer("")
        self.printer(self._totals_tables(
            s.alloc, s.cpu_req_by_node, s.mem_req_by_node,
            s.all_run, s.all_notrun, s.cpu_req_all, s.mem_req_all
        ))


def main():
    KwokStats().run()

if __name__ == "__main__":
    main()
