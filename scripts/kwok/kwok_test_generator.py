#!/usr/bin/env python3
# kwok_instance_generator.py

import os, sys, json, csv, time, argparse, random, re
from typing import Tuple, Set, List, Dict, Optional

from kwok_shared import (
    parse_cpu_interval, parse_mem_interval, resolve_interval_or_fallback,
    gen_parts_constrained, cpu_m_str_to_int, mem_str_to_mib_int, ensure_dir,
    seed_normalize_and_zero_pad, format_interval_cpu, format_interval_mem, csv_append_row
)

class KwokInstanceGenerator:
    def __init__(self, args: argparse.Namespace):
        self.args = args
        ensure_dir(self.args.output_dir)

        # ---- pre-compute cluster targets and resolve intervals ONCE ----
        self.node_mc = cpu_m_str_to_int(args.node_cpu)
        self.node_mi = mem_str_to_mib_int(args.node_mem)
        self.total_pods = args.num_nodes * args.pods_per_node
        self.tgt_mc_node = int(self.node_mc * args.util)
        self.tgt_mi_node = int(self.node_mi * args.util)
        self.tgt_mc_cluster = self.tgt_mc_node * args.num_nodes
        self.tgt_mi_cluster = self.tgt_mi_node * args.num_nodes

        parsed_cpu = parse_cpu_interval(args.cpu_interval)
        parsed_mem = parse_mem_interval(args.mem_interval)
        self.cpu_interval_used, _ = resolve_interval_or_fallback(
            parsed_cpu, self.total_pods, self.tgt_mc_cluster, spread=0.9
        )
        self.mem_interval_used, _ = resolve_interval_or_fallback(
            parsed_mem, self.total_pods, self.tgt_mi_cluster, spread=0.9
        )

        # build basename using the **resolved** intervals
        _, fname_jsonl = self._make_params_signature(
            args, self.cpu_interval_used, self.mem_interval_used
        )
        self.seed_file_jsonl = os.path.join(self.args.output_dir, fname_jsonl)
        self.seed_file_csv = os.path.splitext(self.seed_file_jsonl)[0] + ".csv"

        self.seen: Set[int] = self._load_existing_seeds(self.seed_file_csv, self.seed_file_jsonl)
        self.rng = random.Random(int(time.time_ns()))

    # ---------- helpers ----------

    @staticmethod
    def _sanitize(s: str) -> str:
        return re.sub(r"[^A-Za-z0-9_.-]+", "-", s)

    def _make_params_signature(
        self,
        args: argparse.Namespace,
        cpu_interval_used: Optional[Tuple[int,int]],
        mem_interval_used: Optional[Tuple[int,int]],
    ) -> str:
        sig = {
            "n": args.num_nodes,
            "ppn": args.pods_per_node,
            "rs": args.num_replicaset,
            "prio": args.num_priorities,
            "u": f"{args.util:.3f}",
            "tol": f"{args.util_tolerance:.3f}",
            "cpu": str(args.node_cpu),
            "mem": str(args.node_mem),
            "ci": format_interval_cpu(cpu_interval_used) if cpu_interval_used else "auto",
            "mi": format_interval_mem(mem_interval_used) if mem_interval_used else "auto",
        }
        sig_str = json.dumps(sig, sort_keys=True, separators=(",", ":"))

        ci_str = self._sanitize(sig["ci"])
        mi_str = self._sanitize(sig["mi"])
        fname = (
            f"seeds_"
            f"{args.num_nodes}n_"
            f"{args.pods_per_node}ppn_"
            f"{args.num_replicaset}rs_"
            f"{args.num_priorities}prio_"
            f"{args.util:.3f}u_{args.util_tolerance:.3f}tol_"
            f"cpu{self._sanitize(args.node_cpu)}_"
            f"mem{self._sanitize(args.node_mem)}_"
            f"ci{ci_str}_"
            f"mi{mi_str}.jsonl"
        )
        return sig_str, fname

    @staticmethod
    def _load_existing_seeds(csv_path: str, jsonl_path: str) -> Set[int]:
        seen: Set[int] = set()
        if os.path.exists(csv_path):
            try:
                with open(csv_path, "r", encoding="utf-8", newline="") as f:
                    rd = csv.DictReader(f)
                    for row in rd:
                        if not row: continue
                        if "seed" in row and row["seed"] is not None:
                            ss = str(row["seed"])
                            seen.add(int(ss.lstrip("0") or "0"))
                    return seen
            except Exception:
                print(f"[instance-generator] no existing seeds found in {csv_path} (error)", file=sys.stderr)
                pass
        
        return seen

    @staticmethod
    def _percentiles(xs: List[int], ps=(5,25,50,75,95)) -> Dict[str, int]:
        if not xs:
            return {f"p{p}": 0 for p in ps}
        ys = sorted(xs)
        n = len(ys)
        out = {}
        for p in ps:
            k = max(1, min(n, int(round(p/100.0 * n))))
            out[f"p{p}"] = ys[k-1]
        return out

    def _stat_header(self) -> List[str]:
        # Keep a stable column order for CSV
        base = [
            "seed",
            "cpu_m_min","cpu_m_max","cpu_m_mean","cpu_m_std","cpu_m_cv",
            "cpu_m_p5","cpu_m_p25","cpu_m_p50","cpu_m_p75","cpu_m_p95",
            "mem_mi_min","mem_mi_max","mem_mi_mean","mem_mi_std","mem_mi_cv",
            "mem_mi_p5","mem_mi_p25","mem_mi_p50","mem_mi_p75","mem_mi_p95",
            "unique_pair_ratio",
        ]
        return base

    def _compute_stats(self, cpu_parts: List[int], mem_parts: List[int]) -> Dict[str, object]:
        assert len(cpu_parts) == len(mem_parts)
        n = len(cpu_parts)
        if n == 0:
            return {}
        def basic(xs: List[int], label: str) -> Dict[str, object]:
            mn = min(xs); mx = max(xs); sm = sum(xs); mean = sm / n
            var = sum((x - mean) ** 2 for x in xs) / n
            std = var ** 0.5
            cv = (std / mean) if mean > 0 else 0.0
            q = self._percentiles(xs)
            return {
                f"{label}_min": mn,
                f"{label}_max": mx,
                f"{label}_mean": mean,
                f"{label}_std": std,
                f"{label}_cv": cv,
                **{f"{label}_{k}": v for k, v in q.items()},
            }
        pairs = {(int(cpu_parts[i]), int(mem_parts[i])) for i in range(n)}
        uniq_ratio = len(pairs) / n
        out = {}
        out.update(basic(cpu_parts, "cpu_m"))
        out.update(basic(mem_parts, "mem_mi"))
        out["unique_pair_ratio"] = uniq_ratio
        return out

    def _generate_and_verify(
        self,
        seed: int,
        num_nodes: int,
        util: float,
        util_tolerance: float,
        dist_mode: str,
        variance: int,
    ) -> Tuple[bool, str, List[int], List[int]]:
        rng = random.Random(seed)
        cpu_interval_used = self.cpu_interval_used
        mem_interval_used = self.mem_interval_used

        cpu_parts = gen_parts_constrained(self.tgt_mc_cluster, self.total_pods, rng, cpu_interval_used, dist_mode, variance)
        mem_parts = gen_parts_constrained(self.tgt_mi_cluster, self.total_pods, rng, mem_interval_used, dist_mode, variance)

        sum_cpu = int(sum(cpu_parts))
        sum_mem = int(sum(mem_parts))

        alloc_cpu = self.node_mc * num_nodes
        alloc_mem = self.node_mi * num_nodes
        lo_cpu = int(alloc_cpu * (util - util_tolerance))
        hi_cpu = int(alloc_cpu * (util + util_tolerance))
        lo_mem = int(alloc_mem * (util - util_tolerance))
        hi_mem = int(alloc_mem * (util + util_tolerance))

        if not (lo_cpu <= sum_cpu <= hi_cpu):
            return (False, f"CPU total {sum_cpu} outside [{lo_cpu},{hi_cpu}]", [], [])
        if not (lo_mem <= sum_mem <= hi_mem):
            return (False, f"MEM total {sum_mem} outside [{lo_mem},{hi_mem}]", [], [])

        pairs = set()
        for i in range(self.total_pods):
            pair = (int(cpu_parts[i]), int(mem_parts[i]))
            if pair in pairs:
                return (False, f"duplicate (cpu,mem) pair found: {pair}", [], [])
            pairs.add(pair)

        return (True, "ok", list(map(int, cpu_parts)), list(map(int, mem_parts)))

    def _append_csv_row(self, seed_str: str, stats: Dict[str, object]) -> None:
        header = self._stat_header()
        row_out = {"seed": seed_str}
        for k in header:
            if k == "seed":
                continue
            row_out[k] = stats.get(k, "")
        csv_append_row(self.seed_file_csv, header, row_out)

    def _record(self, seed_str: str, stats: Dict[str, object] = {}) -> None:
        self._append_csv_row(seed_str, stats)
    
    def _try_save_seed(self, seed_int: int) -> bool:
        seed_int, seed_str = seed_normalize_and_zero_pad(seed_int, self.args.seed_width)
        if seed_int in self.seen:
            print(f"[instance-generator] seed={seed_str} already present — skip")
            return True

        ok, reason, cpu_parts, mem_parts = self._generate_and_verify(
            seed_int,
            self.args.num_nodes,
            self.args.util, self.args.util_tolerance,
            self.args.dist_mode, self.args.variance,
        )
        if not ok:
            print(f"[instance-generator] rejected seed={seed_str}: {reason}")
            return False

        stats = self._compute_stats(cpu_parts, mem_parts)
        self._record(seed_str, stats)
        self.seen.add(seed_int)
        return True

    def generate(self) -> None:
        print(f"[instance-generator] Using seed files:\n  JSONL: {self.seed_file_jsonl}\n  CSV  : {self.seed_file_csv}")
        print(f"[instance-generator] {len(self.seen)} seed(s) already recorded for this parameter set.")

        # single-seed mode
        if self.args.seed is not None:
            self._try_save_seed(int(self.args.seed))
            return

        # multi / infinite mode
        to_make = int(self.args.count)
        made = 0
        while to_make == 0 or made < to_make:
            s = self.rng.randint(1, 2**63 - 1)
            if self._try_save_seed(s):
                made += 1
                print(f"[instance-generator] count={made} seed={s} recorded")
            if self.args.count == 0:
                time.sleep(0.0001)
        print("[instance-generator] all requested seeds generated")

# ---------- CLI ----------

def build_argparser() -> argparse.ArgumentParser:
    ap = argparse.ArgumentParser()
    ap.add_argument("--output-dir", default="./scripts/kwok/seeds", help="Directory to store seed files")
    ap.add_argument("--num_nodes", type=int, default=8)
    ap.add_argument("--pods_per_node", type=int, default=4)
    ap.add_argument("--num_replicaset", type=int, default=0)
    ap.add_argument("--num-priorities", type=int, default=4, dest="num_priorities")
    ap.add_argument("--util", type=float, default=0.90)
    ap.add_argument("--util-tolerance", type=float, default=0.01, dest="util_tolerance")
    ap.add_argument("--node-cpu", default="24", dest="node_cpu")
    ap.add_argument("--node-mem", default="32Gi", dest="node_mem")
    ap.add_argument("--cpu-interval", default="50m,500m", dest="cpu_interval")
    ap.add_argument("--mem-interval", default="64Mi,1024Mi", dest="mem_interval")
    ap.add_argument("--dist-mode", default="random", choices=["random","even"])
    ap.add_argument("--variance", type=int, default=50)
    ap.add_argument("--seed", type=int, default=None, help="If set, verify exactly this seed once and save if valid.")
    ap.add_argument("--seed-width", type=int, default=19, help="Zero-pad/cut seeds to this width.")
    ap.add_argument("--count", type=int, default=0, help="How many seeds to generate when --seed is not set. Use 0 for infinite.")
    return ap

def main():
    args = build_argparser().parse_args()
    gen = KwokInstanceGenerator(args)
    gen.generate()

if __name__ == "__main__":
    main()
