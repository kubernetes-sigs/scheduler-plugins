import json, re, argparse
from pathlib import Path
from typing import Dict, Optional
import pandas as pd

DIR_RE_SOLVER = re.compile(r"^nodes(?P<nodes>\d+)_pods(?P<pods>\d+)_prio(?P<prio>\d+)_util(?P<util>\d{3})_timeout(?P<timeout>\d{2})$")

def rate(num, den):
    return (num / den) if den and den > 0 else float("nan")

def util_tag(u):
    try:
        x = float(u)
        pct = x if x > 1.5 else x * 100.0
        return f"{int(round(pct)):03d}"
    except Exception:
        return str(u)

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

def strip_outer_quotes(s: Optional[str]) -> Optional[str]:
    if s is None:
        return None
    s = str(s)
    if len(s) >= 2 and ((s[0] == s[-1] == '"') or (s[0] == s[-1] == "'")):
        return s[1:-1]
    return s

def parse_json_cell(raw):
    if raw is None:
        return None
    s = strip_outer_quotes(str(raw).strip())
    if not s:
        return None
    for cand in (s, s.replace('""', '"').replace("''", '"')):
        try:
            return json.loads(cand)
        except Exception:
            continue
    return None

def prio_map(raw) -> Dict[int, int]:
    """
    Build {priority: count} from either:
      - a dict-like object, or
      - a JSON/CSV cell string that decodes to such a dict.
    Priority keys may optionally start with 'p' (e.g., 'p1', '2').
    """
    out: Dict[int, int] = {}

    # If it's already a dict, use it directly; otherwise parse as JSON cell
    obj = raw if isinstance(raw, dict) else parse_json_cell(raw)

    if not isinstance(obj, dict):
        return out

    for k, v in obj.items():
        ks = str(k).strip()
        if ks.lower().startswith("p"):
            ks = ks[1:]
        pk = int(ks)
        out[pk] = int(v)
    return out


def load_csv(csv_path: Path) -> pd.DataFrame:
    if not csv_path.exists():
        raise FileNotFoundError(f"results.csv not found: {csv_path}")
    df = pd.read_csv(csv_path, dtype=str).fillna("")
    lut = {c.lower(): c for c in df.columns}
    need = {
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
    df = df.rename(columns={lut[k]: v for k, v in need.items() if lut[k] != v})
    # minimal conversions
    df["seed"] = df["seed"].astype(str).str.strip()
    df["util_run_cpu"] = pd.to_numeric(df["util_run_cpu"].astype(str), errors="coerce")
    df["util_run_mem"] = pd.to_numeric(df["util_run_mem"].astype(str), errors="coerce")
    df["solver_duration_ms"] = pd.to_numeric(df["solver_duration_ms"].astype(str), errors="coerce")
    df["solver_status"] = df["solver_status"].astype(str).str.strip().str.upper()
    # placed_by_prio using running info (could be changed to score from solver)
    df["placed_by_prio"] = df.apply(lambda r: json.dumps(parse_json_cell(r.get("placed_by_prio_running", "")), separators=(",", ":")), axis=1)
    return df


# placement compare
def place_compare(a: Dict[int, int], b: Dict[int, int]) -> int:
    """1 if a>b, 0 if equal, -1 if a<b (compare from highest priority down)."""
    keys = sorted(set(a.keys()) | set(b.keys()), reverse=True)
    for k in keys:
        av = int(a.get(k, 0))
        bv = int(b.get(k, 0))
        if av > bv:
            return 1
        if av < bv:
            return -1
    return 0

def cmp_placed_by_prio_row(row) -> int:
    s_map = prio_map(row.get("placed_by_prio_solver", ""))
    d_map = prio_map(row.get("placed_by_prio_default", ""))
    return place_compare(s_map, d_map)

def default_vs_solver_per_seed(
    solver_csv: Path,
    default_csv: Path,
    cfg_name: str,
) -> pd.DataFrame:
    df_s = load_csv(solver_csv)
    df_d = load_csv(default_csv)

    # warnings for asymmetry
    seeds_s = set(df_s["seed"])
    seeds_d = set(df_d["seed"])
    missing_in_default = sorted(seeds_s - seeds_d)
    missing_in_solver = sorted(seeds_d - seeds_s)
    if missing_in_default:
        print(f"[warn] {cfg_name}: {len(missing_in_default)} seed(s) in solver but not default; e.g. {missing_in_default[:5]}")
    if missing_in_solver:
        print(f"[warn] {cfg_name}: {len(missing_in_solver)} seed(s) in default but not solver; e.g. {missing_in_solver[:5]}")

    # join on seed
    joined = df_s[
        [
            "seed",
            "util_run_cpu",
            "util_run_mem",
            "placed_by_prio",
            "unscheduled_cnt",
            "error",
            "solver_status",
            "solver_name",
            "solver_duration_ms",
        ]
    ].rename(
        columns={
            "util_run_cpu": "util_cpu_solver",
            "util_run_mem": "util_mem_solver",
            "placed_by_prio": "placed_by_prio_solver",
            "unscheduled_cnt": "unscheduled_cnt_solver",
        }
    ).merge(
        df_d[
            [
                "seed",
                "util_run_cpu",
                "util_run_mem",
                "placed_by_prio",
                "unscheduled_cnt",
            ]
        ].rename(
            columns={
                "util_run_cpu": "util_cpu_default",
                "util_run_mem": "util_mem_default",
                "placed_by_prio": "placed_by_prio_default",
                "unscheduled_cnt": "unscheduled_cnt_default",
            }
        ),
        on="seed",
        how="inner",
        validate="one_to_one",
    )

    # default_all_scheduled: both have 0 unscheduled
    no_pending_default = pd.to_numeric(joined["unscheduled_cnt_default"], errors="coerce").eq(0)
    no_pending_solver = pd.to_numeric(joined["unscheduled_cnt_solver"], errors="coerce").eq(0)
    joined["default_all_running"] = no_pending_default & no_pending_solver

    # solver_called: infer from presence of best_* info (no solver_attempts).
    joined["solver_called"] = (
        joined["solver_status"].astype(str).str.strip().ne("")
        | joined["solver_name"].astype(str).str.strip().ne("")
        | pd.to_numeric(joined["solver_duration_ms"], errors="coerce").notna()
    ).astype(int)

    joined["placed_cmp"] = joined.apply(cmp_placed_by_prio_row, axis=1)

    # deltas for resource utilization
    joined["cpu_delta"] = joined["util_cpu_solver"] - joined["util_cpu_default"]
    joined["mem_delta"] = joined["util_mem_solver"] - joined["util_mem_default"]

    return joined


def main():
    p = argparse.ArgumentParser(description="Combine solver and default results into summary CSVs.")
    p.add_argument(
        "--out-dir",
        type=Path,
        default=Path("analysis"),
        help="Output directory for combined results CSVs.",
    )
    p.add_argument(
        "--results-root",
        type=Path,
        default=Path("G:/My Drive/Datalogi/MSc - SDU/Master Thesis/Results/results"),
        help="Root directory containing results folders.",
    )
    p.add_argument(
        "--solver-dir",
        type=str,
        default="all_synch_python",
        help="Subdirectory name under results root for solver results.",
    )
    p.add_argument(
        "--default-dir",
        type=str,
        default="default",
        help="Subdirectory name under results root for default results.",
    )
    p.add_argument(
        "--results-csv",
        type=str,
        default="results.csv",
        help="Filename of results CSV files under each results subdirectory.",
    )
    p.add_argument(
        "--decimals",
        type=int,
        default=4,
        help="Number of decimal places for rounding rates and averages in output.",
    )
    args = p.parse_args()
    
    
    solver_root = (args.results_root / args.solver_dir).resolve()
    base_root = (args.results_root / args.default_dir).resolve()
    out_dir = args.out_dir.resolve()
    out_dir.mkdir(parents=True, exist_ok=True)

    per_combo_rows = []

    solver_combos = sorted([p for p in solver_root.iterdir() if p.is_dir()])
    if not solver_combos:
        print(f"[warn] no solver combinations under {solver_root}")

    for solver_dir in solver_combos:
        meta = parse_solver_dirname(solver_dir.name)
        if not meta:
            print(f"[skip] bad folder name: {solver_dir.name}")
            continue

        default_dir = base_root / meta["default_dirname"]
        solver_csv = solver_dir / args.results_csv
        default_csv = default_dir / args.results_csv

        if not default_dir.exists():
            print(f"[skip] default folder missing: {default_dir.name}")
            continue
        if not solver_csv.exists():
            print(f"[skip] solver results.csv missing: {solver_csv}")
            continue
        if not default_csv.exists():
            print(f"[skip] default results.csv missing: {default_csv}")
            continue

        try:
            per_seed_df = default_vs_solver_per_seed(solver_csv, default_csv, solver_dir.name)
        except SystemExit:
            raise
        except Exception as e:
            print(f"[error] {solver_dir.name}: {e}")
            continue

        if per_seed_df.empty:
            print(f"[warn] {solver_dir.name}: no overlapping seeds")
            continue

        # 1) default_all_scheduled mask (exclude from others)
        mask_default_all = per_seed_df["default_all_running"]
        n_default_all = int(mask_default_all.sum())
        not_all_running = per_seed_df[~mask_default_all].copy()

        status = not_all_running["solver_status"]

        is_optimal = status.eq("OPTIMAL")
        is_feasible = status.eq("FEASIBLE")
        is_ok = is_optimal | is_feasible

        placed_equal = not_all_running["placed_cmp"].eq(0)
        placed_better = not_all_running["placed_cmp"].gt(0)
        placed_worse = not_all_running["placed_cmp"].lt(0)

        n_solver_called = int(not_all_running["solver_called"].sum())

        # If solver proved OPTIMAL and placement == default → default was already optimal
        n_default_optimal = int((is_optimal & placed_equal).sum())

        # Solver proved OPTIMAL and strictly better placement than default
        n_solver_optimal = int((is_optimal & placed_better).sum())

        # Solver FEASIBLE and strictly better placement than default
        n_solver_feasible = int((is_feasible & placed_better).sum())

        # Everything else is "failed":
        n_solver_failed = int(
            (
                (~is_ok)  # FAILED
                | (is_feasible & ~placed_better)  # FEASIBLE equal/worse
                | (is_optimal & placed_worse)  # OPTIMAL worse; should not happen
            ).sum()
        )

        # Solver Improved sums OPTIMAL + FEASIBLE
        n_solver_improve = n_solver_optimal + n_solver_feasible

        # Other counts
        n_other = len(not_all_running) - (
            n_default_optimal + n_solver_optimal + n_solver_feasible + n_solver_failed
        )

        # CPU/MEM deltas for info
        cpu_delta_mean = not_all_running["cpu_delta"].mean()
        mem_delta_mean = not_all_running["mem_delta"].mean()
        cpu_delta_sum = float(not_all_running["cpu_delta"].sum())
        mem_delta_sum = float(not_all_running["mem_delta"].sum())

        # Rates over seeds
        n_seeds = max(1, len(per_seed_df))

        default_all_rate = rate(n_default_all, n_seeds)
        solver_called_rate = rate(n_solver_called, n_seeds)
        solver_failed_rate = rate(n_solver_failed, n_seeds)
        default_optimal_rate = rate(n_default_optimal, n_seeds)
        solver_optimal_rate = rate(n_solver_optimal, n_seeds)
        solver_feasible_rate = rate(n_solver_feasible, n_seeds)
        solver_improve_rate = rate(n_solver_improve, n_seeds)
        other_rate = 1.0 - (
            default_all_rate
            + default_optimal_rate
            + solver_optimal_rate
            + solver_feasible_rate
            + solver_failed_rate
        )
        if (
            default_all_rate
            + default_optimal_rate
            + solver_optimal_rate
            + solver_feasible_rate
            + solver_failed_rate
            > 1.00
        ):
            print(f"[warn] {solver_dir.name}: rates sum > 1.00")
        if (
            default_all_rate
            + default_optimal_rate
            + solver_optimal_rate
            + solver_feasible_rate
            + solver_failed_rate
            < 0.00
        ):
            print(f"[warn] {solver_dir.name}: rates sum < 0.00")

        t_sum = float(not_all_running["solver_duration_ms"].sum())
        t_mean = not_all_running["solver_duration_ms"].mean()

        count_sum = (
            n_default_all
            + n_default_optimal
            + n_solver_optimal
            + n_solver_feasible
            + n_solver_failed
        )
        if count_sum != len(per_seed_df):
            print(
                f"[warn] {solver_dir.name}: category counts sum={count_sum} != joined={len(per_seed_df)}"
            )

        per_combo_rows.append(
            {
                "util": meta["util"],
                "nodes": meta["nodes"],
                "pods": meta["pods"],
                "pods_per_node": meta["pods_per_node"],
                "priorities": meta["priorities"],
                "timeout_s": meta["timeout"],
                "config_dir": solver_dir.name,
                "n_seeds": int(len(per_seed_df)),
                "n_seeds_not_all_running": int(len(not_all_running)),
                "n_default_all_running": n_default_all,
                "default_all_running_rate": f"{default_all_rate:.{args.decimals}f}" if args.decimals is not None else default_all_rate,
                "n_solver_called": n_solver_called,
                "solver_called_rate": f"{solver_called_rate:.{args.decimals}f}" if args.decimals is not None else solver_called_rate,
                "n_solver_failed": n_solver_failed,
                "solver_failed_rate": f"{solver_failed_rate:.{args.decimals}f}" if args.decimals is not None else solver_failed_rate,
                "n_default_optimal": n_default_optimal,
                "default_optimal_rate": f"{default_optimal_rate:.{args.decimals}f}" if args.decimals is not None else default_optimal_rate,
                "n_solver_optimal": n_solver_optimal,
                "solver_optimal_rate": f"{solver_optimal_rate:.{args.decimals}f}" if args.decimals is not None else solver_optimal_rate,
                "n_solver_feasible": n_solver_feasible,
                "solver_feasible_rate": f"{solver_feasible_rate:.{args.decimals}f}" if args.decimals is not None else solver_feasible_rate,
                "n_solver_improve": n_solver_improve,
                "solver_improve_rate": f"{solver_improve_rate:.{args.decimals}f}" if args.decimals is not None else solver_improve_rate,
                "n_other": n_other,
                "other_rate": f"{other_rate:.{args.decimals}f}" if args.decimals is not None else other_rate,
                "solver_duration_ms_sum": f"{t_sum:.{args.decimals}f}" if args.decimals is not None else t_sum,
                "solver_duration_ms_mean": f"{t_mean:.{args.decimals}f}" if args.decimals is not None else t_mean,
                "cpu_delta_sum": f"{cpu_delta_sum:.{args.decimals}f}" if args.decimals is not None else cpu_delta_sum,
                "mem_delta_sum": f"{mem_delta_sum:.{args.decimals}f}" if args.decimals is not None else mem_delta_sum,
                "cpu_delta_mean": f"{cpu_delta_mean:.{args.decimals}f}" if args.decimals is not None else cpu_delta_mean,
                "mem_delta_mean": f"{mem_delta_mean:.{args.decimals}f}" if args.decimals is not None else mem_delta_mean,
            }
        )

    # Write per-combo CSV
    out_dir.mkdir(parents=True, exist_ok=True)
    per_combo_df = pd.DataFrame(per_combo_rows)
    # make sure these are numeric
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
    per_combo_df.sort_values(
        ["util", "nodes", "pods_per_node", "priorities", "timeout_s", "config_dir"]
    ).to_csv(out_per_combo, index=False)
    print(f"[ok] wrote {out_per_combo} (rows={len(per_combo_df)})")

if __name__ == "__main__":
    main()
