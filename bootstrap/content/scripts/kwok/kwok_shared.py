#!/usr/bin/env python3

# kwok_shared.py

import time, subprocess, json, csv, re
from typing import List, Dict, Tuple, Optional
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path
from decimal import Decimal

# TODO: reach here in this file

MEM_UNIT_TABLE = {
    # bytes
    "": 1, "b": 1, "byte": 1, "bytes": 1,
    # SI (10^3)
    "k": 10**3, "kb": 10**3,
    "m": 10**6, "mb": 10**6,
    "g": 10**9, "gb": 10**9,
    "t": 10**12, "tb": 10**12,
    "p": 10**15, "pb": 10**15,
    "e": 10**18, "eb": 10**18,
    # IEC (2^10)
    "ki": 1024, "kib": 1024,
    "mi": 1024**2, "mib": 1024**2,
    "gi": 1024**3, "gib": 1024**3,
    "ti": 1024**4, "tib": 1024**4,
    "pi": 1024**5, "pib": 1024**5,
    "ei": 1024**6, "eib": 1024**6,
}

# ---------- quantity helpers ----------
def qty_to_mcpu_int(token: str) -> int:
    """
    Convert any CPU quantity to millicores.
    Accepts: '250m', '0.25', '1.5', '2cpu', '2 cores', etc.
    No unit => cores.
    """
    t = (token or "").strip().lower()
    m = re.fullmatch(r'\s*([0-9]+(?:\.[0-9]+)?)\s*([a-z ]*)\s*', t)
    if not m:
        raise ValueError(f"Invalid CPU quantity: {token!r}")
    val = Decimal(m.group(1))
    unit = (m.group(2) or "").replace(" ", "")
    if unit in ("m", "mcpu", "millicpu"):
        milli = val
    elif unit in ("", "c", "cpu", "core", "cores"):
        milli = val * Decimal(1000)
    else:
        raise ValueError(f"Unsupported CPU unit: {unit!r}")
    return max(1, int(milli))

def qty_to_bytes_int(token: str) -> int:
    
    """
    Convert any Kubernetes-like memory quantity to integer bytes.
    Accepts: '1536Mi', '1.5Gi', '500MB', '4G', '1024', '42 kib', etc.
    No unit => bytes.
    """
    t = (token or "").strip().lower()
    m = re.fullmatch(r'\s*([0-9]+(?:\.[0-9]+)?)\s*([a-z]+)?\s*', t)
    if not m:
        raise ValueError(f"Invalid memory quantity: {token!r}")
    val = Decimal(m.group(1))
    unit = (m.group(2) or "")
    mult = MEM_UNIT_TABLE.get(unit)
    if mult is None:
        raise ValueError(f"Unsupported memory unit: {unit!r}")
    bytes_int = int(val * mult)
    return max(1, bytes_int)  # keep >0 for downstream constraints

def qty_to_mcpu_str(m: int) -> str:
    """
    Convert milli CPU integer (e.g. 100, 1000) to CPU string (e.g. "100m", "1").
    """
    return str(m // 1000) if m % 1000 == 0 else f"{m}m"

def qty_to_bytes_str(b: int) -> str:
    """Return bytes as a decimal quantity string for K8s."""
    return str(int(max(1, b)))

# ---------- kubectl helpers ----------

def get_json_ctx(ctx: str, base_cmd: list[str]) -> dict:
    """
    Get the JSON output from a kubectl command.
    """
    cmd = ["kubectl"]
    if ctx:
        cmd += ["--context", ctx]
    cmd += base_cmd
    try:
        out = subprocess.check_output(cmd, stderr=subprocess.STDOUT)
    except subprocess.CalledProcessError as e:
        msg = (e.stdout or b"").decode("utf-8", "replace")
        tail = msg[-1200:]
        raise RuntimeError(
            f"kubectl failed: rc={e.returncode} cmd={' '.join(cmd)} output_tail={tail!r}"
        ) from e
    return json.loads(out)

# ---------- file I/O helpers ----------

def dir_exists(dir_path: str) -> None:
    p = Path(dir_path)
    if not p.exists():
        raise SystemExit(f"{p} not found: {p}")
    if not p.is_dir():
        raise SystemExit(f"{p} is not a directory")

def file_exists(file: Optional[str]) -> None:
    if not file:
        return
    f = Path(file)
    if not f.exists() or not f.is_file():
        raise SystemExit(f"--seed-file not found or not a regular file: {f}")
    try:
        with open(f, "r", encoding="utf-8"):
            pass
    except Exception as e:
        raise SystemExit(f"--seed-file not readable: {f} ({e})")

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
    cpu_run_util: float                 # running requests / total alloc CPU
    mem_run_util: float                 # running requests / total alloc MEM
    pods_running: List[Tuple[str,str]]  # [(pod, node), ...] for Running pods
    pods_unscheduled: List[str]         # not-Running pod names
    pods_run_by_node: Dict[str,int]     # node -> running pods count
    cpu_req_by_node: Dict[str,int]      # node -> mCPU (Running & assigned only)
    mem_req_by_node: Dict[str,int]      # node -> bytes (Running & assigned only)
    cpu_alloc_by_node: Dict[str,int]    # node -> allocatable mCPU
    mem_alloc_by_node: Dict[str,int]    # node -> allocatable bytes

def stat_snapshot(ctx: str, ns: str, expected: int, settle_timeout: float) -> Snapshot:
    _, running, unscheduled = get_running_and_unscheduled(
        ctx, ns, expected=expected, settle_timeout=settle_timeout
    )
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

def get_running_and_unscheduled(
    ctx: str,
    ns: str,
    expected: int,
    interval: float = 0.5,
    settle_timeout: Optional[int] = None,
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

        if settle_timeout is not None and (time.time() - start) >= settle_timeout:
            return "timeout", running_pairs, unschedulable

        time.sleep(interval)
