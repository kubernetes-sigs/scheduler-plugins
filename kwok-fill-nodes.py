#!/usr/bin/env python3
import os, sys, argparse, math, random, textwrap, subprocess, time

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
    base, rem = divmod(total, n)
    return [base + (1 if i < rem else 0) for i in range(n)]

def partition_int(total:int, k:int, min_each:int, rng:random.Random, variance:int) -> list[int]:
    '''
    Partition an integer into k parts, each at least min_each, with some randomness.
    Used to simulate resource allocation in a Kubernetes cluster.
    '''
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

def prio_for_step_desc(step:int) -> int:
    # 1->p4, 2->p3, 3->p2, 4->p1, 5->p4, ...
    return 4 - ((step - 1) % 4)

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


def yaml_rs(ns:str, rs_name:str, replicas:int, qcpu:str, qmem:str, pc:str) -> str:
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

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cluster_name")
    ap.add_argument("num_nodes", type=int)
    ap.add_argument("pods_per_node", type=int)
    ap.add_argument("num_replicaset", type=int)
    ap.add_argument("--target-util", type=float, default=0.90)
    ap.add_argument("--namespace", default="crossnode-test")
    ap.add_argument("--node-cpu", default="24")
    ap.add_argument("--node-mem", default="32Gi")
    ap.add_argument("--dist-mode", default=os.getenv("DIST_MODE","random"), choices=["random","even"])
    ap.add_argument("--variance", type=int, default=int(os.getenv("VARIANCE","50")))
    ap.add_argument("--seed", type=int, default=None)
    args = ap.parse_args()
    
    if args.seed is not None:
        rng = random.Random(args.seed)
        print(f"[kwok-fill] Using fixed seed={args.seed}")
    else:
        auto_seed = int(time.time_ns())  # nanoseconds since epoch
        rng = random.Random(auto_seed)
        print(f"[kwok-fill] Using auto-random seed={auto_seed}")
    
    ctx = f"kwok-{args.cluster_name}"
    ns = args.namespace

    # Ensure NS & PriorityClasses
    subprocess.run(["kubectl","--context",ctx,"get","ns",ns], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL) \
        .check_returncode() if subprocess.run(["kubectl","--context",ctx,"get","ns",ns], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode==0 \
        else subprocess.run(["kubectl","--context",ctx,"create","ns",ns], check=True)
    # Apply PriorityClasses
    pcs = "".join(yaml_priority_class(f"p{v}", v) for v in (1,2,3,4))
    subprocess.run(["kubectl","--context",ctx,"apply","-f","-"], input=pcs.encode(), check=True)

    # Create KWOK nodes
    pods_cap = args.pods_per_node * 10
    node_yaml = []
    for i in range(1, args.num_nodes+1):
        node_yaml.append(yaml_kwok_node(f"kwok-node-{i}", args.node_cpu, args.node_mem, pods_cap))
    subprocess.run(["kubectl","--context",ctx,"apply","-f","-"], input=("".join(node_yaml)).encode(), check=True)

    # Numbers
    node_mc = cpu_to_mc(args.node_cpu)
    node_mi = mem_to_mi(args.node_mem)
    target_mc_node = int(node_mc * args.target_util)
    target_mi_node = int(node_mi * args.target_util)
    total_pods = args.num_nodes * args.pods_per_node
    target_mc_cluster = target_mc_node * args.num_nodes
    target_mi_cluster = target_mi_node * args.num_nodes

    if args.num_replicaset <= 0:
        # Naked pod mode (omitted here to keep this short)
        print("Naked pod mode not implemented in this minimal example.", file=sys.stderr)
        sys.exit(1)

    # RS sizes
    rs_replicas = partition_int(total_pods, args.num_replicaset, 1, rng, args.variance)
    per_rep_cpu = pick_dist(target_mc_cluster, total_pods, args.dist_mode, rng, args.variance)
    per_rep_mem = pick_dist(target_mi_cluster, total_pods, args.dist_mode, rng, args.variance)

    # Emit RS manifests and apply
    offset = 0
    out = []
    for i, count in enumerate(rs_replicas, start=1):
        prio = prio_for_step_desc(i)
        pc = f"p{prio}"
        rsname = f"rs{i}-{pc}"
        slice_cpu = per_rep_cpu[offset:offset+count]
        slice_mem = per_rep_mem[offset:offset+count]
        avg_mc = max(1, sum(slice_cpu)//count)
        avg_mi = max(1, sum(slice_mem)//count)
        out.append(yaml_rs(ns, rsname, count, mc(avg_mc), mi(avg_mi), pc))
        offset += count
    subprocess.run(["kubectl","--context",ctx,"apply","-f","-"], input=("".join(out)).encode(), check=True)

if __name__ == "__main__":
    main()
