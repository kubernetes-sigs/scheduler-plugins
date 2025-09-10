#!/usr/bin/env python3

# kwok_test_generator.py

import csv, json, time, random, argparse
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional, Tuple, List, Dict, Any

from kwok_shared import (
    ensure_kwok_cluster, delete_kwok_nodes, create_kwok_nodes,
    ensure_namespace, ensure_priority_classes, apply_yaml,
    wait_each, wait_rs_pods, get_scheduled_and_unscheduled,
    stat_snapshot, csv_append_row,
    cpu_m_str_to_int, mem_str_to_mib_int, cpu_m_int_to_str, mem_mi_int_to_str,
    parse_cpu_interval, parse_mem_interval, resolve_interval_or_fallback,
    gen_parts_constrained, partition_int, format_interval_cpu, format_interval_mem,
    yaml_kwok_pod, yaml_kwok_rs, parse_timeout_s, get_timestamp,
    count_running_by_priority,
    coerce_str, coerce_int, coerce_float, coerce_wait_mode, normalize_interval
)

RESULTS_HEADER = [
    "timestamp","kwok_config","seed",
    "num_nodes","pods_per_node","num_replicaset","num_priorities",
    "node_cpu","node_mem",
    "cpu_interval","mem_interval",
    "util","util_tolerance",
    "util_run_cpu","util_run_mem","cpu_m_run","mem_b_run",
    "wait_mode","wait_timeout","settle_timeout",
    "scheduled_count","unscheduled_count",
    "pods_run_by_node","placed_by_priority","pod_node",
]


# ---------- Strongly-typed per-config settings (with defaults) ----------
@dataclass
class TestConfig:
    # cluster & namespace
    namespace: str = "crossnode-test"

    # topology / priorities
    num_nodes: int = 4
    pods_per_node: int = 4
    num_replicaset: int = 0
    num_priorities: int = 4

    # node capacity
    node_cpu: str = "24"      # e.g. "24" or "2500m"
    node_mem: str = "32Gi"    # e.g. "32Gi"

    # per-pod intervals (normalized to "lo,hi" strings)
    cpu_interval: Optional[str] = "100m,1000m"
    mem_interval: Optional[str] = "128Mi,2048Mi"

    # utilization target
    util: float = 0.9
    util_tolerance: float = 0.1

    # waits
    wait_mode: Optional[str] = "ready"  # one of None/"none","exist","ready","running"
    wait_timeout: str = "120s"
    settle_timeout: str = "30s"

    # internal: remember where this came from
    source_file: Optional[Path] = field(default=None, repr=False)


class KwokTestGenerator:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.ctx: Optional[str] = None   # built per-config from YAML cluster_name
        self.results_dir = Path(args.results_dir)
        self.failed_cfg_f = Path(args.failed_kwok_configs_file)
        self.failed_seeds_f = Path(args.failed_seeds_file)

        # filesystem prep
        self.failed_cfg_f.parent.mkdir(parents=True, exist_ok=True)
        self.failed_seeds_f.parent.mkdir(parents=True, exist_ok=True)
        self.results_dir.mkdir(parents=True, exist_ok=True)
        self.failed_cfg_f.touch(exist_ok=True)
        self.failed_seeds_f.touch(exist_ok=True)

    # ---------- YAML loader (robust & flexible) ----------

    @staticmethod
    def _pick_runner_doc(docs: List[Any]) -> Dict[str, Any]:
        """
        Find the doc that contains run settings: doc with kind: KwokRunConfiguration
        """
        known_keys = {
            "cluster_name","namespace","num_nodes","pods_per_node",
            "num_replicaset","num_priorities","node_cpu","node_mem",
            "cpu_interval","cpu_interval_lo","cpu_interval_hi",
            "mem_interval","mem_interval_lo","mem_interval_hi",
            "util","util_tolerance","wait_mode","wait_timeout","settle_timeout"
        }
        for d in docs:
            if isinstance(d, dict) and (str(d.get("kind","")) == "KwokRunConfiguration"):
                return d
        raise KeyError("No Kwok runner settings found in YAML (expected 'kind: KwokRunConfiguration', a 'kwok_run:' section, or known keys).")

    @staticmethod
    def _load_run_config(cfg_path: Path) -> TestConfig:
        import yaml
        with open(cfg_path, "r", encoding="utf-8") as f:
            docs = list(yaml.safe_load_all(f)) or []
        runner_doc = KwokTestGenerator._pick_runner_doc(docs)

        rc = TestConfig(source_file=cfg_path)
        rc.namespace      = coerce_str(runner_doc, "namespace", rc.namespace)
        rc.num_nodes      = coerce_int(runner_doc, "num_nodes", rc.num_nodes)
        rc.pods_per_node  = coerce_int(runner_doc, "pods_per_node", rc.pods_per_node)
        rc.num_replicaset = coerce_int(runner_doc, "num_replicaset", rc.num_replicaset)
        rc.num_priorities = coerce_int(runner_doc, "num_priorities", rc.num_priorities)
        rc.node_cpu       = coerce_str(runner_doc, "node_cpu", rc.node_cpu)
        rc.node_mem       = coerce_str(runner_doc, "node_mem", rc.node_mem)
        rc.util           = coerce_float(runner_doc, "util", rc.util)
        rc.util_tolerance = coerce_float(runner_doc, "util_tolerance", rc.util_tolerance)
        rc.wait_mode      = coerce_wait_mode(runner_doc, "wait_mode", rc.wait_mode)
        rc.wait_timeout   = coerce_str(runner_doc, "wait_timeout", rc.wait_timeout)
        rc.settle_timeout = coerce_str(runner_doc, "settle_timeout", rc.settle_timeout)
        rc.cpu_interval = normalize_interval(
            runner_doc, ("cpu_interval", "cpu_interval_lo", "cpu_interval_hi"), allow_none=True
        ) or rc.cpu_interval
        rc.mem_interval = normalize_interval(
            runner_doc, ("mem_interval", "mem_interval_lo", "mem_interval_hi"), allow_none=True
        ) or rc.mem_interval

        return rc

    @staticmethod
    def _read_seeds_file(path: Path) -> List[int]:
        seeds: List[int] = []
        if not path.exists():
            return seeds
        if path.suffix.lower() == ".csv":
            with open(path, "r", encoding="utf-8", newline="") as f:
                rd = csv.DictReader(f)
                for row in rd:
                    s = (row.get("seed") or "").strip()
                    if s:
                        seeds.append(seed_to_int(s))
        else:
            with open(path, "r", encoding="utf-8") as f:
                for line in f:
                    s = line.strip()
                    if s:
                        seeds.append(seed_to_int(s))
        return seeds

    @staticmethod
    def _load_seen_results(result_csv: Path) -> set[int]:
        seen: set[int] = set()
        if not result_csv.exists():
            return seen
        try:
            with open(result_csv, "r", encoding="utf-8", newline="") as f:
                rd = csv.DictReader(f)
                for row in rd:
                    s = (row.get("seed") or "").strip()
                    if s:
                        seen.add(seed_to_int(s))
        except Exception:
            pass
        return seen

    @staticmethod
    def _rewrite_results_without_seed(result_csv: Path, target_seed: int) -> None:
        if not result_csv.exists():
            return
        rows: List[dict] = []
        with open(result_csv, "r", encoding="utf-8", newline="") as f:
            rd = csv.DictReader(f)
            hdr = rd.fieldnames or []
            for row in rd:
                s = seed_to_int(row.get("seed", 0))
                if s != target_seed:
                    rows.append(row)
        with open(result_csv, "w", encoding="utf-8", newline="") as f:
            wr = csv.DictWriter(f, fieldnames=hdr)
            wr.writeheader()
            wr.writerows(rows)

    @staticmethod
    def _prio_for_step_desc(step: int, num_prios: int) -> int:
        if num_prios <= 1:
            return 1
        return num_prios - ((step - 1) % num_prios)

    @staticmethod
    def _check_seed_file_exists(seed_file: Optional[str]) -> None:
        if not seed_file:
            return
        sf = Path(seed_file)
        if not sf.exists() or not sf.is_file():
            raise SystemExit(f"--seed-file not found or not a regular file: {sf}")
        try:
            with open(sf, "r", encoding="utf-8"):
                pass
        except Exception as e:
            raise SystemExit(f"--seed-file not readable: {sf} ({e})")

    @staticmethod
    def _get_kwok_configs(dir_path: str) -> List[Path]:
        cfg_dir = Path(dir_path)
        if not cfg_dir.exists():
            raise SystemExit(f"--kwok-config-dir not found: {cfg_dir}")
        cfgs = sorted([p for p in cfg_dir.glob("**/*") if p.is_file() and p.suffix.lower() in (".yaml", ".yml")])
        if not cfgs:
            raise SystemExit("No KWOK configs found (must be at least one).")
        return cfgs

    # ---------- k8s workload builders ----------
    def _apply_standalone_pods(
        self, ns: str, rng: random.Random, total_pods: int,
        cpu_parts: List[int], mem_parts: List[int],
        num_priorities: int, wait_mode: Optional[str], wait_timeout_s: int
    ) -> List[Dict[str, object]]:
        specs: List[Dict[str, object]] = []
        names: List[str] = []
        for i in range(total_pods):
            prio = rng.randint(1, max(1, num_priorities))
            pc = f"p{prio}"
            name = f"pod-{i+1:03d}-{pc}"
            cpu_m = max(1, int(cpu_parts[i]))
            mem_mi = max(1, int(mem_parts[i]))
            qcpu = cpu_m_int_to_str(cpu_m)
            qmem = mem_mi_int_to_str(mem_mi)
            apply_yaml(self.ctx, yaml_kwok_pod(ns, name, qcpu, qmem, pc))
            names.append(name)
            specs.append({"name": name, "cpu_m": cpu_m, "mem_mi": mem_mi, "priority": pc})
        if wait_mode:
            for name in names:
                _ = wait_each(self.ctx, "pod", name, ns, wait_timeout_s, wait_mode)
        return specs

    def _apply_replicasets(
        self, ns: str, rng: random.Random, total_pods: int,
        cpu_parts: List[int], mem_parts: List[int],
        num_replicaset: int, num_priorities: int,
        cpu_interval_used: Optional[Tuple[int,int]],
        mem_interval_used: Optional[Tuple[int,int]],
        wait_mode: Optional[str], wait_timeout_s: int
    ) -> List[Dict[str, object]]:
        specs: List[Dict[str, object]] = []
        replicas = partition_int(total_pods, num_replicaset, 1, rng, variance=50)
        offset = 0
        for i, count in enumerate(replicas, start=1):
            pc = f"p{self._prio_for_step_desc(i, num_priorities)}"
            rsname = f"rs-{i:02d}-{pc}"
            slice_cpu = cpu_parts[offset:offset+count]
            slice_mem = mem_parts[offset:offset+count]
            offset += count

            avg_mc = max(1, sum(slice_cpu) // count)
            avg_mi = max(1, sum(slice_mem) // count)
            if cpu_interval_used:
                lo, hi = cpu_interval_used
                avg_mc = min(max(avg_mc, lo), hi)
            if mem_interval_used:
                lo, hi = mem_interval_used
                avg_mi = min(max(avg_mi, lo), hi)

            qcpu = cpu_m_int_to_str(int(avg_mc))
            qmem = mem_mi_int_to_str(int(avg_mi))
            apply_yaml(self.ctx, yaml_kwok_rs(ns, rsname, int(count), qcpu, qmem, pc))
            specs.append({"name": rsname, "replicas": int(count), "cpu_m": int(avg_mc), "mem_mi": int(avg_mi), "priority": pc})

            if wait_mode:
                _ = wait_rs_pods(self.ctx, rsname, ns, wait_timeout_s, wait_mode)
        return specs

    # ---------- per-seed execution ----------
    def _run_one_seed(
        self,
        cfg: Path,
        namespace: str,
        result_csv: Path,
        seen: set[int],
        seed_int: int,
        num_nodes: int, pods_per_node: int, num_replicaset: int, num_priorities: int,
        node_cpu: str, node_mem: str,
        util: float, util_tol: float,
        node_mc: int, node_mi: int, total_pods: int,
        cpu_interval_used: Optional[Tuple[int,int]],
        mem_interval_used: Optional[Tuple[int,int]],
        wait_mode: Optional[str], wait_timeout_raw: str, settle_timeout_raw: str,
        wait_timeout_s: int, settle_timeout_s: int,
    ) -> None:
        if (not self.args.override) and (seed_int in seen):
            print(f"[kwok-test-gen] cfg={cfg.stem} seed={seed_int} -> already present, skipping; use --override to replace")
            return

        replace_existing = self.args.override and (seed_int in seen)
        if replace_existing:
            print(f"[kwok-test-gen] cfg={cfg.stem} seed={seed_int} -> replacing existing entry")
        else:
            print(f"[kwok-test-gen] cfg={cfg.stem} seed={seed_int} -> running a new seed")

        try:
            tgt_mc_cluster = int(node_mc * util) * num_nodes
            tgt_mi_cluster = int(node_mi * util) * num_nodes

            rng = random.Random(seed_int)
            cpu_parts = gen_parts_constrained(tgt_mc_cluster, total_pods, rng, cpu_interval_used, "random", variance=50)
            mem_parts = gen_parts_constrained(tgt_mi_cluster, total_pods, rng, mem_interval_used, "random", variance=50)

            # Verify util window (cluster totals)
            alloc_cpu = node_mc * num_nodes
            alloc_mem = node_mi * num_nodes
            sum_cpu = int(sum(cpu_parts))
            sum_mem = int(sum(mem_parts))
            lo_cpu = int(alloc_cpu * (util - util_tol))
            hi_cpu = int(alloc_cpu * (util + util_tol))
            lo_mem = int(alloc_mem * (util - util_tol))
            hi_mem = int(alloc_mem * (util + util_tol))

            if not (lo_cpu <= sum_cpu <= hi_cpu) or not (lo_mem <= sum_mem <= hi_mem):
                with open(self.failed_seeds_f, "a", encoding="utf-8") as f:
                    f.write(
                        f"{cfg}\t{seed_int}\tTotals outside util tolerance "
                        f"(cpu={sum_cpu}, range=[{lo_cpu},{hi_cpu}]; "
                        f"mem={sum_mem}, range=[{lo_mem},{hi_mem}])\n"
                    )
                print(
                    f"[kwok-test-gen][seed-failed] cfg={cfg.stem} seed={seed_int} -> "
                    f"totals outside util tolerance "
                    f"(cpu={sum_cpu}, range=[{lo_cpu},{hi_cpu}]; "
                    f"mem={sum_mem}, range=[{lo_mem},{hi_mem}])"
                )
                return

            # Isolate run
            ensure_namespace(self.ctx, namespace, recreate=True)

            pod_specs: List[Dict[str, object]] = []
            rs_specs: List[Dict[str, object]] = []

            if num_replicaset > 0:
                rs_specs = self._apply_replicasets(
                    namespace, rng, total_pods, cpu_parts, mem_parts,
                    num_replicaset, num_priorities, cpu_interval_used, mem_interval_used,
                    wait_mode, wait_timeout_s
                )
            else:
                pod_specs = self._apply_standalone_pods(
                    namespace, rng, total_pods, cpu_parts, mem_parts,
                    num_priorities, wait_mode, wait_timeout_s
                )

            # settle & collect
            _, scheduled_pairs, unschedulable_names = get_scheduled_and_unscheduled(
                self.ctx, namespace, expected=total_pods, settle_timeout=settle_timeout_s
            )
            scheduled_by_name = {name: node for (name, node) in scheduled_pairs}
            placed_by_priority = count_running_by_priority(self.ctx, namespace)
            snap = stat_snapshot(self.ctx, namespace, expected=total_pods, settle_timeout=settle_timeout_s)

            # pod_node aggregation
            standalone_by_name = {p["name"]: p for p in (pod_specs or [])}
            rs_by_name = {r["name"]: r for r in (rs_specs or [])}
            all_event_pods = set(scheduled_by_name.keys()) | set(unschedulable_names)
            pod_node_list: List[Dict[str, object]] = []
            for pname in sorted(all_event_pods):
                node = scheduled_by_name.get(pname, "")
                if pname in standalone_by_name:
                    spec = standalone_by_name[pname]
                    pod_node_list.append({
                        "name": pname,
                        "cpu_m": int(spec["cpu_m"]),
                        "mem_mi": int(spec["mem_mi"]),
                        "priority": str(spec["priority"]),
                        "node": node,
                    })
                else:
                    rsname = pname.rsplit("-", 1)[0] if "-" in pname else ""
                    r = rs_by_name.get(rsname, {})
                    pod_node_list.append({
                        "name": pname,
                        "cpu_m": int(r.get("cpu_m", 0)),
                        "mem_mi": int(r.get("mem_mi", 0)),
                        "priority": str(r.get("priority", "")),
                        "node": node,
                    })

            if self.args.save_only_unscheduled and len(unschedulable_names) == 0:
                return

            result_row = {
                "timestamp": get_timestamp(),
                "kwok_config": str(cfg),
                "seed": str(seed_int),
                "num_nodes": num_nodes,
                "pods_per_node": pods_per_node,
                "num_replicaset": num_replicaset,
                "num_priorities": num_priorities,
                "node_cpu": node_cpu,
                "node_mem": node_mem,
                "cpu_interval": format_interval_cpu(cpu_interval_used) if cpu_interval_used else "",
                "mem_interval": format_interval_mem(mem_interval_used) if mem_interval_used else "",
                "util": util,
                "util_tolerance": util_tol,
                "util_run_cpu": f"{snap.cpu_run_util:.3f}",
                "util_run_mem": f"{snap.mem_run_util:.3f}",
                "cpu_m_run": int(sum(snap.cpu_req_by_node.values())),
                "mem_b_run": int(sum(snap.mem_req_by_node.values())),
                "wait_mode": (wait_mode or ""),
                "wait_timeout": wait_timeout_raw,
                "settle_timeout": settle_timeout_raw,
                "scheduled_count": int(len(scheduled_pairs)),
                "unscheduled_count": int(len(unschedulable_names)),
                "pods_run_by_node": json.dumps(snap.pods_run_by_node, separators=(",", ":")),
                "placed_by_priority": json.dumps(placed_by_priority, separators=(",", ":")),
                "pod_node": json.dumps(pod_node_list, separators=(",", ":")),
            }

            if replace_existing:
                self._rewrite_results_without_seed(result_csv, seed_int)

            csv_append_row(result_csv, RESULTS_HEADER, result_row)
            seen.add(seed_int)
            print(f"[kwok-test-gen] cfg={cfg.stem} seed={seed_int} -> appended (unsched={len(unschedulable_names)})")

        except Exception as e:
            with open(self.failed_seeds_f, "a", encoding="utf-8") as f:
                f.write(f"{cfg}\t{seed_int}\t{e}\n")

    def _run_for_config(self, cfg: Path) -> None:
        try:
            rc = self._load_run_config(cfg)
        except Exception as e:
            print(f"[kwok-test-gen][config-failed] {cfg}: {e}")
            with open(self.failed_cfg_f, "a", encoding="utf-8") as f:
                f.write(str(cfg) + "\n")
            return

        self.ctx = f"kwok-{self.args.cluster_name}"

        # recreate cluster from config (always)
        try:
            ensure_kwok_cluster(self.ctx, cfg, recreate=True)
        except Exception as e:
            print(f"[kwok-test-gen][config-failed] ensure cluster {cfg}: {e}")
            with open(self.failed_cfg_f, "a", encoding="utf-8") as f:
                f.write(str(cfg) + "\n")
            return

        wait_mode = None if rc.wait_mode in (None, "none", "None", "") else str(rc.wait_mode)
        wait_timeout_s = parse_timeout_s(rc.wait_timeout)
        settle_timeout_s = parse_timeout_s(rc.settle_timeout)

        # nodes & priorities (once per config)
        DEFAULT_POD_CAP = max(30, rc.pods_per_node * 3)
        delete_kwok_nodes(self.ctx)
        create_kwok_nodes(self.ctx, rc.num_nodes, rc.node_cpu, rc.node_mem, pods_cap=DEFAULT_POD_CAP)
        ensure_priority_classes(self.ctx, rc.num_priorities, prefix="p", start=1, delete_extras=True)

        # totals & intervals (once per config)
        total_pods = rc.num_nodes * rc.pods_per_node
        node_mc = cpu_m_str_to_int(rc.node_cpu)
        node_mi = mem_str_to_mib_int(rc.node_mem)

        tgt_mc_cluster = int(node_mc * rc.util) * rc.num_nodes
        tgt_mi_cluster = int(node_mi * rc.util) * rc.num_nodes

        parsed_cpu = parse_cpu_interval(rc.cpu_interval)
        parsed_mem = parse_mem_interval(rc.mem_interval)
        cpu_interval_used, _ = resolve_interval_or_fallback(parsed_cpu, total_pods, tgt_mc_cluster, spread=0.9)
        mem_interval_used, _ = resolve_interval_or_fallback(parsed_mem, total_pods, tgt_mi_cluster, spread=0.9)

        # per-config CSV + seen
        result_csv = self.results_dir / f"{cfg.stem}.csv"
        seen = self._load_seen_results(result_csv)

        # seed generators
        if self.args.count is not None and int(self.args.count) >= -1:
            to_make = int(self.args.count)
            rng = random.Random(int(time.time_ns()))
            made = 0
            while to_make == -1 or made < to_make:
                s = rng.randint(1, 2**63 - 1)
                self._run_one_seed(
                    cfg, rc.namespace, result_csv, seen, s,
                    rc.num_nodes, rc.pods_per_node, rc.num_replicaset, rc.num_priorities,
                    rc.node_cpu, rc.node_mem, rc.util, rc.util_tolerance,
                    node_mc, node_mi, total_pods,
                    cpu_interval_used, mem_interval_used,
                    wait_mode, rc.wait_timeout, rc.settle_timeout, wait_timeout_s, settle_timeout_s,
                )
                if to_make != -1:
                    made += 1
            return

        if self.args.seed_file:
            for s in self._read_seeds_file(Path(self.args.seed_file)):
                self._run_one_seed(
                    cfg, rc.namespace, result_csv, seen, int(s),
                    rc.num_nodes, rc.pods_per_node, rc.num_replicaset, rc.num_priorities,
                    rc.node_cpu, rc.node_mem, rc.util, rc.util_tolerance,
                    node_mc, node_mi, total_pods,
                    cpu_interval_used, mem_interval_used,
                    wait_mode, rc.wait_timeout, rc.settle_timeout, wait_timeout_s, settle_timeout_s,
                )
            return

        if self.args.seed is not None:
            self._run_one_seed(
                cfg, rc.namespace, result_csv, seen, int(self.args.seed),
                rc.num_nodes, rc.pods_per_node, rc.num_replicaset, rc.num_priorities,
                rc.node_cpu, rc.node_mem, rc.util, rc.util_tolerance,
                node_mc, node_mi, total_pods,
                cpu_interval_used, mem_interval_used,
                wait_mode, rc.wait_timeout, rc.settle_timeout, wait_timeout_s, settle_timeout_s,
            )
            return

        print(f"[kwok-test-gen] No seeds provided for cfg={cfg.name} (use --seed / --seed-file / --count).")

    def run(self) -> None:
        # sanity for seed options
        if self.args.seed is not None and self.args.seed < 1:
            raise SystemExit("--seed must be a positive integer")
        if self.args.seed_file and self.args.count is not None:
            raise SystemExit("--seed-file cannot be used with --count")
        if self.args.seed is not None and self.args.count is not None:
            raise SystemExit("--seed cannot be used with --count")
        if self.args.count is not None and self.args.count < -1:
            raise SystemExit("--count must be -1 (infinite) or a positive integer")
        if self.args.count is None and self.args.seed_file:
            self._check_seed_file_exists(self.args.seed_file)

        cfgs = self._get_kwok_configs(self.args.kwok_config_dir)
        print(f"[kwok-test-gen] configs={len(cfgs)}")
        for cfg in cfgs:
            print(f"\n[kwok-test-gen] ===== {cfg} =====")
            self._run_for_config(cfg)


# ---------- CLI ----------
def build_argparser() -> argparse.ArgumentParser:
    ap = argparse.ArgumentParser(description="Unified KWOK generator+applier runner.")
    
    # cluster
    ap.add_argument("--cluster-name", dest="cluster_name", default="kwok1",
                    help="KWOK cluster name")
    
    # configs
    ap.add_argument("--kwok-config-dir", dest="kwok_config_dir",
                    default="./scripts/kwok/kwok_configs",
                    help="Directory containing one or more KWOK config YAMLs")
    ap.add_argument("--results-dir", dest="results_dir", default="./scripts/kwok/results",
                    help="Directory to store results CSV files (one per KWOK config)")
    
    # failure logs
    ap.add_argument("--failed-kwok-configs-file", dest="failed_kwok_configs_file",
                    default="./scripts/kwok/failed/failed_kwok_configs.csv",
                    help="File path to record failed KWOK configs")
    ap.add_argument("--failed-seeds-file", dest="failed_seeds_file",
                    default="./scripts/kwok/failed/failed_seeds.csv",
                    help="File path to record failed (config, seed) runs")

    # seeds
    ap.add_argument("--seed", type=int, default=None,
                    help="Run exactly this seed (per kwok-config)")
    ap.add_argument("--seed-file", dest="seed_file", default=None,
                    help="Path to seeds file (CSV with 'seed' col or newline list).")
    ap.add_argument("--override", action="store_true",
                    help="Re-run and replace results for seeds already present")
    ap.add_argument("--save-only-unscheduled", action="store_true",
                    help="Save a result row only if there were unscheduled pods")
    ap.add_argument("--count", type=int, default=None,
                    help="Generate random seeds; -1=infinite.")
    
    # NOTE: other args are provided in YAML per-config
    
    return ap


def main():
    args = build_argparser().parse_args()
    runner = KwokTestGenerator(args)
    runner.run()


if __name__ == "__main__":
    main()
