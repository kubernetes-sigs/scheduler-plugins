#!/usr/bin/env python3
from __future__ import annotations

import argparse
import math
from pathlib import Path
from typing import List, Tuple

import pandas as pd
import matplotlib.pyplot as plt


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=(
            "Plot per-priority cumulative runtime curves from one or more "
            "trace-monitor CSV files as a grid of subplots (one priority per subplot).\n\n"
            "Each CSV contributes one line per subplot; this is suitable for "
            "comparing multiple schedulers or configurations."
        )
    )
    p.add_argument(
        "csvs",
        nargs="+",
        help="One or more trace-monitor CSV files.",
    )
    p.add_argument(
        "--labels",
        nargs="*",
        default=None,
        help=(
            "Optional labels for each CSV (same order as csvs). "
            "If omitted, the filename stem is used."
        ),
    )
    p.add_argument(
        "--xcol",
        default="wall_time_s",
        choices=["wall_time_s", "sim_time_s"],
        help="X-axis column to use (default: wall_time_s).",
    )
    p.add_argument(
        "--out",
        default=None,
        help=(
            "Output image path (e.g., figures/prio_grid.png). "
            "Default: <first-csv-stem>_prio_grid.png in the same directory as the first CSV."
        ),
    )
    p.add_argument(
        "--no-show",
        action="store_true",
        help="Do not open an interactive window; just save the figure.",
    )
    return p.parse_args()


def detect_priority_columns(df: pd.DataFrame) -> List[Tuple[int, str]]:
    """
    Find columns named like 'prio<k>_run_time_s' and return
    a sorted list of (k, column_name).
    """
    prios: List[Tuple[int, str]] = []
    for col in df.columns:
        if col.startswith("prio") and col.endswith("_run_time_s"):
            mid = col[len("prio") : -len("_run_time_s")]
            try:
                k = int(mid)
                prios.append((k, col))
            except ValueError:
                continue
    prios.sort(key=lambda t: t[0])
    return prios


def main() -> None:
    args = parse_args()

    csv_paths = [Path(p) for p in args.csvs]
    if not csv_paths:
        raise SystemExit("You must provide at least one CSV file.")

    # Build labels
    if args.labels is not None and len(args.labels) > 0:
        if len(args.labels) != len(csv_paths):
            raise SystemExit(
                f"--labels must have same length as csvs "
                f"(got {len(args.labels)} labels, {len(csv_paths)} csvs)"
            )
        labels = args.labels
    else:
        labels = [p.stem for p in csv_paths]

    # Load all CSVs
    dfs: List[pd.DataFrame] = []
    for p in csv_paths:
        if not p.exists():
            raise SystemExit(f"CSV file not found: {p}")
        dfs.append(pd.read_csv(p))

    # Use the first CSV to detect priorities
    df0 = dfs[0]
    if args.xcol not in df0.columns:
        raise SystemExit(f"Column {args.xcol!r} not found in {csv_paths[0]}")

    prios = detect_priority_columns(df0)
    if not prios:
        raise SystemExit(
            f"No 'prio*_run_time_s' columns found in {csv_paths[0]}. "
            "Did you pass the correct trace-monitor CSV?"
        )

    # Ensure all other CSVs have the required columns
    required_cols = [args.xcol] + [col for _, col in prios]
    for p, df in zip(csv_paths, dfs):
        missing = [c for c in required_cols if c not in df.columns]
        if missing:
            raise SystemExit(
                f"CSV {p} is missing required columns: {', '.join(missing)}"
            )

    # Layout: grid of subplots, one per priority
    n_prios = len(prios)
    ncols = min(3, n_prios)
    nrows = math.ceil(n_prios / ncols)

    fig_width = 4 * ncols
    fig_height = 3 * nrows
    fig, axes = plt.subplots(
        nrows=nrows,
        ncols=ncols,
        sharex=True,
        figsize=(fig_width, fig_height),
    )

    if isinstance(axes, plt.Axes):
        axes_list = [axes]
    else:
        axes_list = axes.ravel()

    # Plot per priority
    for idx, (prio, col) in enumerate(prios):
        ax = axes_list[idx]

        for df, label in zip(dfs, labels):
            x = df[args.xcol].values
            y = df[col].values
            # Let matplotlib handle colors; just fix line width.
            ax.plot(x, y, label=label, linewidth=1.5)

        ax.set_title(f"Priority {prio}")
        ax.grid(True, linestyle="--", linewidth=0.5, alpha=0.6)

        if idx % ncols == 0:
            ax.set_ylabel("Cumulative runtime (pod-seconds)")
        if idx >= n_prios - ncols:
            ax.set_xlabel(args.xcol)

    # Hide unused axes, if any
    for j in range(n_prios, len(axes_list)):
        axes_list[j].set_visible(False)

    # Collect legend entries from first visible axis
    first_ax = next((ax for ax in axes_list if ax.get_visible()), axes_list[0])
    handles, lbls = first_ax.get_legend_handles_labels()
    if handles:
        fig.legend(
            handles,
            lbls,
            loc="upper center",
            bbox_to_anchor=(0.5, 0.98),
            ncol=max(1, len(lbls)),
            frameon=False,
        )

    fig.tight_layout(rect=(0.02, 0.03, 0.98, 0.93))

    # Output path
    if args.out:
        out_path = Path(args.out)
    else:
        first = csv_paths[0]
        out_path = first.with_name(first.stem + "_prio_grid.png")

    out_path.parent.mkdir(parents=True, exist_ok=True)
    fig.savefig(out_path, dpi=200)
    print(f"Saved figure to: {out_path}")

    if not args.no_show:
        plt.show()


if __name__ == "__main__":
    main()
