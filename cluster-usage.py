#!/usr/bin/env python3
import json, subprocess
from typing import Tuple, Dict, List
from tabulate import tabulate

# ---------- Quantity helpers ----------
def cpu_to_m(v: str) -> int:
    if not v: return 0
    return int(v[:-1]) if v.endswith('m') else int(float(v) * 1000)

def mem_to_bytes(v: str) -> int:
    if not v: return 0
    s = v.strip()
    try:
        if s.endswith("Ki"): return int(s[:-2]) * 1024
        if s.endswith("Mi"): return int(s[:-2]) * 1024 * 1024
        if s.endswith("Gi"): return int(s[:-2]) * 1024 * 1024 * 1024
        if s.endswith("Ti"): return int(s[:-2]) * 1024 * 1024 * 1024 * 1024
        return int(s)  # bytes
    except:
        return 0

def bytes_to_mib(b: int) -> int:
    return b // (1024 * 1024)

def bytes_to_kistr(b: int) -> str:
    kib = max(0, (b + 1023) // 1024)  # ceil to Ki
    return f"{kib}Ki"

def mem_to_mi(v: str) -> int:
    if not v: return 0
    s = v
    if s.endswith('Ki'): return max(0, int(s[:-2]) // 1024)
    if s.endswith('Mi'): return int(s[:-2])
    if s.endswith('Gi'): return int(s[:-2]) * 1024
    if s.endswith('Ti'): return int(s[:-2]) * 1024 * 1024
    try: return int(s)  # assume Mi
    except: return 0

def m_to_cpu_str(m: int) -> str:
    return str(m // 1000) if m % 1000 == 0 else f"{m}m"

def mi_to_mem_str(mi: int) -> str:
    return f"{mi}Mi"

def get_json(cmd: List[str]) -> dict:
    out = subprocess.check_output(cmd)
    return json.loads(out)

# ---------- Data collection ----------
def collect_alloc_and_usage() -> Tuple[
    Dict[str,Tuple[int,int]],  # alloc per node (cpu m, mem bytes)
    Dict[str,int],             # cpu req per node (m)
    Dict[str,int],             # mem req per node (bytes)
    Dict[str,int],             # pods_run_by_node
    int, int                   # all_run, all_notrun
]:
    nodes = get_json(["kubectl","get","nodes","-o","json"])
    pods  = get_json(["kubectl","get","pods","--all-namespaces","-o","json"])

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

    for p in pods["items"]:
        phase = (p.get("status",{}) or {}).get("phase","")
        node = (p.get("spec",{}) or {}).get("nodeName","")

        if phase == "Running":
            all_run += 1
            if node in pods_run_by_node:
                pods_run_by_node[node] += 1
        elif phase:
            all_notrun += 1

        if not node or node not in alloc:
            continue
        c, m = sum_requests(p)
        cpu_req[node] += c
        mem_req[node] += m

    return alloc, cpu_req, mem_req, pods_run_by_node, all_run, all_notrun

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

def compute_totals(alloc: Dict[str,Tuple[int,int]], cpu_req: Dict[str,int], mem_req: Dict[str,int]):
    tot_cpu_alloc = sum(v[0] for v in alloc.values())          # mCPU
    tot_mem_alloc_b = sum(v[1] for v in alloc.values())        # bytes
    tot_cpu_req   = sum(cpu_req.values())                      # mCPU
    tot_mem_req_b = sum(mem_req.values())                      # bytes
    return tot_cpu_alloc, tot_mem_alloc_b, tot_cpu_req, tot_mem_req_b

# ---------- Pretty printing ----------
def print_node_table(alloc, cpu_req_by_node, mem_req_by_node, pods_run_by_node):
    pods_run_by_node = pods_run_by_node or {n: 0 for n in alloc}
    headers = [
        "NODE", "CPU_ALLOC(m)", "CPU_REQ(m)", "CPU_UTIL(%)", "CPU_FREE(m)",
        "MEM_ALLOC(Mi)", "MEM_REQ(Mi)", "MEM_UTIL (%)", "MEM_FREE(Mi)", "PODS_RUN"
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

    print(tabulate(rows, headers=headers, tablefmt="fancy_grid", stralign="right"))

def print_totals_table(alloc, cpu_req_by_node, mem_req_by_node, all_run:int, all_notrun:int):
    tot_cpu_alloc, tot_mem_alloc_b, tot_cpu_req, tot_mem_req_b = compute_totals(alloc, cpu_req_by_node, mem_req_by_node)
    cpu_util = (tot_cpu_req / tot_cpu_alloc * 100.0) if tot_cpu_alloc else 0.0
    mem_util = (tot_mem_req_b / tot_mem_alloc_b * 100.0) if tot_mem_alloc_b else 0.0

    headers = [
        "TOTAL_PODS_RUN", "TOTAL_PODS_NOTRUN",
        "TOTAL_CPU_ALLOC(m)", "TOTAL_CPU_REQ(m)", "TOTAL_CPU_UTIL(%)",
        "TOTAL_MEM_ALLOC(Mi)", "TOTAL_MEM_REQ(Mi)", "TOTAL_MEM_UTIL(%)"
    ]
    row = [[
        all_run, all_notrun,
        tot_cpu_alloc, tot_cpu_req, f"{cpu_util:.1f}",
        bytes_to_mib(tot_mem_alloc_b), bytes_to_mib(tot_mem_req_b), f"{mem_util:.1f}",
    ]]
    print(tabulate(row, headers=headers, tablefmt="fancy_grid", stralign="right"))

def main():
    alloc, cpu_req_by_node, mem_req_by_node, pods_run_by_node, all_run, all_notrun = collect_alloc_and_usage()
    print_node_table(alloc, cpu_req_by_node, mem_req_by_node, pods_run_by_node)
    print()  # spacer
    print_totals_table(alloc, cpu_req_by_node, mem_req_by_node, all_run, all_notrun)

if __name__ == "__main__":
    main()
