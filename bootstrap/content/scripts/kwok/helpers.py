#!/usr/bin/env python3

# kwok_shared.py

import contextlib
import fcntl
import os
import hashlib, random
import time, subprocess, json, csv, re, logging, textwrap, sys, yaml
from typing import List, Dict, Tuple, Optional, Any
from dataclasses import dataclass
from pathlib import Path
from decimal import Decimal

MEM_UNIT_TABLE = {
    # bytes
    "": 1, "b": 1, "byte": 1, "bytes": 1,
    # SI (10^3)
    "k": 10**3, "kb": 10**3,
    "m": 10**6, "mb": 10**6,
    "g": 10**9, "gb": 10**9,
    # IEC (2^10)
    "ki": 1024, "kib": 1024,
    "mi": 1024**2, "mib": 1024**2,
    "gi": 1024**3, "gib": 1024**3,
}


# ====================================================================
# YAML helpers.
# Due to proper indentation, we keep them outside class
# ====================================================================
def read_yaml_docs(path: Path) -> dict:
    with open(path, "r", encoding="utf-8") as f:
        docs = list(yaml.safe_load_all(f))
    return docs

def yaml_priority_class(name: str, value: int) -> str:
    return textwrap.dedent(f"""\
    apiVersion: scheduling.k8s.io/v1
    kind: PriorityClass
    metadata:
      name: {name}
    value: {value}
    preemptionPolicy: PreemptLowerPriority
    globalDefault: false
    description: "pod priority {value}"
    ---
    """)

def yaml_kwok_node(name: str, cpu: str, mem: str, pods_cap: int) -> str:
    return textwrap.dedent(f"""\
    apiVersion: v1
    kind: Node
    metadata:
      name: {name}
      annotations:
        kwok.x-k8s.io/node: "fake"
      labels:
        kubernetes.io/hostname: "{name}"
        kubernetes.io/os: "linux"
        kubernetes.io/arch: "amd64"
        node-role.kubernetes.io/agent: ""
        type: "kwok"
    spec:
      taints:
      - key: kwok.x-k8s.io/node
        value: "fake"
        effect: NoSchedule
    status:
      capacity:
        cpu: "{cpu}"
        memory: "{mem}"
        pods: {pods_cap}
      allocatable:
        cpu: "{cpu}"
        memory: "{mem}"
        pods: {pods_cap}
      nodeInfo:
        architecture: amd64
        kubeletVersion: fake
        kubeProxyVersion: fake
        operatingSystem: linux
      phase: Running
    ---
    """)

def yaml_kwok_rs(ns: str, rs_name: str, replicas: int, cpu: str, mem: str, pc: str) -> str:
    return textwrap.dedent(f"""\
    apiVersion: apps/v1
    kind: ReplicaSet
    metadata:
      name: {rs_name}
      namespace: {ns}
    spec:
      replicas: {replicas}
      selector:
        matchLabels:
          app: {rs_name}
      template:
        metadata:
          labels:
            app: {rs_name}
        spec:
          priorityClassName: {pc}
          restartPolicy: Always
          tolerations:
          - key: "kwok.x-k8s.io/node"
            operator: "Exists"
            effect: "NoSchedule"
          containers:
          - name: filler
            image: registry.k8s.io/pause:3.9
            resources:
              requests: {{cpu: "{cpu}", memory: "{mem}"}}
              limits:   {{cpu: "{cpu}", memory: "{mem}"}}
    ---
    """)

def yaml_kwok_pod(ns: str, name: str, cpu: str, mem: str, pc: str) -> str:
    return textwrap.dedent(f"""\
    apiVersion: v1
    kind: Pod
    metadata:
      name: {name}
      namespace: {ns}
    spec:
      restartPolicy: Always
      priorityClassName: {pc}
      tolerations:
      - key: "kwok.x-k8s.io/node"
        operator: "Exists"
        effect: "NoSchedule"
      containers:
      - name: filler
        image: registry.k8s.io/pause:3.9
        resources:
          requests: {{cpu: "{cpu}", memory: "{mem}"}}
          limits:   {{cpu: "{cpu}", memory: "{mem}"}}
    ---
    """)

def get_timestamp() -> str:
    """
    Get the current timestamp as a string.
    """
    return time.strftime("%Y/%m/%d/%H:%M:%S", time.localtime())

def format_hms(seconds: int) -> str:
    """
    Format seconds into a human-readable string.
    """
    seconds = max(0, int(seconds))
    h, r = divmod(seconds, 3600)
    m, s = divmod(r, 60)
    parts = []
    if h: parts.append(f"{h}h")
    if m: parts.append(f"{m}m")
    if s or not parts: parts.append(f"{s}s")
    return "".join(parts)

##############################################
# ------------ Logging helpers----------------
##############################################
class PrefixFilter(logging.Filter):
    """
    Injects a static 'prefix' field into each LogRecord.
    """
    def __init__(self, prefix: str):
        super().__init__()
        self.prefix = prefix
    def filter(self, record: logging.LogRecord) -> bool:
        record.prefix = self.prefix
        return True

def setup_logging(prefix: str, level: str = "INFO") -> logging.Logger:
    """
    Configure the module logger.
    - prefix: shown before every message (e.g. '[worker 2/5 cluster=kwok1] ').
    - level:  DEBUG/INFO/WARNING/ERROR/CRITICAL (case-insensitive).
    """
    lvl = getattr(logging, str(level).upper(), logging.INFO)
    logger = logging.getLogger("kwok")
    logger.propagate = False
    logger.setLevel(lvl)
    logger.handlers.clear()
    h = logging.StreamHandler(stream=sys.stdout)
    fmt = logging.Formatter("%(asctime)s %(prefix)s%(message)s", datefmt="%H:%M:%S")
    h.setFormatter(fmt)
    h.addFilter(PrefixFilter(prefix))
    logger.addHandler(h)
    logging.captureWarnings(True)
    return logger

def make_header_footer(msg: str, width: int = 100, border: str = "=") -> Tuple[str, str]:
    """
    Build a centered header line with `msg` between border chars and a matching-width footer line.
    If `msg` is longer than `width`, the width expands to fit it.
    Example:
    >>> h, f = get_header_footer("Running tests")
    >>> print(h, " ... stuff ... ", f, sep="")
    """
    msg = str(msg).strip().replace("\n", " ")
    inner = f" {msg} "                       # space padding around the message
    w = max(width, len(inner))               # ensure width fits the message
    left = (w - len(inner)) // 2
    right = w - len(inner) - left
    header = f"\n{border * left}{inner}{border * right}\n"
    footer = f"\n{border * w}\n"
    return header, footer

##############################################
# ------------ Parser helpers----------------
##############################################
def normalize_interval(doc: Dict[str, Any], key_combo: Tuple[str, str, str], *, allow_none: bool = True) -> Optional[str]:
    """
    Normalize a (single) or (lo, hi) interval from the document.
    It first checks for 'single' key; if not found, it looks for 'lo_key' and 'hi_key'.
    Returns "lo,hi" or "" if not found (or None if allow_none and not found).
    """
    single, lo_key, hi_key = key_combo
    if single in doc and doc[single] is not None:
        v = doc[single]
        if isinstance(v, (int, float)):
            return str(int(v))
        if isinstance(v, str):
            s = v.strip()
            if s:
                return s
        elif isinstance(v, (list, tuple)) and len(v) == 2:
            return f"{str(v[0]).strip()},{str(v[1]).strip()}"
        elif isinstance(v, dict):
            lo = str(v.get("lo", "")).strip()
            hi = str(v.get("hi", "")).strip()
            if lo and hi:
                return f"{lo},{hi}"
    lo = str(doc.get(lo_key, "")).strip()
    hi = str(doc.get(hi_key, "")).strip()
    if lo and hi:
        return f"{lo},{hi}"
    return None if allow_none else ""

def split_interval(t: Optional[Tuple[int, int]]) -> tuple[str, str]:
    """Return (lo, hi) as strings; empty strings if None."""
    if not t:
        return "", ""
    return str(int(t[0])), str(int(t[1]))

def parse_int_interval(s: Optional[str], *, min_lo: int = 1) -> Optional[Tuple[int, int]]:
    """
    Parse a string interval "lo,hi" or "x" into a (lo, hi) tuple.
    Returns None if s is None or empty.
    Ensures lo >= min_lo and hi >= lo.
    """
    if not s:
        return None
    parts = [x.strip() for x in str(s).split(",", 1)]
    if len(parts) == 1:
        lo = hi = int(parts[0])
    else:
        lo, hi = int(parts[0]), int(parts[1])
    lo = max(min_lo, lo)
    hi = max(lo, hi)
    return lo, hi

def parse_qty_interval(s: Optional[str]) -> Optional[Tuple[str, str]]:
    if not s:
        return None
    parts = [x.strip() for x in s.split(",", 1)]
    if len(parts) == 1:
        return (parts[0], parts[0])
    return (parts[0], parts[1])

def parse_timeout_s(t:str | None, default: int = 60) -> int:
    """
    Parse a timeout string into seconds.
    """
    if not t:
        return default
    try:
        if t.endswith("ms"): return max(1, int(int(t[:-2]) / 1000))
        if t.endswith("s"):  return int(t[:-1])
        if t.endswith("m"):  return int(t[:-1]) * 60
        if t.endswith("h"):  return int(t[:-1]) * 3600
        return int(t)
    except Exception:
        return default

def coerce_bool(v, default=False):
    if v is None: return default
    if isinstance(v, bool): return v
    s = str(v).strip().lower()
    if s in ("1","true","yes","y","on"): return True
    if s in ("0","false","no","n","off"): return False
    return default

def get_str(v):
    if v is None: return None
    s = str(v).strip()
    return s if s else None

def get_int_from_dict(doc: Dict[str, Any], key: str, default: int) -> int:
    """
    Get an integer value from the document, returning a default if not found or invalid.
    """
    v = doc.get(key, default)
    try:
        return int(v)
    except Exception:
        return default

def get_float_from_dict(doc: Dict[str, Any], key: str, default: float) -> float:
    """
    Get a float value from the document, returning a default if not found or invalid.
    """
    v = doc.get(key, default)
    try:
        return float(v)
    except Exception:
        return default

def get_str_from_dict(doc: Dict[str, Any], key: str, default: Optional[str]) -> Optional[str]:
    """
    Get a string value from the document, returning a default if not found or empty.
    """
    v = doc.get(key, default)
    if v is None:
        return default
    s = str(v).strip()
    return s if s else default

##############################################
# ------------ Quantity helpers----------------
##############################################
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

##############################################
# ------------ kubectl helpers----------------
##############################################
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

##############################################
# ------------ kwokctl helpers----------------
##############################################
@contextlib.contextmanager
def kwok_cache_lock():
    """
    Take an exclusive lock so only one process runs 'kwokctl create cluster'
    at a time, avoiding races in ~/.kwok/cache when downloading binaries.
    Override lock path via env KWOK_CACHE_LOCK if desired.
    """
    lock_path = os.environ.get("KWOK_CACHE_LOCK", "/tmp/kwokctl-cache.lock")
    os.makedirs(os.path.dirname(lock_path), exist_ok=True)
    fd = os.open(lock_path, os.O_CREAT | os.O_RDWR, 0o666)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)
        yield
    finally:
        try:
            fcntl.flock(fd, fcntl.LOCK_UN)
        finally:
            os.close(fd)

##############################################
# ------------ File I/O helpers----------------
##############################################
def dir_exists(dir_path: str) -> bool:
    p = Path(dir_path)
    if not p.exists() or not p.is_dir():
        return False
    return True

def file_exists(file: Optional[str]) -> bool:
    if not file:
        return False
    f = Path(file)
    if not f.exists() or not f.is_file():
        return False
    return True

##############################################
# ------------ CSV helpers----------------
##############################################
def csv_read_header(path: Path) -> list[str] | None:
    """Return the header row for a CSV file, or None if unreadable/empty."""
    try:
        with open(path, "r", encoding="utf-8", newline="") as fh:
            rdr = csv.reader(fh)
            row = next(rdr, None)
            if row is None:
                return None
            # normalize by stripping surrounding spaces
            return [c.strip() for c in row]
    except Exception:
        return None

def ensure_csv_with_header(path: Path, header: List[str]) -> None:
    """
    Ensure the CSV file exists with the given header.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    if not path.exists():
        with open(path, "w", encoding="utf-8", newline="") as f:
            csv.DictWriter(f, fieldnames=header).writeheader()

def count_csv_rows(path: Path) -> int:
    """
    Count data rows (excluding header).
    """
    if not path.exists():
        return 0
    with open(path, "r", encoding="utf-8", newline="") as f:
        rd = csv.DictReader(f)
        return sum(1 for _ in rd)

def csv_append_row(
    file_path: str | Path,
    header: list[str],
    row: dict,
) -> None:
    """
    Append a row to CSV, writing the header if the file is new.
    Rules:
      - Reject if 'row' contains any keys not in 'header'.
      - Missing header fields are written as empty strings.
      - File column order always follows 'header' (incoming row order ignored).
    """
    p = Path(file_path)
    p.parent.mkdir(parents=True, exist_ok=True)
    header_set = set(header)
    row_keys = set(row.keys())
    extra = row_keys - header_set
    if extra:
        raise ValueError(f"Row contains fields not in header: {sorted(extra)}")
    # Build in header order; fill missing with ""
    safe_row = {k: ("" if row.get(k) is None else row.get(k, "")) for k in header}
    with open(p, "a", encoding="utf-8", newline="") as f:
        wr = csv.DictWriter(f, fieldnames=header)
        if f.tell() == 0:
            wr.writeheader()
        wr.writerow(safe_row)
        f.flush()

######################################################
# ---------- Seed helpers --------------
######################################################
def derive_seed(base_seed: int, *labels: object, nbytes: int = 16) -> int:
    """
    Deterministically derive a child seed from a base seed and a sequence of labels.
    This is to make sure that changes in one part of the code do not affect RNG streams in other parts.
    """
    h = hashlib.blake2b(digest_size=nbytes)
    # fixed-width encode base to keep derivations stable
    h.update(int(base_seed).to_bytes(16, "big", signed=False))
    for lab in labels:
        h.update(b"\x00")  # separator
        h.update(str(lab).encode("utf-8"))
    return int.from_bytes(h.digest(), "big")

def seeded_random(base_seed: int, *labels: object) -> random.Random:
    """
    Convenience: Random() seeded from _derive_seed(base_seed, *labels).
    """
    return random.Random(derive_seed(base_seed, *labels))

def generate_seeds(gen_seeds_to_file: Optional[List[str]]) -> None:
    """
    Generate random seeds and if requested, write them to one or multiple files.
    Modes:
    --generate-seeds-to-file PATH NUM
    --generate-seeds-to-file PATH NUM PARTS
    If PARTS provided and PATH contains '{i}', substitute it with 1..PARTS.
    Else, create PATH_part-<i>(.ext)
    """
    argsv = gen_seeds_to_file
    if not argsv or len(argsv) not in (2, 3):
        raise SystemExit("--gen-seeds-to-file requires PATH NUM [PARTS]")
    path_str, num_str = argsv[0], argsv[1]
    try:
        total = int(num_str)
    except Exception:
        raise SystemExit("--gen-seeds-to-file: NUM must be an integer")
    if total < 1:
        raise SystemExit("--gen-seeds-to-file: NUM must be >= 1")
    parts = 1
    if len(argsv) == 3:
        try:
            parts = int(argsv[2])
        except Exception:
            raise SystemExit("--gen-seeds-to-file: PARTS must be an integer")
        if parts < 1:
            raise SystemExit("--gen-seeds-to-file: PARTS must be >= 1")
        if parts > total:
            raise SystemExit("--gen-seeds-to-file: PARTS cannot exceed NUM")
    # RNG + seeds
    now_ns = int(time.time_ns())
    r = seeded_random(now_ns, "base")
    seeds = [(r.getrandbits(63) or 1) for _ in range(total)]
    # Split even-ish across parts
    base = total // parts
    rem = total % parts
    def _resolve_out(i: int) -> Path:
        if "{i}" in path_str:
            return Path(path_str.replace("{i}", str(i)))
        # No template; if multiple parts, add _part-i before extension inline
        p = Path(path_str)
        if parts > 1:
            if p.suffix:
                return p.with_name(f"{p.stem}-{i}{p.suffix}")
            else:
                return p.with_name(f"{p.name}-{i}")
        return p  # single file
    # Ensure parent dir of first output exists
    first_out = _resolve_out(1)
    first_out.parent.mkdir(parents=True, exist_ok=True)
    offset = 0
    written_total = 0
    for i in range(1, parts + 1):
        size = base + (1 if i <= rem else 0)
        chunk = seeds[offset:offset + size]
        offset += size
        outp = _resolve_out(i)
        outp.parent.mkdir(parents=True, exist_ok=True)
        with open(outp, "w", encoding="utf-8", newline="") as f:
            for s in chunk:
                f.write(f"{s}\n")
        print(f"wrote {len(chunk)} seeds to {outp}")
        written_total += len(chunk)
    print(f"wrote {written_total} seeds across {parts} file(s)")

##############################################
# ------------ Stats helpers----------------
##############################################
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

def compute_stat_totals(alloc: Dict[str,Tuple[int,int]], cpu_req_by_node: Dict[str,int], mem_req_by_node: Dict[str,int]) -> tuple[int, int, int, int]:
    """
    Compute total cluster resource usage statistics.
    """
    tot_cpu_alloc = sum(v[0] for v in alloc.values())    # mCPU
    tot_mem_alloc_b = sum(v[1] for v in alloc.values())  # bytes
    tot_cpu_req_run = sum(cpu_req_by_node.values())      # mCPU
    tot_mem_req_run_b = sum(mem_req_by_node.values())    # bytes
    return tot_cpu_alloc, tot_mem_alloc_b, tot_cpu_req_run, tot_mem_req_run_b

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
