#!/usr/bin/env python3
# kwok_seed_applier.py
#
# Strict conventions:
# - KWOK config must be YAML with top-level keys:
#     wait_mode:        one of ["exist","ready","running"] or "none"/"" to disable
#     wait_timeout:     duration string like "500ms", "8s", "2m", "1h"
#     settle_timeout:   duration string like above
#   If any are missing or YAML can't be read, this config is recorded in ./failed_runs
#   and we continue with the next config.
#
# - Seed filenames must match: seeds_<...>.jsonl with resolved intervals:
#     seeds_<N>n_<PPN>ppn_<RS>rs_<PR>prio_<util>u_<tol>tol_cpu<cpu>_mem<mem>_ci<lo-hi>_mi<lo-hi>.jsonl
#   If parsing fails, record the seed file path in ./failed_runs and continue to the next seed file.
#
# Behavior:
# - For each (config, seedfile):
#   * Ensure KWOK cluster for the given context
#   * Parse fixed params from filename
#   * Recreate nodes only when topology/capacity/priorities/pod-cap change
#   * For each seed row: recreate namespace & SA, apply workload deterministically, settle, collect stats
#   * Append CSV row to "<config_name>__<seed_stem>.csv"
#
# All kubectl calls use the provided context (no reliance on current-context).

import os, re, csv, json, time, random, argparse
from pathlib import Path
import yaml, tempfile
from typing import Dict, List, Tuple, Optional

from kwok_shared import (
    check_context, ensure_namespace,
    ensure_priority_classes, delete_kwok_nodes, create_kwok_nodes,
    parse_cpu_interval, parse_mem_interval, resolve_interval_or_fallback,
    gen_parts_constrained, partition_int,
    cpu_m_str_to_int, mem_str_to_mib_int, cpu_m_int_to_str, mem_mi_int_to_str,
    yaml_kwok_pod, yaml_kwok_rs, apply_yaml, wait_each, wait_rs_pods,
    get_json_ctx, parse_timeout_s, stat_snapshot,
    get_scheduled_and_unscheduled, seed_to_int, ensure_kwok_cluster, csv_append_row
)

# ---------- filename parsing (strict) ----------

_SEED_RE = re.compile(
    r"""
    ^seeds_
    (?P<num_nodes>\d+)n_
    (?P<pods_per_node>\d+)ppn_
    (?P<num_replicaset>\d+)rs_
    (?P<num_priorities>\d+)prio_
    (?P<util>\d+(?:\.\d+)?)u_
    (?P<util_tolerance>\d+(?:\.\d+)?)tol_
    cpu(?P<node_cpu>[A-Za-z0-9_.-]+)_
    mem(?P<node_mem>[A-Za-z0-9_.-]+)_
    ci(?P<cpu_interval>[A-Za-z0-9_.-]+)_
    mi(?P<mem_interval>[A-Za-z0-9_.-]+)
    \.csv$
    """,
    re.X,
)

def _unsanitize_interval(token: str) -> str:
    # "50m-500m" -> "50m,500m" (resolved intervals required)
    parts = token.split("-")
    return ",".join(parts) if len(parts) >= 2 else token

def parse_seed_filename(seed_path: str) -> Dict[str, object]:
    name = Path(seed_path).name
    m = _SEED_RE.match(name)
    if not m:
        raise ValueError(f"Unrecognized seed filename format: {name}")
    d = m.groupdict()
    return {
        "num_nodes": int(d["num_nodes"]),
        "pods_per_node": int(d["pods_per_node"]),
        "num_replicaset": int(d["num_replicaset"]),
        "num_priorities": int(d["num_priorities"]),
        "util": float(d["util"]),
        "util_tolerance": float(d["util_tolerance"]),
        "node_cpu": d["node_cpu"],
        "node_mem": d["node_mem"],
        "cpu_interval": _unsanitize_interval(d["cpu_interval"]),
        "mem_interval": _unsanitize_interval(d["mem_interval"]),
    }

# ---------- runner ----------

class KwokSeedApplier:
    def __init__(
        self,
        cluster_name: str,
        kwok_config_dir: str,
        seed_dir: str,
        failed_runs_file: str,
        *,
        namespace: str = "crossnode-test",
        wait_mode: Optional[str] = None,    # CLI defaults (used if config omits or sets "none")
        wait_timeout: str = "10s",
        settle_timeout: str = "5s",
    ):
        self.cluster_ctx = f"kwok-{cluster_name}"
        self.kwok_config_dir = Path(kwok_config_dir)
        self.seed_dir = Path(seed_dir)
        self.failed_runs_path = Path(failed_runs_file)

        self.namespace = namespace

        # CLI defaults
        self.wait_mode = (None if wait_mode in (None, "None", "none", "") else str(wait_mode))
        self.wait_timeout_raw = wait_timeout or "0s"
        self.settle_timeout_raw = settle_timeout or "0s"
        self.wait_timeout_s = parse_timeout_s(self.wait_timeout_raw)
        self.settle_timeout_s = parse_timeout_s(self.settle_timeout_raw)

        # Dist defaults (match generator)
        self.dist_mode = "random"
        self.variance = 50

        # ensure failed_runs file exists
        self.failed_runs_path.touch(exist_ok=True)

    # ---------- IO helpers ----------
    
    @staticmethod
    def _result_path_for(seed_path: Path, kwok_config: Path) -> Path:
        # Result filename: "<config-stem>_<seed-stem>.csv" (no dirs in the printout)
        return seed_path.with_name(f"{kwok_config.stem}_{seed_path.stem}.csv")
    
    @staticmethod
    def _read_seen_result(result_path: Path) -> set[int]:
        seen: set[int] = set()
        if not result_path.exists():
            return seen
        try:
            with open(result_path, "r", encoding="utf-8", newline="") as f:
                rd = csv.DictReader(f)
                for row in rd:
                    if row and "seed" in row and row["seed"] is not None:
                        try:
                            seen.add(seed_to_int(row["seed"]))
                        except Exception:
                            pass
        except Exception:
            return set()
        return seen
    
    @staticmethod
    def _append_result_row(result_path: Path, row: Dict[str, object]) -> None:
        header = [
            "timestamp","kwok_config","seed",
            "num_nodes","pods_per_node","num_replicaset","num_priorities",
            "node_cpu","node_mem",
            "cpu_interval","mem_interval",
            "util_total","util_tolerance",
            "util_run_cpu","util_run_mem","cpu_m_run","mem_b_run",
            "wait_mode","wait_timeout","settle_timeout",
            "scheduled_count","unscheduled_count",
            "placed_by_priority","pods_run_by_node",
            "pod_node",
        ]
        csv_append_row(result_path, header, row)

    @staticmethod
    def _rs_name_from_pod(pod_name: str) -> Optional[str]:
        return pod_name.rsplit("-", 1)[0] if "-" in pod_name else None

    @staticmethod
    def _read_seed(path: Path) -> List[dict]:
        rows: List[dict] = []
        with open(path, "r", encoding="utf-8", newline="") as f:
            rd = csv.DictReader(f)
            for row in rd:
                if not row:
                    continue
                s = (row.get("seed") or "").strip()
                if not s:
                    continue
                rows.append({"seed": s})
        return rows

    def _record_fail(self, path: Path) -> None:
        with open(self.failed_runs_path, "a", encoding="utf-8") as f:
            f.write(str(path) + "\n")
            f.flush()

    # ---------- cluster/apply helpers ----------

    @staticmethod
    def _fmt_ts() -> str:
        return time.strftime("%Y/%m/%d/%H:%M:%S", time.localtime())

    @staticmethod
    def _normalize_interval_cpu(v) -> Optional[Tuple[int, int]]:
        if v is None:
            return None
        if isinstance(v, (list, tuple)) and len(v) == 2:
            return int(v[0]), int(v[1])
        if isinstance(v, str) and "," in v:
            return parse_cpu_interval(v)
        return None

    @staticmethod
    def _normalize_interval_mem(v) -> Optional[Tuple[int, int]]:
        if v is None:
            return None
        if isinstance(v, (list, tuple)) and len(v) == 2:
            return int(v[0]), int(v[1])
        if isinstance(v, str) and "," in v:
            return parse_mem_interval(v)
        return None

    @staticmethod
    def _prio_for_step_desc(step: int, num_prios: int) -> int:
        if num_prios <= 1:
            return 1
        return num_prios - ((step - 1) % num_prios)

    def _count_running_by_priority(self) -> Dict[str, int]:
        out: Dict[str, int] = {}
        pods = get_json_ctx(self.cluster_ctx, ["-n", self.namespace, "get", "pods", "-o", "json"]).get("items", [])
        for p in pods:
            if (p.get("status") or {}).get("phase", "") != "Running":
                continue
            pc = (p.get("spec") or {}).get("priorityClassName") or ""
            out[pc] = out.get(pc, 0) + 1
        return out

    def _apply_standalone_pods(
        self,
        rng: random.Random,
        total_pods: int,
        cpu_parts: List[int],
        mem_parts: List[int],
        num_priorities: int,
    ) -> List[Dict[str, object]]:
        pod_specs: List[Dict[str, object]] = []
        names = []
        for i in range(total_pods):
            prio = rng.randint(1, max(1, num_priorities))
            pc = f"p{prio}"
            name = f"pod-{i+1:03d}-{pc}"
            cpu_m = max(1, int(cpu_parts[i]))
            mem_mi = max(1, int(mem_parts[i]))
            qcpu = cpu_m_int_to_str(cpu_m)
            qmem = mem_mi_int_to_str(mem_mi)
            pod_yaml = yaml_kwok_pod(self.namespace, name, qcpu, qmem, pc)
            apply_yaml(self.cluster_ctx, pod_yaml)
            names.append(name)
            
            pod_specs.append({"name": name, "cpu_m": cpu_m, "mem_mi": mem_mi, "priority": pc})

        if self.wait_mode:
            for name in names:
                _ = wait_each(self.cluster_ctx, "pod", name, self.namespace, self.wait_timeout_s, self.wait_mode)

        return pod_specs

    def _apply_replicasets(
        self,
        rng: random.Random,
        total_pods: int,
        cpu_parts: List[int],
        mem_parts: List[int],
        num_replicaset: int,
        num_priorities: int,
        cpu_interval_used: Optional[Tuple[int, int]],
        mem_interval_used: Optional[Tuple[int, int]],
    ) -> List[Dict[str, object]]:
        rs_specs: List[Dict[str, object]] = []
        replicas = partition_int(total_pods, num_replicaset, 1, rng, self.variance)
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

            rs_yaml = yaml_kwok_rs(self.namespace, rsname, count, qcpu, qmem, pc)
            apply_yaml(self.cluster_ctx, rs_yaml)

            # record exactly what we put in the YAML template for this RS
            rs_specs.append({
                "name": rsname,
                "replicas": int(count),
                "cpu_m": int(avg_mc),
                "mem_mi": int(avg_mi),
                "priority": pc,
            })

            if self.wait_mode:
                _ = wait_rs_pods(self.cluster_ctx, rsname, self.namespace, self.wait_timeout_s, self.wait_mode)

        return rs_specs


    # ---------- strict config waits ----------

    @staticmethod
    def _read_waits_from_kwok_config(cfg: Path) -> tuple[Optional[str], str, str]:
        """
        Read a YAML document and extract strict wait settings.:
        Returns: (wait_mode or None, wait_timeout_raw, settle_timeout_raw)
        Raises on any issue so the caller can record the failure and continue.
        """
        with open(cfg, "r", encoding="utf-8") as f:
            docs = list(yaml.safe_load_all(f))

        if not docs:
            raise ValueError("Empty YAML")

        # find the 'waits' doc
        waits_doc = None
        for d in docs:
            if isinstance(d, dict) and any(k in d for k in ("wait_mode", "wait_timeout", "settle_timeout")):
                waits_doc = d
                break
        if not isinstance(waits_doc, dict):
            raise KeyError("Missing wait_mode/wait_timeout/settle_timeout document")

        def must_get(key: str) -> str:
            v = waits_doc.get(key)
            if v is None or str(v).strip() == "":
                raise KeyError(f"Missing required key '{key}'")
            return str(v).strip()

        mode_raw = must_get("wait_mode")
        wait_raw = must_get("wait_timeout")
        settle_raw = must_get("settle_timeout")

        mode_norm: Optional[str]
        if mode_raw.lower() in ("none", ""):
            mode_norm = None
        elif mode_raw not in ("exist", "ready", "running"):
            raise ValueError(f"Invalid wait_mode: {mode_raw}")
        else:
            mode_norm = mode_raw

        return mode_norm, wait_raw, settle_raw


    # ---------- main loop ----------

    def run(self) -> None:
        kwok_cfgs = sorted(
            [p for p in self.kwok_config_dir.glob("**/*") if p.is_file() and p.suffix.lower() in (".yaml",".yml")]
        )
        seed_files = sorted(
            [p for p in self.seed_dir.glob("**/seeds_*.csv") if p.is_file()]
        )
        if not kwok_cfgs:
            raise RuntimeError(f"No KWOK configs found in: {self.kwok_config_dir}")
        if not seed_files:
            raise RuntimeError(f"No seed files found in: {self.seed_dir}")

        print(f"[seed-applier] context={self.cluster_ctx} kwok_configs={len(kwok_cfgs)} seeds={len(seed_files)}")

        DEFAULT_POD_CAP = 30

        for cfg in kwok_cfgs:
            print(f"\n[seed-applier] ===== KWOK CONFIG: {cfg} =====")

            # Strict per-config waits; on failure, record and continue
            try:
                cfg_mode, cfg_wait_raw, cfg_settle_raw = self._read_waits_from_kwok_config(cfg)
            except Exception as e:
                print(f"[seed-applier][config-failed] {cfg}: {e}")
                self._record_fail(cfg)
                continue
            # Ensure cluster for this config; if fails, record and continue
            try:
                ensure_kwok_cluster(self.cluster_ctx, cfg, recreate=True)
            except Exception as e:
                print(f"[seed-applier][config-failed] ensure cluster {cfg}: {e}")
                self._record_fail(cfg)
                continue

            # Effective settings = config (strict) or CLI defaults
            self.wait_mode = (None if cfg_mode in (None, "None", "none", "") else str(cfg_mode))
            self.wait_timeout_raw = cfg_wait_raw or self.wait_timeout_raw
            self.settle_timeout_raw = cfg_settle_raw or self.settle_timeout_raw
            self.wait_timeout_s = parse_timeout_s(self.wait_timeout_raw)
            self.settle_timeout_s = parse_timeout_s(self.settle_timeout_raw)

            print(f"[seed-applier] waits: mode={self.wait_mode or 'None'} wait_timeout={self.wait_timeout_raw} settle_timeout={self.settle_timeout_raw}")

            prev = {"num_nodes": None, "node_cpu": None, "node_mem": None, "pods_cap": 0, "num_priorities": None}

            for seed_path in seed_files:
                # Parse seed filename; on failure, record and continue to next seed file
                try:
                    params = parse_seed_filename(str(seed_path.name))
                except Exception as e:
                    print(f"[seed-applier][seed-failed] {seed_path.name}: {e}")
                    self._record_fail(seed_path)
                    continue
                    
                

                num_nodes       = int(params["num_nodes"])
                pods_per_node   = int(params["pods_per_node"])
                num_replicaset  = int(params["num_replicaset"])
                num_priorities  = int(params["num_priorities"])
                util            = float(params["util"])
                util_tol        = float(params["util_tolerance"])
                node_cpu        = str(params["node_cpu"])
                node_mem        = str(params["node_mem"])
                cpu_int_raw     = str(params["cpu_interval"])
                mem_int_raw     = str(params["mem_interval"])

                result_path = self._result_path_for(seed_path, cfg)
                seed_rows = self._read_seed(seed_path)
                seen = self._read_seen_result(result_path)

                pods_cap_needed = max(pods_per_node * 3, DEFAULT_POD_CAP)
                
                print(f"\n[seed-applier] ---- SEED FILE: {seed_path} {len(seed_rows)}; already tested: {len(seen)}; left: {max(0, len(seed_rows)-len(seen))} ----")
                print(f"[seed-applier] result file : {result_path.name}")  # print just the basename

                # Decide if nodes/PCs must be (re)created
                need_nodes = (
                    prev["num_nodes"] is None or
                    prev["num_nodes"] != num_nodes or
                    prev["node_cpu"]  != node_cpu  or
                    prev["node_mem"]  != node_mem  or
                    prev["num_priorities"] != num_priorities or
                    pods_cap_needed   >  prev["pods_cap"]
                )

                if need_nodes:
                    delete_kwok_nodes(self.cluster_ctx)
                    create_kwok_nodes(self.cluster_ctx, num_nodes, node_cpu, node_mem, pods_cap=pods_cap_needed)
                    ensure_priority_classes(self.cluster_ctx, num_priorities, prefix="p", start=1, delete_extras=True)
                    prev = {
                        "num_nodes": num_nodes,
                        "node_cpu": node_cpu,
                        "node_mem": node_mem,
                        "pods_cap": pods_cap_needed,
                        "num_priorities": num_priorities,
                    }

                # Convert intervals and ensure feasibility vs. totals implied by util
                total_pods = num_nodes * pods_per_node
                node_mc = cpu_m_str_to_int(node_cpu)
                node_mi = mem_str_to_mib_int(node_mem)
                tgt_mc_node = int(node_mc * util)
                tgt_mi_node = int(node_mi * util)
                tgt_mc_cluster = tgt_mc_node * num_nodes
                tgt_mi_cluster = tgt_mi_node * num_nodes

                cpu_interval_used = self._normalize_interval_cpu(cpu_int_raw)
                mem_interval_used = self._normalize_interval_mem(mem_int_raw)
                if cpu_interval_used is None:
                    cpu_interval_used, _ = resolve_interval_or_fallback(parse_cpu_interval(cpu_int_raw), total_pods, tgt_mc_cluster, spread=0.9)
                if mem_interval_used is None:
                    mem_interval_used, _ = resolve_interval_or_fallback(parse_mem_interval(mem_int_raw), total_pods, tgt_mi_cluster, spread=0.9)

                # Walk each seed row
                for row in seed_rows:
                    try:
                        # Fresh namespace & default SA per run for isolation
                        ensure_namespace(self.cluster_ctx, self.namespace, recreate=True)

                        seed_int = seed_to_int(row.get("seed", 0))
                        if seed_int in seen:
                            continue

                        # Deterministic RNG -> exactly reconstruct per-pod requests
                        rng = random.Random(seed_int)
                        cpu_parts = gen_parts_constrained(tgt_mc_cluster, total_pods, rng, cpu_interval_used, self.dist_mode, self.variance)
                        mem_parts = gen_parts_constrained(tgt_mi_cluster, total_pods, rng, mem_interval_used, self.dist_mode, self.variance)

                        pod_specs: List[Dict[str, object]] = []
                        rs_specs: List[Dict[str, object]] = []

                        # Apply workload and capture the exact requests we emitted in YAML
                        if num_replicaset > 0:
                            rs_specs = self._apply_replicasets(
                                rng, total_pods, cpu_parts, mem_parts, num_replicaset, num_priorities,
                                cpu_interval_used, mem_interval_used
                            )
                        else:
                            pod_specs = self._apply_standalone_pods(
                                rng, total_pods, cpu_parts, mem_parts, num_priorities
                            )


                        # Event-based settle to collect outcome (scheduled now has (name,node))
                        _, scheduled_pairs, unschedulable_names = get_scheduled_and_unscheduled(
                            self.cluster_ctx, self.namespace, expected=total_pods, settle_timeout=self.settle_timeout_s
                        )
                        scheduled_by_name = {name: node for (name, node) in scheduled_pairs}

                        # Count running by priority (after settle)
                        placed_by_priority = self._count_running_by_priority()

                        # Snapshot for utilization and running-only totals
                        s = stat_snapshot(self.cluster_ctx, ns=self.namespace,
                                        expected=total_pods, settle_timeout=self.settle_timeout_s)

                        # ----- Build pod_node: [{name,cpu_m,mem_mi,priority,node|""}, ...] -----

                        # Index standalone pod specs by name
                        standalone_by_name = {p["name"]: p for p in (pod_specs or [])}

                        # Index RS specs by RS name (so we can attach template requests to RS pods)
                        rs_by_name = {rs["name"]: rs for rs in (rs_specs or [])}

                        # set of all pod names we know from events
                        all_event_pods = set(scheduled_by_name.keys()) | set(unschedulable_names)

                        pod_node_list: List[Dict[str, object]] = []

                        for pname in sorted(all_event_pods):
                            node = scheduled_by_name.get(pname, "")
                            if pname in standalone_by_name:
                                spec = standalone_by_name[pname]
                                entry = {
                                    "name": pname,
                                    "cpu_m": int(spec["cpu_m"]),
                                    "mem_mi": int(spec["mem_mi"]),
                                    "priority": str(spec["priority"]),
                                    "node": node,
                                }
                            else:
                                # RS-managed pod: derive RS name and attach its template requests
                                rsname = self._rs_name_from_pod(pname)
                                r = rs_by_name.get(rsname, {})
                                entry = {
                                    "name": pname,
                                    "cpu_m": int(r.get("cpu_m", 0)),
                                    "mem_mi": int(r.get("mem_mi", 0)),
                                    "priority": str(r.get("priority", "")),
                                    "node": node,
                                }
                            pod_node_list.append(entry)

                        result_row = {
                            "timestamp": self._fmt_ts(),
                            "kwok_config": str(cfg),
                            "seed": str(row.get("seed", "")),
                            "num_nodes": num_nodes,
                            "pods_per_node": pods_per_node,
                            "num_replicaset": num_replicaset,
                            "num_priorities": num_priorities,
                            "util_total": util,
                            "util_tolerance": util_tol,
                            "util_run_cpu": f"{s.cpu_run_util:.3f}",
                            "util_run_mem": f"{s.mem_run_util:.3f}",
                            "cpu_m_run": int(sum(s.cpu_req_by_node.values())),
                            "mem_b_run": int(sum(s.mem_req_by_node.values())),
                            "node_cpu": node_cpu,
                            "node_mem": node_mem,
                            "cpu_interval": cpu_int_raw,
                            "mem_interval": mem_int_raw,
                            "wait_mode": (self.wait_mode or ""),
                            "wait_timeout": self.wait_timeout_raw,
                            "settle_timeout": self.settle_timeout_raw,
                            "scheduled_count": int(len(scheduled_pairs)),
                            "unscheduled_count": int(len(unschedulable_names)),
                            "pods_run_by_node": json.dumps(s.pods_run_by_node, separators=(",", ":")),
                            "placed_by_priority": json.dumps(placed_by_priority, separators=(",", ":")),
                            "pod_node": json.dumps(pod_node_list, separators=(",", ":")),  # <— NEW
                        }

                        self._append_result_row(result_path, result_row)
                        seen.add(seed_int)

                        left = max(0, len(seed_rows) - len(seen))
                        print(f"[seed-applier] cfg={cfg.stem} seed={row.get('seed')} -> result appended | left to test: {left}")

                    except Exception as e:
                        # Failure while processing a specific ROW shouldn't poison the whole seed file.
                        print(f"[seed-applier][row-failed] cfg={cfg.name} seed={row.get('seed')}: {e}")
                        # We do NOT record row-level failures in failed_runs per your instruction.

        print("\n[seed-applier] done.")

# ---------- CLI ----------

def build_argparser() -> argparse.ArgumentParser:
    ap = argparse.ArgumentParser(description="Apply KWOK seeds across multiple KWOK configs (strict conventions).")
    ap.add_argument("--cluster_name", default="kwok1", help="Cluster short name (context will be kwok-<cluster_name>)")
    ap.add_argument("--kwok-config-dir", default="./scripts/kwok/kwok_configs", help="Directory with kwokctl cluster YAMLs")
    ap.add_argument("--seed-dir", default="./scripts/kwok/seeds", help="Directory containing seeds_*.csv")
    ap.add_argument("--failed-runs-file", default="./scripts/failed_runs.txt", help="File to record failed configs/seeds")
    ap.add_argument("--namespace", "-n", default="crossnode-test", help="Namespace to use for tests")
    return ap

def main():
    args = build_argparser().parse_args()
    runner = KwokSeedApplier(
        cluster_name=args.cluster_name,
        kwok_config_dir=args.kwok_config_dir,
        seed_dir=args.seed_dir,
        failed_runs_file=args.failed_runs_file,
        namespace=args.namespace,
    )
    runner.run()

if __name__ == "__main__":
    main()
