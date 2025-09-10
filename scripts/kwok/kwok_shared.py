#!/usr/bin/env python3

# kwok_shared.py

import os, time, random, subprocess, json, textwrap, tempfile, csv
from typing import List, Dict, Tuple, Optional, Any
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
def run(cmd:list[str], **kw) -> subprocess.CompletedProcess:
    """
    Run a command as a subprocess and return the completed process.
    A subprocess is a child process that is launched by another process (the parent, i.e. the current Python process).
    """
    return subprocess.run(cmd, **kw)

def apply_yaml(ctx:str, yaml_text:str) -> subprocess.CompletedProcess:
    """
    Apply a YAML configuration to the cluster.
    """
    return run(["kubectl","--context",ctx,"apply","-f","-"], input=yaml_text.encode(), check=True)

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

def scale_replicaset(ctx: str, ns: str, rs_name: str, replicas: int) -> None:
    """Scale a ReplicaSet to a desired replica count."""
    run([
        "kubectl", "--context", ctx, "-n", ns,
        "scale", "replicaset", rs_name, f"--replicas={replicas}"
    ], check=True)

def get_rs_spec_replicas(ctx: str, ns: str, rs_name: str) -> Optional[int]:
    """Fetch .spec.replicas for a ReplicaSet, or None if not found."""
    r = run(["kubectl","--context",ctx,"-n",ns,"get","replicaset",rs_name,"-o","json"],
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    if r.returncode != 0:
        return None
    try:
        return int((json.loads(r.stdout).get("spec") or {}).get("replicas") or 0)
    except Exception:
        return None

# -------------- cluster management --------------
def ensure_namespace(ctx: str, ns: str, *, recreate: bool = False, retries: int = 15, delay: float = 0.5) -> None:
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
    
    # Ensure the default serviceaccount exists in the namespace; before we exit
    for _ in range(1, retries + 1):
        r = run(["kubectl","--context",ctx,"-n",ns,"get","sa","default"],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if r.returncode == 0:
            print(f"[kwok-fill] Found default serviceaccount in ns '{ns}'")
            break
        time.sleep(delay)

def delete_namespace(ctx: str, ns: str) -> None:
    """
    Delete the specified namespace in the given context.
    """
    cmd = ["kubectl", "--context", ctx, "delete", "namespace", ns, "--ignore-not-found"]
    print(f"[kwok-fill] Deleting namespace '{ns}'...")
    try:
        subprocess.run(
            cmd,
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.STDOUT
        )
        print(f"[kwok-fill] Namespace '{ns}' deleted (ctx={ctx})")
    except:
        print(f"[kwok-fill] Error deleting namespace '{ns}' in ctx={ctx}")

import time

def ensure_priority_classes(
    ctx: str,
    num_priorities: int,
    *,
    prefix: str = "p",
    start: int = 1,
    delete_extras: bool = False,
) -> None:
    """
    Ensure PriorityClasses {prefix}{i} for i in [start, start+num_priorities).
    If delete_extras=True, remove any of our prefixed PCs outside that set
    (never touches system/global-default classes).
    """
    pcs_yaml = "".join(
        yaml_priority_class(f"{prefix}{v}", v)
        for v in range(start, start + num_priorities)
    )
    if pcs_yaml:
        apply_yaml(ctx, pcs_yaml)

    if delete_extras:
        cleanup_priority_classes(ctx, desired_count=num_priorities, prefix=prefix, start=start)

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

# ---------- KWOK management --------------
def prep_kwokctl_config_file(src: Path) -> tuple[Path, Path | None]:
    """
    If `src` is a multi-doc YAML, extract the document with
    kind: KwokctlConfiguration into a temp file and return it.
    Returns (path_to_pass_to_kwokctl, tmp_file_to_cleanup_or_None).
    Falls back to (src, None) if PyYAML not present or doc not found.
    """
    try:
        import yaml  # local import to keep optional
    except Exception:
        return src, None

    try:
        with open(src, "r", encoding="utf-8") as f:
            docs = list(yaml.safe_load_all(f))
    except Exception:
        return src, None

    chosen = None
    for d in docs:
        if isinstance(d, dict) and d.get("kind") == "KwokctlConfiguration":
            # optionally also check apiVersion prefix
            apiver = str(d.get("apiVersion", ""))
            if not apiver.startswith("config.kwok.x-k8s.io/"):
                continue
            chosen = d
            break
    if chosen is None:
        return src, None

    # write chosen doc to a temp file
    tf = tempfile.NamedTemporaryFile("w", delete=False, suffix=".yaml")
    try:
        yaml.safe_dump(chosen, tf, sort_keys=False)
    finally:
        tf.close()
    return Path(tf.name), Path(tf.name)

def ensure_kwok_cluster(name_or_ctx: str, kwok_config: str | Path | None, recreate: bool = True) -> None:
    cluster_name = name_or_ctx[len("kwok-"):] if name_or_ctx.startswith("kwok-") else name_or_ctx
    cfg_path = Path(kwok_config) if kwok_config else None

    if recreate:
        print(f"[setup] recreating kwok cluster '{cluster_name}'")
        subprocess.run(["kwokctl", "delete", "cluster", "--name", cluster_name], check=False)

    if cfg_path and cfg_path.exists():
        cfg_for_kwokctl, tmp_to_cleanup = prep_kwokctl_config_file(cfg_path)
        try:
            print(f"[setup] kwokctl create cluster --name {cluster_name} --config {cfg_path}")
            subprocess.run(
                ["kwokctl", "create", "cluster", "--name", cluster_name, "--config", str(cfg_for_kwokctl)],
                check=True
            )
        finally:
            if tmp_to_cleanup and tmp_to_cleanup.exists():
                try: os.unlink(tmp_to_cleanup)
                except OSError: pass
    else:
        if cfg_path and not cfg_path.exists():
            print(f"[setup][warn] KWOK_CONFIG='{cfg_path}' not found; creating with defaults")
        print(f"[setup] kwokctl create cluster --name {cluster_name}")
        subprocess.run(["kwokctl", "create", "cluster", "--name", cluster_name], check=True)

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

def delete_kwok_nodes(ctx: str) -> None:
    """
    Delete all KWOK nodes in the given context.
    """
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

# ---------- stats helpers ----------


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

# ---------- kubectl waiters / monitors --------
def wait_each(ctx: str, kind: str, name: str, ns: str, timeout_sec: int, mode: str) -> int:
    """
    Wait for a specific resource to reach a desired state.
    """
    if kind == "pod":
        return wait_pod(ctx, name, ns, timeout_sec, mode)
    elif kind in ("replicaset", "rs"):
        return wait_rs_pods(ctx, name, ns, timeout_sec, mode)
    else:
        raise Exception(f"unknown kind for wait_each: {kind}")

def wait_pod(ctx: str, name: str, ns: str, timeout_sec: int, mode: str = "ready") -> int:
    deadline = time.time() + timeout_sec
    while time.time() < deadline:
        r = run(["kubectl","--context",ctx,"-n",ns,"get","pod",name,"-o","json"],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)

        if mode == "exist":
            if r.returncode == 0:
                return 1
            time.sleep(0.5)
            continue

        if r.returncode != 0:
            time.sleep(0.5); continue

        try:
            pod = json.loads(r.stdout)
            phase = (pod.get("status") or {}).get("phase") or ""
            conditions = pod.get("status", {}).get("conditions", []) or []
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
    Wait until the RS has its desired number of pods meeting the condition.
    mode:
      - "exist": counts pods by presence (len of pod list)
      - "ready": counts Ready=True
      - "running": counts phase == Running
    Returns the number of pods that met the condition (possibly less than desired on timeout).
    """
    deadline = time.time() + timeout_sec
    last_count = 0

    while time.time() < deadline:
        desired = get_rs_spec_replicas(ctx, ns, rs_name)

        r = run(["kubectl","--context",ctx,"-n",ns,"get","pods","-l",f"app={rs_name}","-o","json"],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r.returncode != 0:
            time.sleep(0.5); continue

        try:
            podlist = json.loads(r.stdout).get("items", [])
            if mode == "exist":
                count = len(podlist)
            else:
                count = 0
                for p in podlist:
                    phase = (p.get("status") or {}).get("phase") or ""
                    conditions = p.get("status", {}).get("conditions", []) or []
                    ready = any(c.get("type") == "Ready" and c.get("status") == "True" for c in conditions)
                    if mode == "running" and phase == "Running":
                        count += 1
                    elif mode == "ready" and ready:
                        count += 1

            last_count = count
            if desired is not None and count >= desired:
                return count
        except Exception:
            pass

        time.sleep(0.5)

    return last_count

def get_timestamp() -> str:
    return time.strftime("%Y/%m/%d/%H:%M:%S", time.localtime())

def coerce_int(doc: Dict[str, Any], key: str, default: int) -> int:
    v = doc.get(key, default)
    try:
        return int(v)
    except Exception:
        return default

def coerce_float(doc: Dict[str, Any], key: str, default: float) -> float:
    v = doc.get(key, default)
    try:
        return float(v)
    except Exception:
        return default

def coerce_str(doc: Dict[str, Any], key: str, default: str) -> str:
    v = doc.get(key, default)
    s = str(v).strip()
    return s if s != "" else default

def coerce_wait_mode(doc: Dict[str, Any], key: str, default: Optional[str]) -> Optional[str]:
    v = doc.get(key, default)
    if v is None:
        return None
    s = str(v).strip()
    if s.lower() in ("", "none"):
        return None
    if s not in ("exist", "ready", "running"):
        raise ValueError(f"Invalid wait_mode: {s}")
    return s

def normalize_interval(
    doc: Dict[str, Any],
    key_combo: Tuple[str, str, str],
    *,  # supports: single, lo/hi pair, list, or dict
    allow_none: bool = True,
) -> Optional[str]:
    """
    Normalize an interval to the canonical "lo,hi" string.
    Accepts:
        - "cpu_interval": "100m,1000m"
        - "cpu_interval": ["100m","1000m"]
        - "cpu_interval": {"lo":"100m","hi":"1000m"}
        - "cpu_interval_lo": "100m" + "cpu_interval_hi": "1000m"
    """
    single, lo_key, hi_key = key_combo

    if single in doc and doc[single] is not None:
        v = doc[single]
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

def count_running_by_priority(ctx: str, ns: str) -> Dict[str, int]:
    out: Dict[str, int] = {}
    pods = get_json_ctx(ctx, ["-n", ns, "get", "pods", "-o", "json"]).get("items", [])
    for p in pods:
        if (p.get("status") or {}).get("phase", "") != "Running":
            continue
        pc = (p.get("spec") or {}).get("priorityClassName") or ""
        out[pc] = out.get(pc, 0) + 1
    return out

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


# ----------- KWOK yaml builders ----------
def yaml_priority_class(name: str, value: int) -> str:
    """
    Generate a YAML manifest for a KWOK PriorityClass.
    """
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
    """
    Generate a YAML manifest for a KWOK Node.
    """
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
    """
    Generate a YAML manifest for a KWOK ReplicaSet.
    Replicas will be 0 initially; we scale incrementally after creation. This is to ensure same cluster state at the same seed.
    """
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
    """
    Generate a YAML manifest for a KWOK Pod.
    """
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
    """
    Format a CPU interval for YAML output.
    """
    return f"{cpu_m_int_to_str(tup[0])},{cpu_m_int_to_str(tup[1])}"

def format_interval_mem(tup: tuple[int, int]) -> str:
    """
    Format a memory interval for YAML output.
    """
    return f"{mem_mi_int_to_str(tup[0])},{mem_mi_int_to_str(tup[1])}"

def parse_cpu_interval(s: Optional[str]) -> Optional[Tuple[int,int]]:
    """
    Parse a CPU interval string into a tuple of (min, max) values.
    """
    if not s: return None
    lo, hi = [x.strip() for x in s.split(",", 1)]
    return cpu_m_str_to_int(lo), cpu_m_str_to_int(hi)

def parse_mem_interval(s: Optional[str]) -> Optional[Tuple[int,int]]:
    """
    Parse a memory interval string into a tuple of (min, max) values.
    """
    if not s: return None
    lo, hi = [x.strip() for x in s.split(",", 1)]
    return mem_str_to_mib_int(lo), mem_str_to_mib_int(hi)

def feasible_range(interval: Tuple[int,int], k:int) -> Tuple[int,int]:
    """
    Get the feasible range for a given interval and number of pods.
    """
    lo, hi = interval
    return lo * k, hi * k

def resolve_interval_or_fallback(interval: Optional[Tuple[int,int]], k:int, total:int, spread: Optional[float] = None) -> tuple[Optional[Tuple[int,int]], bool]:
    """
    Resolve an interval by checking its feasibility against the target.
    If not feasible and mode=='auto' -> use auto_interval_from_target(total,k,spread)
    Return true if auto-derived interval is used.
    """
    if interval is None: # No interval specified
        return None, False
    lo_sum, hi_sum = feasible_range(interval, k)
    if lo_sum <= total <= hi_sum: # Interval is feasible
        return interval, False

    # Interval is not feasible; use auto-derived interval
    if k <= 0:
        lo = max(1, total)
        hi = max(1, total)
        return (lo, hi), True
    avg = total / k
    if spread is not None and spread > 0:
        lo = max(1, int(avg * (1.0 - spread)))
        hi = max(lo + 1, int(avg * (1.0 + spread)))
        return (lo, hi), True
    base = total // k
    rem  = total - base * k
    lo = max(1, base)
    hi = max(1, base + (1 if rem else 0))
    return (lo, hi), True

# ----------- partitioning & distributions -----------
def split_even(total:int, n:int) -> list[int]:
    """
    Split a total into n nearly equal parts.
    Used for distributing resources.
    """
    if n <= 0: return []
    base, rem = divmod(total, n)
    return [base + (1 if i < rem else 0) for i in range(n)]

def partition_int(total:int, k:int, min_each:int, rng:random.Random, variance:int) -> list[int]:
    """
    Partition an integer into k parts with a minimum value for each part.
    Used for distributing resources.
    """
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
    """
    Pick a distribution strategy based on the mode.
    Mode "even": Distribute resources evenly across all parts.
    Mode "partition": Partition resources based on weights.
    """
    return split_even(total, n) if mode == "even" else partition_int(total, n, 1, rng, variance)

def scale_bounded_to_sum(vals: list[int], target: int, lo: int, hi: int) -> list[int]:
    """
    Scale a list of values to sum up to a target while respecting bounds.
    """
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
    """
    Generate constrained partitions for a resource distribution.
    """
    if n <= 0: return []
    if not interval:
        return pick_dist(total, n, fallback_dist, rng, variance)
    lo, hi = interval
    raw = [rng.randint(lo, hi) for _ in range(n)]
    return scale_bounded_to_sum(raw, total, lo, hi)

# --------- Other utilities -----------

def env_default(name: str, fallback: str) -> str:
    return os.environ.get(name, fallback)

def env_bool(name: str, fallback: bool = False) -> bool:
    v = os.environ.get(name)
    if v is None:
        return fallback
    return v.strip().lower() in ("1", "true", "yes", "on")

def parse_int_list(s: str) -> list[int]:
    if not s:
        return []
    parts = [p for chunk in s.split(",") for p in chunk.strip().split()]
    return [int(x) for x in parts if x.strip()]

def parse_float_list(s: str) -> list[float]:
    if not s:
        return []
    parts = [p for chunk in s.split(",") for p in chunk.strip().split()]
    return [float(x) for x in parts if x.strip()]

def parse_timeout_s(t:str) -> int:
    """
    Parse a timeout string into seconds.
    """
    try:
        if t.endswith("ms"): return max(1, int(int(t[:-2]) / 1000))
        if t.endswith("s"):  return int(t[:-1])
        if t.endswith("m"):  return int(t[:-1]) * 60
        if t.endswith("h"):  return int(t[:-1]) * 3600
        return int(t)
    except Exception:
        return 60

def ensure_dir(p) -> None:
    Path(p).mkdir(parents=True, exist_ok=True)