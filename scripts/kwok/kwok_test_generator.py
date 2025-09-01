#!/usr/bin/env python3

# kwok_test_generator.py

import os, sys, argparse, random, time, json
from typing import List
from dataclasses import dataclass
from collections import OrderedDict

from kwok_shared import (
    parse_timeout_s, parse_cpu_interval, parse_mem_interval, resolve_interval_or_fallback, format_interval_cpu, format_interval_mem,
    partition_int, gen_parts_constrained, count_scheduled_in_ns, pods_per_node_in_ns,
    wait_each, wait_until_settled_or_unschedulable_events,
    ensure_namespace, create_kwok_nodes, delete_kwok_nodes, ensure_priority_classes,
    yaml_kwok_pod, yaml_kwok_rs, apply_yaml, cpu_m_str_to_int, mem_str_to_mib_int, cpu_m_int_to_str, mem_mi_int_to_str,
    compute_stat_totals, stat_snapshot, check_context, set_context,
    get_json_ctx, sum_pod_requests, bytes_to_mib,
)
@dataclass
class TestTargets:
    node_mc: int
    node_mi: int
    total_pods: int
    target_mc_node: int
    target_mi_node: int
    target_mc_cluster: int
    target_mi_cluster: int

def load_instance_row(seed: int, path: str) -> dict | None:
    """
    Load a specific instance row from the seed file.
    """
    try:
        with open(path, "r", encoding="utf-8") as f:
            for line in f:
                if not line.strip():
                    continue
                try:
                    row = json.loads(line)
                except Exception:
                    continue
                if int(row.get("seed", -1)) == int(seed):
                    return row
    except FileNotFoundError:
        print(f"[kwok-fill][ERROR] seed file not found: {path}", file=sys.stderr)
    return None

def load_instance_into_args(args: argparse.Namespace) -> None:
    """
    If --load-instance is set, use --seed to look up an instance in --seed-file,
    then override compatible CLI args with the saved row. Errors if --simulate is set.
    """
    if not getattr(args, "load_instance", False):
        return

    # Disallow combined usage with --simulate
    if getattr(args, "simulate", False):
        print("[kwok-fill][ERROR] --load-instance cannot be used together with --simulate", file=sys.stderr)
        sys.exit(2)

    if args.seed is None:
        print("[kwok-fill][ERROR] --load-instance requires --seed to be specified", file=sys.stderr)
        sys.exit(2)

    row = load_instance_row(args.seed, args.seed_file)
    if not row:
        print(f"[kwok-fill][ERROR] seed {args.seed} not found in {args.seed_file}", file=sys.stderr)
        sys.exit(2)

    overrides = {
        "num_nodes": "num_nodes",
        "pods_per_node": "pods_per_node",
        "num_replicaset": "num_replicaset",
        "target_util": "target_util",
        "target_util_tolerance": "target_util_tolerance",
        "node_cpu": "node_cpu",
        "node_mem": "node_mem",
        "cpu_interval": "cpu_interval",
        "mem_interval": "mem_interval",
        "dist_mode": "dist_mode",
        "variance": "variance",
        "num_priorities": "num_priorities",
        "simulate_add_condition": "simulate_add_condition",
    }

    for arg_name, row_key in overrides.items():
        if row_key in row and row[row_key] is not None:
            setattr(args, arg_name, row[row_key])

    print(f"[kwok-fill] Loaded instance config from {args.seed_file} using seed={args.seed}")

class KwokTestGeneratorRunner:
    def __init__(self, args: argparse.Namespace):
        self.args = args
        ctx_name = f"kwok-{args.cluster_name}"
        resolved_ctx = check_context(ctx_name)
        if resolved_ctx:
            self.ctx = resolved_ctx
            # Just set current context, for no confusing
            set_context(self.ctx)
            print(f"[kwok-fill] Using kubectl context '{self.ctx}' for cluster '{args.cluster_name}'")
        else:
            raise Exception(f"Could not resolve context for cluster '{args.cluster_name}'")
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
        node_mc = cpu_m_str_to_int(args.node_cpu)
        node_mi = mem_str_to_mib_int(args.node_mem)
        total_pods = args.num_nodes * args.pods_per_node
        target_mc_node = int(node_mc * args.target_util)
        target_mi_node = int(node_mi * args.target_util)
        target_mc_cluster = target_mc_node * args.num_nodes
        target_mi_cluster = target_mi_node * args.num_nodes
        self.T = TestTargets(node_mc, node_mi, total_pods, target_mc_node, target_mi_node,
                         target_mc_cluster, target_mi_cluster)

        # intervals (parsed)
        self.cpu_int = parse_cpu_interval(args.cpu_interval)
        self.mem_int = parse_mem_interval(args.mem_interval)

    def _collect_pair_buckets(self, ns: str, mode_rs: bool) -> dict[tuple[int,int], list[str]]:
        """
        mode_rs=True  -> collect RS template (cpu_m, mem_mi) -> [rs_name...]
        mode_rs=False -> collect standalone pod (cpu_m, mem_mi) -> [pod_name...]
        """
        buckets: dict[tuple[int,int], list[str]] = {}

        if mode_rs:
            rs_items = get_json_ctx(self.ctx, ["-n", ns, "get", "replicasets", "-o", "json"]).get("items", [])
            for rs in rs_items:
                name = (rs.get("metadata") or {}).get("name", "<nors>")
                tpl = (rs.get("spec") or {}).get("template") or {}
                containers = ((tpl.get("spec") or {}).get("containers") or [])
                mc_sum = 0
                mi_sum = 0
                for c in containers:
                    req = ((c.get("resources") or {}).get("requests") or {})
                    mc_sum += cpu_m_str_to_int(str(req.get("cpu", "0")))
                    mi_sum += mem_str_to_mib_int(str(req.get("memory", "0")))
                pair = (int(mc_sum), int(mi_sum))
                buckets.setdefault(pair, []).append(name)
            return buckets

        # standalone pods
        pods = get_json_ctx(self.ctx, ["-n", ns, "get", "pods", "-o", "json"]).get("items", [])
        for p in pods:
            owners = (p.get("metadata", {}).get("ownerReferences") or [])
            if any(o.get("controller") for o in owners):
                continue  # skip controller-owned pods
            name = (p.get("metadata") or {}).get("name", "<noname>")
            mc, mem_b = sum_pod_requests(p)             # (mCPU, bytes)
            pair = (int(mc), int(bytes_to_mib(mem_b)))  # normalize to (mCPU, MiB)
            buckets.setdefault(pair, []).append(name)
        return buckets


    def _assert_unique_pairs_after_apply(self) -> None:
        """
        Fail the run if any (CPU_m, MEM_Mi) duplicates exist:
        - RS mode: check RS templates
        - Standalone mode: check pods without a controller owner
        """
        dup_msgs = []
        # RS mode?
        if self.args.num_replicaset > 0:
            buckets = self._collect_pair_buckets(self.ns, mode_rs=True)
            label = "RS template duplicate"
        else:
            buckets = self._collect_pair_buckets(self.ns, mode_rs=False)
            label = "Standalone duplicate"

        for pair, names in buckets.items():
            if len(names) > 1:
                dup_msgs.append(f"{label} {pair}: {', '.join(sorted(names))}")

        if dup_msgs:
            print("\n[check][FAIL] Found duplicate (CPU_m, MEM_Mi) pairs:")
            for line in dup_msgs[:50]:
                print(" - " + line)
            if len(dup_msgs) > 50:
                print(f"   ... and {len(dup_msgs)-50} more")
            sys.exit(3)
        else:
            print("[check] Uniqueness OK: no duplicate (CPU_m, MEM_Mi) pairs")

    def _save_seed_row(self, path: str, row: dict) -> None:
        """
        Save a specific instance row to the seed file.
        """
        os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
        with open(path, "a", encoding="utf-8") as f:
            f.write(json.dumps(row) + "\n")

    def _prio_for_step_desc(self, step:int, num_prios:int) -> int:
        """
        Get the priority for a specific step in the descending order.
        """
        if num_prios <= 1: return 1
        return num_prios - ((step - 1) % num_prios)

    def _ensure_intervals(self) -> None:
        """
        Ensure that the CPU and memory intervals are set correctly.
        """
        k_pods = self.T.total_pods
        tgt_mc = self.T.target_mc_cluster
        tgt_mi = self.T.target_mi_cluster

        cpu_used, cpu_fell_back = resolve_interval_or_fallback(self.cpu_int, k_pods, tgt_mc, "CPU(m)", spread=0.9)
        self.cpu_int = cpu_used

        mem_used, mem_fell_back = resolve_interval_or_fallback(self.mem_int, k_pods, tgt_mi, "MEM(Mi)", spread=0.9)
        self.mem_int = mem_used

        # Update args if fell back
        if cpu_fell_back and self.cpu_int:
            self.args.cpu_interval = format_interval_cpu(self.cpu_int)
        if mem_fell_back and self.mem_int:
            self.args.mem_interval = format_interval_mem(self.mem_int)

    def _apply_standalone_pods(self) -> int:
        """
        Apply standalone pods to the cluster.
        """
        # Derive per-pod requests for the whole cluster once
        parts_cpu = gen_parts_constrained(
            self.T.target_mc_cluster, self.T.total_pods,
            self.rng, self.cpu_int, self.args.dist_mode, self.args.variance
        )
        parts_mem = gen_parts_constrained(
            self.T.target_mi_cluster, self.T.total_pods,
            self.rng, self.mem_int, self.args.dist_mode, self.args.variance
        )
        created = 0
        for k in range(self.T.total_pods):
            prio = self.rng.randint(1, self.args.num_priorities); pc = f"p{prio}"
            name = f"pod-{k+1:02d}-{pc}"
            pod_yaml = yaml_kwok_pod(self.ns, name, cpu_m_int_to_str(max(1, parts_cpu[k])), mem_mi_int_to_str(max(1, parts_mem[k])), pc)
            apply_yaml(self.ctx, pod_yaml)
            created += 1
            if self.args.wait_each:
                tsec = parse_timeout_s(self.args.wait_timeout)
                wait_each(self.ctx, "pod", name, self.ns, tsec, self.args.wait_mode)
        return created

    def _apply_rs(self) -> tuple[int, set[int]]:
        """
        Apply ReplicaSets to the cluster.
        """
        rs_replicas = partition_int(self.T.total_pods, self.args.num_replicaset, 1, self.rng, self.args.variance)
        per_pod_cpu = gen_parts_constrained(self.T.target_mc_cluster, self.T.total_pods,
                                               self.rng, self.cpu_int, self.args.dist_mode, self.args.variance)
        per_pod_mem = gen_parts_constrained(self.T.target_mi_cluster, self.T.total_pods,
                                               self.rng, self.mem_int, self.args.dist_mode, self.args.variance)
        offset = 0
        total_created = 0
        for i, count in enumerate(rs_replicas, start=1):
            prio = self._prio_for_step_desc(i, self.args.num_priorities); pc = f"p{prio}"
            rsname = f"rs-{i:02d}-{pc}"
            slice_cpu = per_pod_cpu[offset:offset+count]
            slice_mem = per_pod_mem[offset:offset+count]
            avg_mc = max(1, sum(slice_cpu)//count)
            avg_mi = max(1, sum(slice_mem)//count)
            if self.cpu_int: avg_mc = min(max(avg_mc, self.cpu_int[0]), self.cpu_int[1])
            if self.mem_int: avg_mi = min(max(avg_mi, self.mem_int[0]), self.mem_int[1])
            rs_yaml = yaml_kwok_rs(self.ns, rsname, count, cpu_m_int_to_str(avg_mc), mem_mi_int_to_str(avg_mi), pc)
            apply_yaml(self.ctx, rs_yaml)
            if self.args.wait_each:
                tsec = parse_timeout_s(self.args.wait_timeout)
                wait_each(self.ctx, "replicaset", rsname, self.ns, tsec, self.args.wait_mode)
            total_created += count
            offset += count
        return total_created, {len(rs_replicas)}
    
    def _print_outcome_and_snapshot(ctx: str, ns: str, args, outcome: str, info: dict) -> None:
        """
        Shared tail: print outcome details + resource snapshot.
        """
        # Unsched list (if any)
        unsched_list = info.get("unschedulable", []) if outcome == "some_unschedulable" else []
        if unsched_list:
            print(f"[check] Unschedulable pods ({len(unsched_list)}): {', '.join(unsched_list[:20])}"
                f"{' ...' if len(unsched_list) > 20 else ''}")

        # Snapshot
        time.sleep(1)
        snap = stat_snapshot(ctx)
        tot_cpu_alloc, tot_mem_alloc_b, tot_cpu_req_run, tot_mem_req_run_b = compute_stat_totals(
            snap.alloc, snap.cpu_req_by_node, snap.mem_req_by_node
        )
        cpu_run = (tot_cpu_req_run / tot_cpu_alloc) if tot_cpu_alloc else 0.0
        mem_run = (tot_mem_req_run_b / tot_mem_alloc_b) if tot_mem_alloc_b else 0.0
        cpu_all = (snap.cpu_req_all / tot_cpu_alloc) if tot_cpu_alloc else 0.0
        mem_all = (snap.mem_req_all / tot_mem_alloc_b) if tot_mem_alloc_b else 0.0

        print(f"[check] run_util: cpu={cpu_run*100:.1f}% mem={mem_run*100:.1f}% | "
            f"all_util: cpu={cpu_all*100:.1f}% mem={mem_all*100:.1f}% | "
            f"target={args.target_util*100:.1f}%±{args.target_util_tolerance*100:.1f}%")

        # Per-node pods (only in normal mode where we know num_nodes)
        nodes = [f"kwok-node-{i}" for i in range(1, getattr(args, "num_nodes", 0) + 1)]
        if nodes:
            per_node = pods_per_node_in_ns(ctx, ns, nodes)
            print(f"[check] pods/node: {per_node}")
    
    def setup_cluster(self) -> None:
        """
        Set up the cluster by creating the necessary nodes and ensuring intervals.
        """
        delete_kwok_nodes(self.ctx) # we delete as configuration might have changed
        create_kwok_nodes(self.ctx, self.args.num_nodes, self.args.node_cpu, self.args.node_mem, self.args.max_pods_per_node)
        self._ensure_intervals()

    def simulate(self) -> None:
        """
        Simulate the scheduling of pods in the cluster.
        Save the simulation results to the seed file if checks pass.
        """
        found = 0
        attempts = 0
        desired = getattr(self.args, "simulate_add_condition", "some_unschedulable")
        print(f"[sim] starting — need {self.args.simulator_instances} run(s) where outcome == '{desired}'")

        while found < self.args.simulator_instances:
            attempts += 1
            use_seed = int(time.time_ns())
            self.rng.seed(use_seed)
            print(f"[sim] attempt {attempts} seed={use_seed}")

            # fresh ns + PCs
            ensure_namespace(self.ctx, self.ns, recreate=True)
            ensure_priority_classes(self.ctx, self.args.num_priorities, prefix="p", start=1)

            # apply workload
            _ = self._apply_standalone_pods() if self.args.num_replicaset <= 0 else self._apply_rs()[0]

            # conditions-only monitor
            settle_timeout = parse_timeout_s(self.args.settle_timeout)
            outcome, info = wait_until_settled_or_unschedulable_events(self.ctx, self.ns, expected=self.T.total_pods, settle_timeout_sec=settle_timeout)

            # keep only matching outcomes
            if desired == "all" and outcome == "inconclusive":
                print(f"[sim] failed; outcome='{outcome}' != desired='{desired}' — discarding seed and retrying…")
                continue
            if desired == "some_unschedulable" and outcome != "some_unschedulable":
                print(f"[sim] failed; outcome='{outcome}' != desired='{desired}' — discarding seed and retrying…")
                continue
            if desired == "all_scheduled" and outcome != "all_scheduled":
                print(f"[sim] failed; outcome='{outcome}' != desired='{desired}' — discarding seed and retrying…")
                continue
            self._assert_unique_pairs_after_apply()

            unsched_list = info.get("unschedulable", []) if outcome == "some_unschedulable" else []
            scheduled = count_scheduled_in_ns(self.ctx, self.ns)

            if unsched_list:
                print(f"[sim] Unschedulable pods ({len(unsched_list)}): {', '.join(unsched_list[:20])}"
                    f"{' ...' if len(unsched_list) > 20 else ''}")
            
            print(f"[sim] ✓ succeeded; outcome='{outcome}' == desired='{desired}' (outcome='{outcome}', unschedulable={len(unsched_list)})")
            
            # snapshot (running vs total incl. unscheduled)
            time.sleep(1)
            snap = stat_snapshot(self.ctx)
            tot_cpu_alloc, tot_mem_alloc_b, tot_cpu_req_run, tot_mem_req_run_b = compute_stat_totals(
                snap.alloc, snap.cpu_req_by_node, snap.mem_req_by_node
            )
            cpu_util_run = (tot_cpu_req_run / tot_cpu_alloc) if tot_cpu_alloc else 0.0
            mem_util_run = (tot_mem_req_run_b / tot_mem_alloc_b) if tot_mem_alloc_b else 0.0
            cpu_util_all = (snap.cpu_req_all / tot_cpu_alloc) if tot_cpu_alloc else 0.0
            mem_util_all = (snap.mem_req_all / tot_mem_alloc_b) if tot_mem_alloc_b else 0.0

            print(f"[sim] cpu_run={cpu_util_run*100:.2f} mem_run={mem_util_run*100:.2f} "
                f"cpu_all={cpu_util_all*100:.2f} mem_all={mem_util_all*100:.2f}")
            
            row = OrderedDict()
            row["timestamp"] = int(time.time())
            row["seed"] = use_seed
            row["num_nodes"] = self.args.num_nodes
            row["pods_per_node"] = self.args.pods_per_node
            row["num_replicaset"] = self.args.num_replicaset
            row["num_priorities"] = self.args.num_priorities
            row["target_util"] = self.args.target_util
            row["target_util_tolerance"] = self.args.target_util_tolerance
            row["node_cpu"] = self.args.node_cpu
            row["node_mem"] = self.args.node_mem
            row["cpu_interval"] = self.args.cpu_interval
            row["mem_interval"] = self.args.mem_interval
            row["outcome"] = outcome
            row["simulate_add_condition"] = self.args.simulate_add_condition
            row["dist_mode"] = self.args.dist_mode
            row["variance"] = self.args.variance
            row["scheduled_count"] = scheduled
            row["unscheduled_count"] = max(0, self.T.total_pods - scheduled)
            row["unschedulable_pods"] = unsched_list[:50]
            row["wait_each"] = self.args.wait_each
            row["wait_timeout"] = self.args.wait_timeout
            row["wait_mode"] = self.args.wait_mode
            row["settle_timeout"] = self.args.settle_timeout
            
            self._save_seed_row(self.args.seed_file, row)
            found += 1
            print(f"[sim] ✓ saved seed {use_seed}"
                f"-> {self.args.seed_file} ({found}/{self.args.simulator_instances})")

            if self.args.simulate_stop_simulation_if_unschedulable and outcome == "some_unschedulable":
                print("[sim] stopping further simulations due to unschedulable pods (as requested)")
                break

        print("[sim] done.")

    def run_normal(self) -> None:
        """
        Run the normal scheduling simulation.
        """
        ensure_namespace(self.ctx, self.ns, recreate=True)
        ensure_priority_classes(self.ctx, self.args.num_priorities, prefix="p", start=1)

        # --- apply workload ---
        if self.args.num_replicaset <= 0:
            created = self._apply_standalone_pods()
            print(f"[kwok-fill] Applied {created} standalone pods across {self.args.num_nodes} nodes")
        else:
            created, num_rs = self._apply_rs()
            print(f"[kwok-fill] Applied {created} pods via {num_rs} ReplicaSets")

        # --- monitor scheduling ---
        settle_timeout = parse_timeout_s(self.args.settle_timeout)
        outcome, info = wait_until_settled_or_unschedulable_events(self.ctx, self.ns, expected=self.T.total_pods, settle_timeout_sec=settle_timeout)

        if outcome == "some_unschedulable":
            unsched_list = info.get("unschedulable", [])
            print(f"[check] Some pods marked Unschedulable ({len(unsched_list)}): {', '.join(unsched_list[:20])}"
                f"{' ...' if len(unsched_list) > 20 else ''}")
        elif outcome == "all_scheduled":
            print("[check] All pods scheduled successfully")
        else:
            print("[check] Monitoring ended inconclusive")
        
        unsched_list = info.get("unschedulable", []) if outcome == "some_unschedulable" else []
        if unsched_list:
            print(f"[sim] Unschedulable pods ({len(unsched_list)}): {', '.join(unsched_list[:20])}"
                  f"{' ...' if len(unsched_list) > 20 else ''}")

        # --- metrics snapshot (running vs total incl. unscheduled) ---
        time.sleep(1)
        snap = stat_snapshot(self.ctx)
        tot_cpu_alloc, tot_mem_alloc_b, tot_cpu_req_run, tot_mem_req_run_b = compute_stat_totals(
            snap.alloc, snap.cpu_req_by_node, snap.mem_req_by_node
        )
        cpu_util_run = (tot_cpu_req_run / tot_cpu_alloc) if tot_cpu_alloc else 0.0
        mem_util_run = (tot_mem_req_run_b / tot_mem_alloc_b) if tot_mem_alloc_b else 0.0
        cpu_util_all = (snap.cpu_req_all / tot_cpu_alloc) if tot_cpu_alloc else 0.0
        mem_util_all = (snap.mem_req_all / tot_mem_alloc_b) if tot_mem_alloc_b else 0.0

        nodes = [f"kwok-node-{i}" for i in range(1, self.args.num_nodes+1)]
        per_node = pods_per_node_in_ns(self.ctx, self.ns, nodes)
        tol = self.args.target_util_tolerance

        print(f"[check] run_util: cpu={cpu_util_run*100:.1f}% mem={mem_util_run*100:.1f}% | "
            f"all_util: cpu={cpu_util_all*100:.1f}% mem={mem_util_all*100:.1f}% | "
            f"target={self.args.target_util*100:.1f}%±{tol*100:.1f}%")
        self._assert_unique_pairs_after_apply()
        print(f"[check] pods/node: {per_node} (target {self.args.pods_per_node} each)")

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cluster_name")
    ap.add_argument("num_nodes", type=int)
    ap.add_argument("pods_per_node", type=int)
    ap.add_argument("num_replicaset", type=int)
    ap.add_argument("--max-pods-per-node", type=int, default=32)
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
    ap.add_argument("--wait-each", action="store_true", default=False)
    ap.add_argument("--wait-timeout", default="10s")
    ap.add_argument("--settle-timeout", default="5s")
    ap.add_argument("--cpu-interval", default="50m,500m")
    ap.add_argument("--mem-interval", default="64Mi,1024Mi")
    ap.add_argument("--seed-file", default=None)
    ap.add_argument("--simulate", action="store_true", default=False)
    ap.add_argument("--simulate-stop-simulation-if-unschedulable", action="store_true", default=False)
    ap.add_argument("--simulate-add-condition", choices=["all", "some_unschedulable", "all_scheduled"], default="some_unschedulable")
    ap.add_argument("--simulator-instances", type=int, default=10)
    ap.add_argument("--load-instance", action="store_true", default=False)
    args = ap.parse_args()
    
    # If no seed-file was provided, default to <cluster_name>_seeds.jsonl
    if args.seed_file is None:
        args.seed_file = f"{args.cluster_name}_seeds.jsonl"
        print(f"[kwok-fill] No --seed-file specified, using default: {args.seed_file}")
    
    load_instance_into_args(args)
    
    if args.num_priorities < 1:
        print("[kwok-fill] --num-priorities must be >= 1; forcing to 1", file=sys.stderr)
        args.num_priorities = 1

    runner = KwokTestGeneratorRunner(args)
    runner.setup_cluster()
    if args.simulate:
        runner.simulate()
    else:
        runner.run_normal()

if __name__ == "__main__":
    main()