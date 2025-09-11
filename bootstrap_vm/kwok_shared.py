#!/usr/bin/env python3

# kwok_shared.py

import time, subprocess, json, csv
from typing import List, Dict, Tuple, Optional
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path

# ---------- quantity helpers ----------
def cpu_m_str_to_int(v: str) -> int:
    """
    Convert CPU string (e.g. "100m", "1") to milli CPU integer (e.g. 100, 1000).
    """
    if not v: return 0
    return int(v[:-1]) if v.endswith('m') else int(float(v) * 1000)

def cpu_m_int_to_str(m: int) -> str:
    """
    Convert milli CPU integer (e.g. 100, 1000) to CPU string (e.g. "100m", "1").
    """
    return str(m // 1000) if m % 1000 == 0 else f"{m}m"

def mem_str_to_bytes_int(v: str) -> int:
    """
    Convert memory string (e.g. "100Mi", "1Gi") to bytes (e.g. 104857600, 1073741824).
    """
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
    """
    Convert bytes to MiB.
    """
    return b // (1024 * 1024)

def mem_str_to_mib_int(v: str) -> int:
    """
    Convert memory string (e.g. "100Mi", "1Gi") to MiB (e.g. 100, 1024).
    """
    if not v: return 0
    s = v.strip()
    try:
        if s.endswith('Ki'): return max(0, int(s[:-2]) // 1024)
        if s.endswith('Mi'): return int(s[:-2])
        if s.endswith('Gi'): return int(s[:-2]) * 1024
        if s.endswith('Ti'): return int(s[:-2]) * 1024 * 1024
        return int(s)  # assume Mi when bare number
    except:
        return 0

def mem_mi_int_to_str(mi: int) -> str:
    """
    Convert MiB to memory string (e.g. 100Mi -> "100Mi").
    """
    return f"{mi}Mi"

# ---------- kubectl helpers ----------
def get_json_ctx(ctx: str, base_cmd: list[str]) -> dict:
    """
    Get the JSON output from a kubectl command.
    """
    cmd = ["kubectl"]
    if ctx:
        cmd += ["--context", ctx]
    cmd += base_cmd
    out = subprocess.check_output(cmd)
    return json.loads(out)

# ---------- CSV helpers ----------
def csv_append_row(
    file_path: str | Path,
    header: List[str],
    row: dict,
) -> None:
    """
    Append a single row to a CSV/TSV file, writing the header if the file is new/empty.
    - Creates parent dirs.
    - Ignores extra keys in 'row' not present in header (extrasaction='ignore').
    - Uses configurable delimiter (default ',').
    """
    p = Path(file_path)
    p.parent.mkdir(parents=True, exist_ok=True)

    with open(p, "a", encoding="utf-8", newline="") as f:
        wr = csv.DictWriter(f, fieldnames=header)
        wr.writeheader() if f.tell() == 0 else None
        wr.writerow(row)
        f.flush()

# ---------- stats helpers ----------
@dataclass
class Snapshot:
    cpu_run_util: float                    # running requests / total alloc CPU
    mem_run_util: float                    # running requests / total alloc MEM
    pods_scheduled: list[str]              # total running pods
    pods_unscheduled: list[str]            # all not-running pods
    pods_run_by_node: Dict[str,int]        # node -> running pods count
    cpu_req_by_node: Dict[str,int]         # node -> m (Running & assigned only)
    mem_req_by_node: Dict[str,int]         # node -> bytes (Running & assigned only)

def stat_snapshot(ctx: str, ns: str, expected: int, settle_timeout: float) -> Snapshot:
    _, scheduled_pairs, unscheduled = get_scheduled_and_unscheduled(
        ctx, ns, expected=expected, settle_timeout=settle_timeout
    )
    scheduled = [n for (n, _) in scheduled_pairs]

    nodes = get_json_ctx(ctx, ["get","nodes","-o","json"])
    pods  = get_json_ctx(ctx, ["-n", ns, "get","pods","-o","json"])

    # Allocatable per node
    alloc: Dict[str,Tuple[int,int]] = {}
    for n in nodes["items"]:
        name = n["metadata"]["name"]
        a = n.get("status",{}).get("allocatable",{}) or {}
        alloc[name] = (
            cpu_m_str_to_int(a.get("cpu","0")),
            mem_str_to_bytes_int(a.get("memory","0")),
        )

    # Totals for util calc
    total_cpu_alloc_m = sum(v[0] for v in alloc.values())
    total_mem_alloc_b = sum(v[1] for v in alloc.values())

    # Per-node running attribution
    cpu_req_m = {n:0 for n in alloc}
    mem_req_b = {n:0 for n in alloc}
    pods_run_by_node = {n:0 for n in alloc}

    for p in pods["items"]:
        phase = (p.get("status",{}) or {}).get("phase","")
        node  = (p.get("spec",{}) or {}).get("nodeName","")

        if phase == "Running" and node in pods_run_by_node:
            pods_run_by_node[node] += 1

        rcpu, rmem = sum_pod_requests(p)
        if node and node in alloc and phase == "Running":
            cpu_req_m[node] += rcpu
            mem_req_b[node] += rmem

    # Running-only totals
    total_cpu_req_run_m = sum(cpu_req_m.values())
    total_mem_req_run_b = sum(mem_req_b.values())

    # Running utilization (0..1)
    cpu_run_util = (total_cpu_req_run_m / total_cpu_alloc_m) if total_cpu_alloc_m else 0.0
    mem_run_util = (total_mem_req_run_b / total_mem_alloc_b) if total_mem_alloc_b else 0.0

    return Snapshot(
        cpu_run_util=float(cpu_run_util),
        mem_run_util=float(mem_run_util),
        pods_scheduled=scheduled,
        pods_unscheduled=unscheduled,
        pods_run_by_node=pods_run_by_node,
        cpu_req_by_node=cpu_req_m,
        mem_req_by_node=mem_req_b,
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
        cpu_sum += cpu_m_str_to_int(req.get("cpu","0"))
        mem_sum_b += mem_str_to_bytes_int(req.get("memory","0"))

    init_cpu_max = 0
    init_mem_max_b = 0
    for c in spec.get("initContainers", []) or []:
        req = (c.get("resources",{}) or {}).get("requests",{}) or {}
        init_cpu_max = max(init_cpu_max, cpu_m_str_to_int(req.get("cpu","0")))
        init_mem_max_b = max(init_mem_max_b, mem_str_to_bytes_int(req.get("memory","0")))

    return cpu_sum + init_cpu_max, mem_sum_b + init_mem_max_b

def compute_stat_totals(alloc: Dict[str,Tuple[int,int]], cpu_req_by_node: Dict[str,int], mem_req_by_node: Dict[str,int]) -> tuple[int, int, int, int]:
    """
    Compute total cluster resource usage statistics.
    """
    tot_cpu_alloc = sum(v[0] for v in alloc.values())    # mCPU
    tot_mem_alloc_b = sum(v[1] for v in alloc.values())  # bytes
    tot_cpu_req_run = sum(cpu_req_by_node.values())      # mCPU
    tot_mem_req_run_b = sum(mem_req_by_node.values())    # bytes
    return tot_cpu_alloc, tot_mem_alloc_b, tot_cpu_req_run, tot_mem_req_run_b

def _rfc3339_to_dt(s: str) -> datetime:
    """
    Convert an RFC3339 formatted string to a datetime object.
    Handle "2025-03-04T12:34:56Z" and "2025-03-04T12:34:56.123456Z"
    """
    if not s:
        return datetime.min
    s = s.strip().replace("Z", "+00:00")
    try:
        return datetime.fromisoformat(s)
    except Exception:
        return datetime.min

def get_scheduled_and_unscheduled(
    ctx: str,
    ns: str,
    expected: int,
    interval: float = 0.5,
    settle_timeout: Optional[int] = None
) -> Tuple[str, List[Tuple[str, str]], List[str]]:
    """
    Poll pod events until we can decide:
      - ("all_scheduled", [(pod, node), ...], [])
      - ("some_unschedulable", [(pod, node), ...], [unschedulable_pods])
      - ("timeout", [(pod, node), ...], [unschedulable_pods])
    Decision is based on the latest event per pod name.
    """
    import re
    node_from_msg = re.compile(r"\bto\s+([A-Za-z0-9._:-]+)\.?$")

    start = time.time()
    while True:
        cmd = ["kubectl", "--context", ctx, "-n", ns,
               "get", "events",
               "--field-selector", "involvedObject.kind=Pod",
               "-o", "json"]
        out = subprocess.check_output(cmd)
        items = (json.loads(out) or {}).get("items", [])

        # name -> (ts, state, node)
        latest: Dict[str, tuple[datetime, str, str]] = {}

        for ev in items:
            inv = (ev.get("involvedObject") or {})
            name = inv.get("name") or ""
            if not name:
                continue

            reason  = (ev.get("reason") or "").strip()
            message = (ev.get("message") or "").strip()
            ts = (_rfc3339_to_dt(ev.get("eventTime") or "")
                  or _rfc3339_to_dt(ev.get("lastTimestamp") or "")
                  or _rfc3339_to_dt((ev.get("metadata") or {}).get("creationTimestamp") or ""))

            state = "other"
            node = ""
            if reason == "Scheduled":
                state = "scheduled"
                m = node_from_msg.search(message)
                if m:
                    node = m.group(1)
            elif reason == "FailedScheduling":
                state = "failed_scheduling"
            elif reason == "Preempted":
                state = "preempted"
            elif "evict" in message.lower():
                state = "preempted"

            prev = latest.get(name)
            if (prev is None) or (ts > prev[0]):
                latest[name] = (ts, state, node)

        scheduled_pairs = sorted([(n, node) for n, (_, st, node) in latest.items() if st == "scheduled"], key=lambda x: x[0])
        unschedulable = sorted([n for n, (_, st, _) in latest.items() if st in {"failed_scheduling", "preempted"}])

        failed_like = len(unschedulable)

        if len(scheduled_pairs) >= expected:
            return "all_scheduled", scheduled_pairs, []

        if (len(scheduled_pairs) + failed_like) >= expected and failed_like > 0:
            return "some_unschedulable", scheduled_pairs, unschedulable

        if settle_timeout is not None and (time.time() - start) >= settle_timeout:
            return "timeout", scheduled_pairs, unschedulable

        time.sleep(interval)