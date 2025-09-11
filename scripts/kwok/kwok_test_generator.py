#!/usr/bin/env python3

# kwok_test_generator.py

import csv, json, time, random, argparse, re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional, Tuple, List, Dict, Any

from kwok_shared import (
    ensure_kwok_cluster, delete_kwok_nodes, create_kwok_nodes,
    ensure_namespace, ensure_priority_classes, apply_yaml,
    wait_each, wait_rs_pods, get_scheduled_and_unscheduled,
    stat_snapshot, csv_append_row, scale_replicaset,
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
    "pods_run_by_node","placed_by_priority",
    "unscheduled","scheduled",
    "pod_node",
]

@dataclass
class TestConfig:
    # cluster & namespace
    namespace: str = "test"

    # topology / priorities
    num_nodes: int = 4
    pods_per_node: int = 4
    num_replicaset_lo: int = 0
    num_replicaset_hi: int = 0
    num_priorities: int = 4

    # node capacity
    node_cpu: str = "24"      # e.g. "24" or "2500m"
    node_mem: str = "32Gi"    # e.g. "32Gi"

    # per-pod intervals (normalized to "lo,hi" strings)
    cpu_interval: Optional[str] = "100m,1000m"
    mem_interval: Optional[str] = "128Mi,2048Mi"

    # utilization target
    util: float = 0.9
    util_tolerance: float = 0.01

    # waits
    wait_mode: Optional[str] = "ready"  # None/"none","exist","ready","running"
    wait_timeout: str = "5s"
    settle_timeout: str = "5s"

    # internal: remember where this came from
    source_file: Optional[Path] = field(default=None, repr=False)


class KwokTestGenerator:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.ctx: Optional[str] = None
        self.results_dir = Path(args.results_dir)
        self.failed_cfg_f = Path(args.failed_kwok_configs_file)
        self.failed_seeds_f = Path(args.failed_seeds_file)
        self.max_rows_per_file = int(args.max_rows_per_file)

        # Collect rows for summary if --test OR a single --seed is provided
        self._collect_test: bool = bool(args.test or (args.seed is not None))
        self._test_rows: List[Dict[str, Any]] = []

        # filesystem prep
        self.failed_cfg_f.parent.mkdir(parents=True, exist_ok=True)
        self.failed_seeds_f.parent.mkdir(parents=True, exist_ok=True)
        self.results_dir.mkdir(parents=True, exist_ok=True)
        self.failed_cfg_f.touch(exist_ok=True)
        self.failed_seeds_f.touch(exist_ok=True)

    # ---------- CSV helpers ----------
    @staticmethod
    def _ensure_csv_with_header(path: Path, header: List[str]) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        if not path.exists():
            with open(path, "w", encoding="utf-8", newline="") as f:
                csv.DictWriter(f, fieldnames=header).writeheader()

    @staticmethod
    def _append_rows(path: Path, header: List[str], rows: List[Dict[str, Any]]) -> None:
        KwokTestGenerator._ensure_csv_with_header(path, header)
        if not rows:
            return
        with open(path, "a", encoding="utf-8", newline="") as f:
            wr = csv.DictWriter(f, fieldnames=header, extrasaction="ignore")
            wr.writerows(rows)

    @staticmethod
    def _count_csv_rows(path: Path) -> int:
        """Count data rows (excluding header)."""
        if not path.exists():
            return 0
        with open(path, "r", encoding="utf-8", newline="") as f:
            rd = csv.DictReader(f)
            return sum(1 for _ in rd)

    # ---------- YAML loader ----------
    @staticmethod
    def _pick_runner_doc(docs: List[Any]) -> Dict[str, Any]:
        for d in docs:
            if isinstance(d, dict) and (str(d.get("kind","")) == "KwokRunConfiguration"):
                return d
        raise KeyError("No Kwok runner settings found in YAML (expected 'kind: KwokRunConfiguration').")

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
        rc.num_replicaset_lo = coerce_int(runner_doc, "num_replicaset_lo", rc.num_replicaset_lo)
        rc.num_replicaset_hi = coerce_int(runner_doc, "num_replicaset_hi", rc.num_replicaset_hi)
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

    # ---------- result files rotation / discovery ----------
    def _segment_list(self, stem: str) -> List[tuple[int, Path]]:
        """
        Return [(idx, path), ...] for existing segments.
        - Treat legacy '<stem>.csv' as idx=1
        - Treat '<stem>_<N>.csv' as idx=N (N>=1)
        Sorted by idx.
        """
        segs: List[tuple[int, Path]] = []
        base = self.results_dir / f"{stem}.csv"
        if base.exists():
            segs.append((1, base))

        pat = re.compile(rf"^{re.escape(stem)}_(\d+)\.csv$")
        for p in self.results_dir.glob(f"{stem}_*.csv"):
            m = pat.match(p.name)
            if m:
                segs.append((int(m.group(1)), p))

        # de-dup idx=1 if both base and _1 exist; prefer base as #1
        uniq: dict[int, Path] = {}
        for idx, p in sorted(segs, key=lambda t: t[0]):
            if idx not in uniq:
                uniq[idx] = p
        return sorted(uniq.items(), key=lambda t: t[0])


    def _pick_results_csv_to_write(self, stem: str, rows_to_add: int = 1) -> Path:
        """
        Choose a segment to write 'rows_to_add' rows to.
        - Creates '<stem>_1.csv' if no segments exist.
        - Rotates to '<stem>_<last+1>.csv' when current_rows + rows_to_add > limit.
        Always ensures the header on the chosen file.
        """
        segs = self._segment_list(stem)
        if not segs:
            target = self.results_dir / f"{stem}_1.csv"
            self._ensure_csv_with_header(target, RESULTS_HEADER)
            print(f"[kwok-test-gen] starting new segment: {target.name}")
            return target

        last_idx, last_path = segs[-1]
        rows = self._count_csv_rows(last_path)

        # Rotate if we would exceed the limit with this write.
        if rows + rows_to_add > self.max_rows_per_file:
            next_path = self.results_dir / f"{stem}_{last_idx + 1}.csv"
            self._ensure_csv_with_header(next_path, RESULTS_HEADER)
            print(f"[kwok-test-gen][rotate] {last_path.name} is full ({rows}/{self.max_rows_per_file}); "
                f"switching to {next_path.name}")
            return next_path

        # Otherwise use the last segment
        self._ensure_csv_with_header(last_path, RESULTS_HEADER)
        return last_path


    def _load_seen_results_all(self, stem: str) -> set[int]:
        """
        Load 'seen' seeds across legacy '<stem>.csv' and all rotated '<stem>_<N>.csv'.
        """
        seen: set[int] = set()
        for _, p in self._segment_list(stem):
            try:
                with open(p, "r", encoding="utf-8", newline="") as fh:
                    for row in csv.DictReader(fh):
                        s = (row.get("seed") or "").strip()
                        if s:
                            seen.add(int(s))
            except Exception:
                pass
        return seen
    
    
    def _result_file_index(self, stem: str, file: Path) -> int:
        """
        Rotation index for files strictly matching: <stem>_<N>.csv  (N >= 1)
        Returns -1 if not a rotated file. (Legacy <stem>.csv is *not* matched here.)
        """
        m = re.match(rf"^{re.escape(stem)}_(\d+)\.csv$", file.name)
        return int(m.group(1)) if m else -1

    def _result_files_for_cfg(self, stem: str) -> List[Path]:
        """
        Only consider rotated files (<stem>_<N>.csv) for writing/rotation.
        """
        files = [p for p in self.results_dir.glob(f"{stem}_*.csv")
                if self._result_file_index(stem, p) >= 1]
        files.sort(key=lambda p: self._result_file_index(stem, p))
        return files


    # ---------- seed helpers ----------
    @staticmethod
    def _pick_num_replicaset_for_seed(seed_int: int, lo: int, hi: int, total_pods: int) -> int:
        lo = max(0, int(lo))
        hi = max(lo, int(hi))
        hi = min(hi, int(total_pods))
        rng = random.Random(seed_int ^ 0x9E3779B97F4A7C15)
        return rng.randint(lo, hi)

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
                        seeds.append(int(s))
        else:
            with open(path, "r", encoding="utf-8") as f:
                for line in f:
                    s = line.strip()
                    if s:
                        seeds.append(int(s))
        return seeds

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
            if wait_mode == "running":
                _ = wait_each(self.ctx, "pod", name, ns, wait_timeout_s, wait_mode)
        if wait_mode in ("exist", "ready"):
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
            specs.append({"name": rsname, "replicas": int(count), "cpu_m": int(avg_mc), "mem_mi": int(avg_mi), "priority": pc})
            apply_yaml(self.ctx, yaml_kwok_rs(ns, rsname, 0, qcpu, qmem, pc))
            for r in range(1, int(count) + 1):
                scale_replicaset(self.ctx, ns, rsname, r)
                if wait_mode == "running":
                    _ = wait_rs_pods(self.ctx, rsname, ns, wait_timeout_s, wait_mode)
        if wait_mode in ("exist", "ready"):
            for spec in specs:
                _ = wait_each(self.ctx, "rs", spec["name"], ns, wait_timeout_s, wait_mode)
        return specs

    @staticmethod
    def _prio_for_step_desc(step: int, num_prios: int) -> int:
        if num_prios <= 1:
            return 1
        return num_prios - ((step - 1) % num_prios)

    # ---------- per-seed execution ----------
    def _run_one_seed(
        self,
        cfg: Path,
        namespace: str,
        result_stem_csv: Path,  # base path; rotation handled inside
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
        exists = seed_int in seen

        # Save policy:
        # - NEVER save in --test mode
        # - If seed already exists, ALWAYS skip saving and warn
        save_allowed = (not self.args.test) and (not exists)

        if exists:
            print(f"[kwok-test-gen][warn] cfg={cfg.stem} seed={seed_int} already present; will run but NOT save")

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
                if self._collect_test:
                    self._test_rows.append({
                        "kwok_config": str(cfg),
                        "seed": seed_int,
                        "scheduled": 0,
                        "unscheduled": 0,
                        "note": "totals outside util tolerance",
                    })
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
            scheduled_names = sorted([name for (name, _) in scheduled_pairs])
            scheduled_braced = "{" + ",".join(scheduled_names) + "}"
            unscheduled_braced = "{" + ",".join(sorted(unschedulable_names)) + "}"

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

            # record test row (for --test or any single-seed run)
            if self._collect_test:
                self._test_rows.append({
                    "kwok_config": str(cfg),
                    "seed": seed_int,
                    "scheduled": int(len(scheduled_pairs)),
                    "unscheduled": int(len(unschedulable_names)),
                    "note": "",
                })

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
                "unscheduled": unscheduled_braced,
                "scheduled": scheduled_braced,
                "pod_node": json.dumps(pod_node_list, separators=(",", ":")),
            }

            # Save if allowed (rotation-aware)
            if save_allowed:
                dest_csv = self._pick_results_csv_to_write(cfg.stem, rows_to_add=1)
                # ensure header in case kwok_shared.csv_append_row doesn't add it
                self._ensure_csv_with_header(dest_csv, RESULTS_HEADER)
                csv_append_row(dest_csv, RESULTS_HEADER, result_row)
                print(f"[kwok-test-gen] cfg={cfg.stem} seed={seed_int} -> appended to {dest_csv.name} "
                    f"(unsched={len(unschedulable_names)})")
            else:
                print(f"[kwok-test-gen] cfg={cfg.stem} seed={seed_int} -> NOT saved")

        except Exception as e:
            if self._collect_test:
                self._test_rows.append({
                    "kwok_config": str(cfg),
                    "seed": seed_int,
                    "scheduled": None,
                    "unscheduled": None,
                    "note": f"error: {e}",
                })
            with open(self.failed_seeds_f, "a", encoding="utf-8") as f:
                f.write(f"{cfg}\t{seed_int}\t{e}\n")

    # ---------- summary ----------
    def _print_test_summary(self) -> None:
        if not self._test_rows:
            print("\n[kwok-test-gen][summary] No test results.")
            return
        rows = sorted(self._test_rows, key=lambda r: (str(r["kwok_config"]), int(r.get("seed") or 0)))
        cfg_w = max(len("kwok_config"), *(len(Path(r["kwok_config"]).name) for r in rows))
        seed_w = max(len("seed"), *(len(str(r["seed"])) for r in rows))
        sch_w = len("scheduled")
        uns_w = len("unscheduled")

        print("\n==================== TEST SUMMARY ====================")
        header = f"{'kwok_config'.ljust(cfg_w)}  {'seed'.rjust(seed_w)}  {'scheduled'.rjust(sch_w)}  {'unscheduled'.rjust(uns_w)}  note"
        print(header)
        print("-" * len(header))
        for r in rows:
            cfg_name = Path(r["kwok_config"]).name.ljust(cfg_w)
            seed = str(r["seed"]).rjust(seed_w)
            sch = ("" if r["scheduled"] is None else str(r["scheduled"])).rjust(sch_w)
            uns = ("" if r["unscheduled"] is None else str(r["unscheduled"])).rjust(uns_w)
            note = str(r.get("note",""))
            print(f"{cfg_name}  {seed}  {sch}  {uns}  {note}")
        print("======================================================")

    # ---------- per-config ----------
    def _run_for_config(self, cfg: Path) -> None:
        try:
            rc = self._load_run_config(cfg)
        except Exception as e:
            print(f"[kwok-test-gen][config-failed] {cfg}: {e}")
            with open(self.failed_cfg_f, "a", encoding="utf-8") as f:
                f.write(str(cfg) + "\n")
            if self._collect_test:
                self._test_rows.append({
                    "kwok_config": str(cfg),
                    "seed": self.args.seed,
                    "scheduled": None,
                    "unscheduled": None,
                    "note": "config load failed",
                })
            return

        self.ctx = f"kwok-{self.args.cluster_name}"

        # recreate cluster from config (always)
        try:
            ensure_kwok_cluster(self.ctx, cfg, recreate=True)
        except Exception as e:
            print(f"[kwok-test-gen][config-failed] ensure cluster {cfg}: {e}")
            with open(self.failed_cfg_f, "a", encoding="utf-8") as f:
                f.write(str(cfg) + "\n")
            if self._collect_test:
                self._test_rows.append({
                    "kwok_config": str(cfg),
                    "seed": self.args.seed,
                    "scheduled": None,
                    "unscheduled": None,
                    "note": "ensure cluster failed",
                })
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

        # rotation-aware: we still pass a "stem" base path; actual writing picks rotated file
        result_stem_csv = self.results_dir / f"{cfg.stem}.csv"
        seen = self._load_seen_results_all(cfg.stem)

        # seed-only path
        if self.args.seed is not None:
            s = int(self.args.seed)
            num_rs = self._pick_num_replicaset_for_seed(s, rc.num_replicaset_lo, rc.num_replicaset_hi, total_pods)
            self._run_one_seed(
                cfg, rc.namespace, result_stem_csv, seen, s,
                rc.num_nodes, rc.pods_per_node, num_rs, rc.num_priorities,
                rc.node_cpu, rc.node_mem, rc.util, rc.util_tolerance,
                node_mc, node_mi, total_pods,
                cpu_interval_used, mem_interval_used,
                wait_mode, rc.wait_timeout, rc.settle_timeout, wait_timeout_s, settle_timeout_s,
            )
            return

        # count path
        if self.args.count is not None and int(self.args.count) >= -1:
            to_make = int(self.args.count)
            rng = random.Random(int(time.time_ns()))
            made = 0
            while to_make == -1 or made < to_make:
                s = rng.randint(1, 2**63 - 1)
                num_rs = self._pick_num_replicaset_for_seed(s, rc.num_replicaset_lo, rc.num_replicaset_hi, total_pods)
                self._run_one_seed(
                    cfg, rc.namespace, result_stem_csv, seen, s,
                    rc.num_nodes, rc.pods_per_node, num_rs, rc.num_priorities,
                    rc.node_cpu, rc.node_mem, rc.util, rc.util_tolerance,
                    node_mc, node_mi, total_pods,
                    cpu_interval_used, mem_interval_used,
                    wait_mode, rc.wait_timeout, rc.settle_timeout, wait_timeout_s, settle_timeout_s,
                )
                if to_make != -1:
                    made += 1
            return

        # seed-file path
        if self.args.seed_file:
            seeds_list = self._read_seeds_file(Path(self.args.seed_file))
            total_seeds = len(seeds_list)
            for idx, s in enumerate(seeds_list, start=1):
                remaining = total_seeds - idx + 1
                print("\n======================================================================================================")
                print(f"[kwok-test-gen] starting seed={s} for config={cfg.name} (remaining seeds in file: {remaining})")
                print("=======================================================================================")
                s = int(s)
                num_rs = self._pick_num_replicaset_for_seed(s, rc.num_replicaset_lo, rc.num_replicaset_hi, total_pods)
                self._run_one_seed(
                    cfg, rc.namespace, result_stem_csv, seen, s,
                    rc.num_nodes, rc.pods_per_node, num_rs, rc.num_priorities,
                    rc.node_cpu, rc.node_mem, rc.util, rc.util_tolerance,
                    node_mc, node_mi, total_pods,
                    cpu_interval_used, mem_interval_used,
                    wait_mode, rc.wait_timeout, rc.settle_timeout, wait_timeout_s, settle_timeout_s,
                )
            return

        print(f"[kwok-test-gen] No seeds provided for cfg={cfg.name} (use --seed / --seed-file / --count).")

    # ---------- top-level ----------
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

        # test-mode constraints (still enforced)
        if self.args.test:
            if self.args.seed is None:
                raise SystemExit("--test requires exactly one --seed")
            if self.args.count is not None or self.args.seed_file:
                raise SystemExit("--test cannot be combined with --count or --seed-file")

        cfgs = self._get_kwok_configs(self.args.kwok_config_dir)
        total_cfgs = len(cfgs)
        print(f"[kwok-test-gen] configs={total_cfgs}")
        for idx, cfg in enumerate(cfgs, start=1):
            remaining_cfgs = total_cfgs - idx + 1
            print("\n======================================================================================================")
            print(f"[kwok-test-gen] config={cfg}  (remaining configs: {remaining_cfgs})")
            print("======================================================================================================")
            self._run_for_config(cfg)

        # Always print summary if collecting (either --test OR single --seed)
        if self._collect_test:
            self._print_test_summary()


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

    # rotation
    ap.add_argument("--max-rows-per-file", dest="max_rows_per_file", type=int, default=2,
                    help="Maximum number of data rows per results CSV before rotating to <name>_N.csv")

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
    ap.add_argument("--divide-scheduled-unscheduled", action="store_true",
                    help="Also create two per-pod CSVs named '<cfg>_scheduled.csv' and '<cfg>_unscheduled.csv'")
    ap.add_argument("--count", type=int, default=None,
                    help="Generate random seeds; -1=infinite.")

    # Test summary mode — and note: test mode disables all saving
    ap.add_argument("--test", action="store_true",
                    help="Test mode: requires --seed; runs each kwok-config and prints a per-config summary "
                         "of scheduled vs unscheduled at the end. No results are saved in test mode.")

    # Note: other args are provided in YAML per-config
    return ap


def main():
    args = build_argparser().parse_args()
    runner = KwokTestGenerator(args)
    runner.run()


if __name__ == "__main__":
    main()
