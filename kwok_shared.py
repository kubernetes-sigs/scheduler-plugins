#!/usr/bin/env python3

# kwok_shared.py

import sys, time, random, subprocess, json, textwrap
from typing import List, Dict, Tuple, Optional
from dataclasses import dataclass
from datetime import datetime

# ---------- subprocess helpers ----------
def run(cmd:list[str], **kw):
    return subprocess.run(cmd, **kw)

def apply_yaml(ctx:str, yaml_text:str):
    return run(["kubectl","--context",ctx,"apply","-f","-"], input=yaml_text.encode(), check=True)

def get_json_ctx(ctx: Optional[str], base_cmd: list[str]) -> dict:
    cmd = ["kubectl"]
    if ctx:
        cmd += ["--context", ctx]
    cmd += base_cmd
    out = subprocess.check_output(cmd)
    return json.loads(out)

# ---------- quantity helpers ----------
def cpu_to_m(v: str) -> int:
    if not v: return 0
    return int(v[:-1]) if v.endswith('m') else int(float(v) * 1000)

def m_to_cpu_str(m: int) -> str:
    return str(m // 1000) if m % 1000 == 0 else f"{m}m"

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
    s = v.strip()
    try:
        if s.endswith('Ki'): return max(0, int(s[:-2]) // 1024)
        if s.endswith('Mi'): return int(s[:-2])
        if s.endswith('Gi'): return int(s[:-2]) * 1024
        if s.endswith('Ti'): return int(s[:-2]) * 1024 * 1024
        return int(s)  # assume Mi when bare number
    except:
        return 0

def mi_to_mem_str(mi: int) -> str:
    return f"{mi}Mi"

# ---------- stats helpers ----------
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

def stat_snapshot(ctx: str) -> Snapshot:
    nodes = get_json_ctx(ctx, ["get","nodes","-o","json"])
    pods  = get_json_ctx(ctx, ["get","pods","--all-namespaces","-o","json"])

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

    return Snapshot(
        alloc=alloc,
        cpu_req_by_node=cpu_req,
        mem_req_by_node=mem_req,
        pods_run_by_node=pods_run_by_node,
        all_run=all_run,
        all_notrun=all_notrun,
        cpu_req_all=cpu_req_all,
        mem_req_all=mem_req_all,
    )

def compute_stat_totals(alloc: Dict[str,Tuple[int,int]],
                        cpu_req_by_node: Dict[str,int],
                        mem_req_by_node: Dict[str,int]):
    tot_cpu_alloc = sum(v[0] for v in alloc.values())    # mCPU
    tot_mem_alloc_b = sum(v[1] for v in alloc.values())  # bytes
    tot_cpu_req_run = sum(cpu_req_by_node.values())      # mCPU
    tot_mem_req_run_b = sum(mem_req_by_node.values())    # bytes
    return tot_cpu_alloc, tot_mem_alloc_b, tot_cpu_req_run, tot_mem_req_run_b

# ---------- kubernetes state queries ------------

def check_kwok_context(ctx_name: str) -> Optional[str]:
    """
    Return ctx_name if it exists in kubectl config, else None.
    """
    try:
        cfg = get_json_ctx(None, ["config", "view", "-o", "json"])
    except Exception:
        raise Exception(f"Could not read kubectl config to resolve context '{ctx_name}'")

    contexts = cfg.get("contexts", []) or []
    for c in contexts:
        if (c.get("name") or "") == ctx_name:
            return ctx_name
    return None


def exists(ctx:str, kind:str, name:str, ns:str) -> bool:
    r = run(["kubectl","--context",ctx,"-n",ns,"get",kind,name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    return r.returncode == 0

def count_scheduled_in_ns(ctx: str, ns: str) -> int:
    r = run(["kubectl","--context",ctx,"-n",ns,"get","pods","-o","json"],
            stdout=subprocess.PIPE, check=True)
    items = json.loads(r.stdout).get("items", [])
    cnt = 0
    for p in items:
        node = (p.get("spec") or {}).get("nodeName") or ""
        if node: cnt += 1
    return cnt

def _rfc3339_to_dt(s: str) -> datetime:
    # Handle "2025-03-04T12:34:56Z" and "2025-03-04T12:34:56.123456Z"
    if not s:
        return datetime.min
    s = s.strip().replace("Z", "+00:00")
    try:
        return datetime.fromisoformat(s)
    except Exception:
        return datetime.min

def count_scheduled_from_events(ctx: str, ns: str) -> Tuple[int, Dict[str, str]]:
    """
    Reads all Pod events in the namespace and determines each pod's *current*
    scheduling state by looking at the latest relevant event for that pod name.
    Returns (scheduled_count, state_by_pod) where state ∈ {"scheduled","preempted","failed_scheduling","other"}.
    """
    cmd = ["kubectl", "--context", ctx, "-n", ns,
           "get", "events",
           "--field-selector", "involvedObject.kind=Pod",
           "-o", "json"]
    out = subprocess.check_output(cmd)
    items = (json.loads(out) or {}).get("items", [])

    # Track the latest relevant event per pod name
    latest: Dict[str, tuple[datetime, str]] = {}  # name -> (ts, state)

    for ev in items:
        inv = (ev.get("involvedObject") or {})
        name = inv.get("name") or ""
        if not name:
            continue

        reason  = (ev.get("reason") or "").strip()
        message = (ev.get("message") or "").lower()
        # Choose best timestamp available
        ts = (_rfc3339_to_dt(ev.get("eventTime") or "")
              or _rfc3339_to_dt(ev.get("lastTimestamp") or "")
              or _rfc3339_to_dt((ev.get("metadata") or {}).get("creationTimestamp") or ""))

        # Map reasons to coarse states
        if reason == "Scheduled":
            state = "scheduled"
        elif reason == "Preempted":
            state = "preempted"
        elif reason == "FailedScheduling":
            state = "failed_scheduling"
        elif reason == "Evicted" or "evict" in message:
            state = "preempted"  # treat as not-running/terminal for our settle logic
        else:
            state = "other"

        prev = latest.get(name)
        if (prev is None) or (ts > prev[0]):
            latest[name] = (ts, state)

    scheduled_count = sum(1 for _, (_, st) in latest.items() if st == "scheduled")
    state_by_pod = {name: st for name, (_, st) in latest.items()}
    return scheduled_count, state_by_pod

def pods_per_node_in_ns(ctx: str, ns: str, nodes: List[str]) -> Dict[str,int]:
    pods_json = json.loads(run(
        ["kubectl","--context",ctx,"-n",ns,"get","pods","-o","json"],
        stdout=subprocess.PIPE, check=True).stdout)
    by = {n:0 for n in nodes}
    for p in pods_json.get("items", []):
        node = (p.get("spec",{}) or {}).get("nodeName") or ""
        if node in by and (p.get("status",{}) or {}).get("phase") == "Running":
            by[node] += 1
    return by



def set_context(ctx_name: str):
    """
    Set the kubectl context to the specified context name.
    """
    run(["kubectl", "config", "use-context", ctx_name])

# ---------- waiters / monitors --------

def wait_each(ctx: str, kind: str, name: str, ns: str, timeout_sec: int, mode: str) -> int:
    if kind == "pod":
        return wait_pod(ctx, name, ns, timeout_sec, mode)
    elif kind in ("replicaset", "rs"):
        return wait_rs_pods(ctx, name, ns, timeout_sec, mode)
    else:
        raise Exception(f"unknown kind for wait_each: {kind}")

def wait_pod(ctx: str, name: str, ns: str, timeout_sec: int, mode: str = "ready") -> int:
    """
    Wait for a single pod. Returns 1 if pod reached the desired state, 0 otherwise.
    """
    deadline = time.time() + timeout_sec
    while time.time() < deadline:
        r = run(["kubectl","--context",ctx,"-n",ns,"get","pod",name,"-o","json"],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r.returncode != 0:
            time.sleep(0.5); continue
        try:
            pod = json.loads(r.stdout)
            phase = (pod.get("status") or {}).get("phase") or ""
            conditions = pod.get("status", {}).get("conditions", [])
            ready = any(c.get("type")=="Ready" and c.get("status")=="True" for c in conditions)
            if mode == "running" and phase == "Running":
                return 1
            if mode == "ready" and ready:
                return 1
        except Exception:
            pass
        time.sleep(0.5)
    return 0

def wait_rs_pods(ctx: str, rs_name: str, ns: str, timeout_sec: int, mode: str = "ready") -> int:
    """
    Wait for all pods in a ReplicaSet. Returns the number of pods that reached the condition.
    """
    deadline = time.time() + timeout_sec
    last_count = 0
    while time.time() < deadline:
        r = run(["kubectl","--context",ctx,"-n",ns,"get","pods","-l",f"app={rs_name}","-o","json"],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r.returncode != 0:
            time.sleep(0.5); continue
        try:
            podlist = json.loads(r.stdout).get("items", [])
            count = 0
            for p in podlist:
                phase = (p.get("status") or {}).get("phase") or ""
                conditions = p.get("status", {}).get("conditions", [])
                ready = any(c.get("type")=="Ready" and c.get("status")=="True" for c in conditions)
                if mode == "running" and phase == "Running":
                    count += 1
                if mode == "ready" and ready:
                    count += 1
            last_count = count
            # If all pods satisfied the condition, we can exit early
            desired = int((p.get("spec") or {}).get("replicas") or 0) if podlist else None
            if desired is None or count >= len(podlist):
                return count
        except Exception:
            pass
        time.sleep(0.5)
    return last_count


def wait_until_settled_or_unschedulable_events(
    ctx: str,
    ns: str,
    expected: int,
    interval: float = 0.5,
    settle_timeout_sec: int | None = None
) -> tuple[str, dict]:
    """
    Poll Pod events until we can decide:
      - ("all_scheduled", {"scheduled": N})
      - ("some_unschedulable", {"unschedulable": [...]})
      - ("timeout", {"scheduled": N, "unschedulable": [...]})
    Decision is based on the *latest* event per pod name.
    """
    start = time.time()
    while True:
        scheduled, states = count_scheduled_from_events(ctx, ns)
        failed_like = sum(1 for s in states.values() if s in {"failed_scheduling", "preempted"})

        # all_scheduled
        if scheduled >= expected:
            return "all_scheduled", {"scheduled": scheduled}

        # some_unschedulable (we've 'decided' about at least expected pods: scheduled + failed_like)
        if (scheduled + failed_like) >= expected and failed_like > 0:
            unschedulable = [p for p, st in states.items() if st in {"failed_scheduling", "preempted"}]
            return "some_unschedulable", {"unschedulable": sorted(unschedulable)}

        # timeout?
        if settle_timeout_sec is not None and (time.time() - start) >= settle_timeout_sec:
            unschedulable = [p for p, st in states.items() if st in {"failed_scheduling", "preempted"}]
            return "timeout", {"states": states}

        time.sleep(interval)

# -------------- environment management --------------
def ensure_namespace(ctx: str, ns: str, *, recreate: bool = False) -> None:
    """
    Ensure the namespace exists in the given context.
    If recreate=True, delete it first, then (re)create if missing.
    """
    if recreate:
        delete_namespace(ctx, ns)
    rns = run(["kubectl","--context",ctx,"get","ns",ns],
              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if rns.returncode != 0:
        run(["kubectl","--context",ctx,"create","ns",ns], check=True)

def delete_namespace(ctx: str, ns: str):
    cmd = ["kubectl", "--context", ctx, "delete", "namespace", ns, "--ignore-not-found"]
    print(f"[kwok-fill] Deleting namespace '{ns}'...")
    try:
        subprocess.run(cmd, check=True)
        print(f"[kwok-fill] Namespace '{ns}' deleted (ctx={ctx})")
    except:
        print(f"[kwok-fill] Error deleting namespace '{ns}' in ctx={ctx}")

def create_kwok_nodes(ctx: str, num_nodes: int, node_cpu: str, node_mem: str, pods_cap: int) -> None:
    """
    Create KWOK nodes kwok-node-1..kwok-node-N with the given capacity.
    """
    node_yaml = "".join(
        yaml_kwok_node(f"kwok-node-{i}", node_cpu, node_mem, pods_cap)
        for i in range(1, num_nodes + 1)
    )
    if node_yaml:
        apply_yaml(ctx, node_yaml)

def ensure_priority_classes(ctx: str, num_priorities: int, *, prefix: str = "p", start: int = 1) -> None:
    """
    Apply N PriorityClasses named {prefix}{i} for i in [start, start+N).
    """
    pcs_yaml = "".join(
        yaml_priority_class(f"{prefix}{v}", v)
        for v in range(start, start + num_priorities)
    )
    if pcs_yaml:
        apply_yaml(ctx, pcs_yaml)

def cleanup_priority_classes(ctx: str, desired_count: int, *, prefix: str = "p", start: int = 1) -> None:
    """
    Delete PriorityClasses created by us (name startswith prefix) that are not in the desired set.
    Never touches system/global defaults.
    """
    r = run(["kubectl","--context",ctx,"get","priorityclasses.scheduling.k8s.io","-o","json"],
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    if r.returncode != 0:
        return

    system_names = {"system-cluster-critical", "system-node-critical"}
    desired = {f"{prefix}{i}" for i in range(start, start + desired_count)}

    pcs = json.loads(r.stdout).get("items", [])
    for pc in pcs:
        name = (pc.get("metadata") or {}).get("name", "")
        global_default = bool(pc.get("globalDefault"))
        if not name:
            continue
        # Only delete our own prefixed PCs, never system/global defaults, and only if not desired
        if name.startswith(prefix) and name not in desired and name not in system_names and not global_default:
            run(["kubectl","--context",ctx,"delete","priorityclass.scheduling.k8s.io", name],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

def delete_kwok_nodes(ctx: str):
    r = run(["kubectl","--context",ctx,"get","nodes","-l","type=kwok","-o","json"],
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    if r.returncode != 0:
        return
    items = (json.loads(r.stdout).get("items") or [])
    for n in items:
        name = (n.get("metadata") or {}).get("name")
        if name:
            run(["kubectl","--context",ctx,"delete","node",name,"--ignore-not-found"],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

# ----------- yaml builders ----------
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

def yaml_kwok_rs(ns: str, rs_name: str, replicas: int, qcpu: str, qmem: str, pc: str) -> str:
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
              requests: {{cpu: {qcpu}, memory: {qmem}}}
              limits:   {{cpu: {qcpu}, memory: {qmem}}}
    ---
    """)

def yaml_kwok_pod(ns: str, name: str, qcpu: str, qmem: str, pc: str) -> str:
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
          requests: {{cpu: {qcpu}, memory: {qmem}}}
          limits:   {{cpu: {qcpu}, memory: {qmem}}}
    ---
    """)

# ---------- targets & parsing / interval helpers ----

def format_interval_cpu(tup: tuple[int, int]) -> str:
    return f"{m_to_cpu_str(tup[0])},{m_to_cpu_str(tup[1])}"

def format_interval_mem(tup: tuple[int, int]) -> str:
    return f"{mi_to_mem_str(tup[0])},{mi_to_mem_str(tup[1])}"

def parse_timeout_s(t:str) -> int:
    try:
        if t.endswith("ms"): return max(1, int(int(t[:-2]) / 1000))
        if t.endswith("s"):  return int(t[:-1])
        if t.endswith("m"):  return int(t[:-1]) * 60
        if t.endswith("h"):  return int(t[:-1]) * 3600
        return int(t)
    except Exception:
        return 60

def parse_cpu_interval(s: Optional[str]) -> Optional[Tuple[int,int]]:
    if not s: return None
    lo, hi = [x.strip() for x in s.split(",", 1)]
    return cpu_to_m(lo), cpu_to_m(hi)

def parse_mem_interval(s: Optional[str]) -> Optional[Tuple[int,int]]:
    if not s: return None
    lo, hi = [x.strip() for x in s.split(",", 1)]
    return mem_to_mi(lo), mem_to_mi(hi)

def feasible_range(interval: Tuple[int,int], k:int) -> Tuple[int,int]:
    lo, hi = interval
    return lo * k, hi * k

def auto_interval_from_target(total: int, k: int, spread: Optional[float] = None) -> tuple[int,int]:
    """
    If spread>0: non-degenerate interval around average, clamped to >=1 and hi>lo.
    Else: tight reachable interval (avg .. avg or floor/ceil avg).
    """
    if k <= 0:
        return (max(1, total), max(1, total))
    avg = total / k
    if spread is not None and spread > 0:
        lo = max(1, int(avg * (1.0 - spread)))
        hi = max(lo + 1, int(avg * (1.0 + spread)))
        return (lo, hi)
    base = total // k
    rem  = total - base * k
    return (max(1, base), max(1, base + (1 if rem else 0)))

def resolve_interval_or_fallback(interval: Optional[Tuple[int,int]],
                                k:int, total:int,
                                label:str, scope:str,
                                mode:str = "auto",
                                spread: Optional[float] = None) -> tuple[Optional[Tuple[int,int]], bool]:
    """
    - Return (interval_used, fell_back)
    - If infeasible and mode=='auto' -> use auto_interval_from_target(total,k,spread)
    """
    if interval is None:
        return None, False
    lo_sum, hi_sum = feasible_range(interval, k)
    if lo_sum <= total <= hi_sum:
        return interval, False
    msg = (f"[kwok-fill][ERROR] {label} interval {interval[0]}-{interval[1]} over {k} pods "
        f"cannot reach target {total} at {scope} scope (range {lo_sum}..{hi_sum}).")
    if mode == "error":
        print(msg, file=sys.stderr); sys.exit(2)
    auto_int = auto_interval_from_target(total, k, spread=spread)
    print(msg.replace("[ERROR]", "[WARN]") +
        f"\n[kwok-fill][WARN] Falling back to auto-derived interval {auto_int} ({label}/pod).")
    return auto_int, True

def check_feasible_interval(interval: Tuple[int,int] | None, k: int, tgt: int, label: str, scope: str):
    if not interval: return
    lo, hi = interval
    lo_sum, hi_sum = lo*k, hi*k
    if not (lo_sum <= tgt <= hi_sum):
        print(f"[kwok-fill][ERROR] {label} interval {lo}-{hi} over {k} pods "
                f"cannot reach target {tgt} at {scope} scope (range {lo_sum}..{hi_sum}).", file=sys.stderr)
        sys.exit(2)

# ----------- partitioning & distributions -----------
def split_even(total:int, n:int) -> list[int]:
    if n <= 0: return []
    base, rem = divmod(total, n)
    return [base + (1 if i < rem else 0) for i in range(n)]

def partition_int(total:int, k:int, min_each:int, rng:random.Random, variance:int) -> list[int]:
    if k <= 0: return []
    rem = max(0, total - k*min_each)
    if rem == 0: return [min_each]*k
    weights = [rng.randrange(1, max(2, variance+1)) for _ in range(k)]
    sumw = sum(weights)
    parts, allocated = [], 0
    for i in range(k-1):
        share = rem * weights[i] // sumw
        parts.append(min_each + share); allocated += share
    parts.append(min_each + rem - allocated)
    return parts

def pick_dist(total:int, n:int, mode:str, rng:random.Random, variance:int) -> list[int]:
    return split_even(total, n) if mode == "even" else partition_int(total, n, 1, rng, variance)

def scale_bounded_to_sum(vals: list[int], target: int, lo: int, hi: int) -> list[int]:
    n = len(vals)
    if n == 0: return []
    lo_f, hi_f = float(lo), float(hi)
    v = [min(hi_f, max(lo_f, float(x))) for x in vals]
    fixed = set(i for i,x in enumerate(v) if x <= lo_f+1e-9 or x >= hi_f-1e-9)
    remaining = set(range(n)) - fixed

    def cur_sum(idx): return sum(v[i] for i in idx)

    for _ in range(10 + n):
        if not remaining: break
        other_sum = sum(v[i] for i in set(range(n)) - remaining)
        rem_tgt = float(target) - other_sum
        S = cur_sum(remaining)
        if abs(S - rem_tgt) < 1e-6: break
        scale = (rem_tgt / S) if S != 0 else 1.0
        changed = False
        for i in list(remaining):
            nv = v[i] * scale
            if nv < lo_f: v[i] = lo_f; remaining.remove(i); changed = True
            elif nv > hi_f: v[i] = hi_f; remaining.remove(i); changed = True
            else: v[i] = nv
        if not changed and abs(sum(v) - float(target)) < 1e-4:
            break

    rounded = [int(round(x)) for x in v]
    diff = target - sum(rounded)
    if diff != 0:
        sign = 1 if diff > 0 else -1
        for _ in range(abs(diff)):
            for i in range(n):
                if sign > 0 and rounded[i] < hi: rounded[i] += 1; break
                if sign < 0 and rounded[i] > lo: rounded[i] -= 1; break
    return [min(hi, max(lo, x)) for x in rounded]

def gen_parts_constrained(total: int, n: int, rng: random.Random,
                            interval: Optional[Tuple[int,int]],
                            fallback_dist: str, variance: int) -> list[int]:
    if n <= 0: return []
    if not interval:
        return pick_dist(total, n, fallback_dist, rng, variance)
    lo, hi = interval
    raw = [rng.randint(lo, hi) for _ in range(n)]
    return scale_bounded_to_sum(raw, total, lo, hi)