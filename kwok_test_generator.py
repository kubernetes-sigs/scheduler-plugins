#!/usr/bin/env python3

# kwok_test_generator.py

import sys, argparse, random, time, json, os, subprocess, textwrap
from typing import Tuple, Dict, List, Optional
from dataclasses import dataclass

from kwok_shared import (
    run, apply_yaml,
    cpu_to_m, mem_to_mi, m_to_cpu_str, mi_to_mem_str,
)

@dataclass
class Targets:
    node_mc: int
    node_mi: int
    total_pods: int
    target_mc_node: int
    target_mi_node: int
    target_mc_cluster: int
    target_mi_cluster: int


def get_unschedulable_pods(ctx: str, ns: str) -> List[str]:
    """
    Returns pod names where:
      - spec.nodeName is empty (not scheduled), and
      - status.conditions has type=PodScheduled, status=False, reason=Unschedulable
    """
    r = run(["kubectl","--context",ctx,"-n",ns,"get","pods","-o","json"],
            stdout=subprocess.PIPE, check=True)
    items = json.loads(r.stdout).get("items", [])
    out: List[str] = []
    for p in items:
        name = (p.get("metadata") or {}).get("name", "")
        spec = p.get("spec") or {}
        status = p.get("status") or {}
        if not name:
            continue
        if spec.get("nodeName"):  # already scheduled
            continue
        for cond in status.get("conditions") or []:
            if (cond.get("type") == "PodScheduled" and
                str(cond.get("status")).lower() == "false" and
                cond.get("reason") == "Unschedulable"):
                out.append(name)
                break
    return out


def monitor_until_all_scheduled_or_unschedulable(ctx: str, ns: str, expected: int,
                                                 interval: float = 0.5,
                                                 max_iters: Optional[int] = None):
    """
    Wait until every pod is either scheduled or explicitly marked Unschedulable.
    Returns:
      - ("all_scheduled", {"scheduled": N}) if all got nodes
      - ("unschedulable", {"unschedulable": [...]}) if any are unschedulable
    """
    it = 0
    while True:
        scheduled = count_scheduled_pods_in_ns(ctx, ns)
        unsched = get_unschedulable_pods(ctx, ns)
        total_decided = scheduled + len(unsched)

        if scheduled >= expected:
            return "all_scheduled", {"scheduled": scheduled}

        if total_decided >= expected and unsched:
            # all pods accounted for, some failed
            return "unschedulable", {"unschedulable": sorted(unsched)}

        it += 1
        if max_iters is not None and it >= max_iters:
            return "inconclusive", {}

        time.sleep(interval)


def parse_timeout_to_seconds(t:str) -> int:
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

def bounded_scale_to_sum(vals: list[int], target: int, lo: int, hi: int) -> list[int]:
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

def gen_parts_with_intervals(total: int, n: int, rng: random.Random,
                            interval: Optional[Tuple[int,int]],
                            fallback_dist: str, variance: int) -> list[int]:
    if n <= 0: return []
    if not interval:
        return pick_dist(total, n, fallback_dist, rng, variance)
    lo, hi = interval
    raw = [rng.randint(lo, hi) for _ in range(n)]
    return bounded_scale_to_sum(raw, total, lo, hi)

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

def ensure_interval_or_fallback(interval: Optional[Tuple[int,int]],
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

def check_feasible(interval: Tuple[int,int] | None, k: int, tgt: int, label: str, scope: str):
    if not interval: return
    lo, hi = interval
    lo_sum, hi_sum = lo*k, hi*k
    if not (lo_sum <= tgt <= hi_sum):
        print(f"[kwok-fill][ERROR] {label} interval {lo}-{hi} over {k} pods "
                f"cannot reach target {tgt} at {scope} scope (range {lo_sum}..{hi_sum}).", file=sys.stderr)
        sys.exit(2)
    
def exists(ctx:str, kind:str, name:str, ns:str) -> bool:
    r = run(["kubectl","--context",ctx,"-n",ns,"get",kind,name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    return r.returncode == 0

def wait_pods_exist(ctx:str, kind:str, name:str, ns:str, timeout_sec:int):
    deadline = time.time() + timeout_sec
    while time.time() < deadline:
        if exists(ctx, kind, name, ns): return True
        time.sleep(0.5)
    return False

def wait_pods_ready(ctx:str, kind:str, name:str, ns:str, timeout:str) -> bool:
    if kind.lower() in ("pod","pods"):
        cmd = ["kubectl","--context",ctx,"-n",ns,"wait",f"pod/{name}","--for=condition=Ready","--timeout",timeout]
    elif kind.lower() in ("replicaset","replicasets","rs"):
        cmd = ["kubectl","--context",ctx,"-n",ns,"wait",f"replicaset/{name}","--for=condition=Available","--timeout",timeout]
    else:
        return wait_pods_exist(ctx, kind, name, ns, parse_timeout_to_seconds(timeout))
    r = run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if r.returncode == 0: return True
    return wait_pods_exist(ctx, kind, name, ns, parse_timeout_to_seconds(timeout))

def wait_pod_running(ctx: str, name: str, ns: str, timeout_sec: int) -> bool:
    deadline = time.time() + timeout_sec
    while time.time() < deadline:
        r = run(["kubectl","--context",ctx,"-n",ns,"get","pod",name,"-o","json"],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r.returncode == 0:
            try:
                pod = json.loads(r.stdout)
                node = (pod.get("spec") or {}).get("nodeName") or ""
                phase = (pod.get("status") or {}).get("phase") or ""
                if node and phase == "Running": return True
            except Exception:
                pass
        time.sleep(0.5)
    return False

def wait_rs_pods_running(ctx: str, rs_name: str, ns: str, timeout_sec: int) -> bool:
    deadline = time.time() + timeout_sec
    while time.time() < deadline:
        r_rs = run(["kubectl","--context",ctx,"-n",ns,"get","rs",rs_name,"-o","json"],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r_rs.returncode != 0:
            time.sleep(0.5); continue
        try:
            rs = json.loads(r_rs.stdout)
            desired = int((rs.get("spec") or {}).get("replicas") or 0)
        except Exception:
            time.sleep(0.5); continue
        if desired == 0: return True

        r_pods = run(["kubectl","--context",ctx,"-n",ns,"get","pods","-l",f"app={rs_name}","-o","json"],
                    stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r_pods.returncode != 0:
            time.sleep(0.5); continue
        try:
            podlist = json.loads(r_pods.stdout).get("items", [])
            running = 0
            for p in podlist:
                node = (p.get("spec") or {}).get("nodeName") or ""
                phase = (p.get("status") or {}).get("phase") or ""
                if node and phase == "Running": running += 1
            if running >= desired: return True
        except Exception:
            pass
        time.sleep(0.5)
    return False

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
    print(f"[kwok-fill] Deleting namespace '{ns}' in context '{ctx}' (ignore if not found)...")
    try:
        subprocess.run(cmd, check=True)
        print(f"[kwok-fill] Namespace '{ns}' deleted (ctx={ctx})")
    except:
        print(f"[kwok-fill] Error deleting namespace '{ns}' in ctx={ctx}")


def apply_priority_classes(ctx: str, num_priorities: int, *, prefix: str = "p", start: int = 1) -> None:
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

def cleanup_nodes(ctx: str, desired_count: int):
    r = run(["kubectl","--context",ctx,"get","nodes","-l","type=kwok","-o","json"],
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    if r.returncode != 0: return
    nodes = [i["metadata"]["name"] for i in (json.loads(r.stdout).get("items") or [])]
    desired = {f"kwok-node-{i}" for i in range(1, desired_count+1)}
    for n in nodes:
        if n not in desired:
            run(["kubectl","--context",ctx,"delete","node",n], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

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
        pods: "{pods_cap}"
      allocatable:
        cpu: "{cpu}"
        memory: "{mem}"
        pods: "{pods_cap}"
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

def yaml_kwok_pod(ns: str, name: str, node: Optional[str], qcpu: str, qmem: str, pc: str, add_node: bool = False) -> str:
    node_selector = f"""
          nodeSelector:
            kubernetes.io/hostname: "{node}"
    """ if add_node and node else ""
    return textwrap.dedent(f"""\
    apiVersion: v1
    kind: Pod
    metadata:
      name: {name}
      namespace: {ns}
    spec:
      restartPolicy: Always
      priorityClassName: {pc}{node_selector}
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

def ns_usage(ctx: str, ns: str) -> Tuple[int,int,int,int]:
    nodes_json = json.loads(run(
        ["kubectl","--context",ctx,"get","nodes","-o","json"],
        stdout=subprocess.PIPE, check=True).stdout)

    alloc_cpu = 0
    alloc_mem_mi = 0
    for n in nodes_json["items"]:
        a = (n.get("status",{}) or {}).get("allocatable",{}) or {}
        alloc_cpu += cpu_to_m(a.get("cpu","0"))
        alloc_mem_mi += mem_to_mi(a.get("memory","0"))

    pods_json = json.loads(run(
        ["kubectl","--context",ctx,"-n",ns,"get","pods","-o","json"],
        stdout=subprocess.PIPE, check=True).stdout)

    req_cpu = 0
    req_mem_mi = 0
    for p in pods_json.get("items", []):
        phase = (p.get("status",{}) or {}).get("phase","")
        if phase != "Running": continue
        spec = p.get("spec",{}) or {}
        for c in spec.get("containers",[]) or []:
            req = (c.get("resources",{}) or {}).get("requests",{}) or {}
            req_cpu += cpu_to_m(req.get("cpu","0"))
            req_mem_mi += mem_to_mi(req.get("memory","0"))
        init_cpu = 0; init_mem = 0
        for c in spec.get("initContainers",[]) or []:
            req = (c.get("resources",{}) or {}).get("requests",{}) or {}
            init_cpu = max(init_cpu, cpu_to_m(req.get("cpu","0")))
            init_mem = max(init_mem, mem_to_mi(req.get("memory","0")))
        req_cpu += init_cpu; req_mem_mi += init_mem

    return alloc_cpu, req_cpu, alloc_mem_mi, req_mem_mi

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

def save_seed_row(path: str, row: dict):
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    with open(path, "a", encoding="utf-8") as f:
        f.write(json.dumps(row, sort_keys=True) + "\n")

def count_scheduled_pods_in_ns(ctx: str, ns: str) -> int:
    r = run(["kubectl","--context",ctx,"-n",ns,"get","pods","-o","json"],
            stdout=subprocess.PIPE, check=True)
    items = json.loads(r.stdout).get("items", [])
    cnt = 0
    for p in items:
        node = (p.get("spec") or {}).get("nodeName") or ""
        if node: cnt += 1
    return cnt

def wait_all_scheduled(ctx: str, ns: str, expected: int, timeout_sec: int, interval: float = 0.5) -> bool:
    deadline = time.time() + timeout_sec
    while time.time() < deadline:
        if count_scheduled_pods_in_ns(ctx, ns) >= expected:
            return True
        time.sleep(interval)
    return False

def prio_for_step_desc(step:int, num_prios:int) -> int:
    if num_prios <= 1: return 1
    return num_prios - ((step - 1) % num_prios)



class KwokFillRunner:
    def __init__(self, args: argparse.Namespace):
        self.args = args
        self.ctx = f"kwok-{args.cluster_name}"
        self.ns = args.namespace
        # seed/rng
        if args.seed is not None:
            self.rng = random.Random(args.seed)
            self.seed_used = args.seed
            print(f"[kwok-fill] Using fixed seed={args.seed}")
        else:
            self.seed_used = int(time.time_ns())
            self.rng = random.Random(self.seed_used)
            print(f"[kwok-fill] Using auto-random seed={self.seed_used}")

        # numbers / targets
        node_mc = cpu_to_m(args.node_cpu)
        node_mi = mem_to_mi(args.node_mem)
        total_pods = args.num_nodes * args.pods_per_node
        target_mc_node = int(node_mc * args.target_util)
        target_mi_node = int(node_mi * args.target_util)
        target_mc_cluster = target_mc_node * args.num_nodes
        target_mi_cluster = target_mi_node * args.num_nodes
        self.T = Targets(node_mc, node_mi, total_pods, target_mc_node, target_mi_node,
                         target_mc_cluster, target_mi_cluster)

        # intervals (parsed)
        self.cpu_int = parse_cpu_interval(args.cpu_interval)
        self.mem_int = parse_mem_interval(args.mem_interval)

        # scope
        self.cluster_scope = (args.num_replicaset > 0) or (not args.add_node_standalone)

    def _ensure_intervals(self):
        if self.cluster_scope:
            self.cpu_int, _ = ensure_interval_or_fallback(
                self.cpu_int, self.T.total_pods, self.T.target_mc_cluster, "CPU(m)", "cluster",
                mode=self.args.on_interval_infeasible, spread=0.9
            )
            self.mem_int, _ = ensure_interval_or_fallback(
                self.mem_int, self.T.total_pods, self.T.target_mi_cluster, "MEM(Mi)", "cluster",
                mode=self.args.on_interval_infeasible, spread=0.9
            )
            check_feasible(self.cpu_int, self.T.total_pods, self.T.target_mc_cluster, "CPU(m)", "cluster")
            check_feasible(self.mem_int, self.T.total_pods, self.T.target_mi_cluster, "MEM(Mi)", "cluster")
        else:
            self.cpu_int, _ = ensure_interval_or_fallback(
                self.cpu_int, self.args.pods_per_node, self.T.target_mc_node, "CPU(m)", "per-node",
                mode=self.args.on_interval_infeasible, spread=0.9
            )
            self.mem_int, _ = ensure_interval_or_fallback(
                self.mem_int, self.args.pods_per_node, self.T.target_mi_node, "MEM(Mi)", "per-node",
                mode=self.args.on_interval_infeasible, spread=0.9
            )
            check_feasible(self.cpu_int, self.args.pods_per_node, self.T.target_mc_node, "CPU(m)", "per-node")
            check_feasible(self.mem_int, self.args.pods_per_node, self.T.target_mi_node, "MEM(Mi)", "per-node")

    def _maybe_wait(self, kind: str, name: str):
        if not self.args.wait_each:
            return
        tsec = parse_timeout_to_seconds(self.args.wait_timeout)
        wm = self.args.wait_mode
        if wm == "running":
            if kind == "pod":
                wait_pod_running(self.ctx, name, self.ns, tsec)
            elif kind in ("replicaset", "rs"):
                wait_rs_pods_running(self.ctx, name, self.ns, tsec)
            else:
                wait_pods_exist(self.ctx, kind, name, self.ns, tsec)
        elif wm == "ready":
            wait_pods_ready(self.ctx, kind, name, self.ns, self.args.wait_timeout)
        else:
            wait_pods_exist(self.ctx, kind, name, self.ns, tsec)

    def _apply_standalone_for_node(self, node: str, parts_cpu: List[int], parts_mem: List[int]) -> int:
        out = []
        for j in range(self.args.pods_per_node):
            prio = self.rng.randint(1, self.args.num_priorities); pc = f"p{prio}"
            name = f"{node}-{j+1}-{pc}"
            out.append(
                yaml_kwok_pod(self.ns, name, node,
                                m_to_cpu_str(max(1, parts_cpu[j])),
                                mi_to_mem_str(max(1, parts_mem[j])),
                                pc, add_node=self.args.add_node_standalone)
            )
        if out: apply_yaml(self.ctx, "".join(out))
        return len(out)


    def _apply_standalone(self) -> int:
        created = 0
        if self.args.add_node_standalone:
            for i in range(1, self.args.num_nodes+1):
                node = f"kwok-node-{i}"
                parts_cpu = gen_parts_with_intervals(self.T.target_mc_node, self.args.pods_per_node,
                                                     self.rng, self.cpu_int, self.args.dist_mode, self.args.variance)
                parts_mem = gen_parts_with_intervals(self.T.target_mi_node, self.args.pods_per_node,
                                                     self.rng, self.mem_int, self.args.dist_mode, self.args.variance)
                created += self.apply_standalone_for_node(node, parts_cpu, parts_mem)
        else:
            parts_cpu = gen_parts_with_intervals(self.T.target_mc_cluster, self.T.total_pods,
                                                 self.rng, self.cpu_int, self.args.dist_mode, self.args.variance)
            parts_mem = gen_parts_with_intervals(self.T.target_mi_cluster, self.T.total_pods,
                                                 self.rng, self.mem_int, self.args.dist_mode, self.args.variance)
            out = []
            for idx in range(self.T.total_pods):
                prio = self.rng.randint(1, self.args.num_priorities); pc = f"p{prio}"
                name = f"fill-{idx+1}-{pc}"
                out.append(
                    yaml_kwok_pod(self.ns, name, None,
                                  m_to_cpu_str(max(1, parts_cpu[idx])),
                                  mi_to_mem_str(max(1, parts_mem[idx])),
                                  pc, add_node=False)
                )
            if out: apply_yaml(self.ctx, "".join(out)); created = len(out)
        return created

    def _apply_rs(self) -> tuple[int, set[int]]:
        rs_replicas = partition_int(self.T.total_pods, self.args.num_replicaset, 1, self.rng, self.args.variance)
        per_pod_cpu = gen_parts_with_intervals(self.T.target_mc_cluster, self.T.total_pods,
                                               self.rng, self.cpu_int, self.args.dist_mode, self.args.variance)
        per_pod_mem = gen_parts_with_intervals(self.T.target_mi_cluster, self.T.total_pods,
                                               self.rng, self.mem_int, self.args.dist_mode, self.args.variance)
        offset = 0
        total_created = 0
        for i, count in enumerate(rs_replicas, start=1):
            prio = prio_for_step_desc(i, self.args.num_priorities); pc = f"p{prio}"
            rsname = f"rs{i}-{pc}"
            slice_cpu = per_pod_cpu[offset:offset+count]
            slice_mem = per_pod_mem[offset:offset+count]
            avg_mc = max(1, sum(slice_cpu)//count)
            avg_mi = max(1, sum(slice_mem)//count)
            if self.cpu_int: avg_mc = min(max(avg_mc, self.cpu_int[0]), self.cpu_int[1])
            if self.mem_int: avg_mi = min(max(avg_mi, self.mem_int[0]), self.mem_int[1])
            y = yaml_kwok_rs(self.ns, rsname, count, m_to_cpu_str(avg_mc), mi_to_mem_str(avg_mi), pc)
            apply_yaml(self.ctx, y)
            self._maybe_wait("replicaset", rsname)
            total_created += count
            offset += count
        return total_created, {len(rs_replicas)}
    
    def setup_cluster(self):
        cleanup_nodes(self.ctx, self.args.num_nodes)
        pods_cap = self.args.pods_per_node * 10
        create_kwok_nodes(self.ctx, self.args.num_nodes, self.args.node_cpu, self.args.node_mem, pods_cap)
        self._ensure_intervals()

    def simulate(self):
        found = 0
        attempts = 0
        print(f"[sim] starting — need {self.args.simulator_instances} unschedulable run(s)")

        while found < self.args.simulator_instances:
            attempts += 1
            use_seed = int(time.time_ns())
            self.rng.seed(use_seed)
            print(f"[sim] attempt {attempts} seed={use_seed}")

            # fresh ns + PCs
            ensure_namespace(self.ctx, self.ns, recreate=True)
            apply_priority_classes(self.ctx, self.args.num_priorities, prefix="p", start=1)

            # apply workload
            _ = self._apply_standalone() if self.args.num_replicaset <= 0 else self._apply_rs()[0]

            # conditions-only monitor
            outcome, info = monitor_until_all_scheduled_or_unschedulable(
                self.ctx, self.ns, expected=self.T.total_pods,
                interval=0.5, max_iters=None  # no timeout unless you want a safety cap
            )

            if outcome == "all_scheduled":
                print("[sim] all pods scheduled — discarding seed and retrying…")
                continue
            if outcome == "inconclusive":
                print("[sim] inconclusive — discarding and retrying…")
                continue

            # outcome == "unschedulable" → save
            try:
                time.sleep(0.5)  # quick snapshot
                alloc_cpu, req_cpu, alloc_mem_mi, req_mem_mi = ns_usage(self.ctx, self.ns)
                cpu_util = (req_cpu / alloc_cpu) if alloc_cpu else 0.0
                mem_util = (req_mem_mi / alloc_mem_mi) if alloc_mem_mi else 0.0
                print(f"[sim][snapshot] cpu_util={cpu_util:.4f} mem_util={mem_util:.4f}")
            except Exception:
                pass

            unsched_list = info.get("unschedulable", [])
            scheduled = count_scheduled_pods_in_ns(self.ctx, self.ns)

            row = {
                "timestamp": int(time.time()),
                "seed": use_seed,
                "num_nodes": self.args.num_nodes,
                "pods_per_node": self.args.pods_per_node,
                "num_replicaset": self.args.num_replicaset,
                "target_util": self.args.target_util,
                "target_util_tolerance": self.args.target_util_tolerance,
                "node_cpu": self.args.node_cpu,
                "node_mem": self.args.node_mem,
                "cpu_interval": self.args.cpu_interval,
                "mem_interval": self.args.mem_interval,
                "dist_mode": self.args.dist_mode,
                "variance": self.args.variance,
                "num_priorities": self.args.num_priorities,
                "add_node_standalone": self.args.add_node_standalone,
                # context
                "scheduled_count": scheduled,
                "unscheduled_count": max(0, self.T.total_pods - scheduled),
                "unschedulable_pods": unsched_list[:50],
            }
            save_seed_row(self.args.save_seed_file, row)
            found += 1
            print(f"[sim] ✓ saved seed {use_seed} (unschedulable={len(unsched_list)}) -> {self.args.save_seed_file} "
                f"({found}/{self.args.simulator_instances})")

        print("[sim] done.")


    def run_normal(self):
        ensure_namespace(self.ctx, self.ns, recreate=True)
        apply_priority_classes(self.ctx, self.args.num_priorities, prefix="p", start=1)

        # --- apply workload ---
        if self.args.num_replicaset <= 0:
            if self.args.wait_each:
                created = 0
                for i in range(1, self.args.num_nodes+1):
                    node = f"kwok-node-{i}"
                    parts_cpu = gen_parts_with_intervals(self.T.target_mc_node, self.args.pods_per_node,
                                                         self.rng, self.cpu_int, self.args.dist_mode, self.args.variance)
                    parts_mem = gen_parts_with_intervals(self.T.target_mi_node, self.args.pods_per_node,
                                                         self.rng, self.mem_int, self.args.dist_mode, self.args.variance)
                    for j in range(self.args.pods_per_node):
                        prio = self.rng.randint(1, self.args.num_priorities); pc = f"p{prio}"
                        name = f"{node}-{j+1}-{pc}"
                        y = yaml_kwok_pod(self.ns, name, node,
                                          m_to_cpu_str(max(1, parts_cpu[j])),
                                          mi_to_mem_str(max(1, parts_mem[j])),
                                          pc, add_node=self.args.add_node_standalone)
                        apply_yaml(self.ctx, y)
                        created += 1
                        self._maybe_wait("pod", name)
            else:
                created = self._apply_standalone()
            print(f"[kwok-fill] Applied {created} standalone pods across {self.args.num_nodes} nodes")
        else:
            created, num_rs = self._apply_rs()
            print(f"[kwok-fill] Applied {created} pods via {num_rs} ReplicaSets")

        # --- monitor scheduling outcome instead of fixed sleep ---
        outcome, info = monitor_until_all_scheduled_or_unschedulable(
            self.ctx, self.ns, expected=self.T.total_pods,
            interval=0.5, max_iters=None
        )

        if outcome == "unschedulable":
            print(f"[check] Some pods marked Unschedulable: {info['unschedulable'][:10]}...")
        elif outcome == "all_scheduled":
            print("[check] All pods scheduled successfully")
        else:
            print("[check] Monitoring ended inconclusive")

        # --- metrics snapshot ---
        alloc_cpu, req_cpu, alloc_mem_mi, req_mem_mi = ns_usage(self.ctx, self.ns)
        cpu_util = (req_cpu / alloc_cpu) if alloc_cpu else 0.0
        mem_util = (req_mem_mi / alloc_mem_mi) if alloc_mem_mi else 0.0
        nodes = [f"kwok-node-{i}" for i in range(1, self.args.num_nodes+1)]
        per_node = pods_per_node_in_ns(self.ctx, self.ns, nodes)

        tol = self.args.target_util_tolerance

        print(f"[check] cpu_util={cpu_util:.4f} mem_util={mem_util:.4f} "
              f"target={self.args.target_util:.2f}±{tol:.3f}")
        print(f"[check] pods/node: {per_node} (target {self.args.pods_per_node} each)")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cluster_name")
    ap.add_argument("num_nodes", type=int)
    ap.add_argument("pods_per_node", type=int)
    ap.add_argument("num_replicaset", type=int)
    ap.add_argument("--target-util", type=float, default=0.90)
    ap.add_argument("--target-util-tolerance", type=float, default=0.01)
    ap.add_argument("--wait-mode", choices=["exist","ready","running"], default="running")
    ap.add_argument("--namespace", default="crossnode-test")
    ap.add_argument("--node-cpu", default="24")
    ap.add_argument("--node-mem", default="32Gi")
    ap.add_argument("--dist-mode", default="random", choices=["random","even"])
    ap.add_argument("--variance", type=int, default=50)
    ap.add_argument("--seed", type=int, default=None)
    ap.add_argument("--num-priorities", type=int, default=4)
    ap.add_argument("--wait-each", action="store_true", default=True)
    ap.add_argument("--wait-timeout", default="60s")
    ap.add_argument("--add-node-standalone", action="store_true", default=False)
    ap.add_argument("--cpu-interval", default="50m,500m")
    ap.add_argument("--mem-interval", default="64Mi,1024Mi")
    ap.add_argument("--on-interval-infeasible", choices=["error","auto"], default="auto")
    ap.add_argument("--save-seed-file", default="./seeds.jsonl")
    ap.add_argument("--simulate", action="store_true", default=False)
    ap.add_argument("--simulator-instances", type=int, default=10)
    ap.add_argument("--simulator-timeout", default="60s")
    args = ap.parse_args()

    if args.num_priorities < 1:
        print("[kwok-fill] --num-priorities must be >= 1; forcing to 1", file=sys.stderr)
        args.num_priorities = 1

    runner = KwokFillRunner(args)
    runner.setup_cluster()
    if args.simulate:
        runner.simulate()
    else:
        runner.run_normal()

if __name__ == "__main__":
    main()