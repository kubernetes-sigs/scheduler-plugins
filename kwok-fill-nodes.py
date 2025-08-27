#!/usr/bin/env python3

import sys, argparse, random, textwrap, subprocess, time, json

def wait_pod_running_on_node(ctx: str, name: str, ns: str, timeout_sec: int) -> bool:
    """Wait until the Pod has nodeName set and phase == Running."""
    deadline = time.time() + timeout_sec
    while time.time() < deadline:
        r = run(["kubectl","--context",ctx,"-n",ns,"get","pod",name,"-o","json"],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r.returncode == 0:
            try:
                pod = json.loads(r.stdout)
                node = (pod.get("spec") or {}).get("nodeName") or ""
                phase = (pod.get("status") or {}).get("phase") or ""
                if node and phase == "Running":
                    return True
            except Exception:
                pass
        time.sleep(0.5)
    return False

def wait_rs_pods_running_on_nodes(ctx: str, rs_name: str, ns: str, timeout_sec: int) -> bool:
    """Wait until all desired replicas for the RS are Running on some node."""
    deadline = time.time() + timeout_sec
    while time.time() < deadline:
        # desired replicas
        r_rs = run(["kubectl","--context",ctx,"-n",ns,"get","rs",rs_name,"-o","json"],
                   stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r_rs.returncode != 0:
            time.sleep(0.5)
            continue
        try:
            rs = json.loads(r_rs.stdout)
            desired = int((rs.get("spec") or {}).get("replicas") or 0)
        except Exception:
            time.sleep(0.5)
            continue
        if desired == 0:
            return True

        # count pods that match the RS label and are Running with a node
        r_pods = run([
            "kubectl","--context",ctx,"-n",ns,"get","pods","-l",f"app={rs_name}","-o","json"
        ], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r_pods.returncode != 0:
            time.sleep(0.5); continue
        try:
            podlist = json.loads(r_pods.stdout).get("items", [])
            running = 0
            for p in podlist:
                node = (p.get("spec") or {}).get("nodeName") or ""
                phase = (p.get("status") or {}).get("phase") or ""
                if node and phase == "Running":
                    running += 1
            if running >= desired:
                return True
        except Exception:
            pass
        time.sleep(0.5)
    return False

def cpu_to_mc(v: str) -> int:
    return int(v[:-1]) if v.endswith('m') else int(float(v) * 1000)

def mem_to_mi(v: str) -> int:
    num = int(''.join(c for c in v if c.isdigit()))
    unit = v[len(str(num)):]
    return {
        '': num, 'Mi': num, 'mi': num,
        'Gi': num*1024, 'gi': num*1024,
        'Ki': max(1, num//1024), 'ki': max(1, num//1024),
        'Ti': num*1024*1024, 'ti': num*1024*1024
    }.get(unit, num)

def mc(n:int) -> str: return f"{n}m"
def mi(n:int) -> str: return f"{n}Mi"

def split_even(total:int, n:int) -> list[int]:
    if n <= 0: return []
    base, rem = divmod(total, n)
    return [base + (1 if i < rem else 0) for i in range(n)]

def partition_int(total:int, k:int, min_each:int, rng:random.Random, variance:int) -> list[int]:
    """Partition an integer into k parts, each >= min_each, with randomness."""
    if k <= 0: return []
    rem = max(0, total - k*min_each)
    if rem == 0: return [min_each]*k
    weights = [rng.randrange(1, max(2, variance+1)) for _ in range(k)]
    sumw = sum(weights)
    parts, allocated = [], 0
    for i in range(k-1):
        share = rem * weights[i] // sumw
        parts.append(min_each + share)
        allocated += share
    parts.append(min_each + rem - allocated)
    return parts

def pick_dist(total:int, n:int, mode:str, rng:random.Random, variance:int) -> list[int]:
    return split_even(total, n) if mode == "even" else partition_int(total, n, 1, rng, variance)

def prio_for_step_desc(step:int, num_prios:int) -> int:
    """Return priority cycling pN..p1.. given step (1-based)."""
    if num_prios <= 1: return 1
    return num_prios - ((step - 1) % num_prios)

def yaml_priority_class(name:str, value:int) -> str:
    return textwrap.dedent(f"""\
    apiVersion: scheduling.k8s.io/v1
    kind: PriorityClass
    metadata: {{name: {name}}}
    value: {value}
    preemptionPolicy: PreemptLowerPriority
    globalDefault: false
    description: "pod priority {value}"
    ---
    """)

def yaml_kwok_node(name:str, cpu:str, mem:str, pods_cap:int) -> str:
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

def yaml_kwok_rs(ns:str, rs_name:str, replicas:int, qcpu:str, qmem:str, pc:str) -> str:
    return textwrap.dedent(f"""\
    apiVersion: apps/v1
    kind: ReplicaSet
    metadata:
      name: {rs_name}
      namespace: {ns}
    spec:
      replicas: {replicas}
      selector:
        matchLabels: {{app: {rs_name}}}
      template:
        metadata: {{labels: {{app: {rs_name}}}}}
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

def yaml_kwok_pod(ns:str, name:str, node:str|None, qcpu:str, qmem:str, pc:str, add_node:bool=False) -> str:
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

def run(cmd:list[str], **kw):
    return subprocess.run(cmd, **kw)

def apply_yaml(ctx:str, yaml_text:str):
    return run(["kubectl","--context",ctx,"apply","-f","-"], input=yaml_text.encode(), check=True)

def exists(ctx:str, kind:str, name:str, ns:str) -> bool:
    r = run(["kubectl","--context",ctx,"-n",ns,"get",kind,name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    return r.returncode == 0

def wait_exist(ctx:str, kind:str, name:str, ns:str, timeout_sec:int):
    deadline = time.time() + timeout_sec
    while time.time() < deadline:
        if exists(ctx, kind, name, ns):
            return True
        time.sleep(0.5)
    return False

def wait_ready(ctx:str, kind:str, name:str, ns:str, timeout:str) -> bool:
    # Prefer readiness conditions; fall back to existence if unsupported
    if kind.lower() in ("pod","pods"):
        cmd = ["kubectl","--context",ctx,"-n",ns,"wait",f"pod/{name}","--for=condition=Ready","--timeout",timeout]
    elif kind.lower() in ("replicaset","replicasets","rs"):
        cmd = ["kubectl","--context",ctx,"-n",ns,"wait",f"replicaset/{name}","--for=condition=Available","--timeout",timeout]
    else:
        # Unknown kind: just wait for exist
        return wait_exist(ctx, kind, name, ns, parse_timeout_to_seconds(timeout))
    r = run(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if r.returncode == 0:
        return True
    # Fallback: existence
    return wait_exist(ctx, kind, name, ns, parse_timeout_to_seconds(timeout))

def parse_timeout_to_seconds(t:str) -> int:
    # Accept "60s", "2m", "150" (seconds)
    try:
        if t.endswith("ms"):
            return max(1, int(int(t[:-2]) / 1000))
        if t.endswith("s"):
            return int(t[:-1])
        if t.endswith("m"):
            return int(t[:-1]) * 60
        if t.endswith("h"):
            return int(t[:-1]) * 3600
        return int(t)
    except Exception:
        return 60

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cluster_name")
    ap.add_argument("num_nodes", type=int)
    ap.add_argument("pods_per_node", type=int)
    ap.add_argument("num_replicaset", type=int)
    ap.add_argument("--target-util", type=float, default=0.90)
    ap.add_argument("--wait-mode", choices=["exist","ready","running"], default="running")
    ap.add_argument("--namespace", default="crossnode-test")
    ap.add_argument("--node-cpu", default="24")
    ap.add_argument("--node-mem", default="32Gi")
    ap.add_argument("--dist-mode", default="random", choices=["random","even"])
    ap.add_argument("--variance", type=int, default=50)
    ap.add_argument("--seed", type=int, default=None)
    ap.add_argument("--num-priorities", type=int, default=4)
    ap.add_argument("--wait-each", action="store_true",
                    help="Apply each ReplicaSet/Pod individually and wait for it before proceeding.")
    ap.add_argument("--wait-timeout", default="60s")
    ap.add_argument("--add-node-standalone", action="store_true", default=False,
                    help="If set, assign standalone Pods to nodes explicitly (default: False, let scheduler decide).")
    args = ap.parse_args()

    if args.num_priorities < 1:
        print("[kwok-fill] --num-priorities must be >= 1; forcing to 1", file=sys.stderr)
        args.num_priorities = 1

    if args.seed is not None:
        rng = random.Random(args.seed)
        print(f"[kwok-fill] Using fixed seed={args.seed}")
    else:
        auto_seed = int(time.time_ns())
        rng = random.Random(auto_seed)
        print(f"[kwok-fill] Using auto-random seed={auto_seed}")

    ctx = f"kwok-{args.cluster_name}"
    ns = args.namespace

    # Ensure NS & PriorityClasses
    get_ns = run(["kubectl","--context",ctx,"get","ns",ns], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if get_ns.returncode != 0:
        run(["kubectl","--context",ctx,"create","ns",ns], check=True)

    pcs = "".join(yaml_priority_class(f"p{v}", v) for v in range(1, args.num_priorities+1))
    apply_yaml(ctx, pcs)

    # Create KWOK nodes
    pods_cap = args.pods_per_node * 10
    node_yaml = []
    for i in range(1, args.num_nodes+1):
        node_yaml.append(yaml_kwok_node(f"kwok-node-{i}", args.node_cpu, args.node_mem, pods_cap))
    apply_yaml(ctx, "".join(node_yaml))

    # Numbers
    node_mc = cpu_to_mc(args.node_cpu)
    node_mi = mem_to_mi(args.node_mem)
    target_mc_node = int(node_mc * args.target_util)
    target_mi_node = int(node_mi * args.target_util)
    total_pods = args.num_nodes * args.pods_per_node
    target_mc_cluster = target_mc_node * args.num_nodes
    target_mi_cluster = target_mi_node * args.num_nodes

    # ----------------------------- Standalone pods mode
    if args.num_replicaset <= 0:
        created = 0
        if args.wait_each:
            for i in range(1, args.num_nodes+1):
                node = f"kwok-node-{i}"
                parts_cpu = pick_dist(target_mc_node, args.pods_per_node, args.dist_mode, rng, args.variance)
                parts_mem = pick_dist(target_mi_node, args.pods_per_node, args.dist_mode, rng, args.variance)
                for j in range(args.pods_per_node):
                    prio = rng.randint(1, args.num_priorities)
                    pc = f"p{prio}"
                    name = f"{node}-{j+1}-{pc}"
                    y = yaml_kwok_pod(ns, name, node, mc(max(1, parts_cpu[j])), mi(max(1, parts_mem[j])), pc,
                                      add_node=args.add_node_standalone)
                    apply_yaml(ctx, y)
                    created += 1
        else:
            out = []
            for i in range(1, args.num_nodes+1):
                node = f"kwok-node-{i}"
                parts_cpu = pick_dist(target_mc_node, args.pods_per_node, args.dist_mode, rng, args.variance)
                parts_mem = pick_dist(target_mi_node, args.pods_per_node, args.dist_mode, rng, args.variance)
                for j in range(args.pods_per_node):
                    prio = rng.randint(1, args.num_priorities)
                    pc = f"p{prio}"
                    name = f"{node}-{j+1}-{pc}"
                    out.append(yaml_kwok_pod(ns, name, node, mc(max(1, parts_cpu[j])), mi(max(1, parts_mem[j])), pc,
                                             add_node=args.add_node_standalone))
            if out:
                apply_yaml(ctx, "".join(out))
                created = len(out)
        print(f"[kwok-fill] Applied {created} standalone pods across {args.num_nodes} nodes")
        return

    # ----------------------------- ReplicaSets mode
    rs_replicas = partition_int(total_pods, args.num_replicaset, 1, rng, args.variance)
    per_rep_cpu = pick_dist(target_mc_cluster, total_pods, args.dist_mode, rng, args.variance)
    per_rep_mem = pick_dist(target_mi_cluster, total_pods, args.dist_mode, rng, args.variance)

    offset = 0
    if args.wait_each:
        total = 0
        for i, count in enumerate(rs_replicas, start=1):
            prio = prio_for_step_desc(i, args.num_priorities)
            pc = f"p{prio}"
            rsname = f"rs{i}-{pc}"
            slice_cpu = per_rep_cpu[offset:offset+count]
            slice_mem = per_rep_mem[offset:offset+count]
            avg_mc = max(1, sum(slice_cpu)//count)
            avg_mi = max(1, sum(slice_mem)//count)
            y = yaml_kwok_rs(ns, rsname, count, mc(avg_mc), mi(avg_mi), pc)
            apply_yaml(ctx, y)
            print(f"[kwok-fill] applied ReplicaSet {rsname} (replicas={count}); waiting ({args.wait_mode}) ...")
            if args.wait_mode == "running":
                ok = wait_rs_pods_running_on_nodes(ctx, rsname, ns, parse_timeout_to_seconds(args.wait_timeout))
            elif args.wait_mode == "ready":
                ok = wait_ready(ctx, "replicaset", rsname, ns, args.wait_timeout)
            else:
                ok = wait_exist(ctx, "replicaset", rsname, ns, parse_timeout_to_seconds(args.wait_timeout))
            if not ok:
                print(f"[kwok-fill][WARN] wait timeout for ReplicaSet {rsname}", file=sys.stderr)
            offset += count
            total += count
        print(f"[kwok-fill] Applied {total} pods via {len(rs_replicas)} ReplicaSets")
    else:
        out = []
        for i, count in enumerate(rs_replicas, start=1):
            prio = prio_for_step_desc(i, args.num_priorities)
            pc = f"p{prio}"
            rsname = f"rs{i}-{pc}"
            slice_cpu = per_rep_cpu[offset:offset+count]
            slice_mem = per_rep_mem[offset:offset+count]
            avg_mc = max(1, sum(slice_cpu)//count)
            avg_mi = max(1, sum(slice_mem)//count)
            out.append(yaml_kwok_rs(ns, rsname, count, mc(avg_mc), mi(avg_mi), pc))
            offset += count
        apply_yaml(ctx, "".join(out))
        print(f"[kwok-fill] Applied {sum(rs_replicas)} pods via {len(rs_replicas)} ReplicaSets")

if __name__ == "__main__":
    main()
