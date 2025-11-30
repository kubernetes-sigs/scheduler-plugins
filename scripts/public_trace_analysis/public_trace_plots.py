#!/usr/bin/env python3
# public_trace_plots.py

import pandas as pd
import matplotlib.pyplot as plt
from typing import Dict, Any, List
from scripts.helpers.trace_helpers import plot_histogram

# ----------------------------------------------------------------------
# CONFIG
# ----------------------------------------------------------------------
# alpha: shape parameter - controls the tail heaviness
# x_min: scale / lower bound of the distribution

DATASETS: List[Dict[str, Any]] = [
    {
        "name": "Google",
        "csv_path": "google-cluster-data/ClusterData2019/data.csv",
        "plots": {
            "inter_arrival_us": {
                "x_label": "Δt (milliseconds)",
                "scale": 1e-3,  # µs → ms
                "fit_pareto": True,
                "pareto_alpha": 0.21,
                "pareto_xmin": 0.001,
            },
            "life_time_us": {
                "fit_pareto": True,
                "pareto_alpha": 0.72,
                "pareto_xmin": 1.0,
            },
            # Use Pareto I for CPU and MEM
            "cpu_request": {
                "x_label": "Requested CPU (fraction of node capacity)",
                "x_max": 0.2,
                "fit_pareto": True,
                "pareto_alpha": 2.5,
                "pareto_xmin": 0.005,
            },
            "mem_request": {
                "x_label": "Requested memory (fraction of node capacity)",
                "x_max": 0.2,
                "fit_pareto": True,
                "pareto_alpha": 2.5,
                "pareto_xmin": 0.005,
            },
            "priority": {
                "x_max": 250,
            },
        },
    },
    {
        "name": "Alibaba",
        "csv_path": "alibaba-clusterdata/cluster-trace-gpu-v2025/data.csv",
        "plots": {
            "inter_arrival_us": {
                "fit_pareto": True,
                "pareto_alpha": 0.46,
                "pareto_xmin": 10.0,
            },
            "life_time_us": {
                "fit_pareto": True,
                "pareto_alpha": 0.43,
                "pareto_xmin": 1.0,
            },
            "cpu_request": {
                "x_label": "Requested CPU (cores)",
            },
            "mem_request": {
                "x_label": "Requested memory (GB)",
            },
        },
    },
]

# ----------------------------------------------------------------------
# BASE PLOT CONFIG
# ----------------------------------------------------------------------

BASE_PLOT_SPECS = [
    {
        "column": "inter_arrival_us",
        "title": "Inter-arrival time distribution",
        "x_label": "Δt (seconds)",
        "y_label": "Probability density",
        "bins": 100,
        "log_y": True,
        "y_min": 1e-6,
        "y_max": 1e-1,
        "x_max": 4_000.0,       # after scaling
        "scale": 1e-6,          # µs → seconds
        "fit_pareto": False,
    },
    {
        "column": "life_time_us",
        "title": "Instance lifetime distribution",
        "x_label": "Lifetime (hours)",
        "y_label": "Probability density",
        "bins": 100,
        "log_y": True,
        "y_min": 1e-5,
        "y_max": 1e-1,
        "x_max": 550.0,
        "scale": 1.0 / (1_000_000.0 * 3600.0),  # µs → hours
        "fit_pareto": False,
    },
    {
        "column": "cpu_request",
        "title": "CPU request distribution",
        "x_label": "Requested CPU",
        "y_label": "Probability density",
        "bins": 100,
        "log_y": True,
        "y_min": 1e-4,
        "y_max": 1e3,
        "x_max": None,
        "scale": 1.0,
        "fit_pareto": False,
    },
    {
        "column": "mem_request",
        "title": "Memory request distribution",
        "x_label": "Requested memory",
        "y_label": "Probability density",
        "bins": 100,
        "log_y": True,
        "y_min": 1e-4,
        "y_max": 1e3,
        "x_max": None,
        "scale": 1.0,
        "fit_pareto": False,
    },
    {
        "column": "priority",
        "title": "Priority distribution",
        "x_label": "Priority",
        "y_label": "Probability density",
        "bins": 10,
        "log_y": False,
        "y_min": None,
        "y_max": None,
        "x_max": None,
        "scale": 1.0,
        "fit_pareto": False,
    },
]



def build_plot_specs_for_dataset(per_column_cfg: Dict[str, Dict[str, Any]] | None) -> Dict[str, Dict[str, Any]]:
    """
    Take BASE_PLOT_SPECS and apply per-column configs.
    """
    per_column_cfg = per_column_cfg or {}
    specs_by_col: Dict[str, Dict[str, Any]] = {}
    for base in BASE_PLOT_SPECS:
        col = base["column"]
        spec = dict(base)  # shallow copy
        if col in per_column_cfg:
            overrides = {k: v for k, v in per_column_cfg[col].items()}
            spec.update(overrides)
        specs_by_col[col] = spec
    return specs_by_col

# ----------------------------------------------------------------------
# Combined grid plot
# ----------------------------------------------------------------------

def make_combined_grid() -> None:
    """
    Create a grid of histograms:
    - Columns = metrics (in BASE_PLOT_SPECS order)
    - Rows = datasets (Google, Alibaba, ...)
    """
    loaded: List[Dict[str, Any]] = []
    for ds in DATASETS:
        name = ds["name"]
        csv_path = ds["csv_path"]
        df = pd.read_csv(csv_path)
        print(f"[INFO] Loaded {len(df)} rows from {csv_path} for dataset {name}")
        specs_by_col = build_plot_specs_for_dataset(ds.get("plots"))
        loaded.append({"name": name, "df": df, "specs_by_col": specs_by_col})

    # Columns = metrics (in BASE_PLOT_SPECS order)
    metric_columns = [spec["column"] for spec in BASE_PLOT_SPECS]

    # Rows = datasets (Google, Alibaba, ...)
    n_rows = len(loaded)
    n_cols = len(metric_columns)

    # Create grid figure
    fig, axes = plt.subplots(
        n_rows,
        n_cols,
        figsize=(2.1 * n_cols, 1.6 * n_rows),
        squeeze=False,
    )

    for row_idx, ds_info in enumerate(loaded):
        ds_name = ds_info["name"]
        df = ds_info["df"]
        specs_by_col = ds_info["specs_by_col"]

        for col_idx, col_name in enumerate(metric_columns):
            ax = axes[row_idx, col_idx]

            # If this metric isn't present in this dataset, hide the axes
            if col_name not in df.columns:
                ax.axis("off")
                continue

            spec = specs_by_col[col_name]
            data = df[col_name].to_numpy()
            
            plot_histogram(
                ax,
                data,
                title="",
                x_label=spec["x_label"],
                y_label=(rf"$\bf{{{ds_name}}}$"f"\n\n{spec['y_label']}" if col_idx == 0 else ""),
                bins=spec["bins"],
                x_max=spec.get("x_max"),
                y_min=spec.get("y_min"),
                y_max=spec.get("y_max"),
                log_y=spec["log_y"],
                scale=spec.get("scale", 1.0),
                pareto_fit=spec.get("fit_pareto", False),
                pareto_alpha=spec.get("pareto_alpha"),
                pareto_xmin=spec.get("pareto_xmin"),
            )

    # Space adjustments
    fig.tight_layout(
        rect=(0.01, 0.01, 0.99, 0.99),
        pad=0.2,    # overall padding around the figure (default ~1.08)
        w_pad=0.2,  # width padding between subplots (inches)
        h_pad=0.2,  # height padding between subplots (inches)
    )

    out_path = "histograms.png"
    fig.savefig(out_path, dpi=300, bbox_inches="tight")
    plt.close(fig)
    print(f"[OK] Saved combined grid to {out_path}")

# ----------------------------------------------------------------------
# main
# ----------------------------------------------------------------------

def main() -> None:
    make_combined_grid()

if __name__ == "__main__":
    main()
