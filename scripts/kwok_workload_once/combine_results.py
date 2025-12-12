#!/usr/bin/env python3
# combine_results.py

import argparse
import json
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple, Union
from scripts.helpers.general_helpers import (
    cmp_placed_by_prio_row,
    parse_json_cell,
    strip_outer_quotes,
)

import pandas as pd

# Matching directory names
DIR_RE_SOLVER = re.compile(r"^nodes(?P<nodes>\d+)_pods(?P<pods>\d+)_prio(?P<prio>\d+)_util(?P<util>\d{3})_timeout(?P<timeout>\d{2})$")

############################################################
# Helpers
############################################################
def rate(num, den):
    return (num / den) if den and den > 0 else float("nan")

def parse_solver_dirname(name: str) -> Optional[Dict]:
    m = DIR_RE_SOLVER.match(name)
    if not m:
        return None
    d = m.groupdict()
    nodes = int(d["nodes"])
    pods = int(d["pods"])
    priorities = int(d["prio"])
    util = float(d["util"])
    timeout = int(d["timeout"])
    return {
        "nodes": nodes,
        "pods": pods,
        "priorities": priorities,
        "util": util,
        "timeout": timeout,
        "pods_per_node": int(pods / nodes),
        "default_dirname": f"nodes{nodes}_pods{pods}_prio{priorities}_util{d['util']}",
    }


def load_csv(csv_path: Path) -> pd.DataFrame:
    if not csv_path.exists():
        raise FileNotFoundError(f"results.csv not found: {csv_path}")
    df = pd.read_csv(csv_path, dtype=str).fillna("")
    cols = {c.lower(): c for c in df.columns}
    rename_map = {
        "seed": "seed",
        "util_run_cpu_now": "util_run_cpu",
        "util_run_mem_now": "util_run_mem",
        "running_placed_by_prio_now": "placed_by_prio_running",
        "unscheduled_count_before": "unscheduled_cnt",
        "error": "error",
        "best_solver_status": "solver_status",
        "best_solver_name": "solver_name",
        "best_solver_duration_ms": "solver_duration_ms",
        "best_solver_score": "solver_score",
    }
    df = df.rename(columns={cols[k]: v for k, v in rename_map.items() if cols[k] != v})
    # minimal conversions
    df["seed"] = df["seed"].astype(str).str.strip()
    df["util_run_cpu"] = pd.to_numeric(df["util_run_cpu"].astype(str), errors="coerce")
    df["util_run_mem"] = pd.to_numeric(df["util_run_mem"].astype(str), errors="coerce")
    df["solver_duration_ms"] = pd.to_numeric(df["solver_duration_ms"].astype(str), errors="coerce")
    df["solver_status"] = df["solver_status"].astype(str).str.strip().str.upper()
    # NOTE: placed_by_prio using what is actual running - for solver this could be changed to score from the solver so it doesn't get affected by post-processing
    df["placed_by_prio"] = df.apply(lambda r: json.dumps(parse_json_cell(r.get("placed_by_prio_running", "")), separators=(",", ":")), axis=1)
    return df

@dataclass(frozen=True)
class CombineResultsArgs:
    results_root: Path
    solver_dir: str
    default_dir: str
    results_csv: str
    out_dir: Path
    decimals: Optional[int]


@dataclass(frozen=True)
class CategoryCounts:
    n_seeds: int
    n_seeds_not_all_running: int
    n_default_all_running: int
    n_solver_called: int
    n_default_optimal: int
    n_solver_optimal: int
    n_solver_feasible: int
    n_solver_failed: int
    n_solver_improve: int
    n_other: int
    default_all_running_rate: float
    solver_called_rate: float
    default_optimal_rate: float
    solver_optimal_rate: float
    solver_feasible_rate: float
    solver_failed_rate: float
    solver_improve_rate: float
    other_rate: float


class CombineResultsAnalyzer:
    def __init__(self, args: CombineResultsArgs) -> None:
        self.args = args

    @staticmethod
    def _format_num(value: float, decimals: Optional[int]) -> Union[float, str]:
        if decimals is None:
            return value
        return f"{value:.{decimals}f}"

    @staticmethod
    def _split_default_all_running(per_seed_df: pd.DataFrame) -> Tuple[pd.Series, pd.DataFrame]:
        mask_default_all = per_seed_df["default_all_running"]
        not_all_running = per_seed_df[~mask_default_all].copy()
        return mask_default_all, not_all_running

    @staticmethod
    def _status_flags(not_all_running: pd.DataFrame) -> Tuple[pd.Series, pd.Series, pd.Series]:
        status = not_all_running["solver_status"]
        is_optimal = status.eq("OPTIMAL")
        is_feasible = status.eq("FEASIBLE")
        is_ok = is_optimal | is_feasible
        return is_optimal, is_feasible, is_ok

    @staticmethod
    def _placement_flags(not_all_running: pd.DataFrame) -> Tuple[pd.Series, pd.Series, pd.Series]:
        placed_equal = not_all_running["placed_cmp"].eq(0)
        placed_better = not_all_running["placed_cmp"].gt(0)
        placed_worse = not_all_running["placed_cmp"].lt(0)
        return placed_equal, placed_better, placed_worse

    @staticmethod
    def _solver_called_count(not_all_running: pd.DataFrame) -> int:
        return int(not_all_running["solver_called"].sum())

    @staticmethod
    def _compute_category_counts(per_seed_df: pd.DataFrame) -> CategoryCounts:
        mask_default_all, not_all_running = CombineResultsAnalyzer._split_default_all_running(per_seed_df)
        is_optimal, is_feasible, is_ok = CombineResultsAnalyzer._status_flags(not_all_running)
        placed_equal, placed_better, placed_worse = CombineResultsAnalyzer._placement_flags(not_all_running)

        n_solver_called = CombineResultsAnalyzer._solver_called_count(not_all_running)
        n_default_all = int(mask_default_all.sum())
        n_default_optimal = int((is_optimal & placed_equal).sum())
        n_solver_optimal = int((is_optimal & placed_better).sum())
        n_solver_feasible = int((is_feasible & placed_better).sum())
        n_solver_failed = int(((~is_ok) | (is_feasible & ~placed_better) | (is_optimal & placed_worse)).sum())
        n_solver_better = n_solver_optimal + n_solver_feasible
        n_other = len(not_all_running) - (n_default_optimal + n_solver_optimal + n_solver_feasible + n_solver_failed)

        n_seeds = max(1, len(per_seed_df))
        default_all_rate = rate(n_default_all, n_seeds)
        solver_called_rate = rate(n_solver_called, n_seeds)
        solver_failed_rate = rate(n_solver_failed, n_seeds)
        default_optimal_rate = rate(n_default_optimal, n_seeds)
        solver_optimal_rate = rate(n_solver_optimal, n_seeds)
        solver_feasible_rate = rate(n_solver_feasible, n_seeds)
        solver_improve_rate = rate(n_solver_better, n_seeds)
        other_rate = 1.0 - (default_all_rate + default_optimal_rate + solver_optimal_rate + solver_feasible_rate + solver_failed_rate)

        return CategoryCounts(
            n_seeds=n_seeds,
            n_seeds_not_all_running=int(len(not_all_running)),
            n_default_all_running=n_default_all,
            n_solver_called=n_solver_called,
            n_default_optimal=n_default_optimal,
            n_solver_optimal=n_solver_optimal,
            n_solver_feasible=n_solver_feasible,
            n_solver_failed=n_solver_failed,
            n_solver_improve=n_solver_better,
            n_other=n_other,
            default_all_running_rate=default_all_rate,
            solver_called_rate=solver_called_rate,
            default_optimal_rate=default_optimal_rate,
            solver_optimal_rate=solver_optimal_rate,
            solver_feasible_rate=solver_feasible_rate,
            solver_failed_rate=solver_failed_rate,
            solver_improve_rate=solver_improve_rate,
            other_rate=other_rate,
        )

    def analyze_combo(self, *, solver_dir: Path, default_dir: Path, meta: Dict[str, Any]) -> Optional[Dict[str, Any]]:
        solver_csv = solver_dir / self.args.results_csv
        default_csv = default_dir / self.args.results_csv
        if not default_dir.exists():
            print(f"[skip] default folder missing: {default_dir.name}")
            return None
        if not solver_csv.exists():
            print(f"[skip] solver results.csv missing: {solver_csv}")
            return None
        if not default_csv.exists():
            print(f"[skip] default results.csv missing: {default_csv}")
            return None

        per_seed_df = default_vs_solver_per_seed(solver_csv, default_csv, solver_dir.name)
        mask_default_all, not_all_running = self._split_default_all_running(per_seed_df)
        counts = self._compute_category_counts(per_seed_df)

        # Sanity warnings (keep behavior)
        rate_sum = (
            counts.default_all_running_rate
            + counts.default_optimal_rate
            + counts.solver_optimal_rate
            + counts.solver_feasible_rate
            + counts.solver_failed_rate
        )
        if rate_sum > 1.00:
            print(f"[warn] {solver_dir.name}: rates sum > 1.00")
        if rate_sum < 0.00:
            print(f"[warn] {solver_dir.name}: rates sum < 0.00")

        # Durations & deltas (keep behavior)
        t_sum = float(not_all_running["solver_duration_ms"].sum())
        t_mean = not_all_running["solver_duration_ms"].mean()
        cpu_delta_mean = not_all_running["cpu_delta"].mean()
        mem_delta_mean = not_all_running["mem_delta"].mean()
        cpu_delta_sum = float(not_all_running["cpu_delta"].sum())
        mem_delta_sum = float(not_all_running["mem_delta"].sum())

        count_sum = (
            counts.n_default_all_running
            + counts.n_default_optimal
            + counts.n_solver_optimal
            + counts.n_solver_feasible
            + counts.n_solver_failed
        )
        if count_sum != len(per_seed_df):
            print(f"[warn] {solver_dir.name}: category counts sum={count_sum} != joined={len(per_seed_df)}")

        decimals = self.args.decimals
        return {
            "util": meta["util"],
            "nodes": meta["nodes"],
            "pods": meta["pods"],
            "pods_per_node": meta["pods_per_node"],
            "priorities": meta["priorities"],
            "timeout_s": meta["timeout"],
            "config_dir": solver_dir.name,
            "n_seeds": int(len(per_seed_df)),
            "n_seeds_not_all_running": int(len(not_all_running)),
            "n_default_all_running": counts.n_default_all_running,
            "default_all_running_rate": self._format_num(counts.default_all_running_rate, decimals),
            "n_solver_called": counts.n_solver_called,
            "solver_called_rate": self._format_num(counts.solver_called_rate, decimals),
            "n_solver_failed": counts.n_solver_failed,
            "solver_failed_rate": self._format_num(counts.solver_failed_rate, decimals),
            "n_default_optimal": counts.n_default_optimal,
            "default_optimal_rate": self._format_num(counts.default_optimal_rate, decimals),
            "n_solver_optimal": counts.n_solver_optimal,
            "solver_optimal_rate": self._format_num(counts.solver_optimal_rate, decimals),
            "n_solver_feasible": counts.n_solver_feasible,
            "solver_feasible_rate": self._format_num(counts.solver_feasible_rate, decimals),
            "n_solver_improve": counts.n_solver_improve,
            "solver_improve_rate": self._format_num(counts.solver_improve_rate, decimals),
            "n_other": counts.n_other,
            "other_rate": self._format_num(counts.other_rate, decimals),
            "solver_duration_ms_sum": self._format_num(t_sum, decimals),
            "solver_duration_ms_mean": self._format_num(float(t_mean) if t_mean == t_mean else float("nan"), decimals),
            "cpu_delta_sum": self._format_num(cpu_delta_sum, decimals),
            "mem_delta_sum": self._format_num(mem_delta_sum, decimals),
            "cpu_delta_mean": self._format_num(float(cpu_delta_mean) if cpu_delta_mean == cpu_delta_mean else float("nan"), decimals),
            "mem_delta_mean": self._format_num(float(mem_delta_mean) if mem_delta_mean == mem_delta_mean else float("nan"), decimals),
        }

    def run(self) -> None:
        solver_root = (self.args.results_root / self.args.solver_dir).resolve()
        default_root = (self.args.results_root / self.args.default_dir).resolve()
        out_dir = self.args.out_dir.resolve()
        out_dir.mkdir(parents=True, exist_ok=True)

        per_combo_rows: List[Dict[str, Any]] = []
        solver_combos = sorted([p for p in solver_root.iterdir() if p.is_dir()])
        for solver_dir in solver_combos:
            meta = parse_solver_dirname(solver_dir.name)
            if not meta:
                print(f"[skip] bad folder name: {solver_dir.name}")
                continue
            default_dir = default_root / meta["default_dirname"]
            row = self.analyze_combo(solver_dir=solver_dir, default_dir=default_dir, meta=meta)
            if row is not None:
                per_combo_rows.append(row)

        per_combo_df = pd.DataFrame(per_combo_rows)
        for c in [
            "n_seeds",
            "n_default_all_running",
            "n_solver_called",
            "n_solver_failed",
            "n_default_optimal",
            "n_solver_optimal",
            "n_solver_feasible",
            "n_solver_improve",
            "n_other",
            "cpu_delta_mean",
            "mem_delta_mean",
            "cpu_delta_sum",
            "mem_delta_sum",
            "solver_duration_ms_sum",
            "solver_duration_ms_mean",
        ]:
            if c in per_combo_df.columns:
                per_combo_df[c] = pd.to_numeric(per_combo_df[c], errors="coerce")
        out_per_combo = out_dir / "per_combo_results.csv"
        per_combo_df.sort_values(["util", "nodes", "pods_per_node", "priorities", "timeout_s", "config_dir"]).to_csv(out_per_combo, index=False)
        print(f"[ok] wrote {out_per_combo} (rows={len(per_combo_df)})")

def default_vs_solver_per_seed(solver_csv: Path, default_csv: Path, cfg_name: str) -> pd.DataFrame:
    df_s = load_csv(solver_csv)
    df_d = load_csv(default_csv)
    
    # join result from solver and default on seed
    joined = df_s[["seed", "util_run_cpu", "util_run_mem", "placed_by_prio", "unscheduled_cnt", "error", "solver_status", "solver_name", "solver_duration_ms"]].rename(
        columns={
            "util_run_cpu": "util_cpu_solver",
            "util_run_mem": "util_mem_solver",
            "placed_by_prio": "placed_by_prio_solver",
            "unscheduled_cnt": "unscheduled_cnt_solver",
        }
    ).merge(
        df_d[["seed", "util_run_cpu", "util_run_mem", "placed_by_prio", "unscheduled_cnt"]].rename(
            columns={
                "util_run_cpu": "util_cpu_default",
                "util_run_mem": "util_mem_default",
                "placed_by_prio": "placed_by_prio_default",
                "unscheduled_cnt": "unscheduled_cnt_default",
            }
        ),
        on="seed", how="inner", validate="one_to_one",
    )

    # default_all_scheduled: both have 0 unscheduled
    no_pending_default = pd.to_numeric(joined["unscheduled_cnt_default"], errors="coerce").eq(0)
    no_pending_solver = pd.to_numeric(joined["unscheduled_cnt_solver"], errors="coerce").eq(0)
    joined["default_all_running"] = no_pending_default & no_pending_solver

    # solver_called: any of status/name/duration present
    joined["solver_called"] = (
        joined["solver_status"].astype(str).str.strip().ne("")
        | joined["solver_name"].astype(str).str.strip().ne("")
        | pd.to_numeric(joined["solver_duration_ms"], errors="coerce").notna()
    ).astype(int)

    # compare placements
    joined["placed_cmp"] = joined.apply(cmp_placed_by_prio_row, axis=1)

    # deltas for resource utilization
    joined["cpu_delta"] = joined["util_cpu_solver"] - joined["util_cpu_default"]
    joined["mem_delta"] = joined["util_mem_solver"] - joined["util_mem_default"]

    return joined


def build_argparser() -> argparse.ArgumentParser:
    ap = argparse.ArgumentParser(description="Combine solver/default results into a per-combination CSV")
    ap.add_argument(
        "--results-root",
        type=Path,
        default=Path("/mnt/g/My Drive/Datalogi/MSc - SDU/Master Thesis/Results/results"),
        help="Root folder containing the solver/default results trees. Can be outside repo directory.",
    )
    ap.add_argument("--solver-dir", default="plugin-scheduler", help="Subfolder under --results-root holding solver runs")
    ap.add_argument("--default-dir", default="default", help="Subfolder under --results-root holding default runs")
    ap.add_argument("--results-csv", default="results.csv", help="Results CSV filename in each run directory")
    ap.add_argument("--out-dir", type=Path, default=Path("analysis/kwok_workload_once"), help="Output directory for aggregated CSV")
    ap.add_argument(
        "--decimals",
        type=int,
        default=4,
        help="Decimal places for formatted numeric columns. Use -1 to disable formatting.",
    )
    return ap

def main(argv: Optional[List[str]] = None) -> None:
    args = build_argparser().parse_args(argv)
    decimals = None if args.decimals is None or int(args.decimals) < 0 else int(args.decimals)
    cr_args = CombineResultsArgs(
        results_root=Path(args.results_root),
        solver_dir=str(args.solver_dir),
        default_dir=str(args.default_dir),
        results_csv=str(args.results_csv),
        out_dir=Path(args.out_dir),
        decimals=decimals,
    )
    CombineResultsAnalyzer(cr_args).run()

if __name__ == "__main__":
    main()
