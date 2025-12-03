#!/usr/bin/env python3
import argparse
import math
import json
from pathlib import Path
from typing import List, Tuple

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

TIME_COL = "wall_time_s"
UNSCHED_COL = "unsched_by_prio"


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=(
            "Compare pending pods between default and python schedulers.\n\n"
            "Top row: per-time *pending difference* (python - default).\n"
            "Bottom row: *cumulative pending difference* (integral of the per-time diff, in pod-seconds).\n\n"
            "If more than one priority:\n"
            "  - Column 0 = TOTAL (all priorities summed)\n"
            "  - Remaining columns = per priority.\n"
        )
    )
    p.add_argument(
        "--default-csv",
        required=True,
        help="Results CSV for the default scheduler.",
    )
    p.add_argument(
        "--python-csv",
        required=True,
        help="Results CSV for the Python optimizer.",
    )
    p.add_argument(
        "--smooth-seconds",
        type=float,
        default=0.0,
        help=(
            "Centered running-average window size in seconds applied "
            "to the *per-time* difference before plotting (top row). "
            "0 means no smoothing. (default: 0.0)"
        ),
    )
    p.add_argument(
        "--out",
        default=None,
        help=(
            "Output image path (e.g., figures/prio_pending_diff_grid.png). "
            "Default: <default-csv-stem>_prio_pending_diff_grid.png in the same "
            "directory as the default CSV."
        ),
    )
    p.add_argument(
        "--no-show",
        action="store_true",
        help="Do not open an interactive window; just save the figure.",
    )
    return p.parse_args()


def detect_priorities_from_unsched(df: pd.DataFrame) -> List[int]:
    """
    Inspect the first non-null unsched_by_prio cell and extract priorities.
    Expects JSON like: {"p1": 0, "p2": 3, ...}
    Returns a sorted list of integer priorities, e.g. [1, 2, 3, 4].
    """
    if UNSCHED_COL not in df.columns:
        raise SystemExit(f"Column {UNSCHED_COL!r} not found in DataFrame")

    series = df[UNSCHED_COL].dropna()
    if series.empty:
        raise SystemExit(f"No non-null values in column {UNSCHED_COL!r}")

    first = series.iloc[0]
    if not isinstance(first, str):
        raise SystemExit(f"Expected {UNSCHED_COL!r} to contain JSON strings, got {type(first)}")

    try:
        d = json.loads(first)
    except json.JSONDecodeError as e:
        raise SystemExit(f"Failed to parse {UNSCHED_COL!r} JSON: {e}")

    prios: List[int] = []
    for key in d.keys():
        if key.startswith("p"):
            try:
                prios.append(int(key[1:]))
            except ValueError:
                continue

    if not prios:
        raise SystemExit(
            f"No 'p<k>' keys found in {UNSCHED_COL!r} JSON; got keys: {list(d.keys())}"
        )

    prios.sort()
    return prios


def build_stepwise_diff(
    t_def: np.ndarray,
    y_def: np.ndarray,
    t_py: np.ndarray,
    y_py: np.ndarray,
) -> Tuple[np.ndarray, np.ndarray]:
    """
    Build a time-aligned difference series between two stepwise-constant signals.

    We:
      - Take the union of timestamps from both series,
      - Treat y_def and y_py as stepwise-constant between their own samples,
      - For each time in the union, take the latest known value from each series
        and compute diff = y_py - y_def.

    Returns:
      times, diff
    """
    if t_def.size == 0 and t_py.size == 0:
        return np.array([]), np.array([])

    # Sort just in case
    order_def = np.argsort(t_def)
    t_def = t_def[order_def]
    y_def = y_def[order_def]

    order_py = np.argsort(t_py)
    t_py = t_py[order_py]
    y_py = y_py[order_py]

    # Union of timestamps
    all_times = np.unique(np.concatenate([t_def, t_py]))

    # Indices & last values
    i_def = 0
    i_py = 0
    last_def = y_def[0] if t_def.size > 0 else 0.0
    last_py = y_py[0] if t_py.size > 0 else 0.0

    out_t: List[float] = []
    out_diff: List[float] = []

    for t in all_times:
        # Advance default series up to time t
        while i_def + 1 < t_def.size and t_def[i_def + 1] <= t:
            i_def += 1
            last_def = y_def[i_def]

        # Advance python series up to time t
        while i_py + 1 < t_py.size and t_py[i_py + 1] <= t:
            i_py += 1
            last_py = y_py[i_py]

        out_t.append(float(t))
        out_diff.append(float(last_py - last_def))  # python - default

    return np.array(out_t, dtype=float), np.array(out_diff, dtype=float)


def time_based_running_average(
    t: np.ndarray,
    y: np.ndarray,
    window_s: float,
) -> np.ndarray:
    """
    Centered running average over a *time* window (seconds).

    For each index i, we average all y[j] such that
      t[j] is in [t[i] - window_s/2, t[i] + window_s/2].

    - window_s <= 0 → returns y unchanged
    - Assumes t is sorted ascending.
    """
    if window_s <= 0.0 or y.size == 0:
        return y

    n = y.size
    out = np.empty_like(y, dtype=float)
    half = window_s / 2.0

    left = 0
    right = 0

    for i in range(n):
        center = t[i]
        lo = center - half
        hi = center + half

        # Move left pointer to the first index with t >= lo
        while left < n and t[left] < lo:
            left += 1

        # Ensure right at least left
        if right < left:
            right = left

        # Move right pointer to the last index with t <= hi
        while right + 1 < n and t[right + 1] <= hi:
            right += 1

        if right < left:
            # No points in window (can happen if window_s is tiny); fall back to original
            out[i] = float(y[i])
        else:
            out[i] = float(np.mean(y[left : right + 1]))

    return out


def main() -> None:
    args = parse_args()

    csv_paths = [Path(args.default_csv), Path(args.python_csv)]

    # Load both CSVs
    dfs: List[pd.DataFrame] = []
    for p in csv_paths:
        if not p.exists():
            raise SystemExit(f"CSV file not found: {p}")
        dfs.append(pd.read_csv(p))

    df_def, df_py = dfs

    # Column checks
    for p, df in zip(csv_paths, dfs):
        missing = []
        if TIME_COL not in df.columns:
            missing.append(TIME_COL)
        if UNSCHED_COL not in df.columns:
            missing.append(UNSCHED_COL)
        if missing:
            raise SystemExit(
                f"CSV {p} is missing required columns: {', '.join(missing)}"
            )

    # Detect priorities from default CSV
    prios = detect_priorities_from_unsched(df_def)  # e.g. [1,2,3,4]
    n_prios = len(prios)

    # Parse unsched_by_prio JSON → DataFrame of columns p1, p2, ...
    unsched_frames: List[pd.DataFrame] = []
    for df in dfs:
        parsed = df[UNSCHED_COL].apply(
            lambda s: json.loads(s) if isinstance(s, str) and s else {}
        )
        unsched_df = pd.DataFrame(list(parsed))
        unsched_frames.append(unsched_df)

    unsched_def, unsched_py = unsched_frames

    t_def = df_def[TIME_COL].values.astype(float)
    t_py = df_py[TIME_COL].values.astype(float)

    smooth_seconds = max(0.0, float(args.smooth_seconds))

    # Groups = [total] + per-priority (if more than one priority)
    groups: List[Tuple[str, np.ndarray, np.ndarray]] = []  # (label, y_def, y_py)

    if n_prios > 1:
        # Total group (all priorities summed)
        cols_def = [f"p{pr}" for pr in prios if f"p{pr}" in unsched_def.columns]
        cols_py = [f"p{pr}" for pr in prios if f"p{pr}" in unsched_py.columns]

        if cols_def:
            pend_def_total = unsched_def[cols_def].fillna(0).to_numpy(dtype=float).sum(axis=1)
        else:
            pend_def_total = np.zeros_like(t_def, dtype=float)

        if cols_py:
            pend_py_total = unsched_py[cols_py].fillna(0).to_numpy(dtype=float).sum(axis=1)
        else:
            pend_py_total = np.zeros_like(t_py, dtype=float)

        groups.append(("Total (all priorities)", pend_def_total, pend_py_total))

    # Per-priority groups
    for pr in prios:
        pkey = f"p{pr}"
        if pkey in unsched_def.columns:
            pend_def = unsched_def[pkey].fillna(0).astype(float).values
        else:
            pend_def = np.zeros_like(t_def, dtype=float)

        if pkey in unsched_py.columns:
            pend_py = unsched_py[pkey].fillna(0).astype(float).values
        else:
            pend_py = np.zeros_like(t_py, dtype=float)

        groups.append((f"Priority {pr}", pend_def, pend_py))

    n_groups = len(groups)
    if n_groups == 0:
        raise SystemExit("No groups to plot (no priorities detected).")

    # Layout: 2 rows (top: diff, bottom: cumulative diff), one column per group
    nrows = 2
    ncols = n_groups

    per_col_width = 2.6
    per_row_height = 2.0
    fig_width = per_col_width * ncols
    fig_height = per_row_height * nrows

    fig, axes = plt.subplots(
        nrows=nrows,
        ncols=ncols,
        sharex=True,
        sharey=False,
        figsize=(fig_width, fig_height),
    )

    # axes is 2 x ncols
    if ncols == 1:
        axes = np.array(axes).reshape(2, 1)
    axes = np.asarray(axes)

    first_diff_line = None

    for j, (label, y_def, y_py) in enumerate(groups):
        ax_diff = axes[0, j]
        ax_cum = axes[1, j]

        # Build per-time diff (stepwise)
        x, diff = build_stepwise_diff(t_def, y_def, t_py, y_py)
        diff_sm = time_based_running_average(x, diff, smooth_seconds)

        # ---- Top row: per-time diff ----
        if x.size > 0:
            line = ax_diff.plot(x, diff_sm, linewidth=1.2)[0]
            if first_diff_line is None:
                first_diff_line = line
            # Symmetric y around 0 for per-time diff
            absmax = float(np.nanmax(np.abs(diff_sm))) if diff_sm.size > 0 else 1.0
            if not math.isfinite(absmax) or absmax <= 0:
                absmax = 1.0
            ax_diff.set_ylim(-absmax * 1.05, absmax * 1.05)

        ax_diff.axhline(0.0, color="black", linewidth=0.5, linestyle="--")
        ax_diff.set_title(label, fontsize=9)
        ax_diff.grid(True, linestyle="--", linewidth=0.5, alpha=0.6)
        ax_diff.tick_params(axis="both", labelsize=6)

        # Y-label only on first column
        if j == 0:
            ax_diff.set_ylabel("Pending diff\n(python - default)", fontsize=8)
        else:
            ax_diff.set_ylabel("")
            ax_diff.tick_params(axis="y", labelleft=False)

        # No x-label on top row
        ax_diff.set_xlabel("")

        # ---- Bottom row: cumulative diff (pod-seconds) ----
        if x.size > 0:
            dt = np.diff(x, prepend=x[0])
            dt = np.clip(dt, 0.0, None)
            cum = np.cumsum(diff * dt)

            ax_cum.plot(x, cum, linewidth=1.2)
            ymin = float(np.nanmin(cum)) if cum.size > 0 else 0.0
            ymax = float(np.nanmax(cum)) if cum.size > 0 else 0.0
            if not math.isfinite(ymin):
                ymin = 0.0
            if not math.isfinite(ymax):
                ymax = 0.0
            if ymin == ymax:
                if ymin == 0.0:
                    ymax = 1.0
                else:
                    ymin *= 0.95
                    ymax *= 1.05
            ax_cum.set_ylim(ymin, ymax * 1.05 if ymax > 0 else ymax * 0.95)

        ax_cum.axhline(0.0, color="black", linewidth=0.5, linestyle="--")
        ax_cum.grid(True, linestyle="--", linewidth=0.5, alpha=0.6)
        ax_cum.tick_params(axis="both", labelsize=6)

        if j == 0:
            ax_cum.set_ylabel("Cumulative diff\n(pod-seconds)", fontsize=8)
        else:
            ax_cum.set_ylabel("")
            ax_cum.tick_params(axis="y", labelleft=False)

        # Bottom row gets x-labels
        ax_cum.set_xlabel("Time (s)", fontsize=8)

    # Tiny legend explaining sign / smoothing
    if first_diff_line is not None:
        legend_label = "(python - default)"
        if smooth_seconds > 0:
            legend_label += f", smoothed over ±{smooth_seconds/2:.1f}s"
        fig.legend(
            [first_diff_line],
            [legend_label],
            loc="upper center",
            bbox_to_anchor=(0.5, 1.02),
            ncol=1,
            frameon=False,
            fontsize=8,
        )

    fig.tight_layout(rect=(0.02, 0.03, 0.98, 0.93))

    # Output path
    if args.out:
        out_path = Path(args.out)
    else:
        first = csv_paths[0]
        out_path = first.with_name("prio_pending_diff_grid.png")

    out_path.parent.mkdir(parents=True, exist_ok=True)
    fig.savefig(out_path, dpi=200)
    print(f"Saved figure to: {out_path}")

    if not args.no_show:
        plt.show()


if __name__ == "__main__":
    main()
