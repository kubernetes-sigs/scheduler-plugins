#!/usr/bin/env python3
# cluster_stats.py

import time
from dataclasses import dataclass
from typing import List, Tuple, Dict, Optional
from scripts.helpers.general_helpers import qty_to_mcpu_int, qty_to_bytes_int
from scripts.helpers.kubectl_helpers import get_json_ctx

@dataclass
class Snapshot:
    cpu_run_util: float                 # running requests / total alloc CPU
    mem_run_util: float                 # running requests / total alloc MEM
    pods_running: List[Tuple[str,str]]  # [(pod, node), ...] for Running pods
    pods_unscheduled: List[str]         # not-Running pod names
    pods_run_by_node: Dict[str,int]     # node -> running pods count
    cpu_req_by_node: Dict[str,int]      # node -> mCPU (Running & assigned only)
    mem_req_by_node: Dict[str,int]      # node -> bytes (Running & assigned only)
    cpu_alloc_by_node: Dict[str,int]    # node -> allocatable mCPU
    mem_alloc_by_node: Dict[str,int]    # node -> allocatable bytes
    running_placed_by_prio: Dict[str,int] = None  # priorityClassName -> count (Running only)
    unschedulable_by_prio: Dict[str,int] = None  # priorityClassName

def stat_snapshot(ctx: str, ns: str, expected: int) -> Snapshot:
    _, running, unscheduled = get_running_and_unscheduled(ctx, ns, expected)
    nodes = get_json_ctx(ctx, ["get","nodes","-o","json"])
    pods  = get_json_ctx(ctx, ["-n", ns, "get","pods","-o","json"])

    # Allocatable per node
    alloc: Dict[str,Tuple[int,int]] = {}
    for n in nodes["items"]:
        name = n["metadata"]["name"]
        a = n.get("status",{}).get("allocatable",{}) or {}
        alloc[name] = (
            qty_to_mcpu_int(a.get("cpu","0")),
            qty_to_bytes_int(a.get("memory","0")),
        )

    total_cpu_alloc_m = sum(v[0] for v in alloc.values())
    total_mem_alloc_b = sum(v[1] for v in alloc.values())

    cpu_req_m = {n:0 for n in alloc}
    mem_req_b = {n:0 for n in alloc}
    pods_run_by_node = {n:0 for n in alloc}

    running_by_prio: Dict[str,int] = {}
    unsched_by_prio: Dict[str,int] = {}

    for p in pods["items"]:
        phase = (p.get("status",{}) or {}).get("phase","")
        node  = (p.get("spec",{}) or {}).get("nodeName","")
        pc    = (p.get("spec",{}) or {}).get("priorityClassName","") or ""

        if phase == "Running":
            running_by_prio[pc] = running_by_prio.get(pc, 0) + 1
            if node in pods_run_by_node:
                pods_run_by_node[node] += 1
        else:
            unsched_by_prio[pc] = unsched_by_prio.get(pc, 0) + 1

        rcpu, rmem = sum_pod_requests(p)
        if node and node in alloc and phase == "Running":
            cpu_req_m[node] += rcpu
            mem_req_b[node] += rmem

    total_cpu_req_run_m = sum(cpu_req_m.values())
    total_mem_req_run_b = sum(mem_req_b.values())

    cpu_run_util = (total_cpu_req_run_m / total_cpu_alloc_m) if total_cpu_alloc_m else 0.0
    mem_run_util = (total_mem_req_run_b / total_mem_alloc_b) if total_mem_alloc_b else 0.0
    
    cpu_alloc_by_node = {n:v[0] for n,v in alloc.items()}
    mem_alloc_by_node = {n:v[1] for n,v in alloc.items()}

    return Snapshot(
        cpu_run_util=float(cpu_run_util),
        mem_run_util=float(mem_run_util),
        pods_running=running,
        pods_unscheduled=unscheduled,
        pods_run_by_node=pods_run_by_node,
        cpu_req_by_node=cpu_req_m,
        mem_req_by_node=mem_req_b,
        cpu_alloc_by_node=cpu_alloc_by_node,
        mem_alloc_by_node=mem_alloc_by_node,
        running_placed_by_prio=running_by_prio,
        unschedulable_by_prio=unsched_by_prio,
    )

def sum_pod_requests(pod: dict) -> tuple[int, int]:
    """
    Sum the CPU and memory requests for a pod by checking its containers and initContainers.
    """
    cpu_sum = 0
    mem_sum_b = 0
    spec = pod.get("spec", {}) or {}

    for c in spec.get("containers", []) or []:
        req = (c.get("resources",{}) or {}).get("requests",{}) or {}
        cpu_sum += qty_to_mcpu_int(req.get("cpu","0"))
        mem_sum_b += qty_to_bytes_int(req.get("memory","0"))

    init_cpu_max = 0
    init_mem_max_b = 0
    for c in spec.get("initContainers", []) or []:
        req = (c.get("resources",{}) or {}).get("requests",{}) or {}
        init_cpu_max = max(init_cpu_max, qty_to_mcpu_int(req.get("cpu","0")))
        init_mem_max_b = max(init_mem_max_b, qty_to_bytes_int(req.get("memory","0")))

    return cpu_sum + init_cpu_max, mem_sum_b + init_mem_max_b

def get_running_and_unscheduled(
    ctx: str,
    ns: str,
    expected: int,
    interval: float = 0.5,
    timeout: Optional[int] = 3,
) -> Tuple[str, List[Tuple[str, str]], List[str]]:
    """
    Poll pods until we can decide:
      - ("all_running", [(pod, node), ...], [])
      - ("some_unschedulable", [(pod, node), ...], [unscheduled_pods])
      - ("timeout", [(pod, node), ...], [unscheduled_pods])

    "running" is based on Pod.status.phase == "Running".
    "unschedulable" is every created pod that's not Running.
    """
    start = time.time()
    while True:
        pods_obj = get_json_ctx(ctx, ["-n", ns, "get", "pods", "-o", "json"])
        pods = pods_obj.get("items", []) or []

        created: List[str] = []
        running_pairs: List[Tuple[str, str]] = []
        running_names: set[str] = set()

        for p in pods:
            md = p.get("metadata") or {}
            spec = p.get("spec") or {}
            st = p.get("status") or {}
            name = md.get("name") or ""
            if not name:
                continue

            created.append(name)

            if (st.get("phase") or "") == "Running":
                node = spec.get("nodeName") or ""
                running_names.add(name)
                running_pairs.append((name, node))

        # Deterministic ordering
        running_pairs.sort(key=lambda t: t[0])

        # "unscheduled" == created pods that are not Running
        unschedulable = sorted([n for n in created if n not in running_names])

        # Keep "success" logic identical to before (just using Running instead of events)
        if len(running_pairs) >= expected:
            return "all_running", running_pairs, []

        # Created enough pods to decide, and some won't run
        if (len(running_pairs) + len(unschedulable)) >= expected and len(unschedulable) > 0:
            return "some_unschedulable", running_pairs, unschedulable

        if timeout is not None and (time.time() - start) >= timeout:
            return "timeout", running_pairs, unschedulable

        time.sleep(interval)