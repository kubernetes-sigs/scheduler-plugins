from pathlib import Path
import pandas as pd
import numpy as np
import matplotlib as mpl
import matplotlib.pyplot as plt
import matplotlib.patches as mpatches
import matplotlib.ticker as mtick

#################################################################
# Load data
#################################################################
DF_PER_COMBO_PATH = Path("analysis/per_combo_results.csv")
df_per_combo = pd.read_csv(DF_PER_COMBO_PATH)

OUT_DIR = Path("analysis/figures")
OUT_DIR.mkdir(parents=True, exist_ok=True)

#################################################################
# Global plotting settings
#################################################################
# filters
PLOT_PPNS       = [4, 8]
PLOT_PRIORITIES = [1, 2, 4]
PLOT_TIMEOUTS   = [1, 10, 20]

# precision
EPS = 1e-9

# fonts
TITLE_FS  = 7
LABEL_FS  = 7
TICK_FS   = 6
LEGEND_FS = 6
ANNOT_FS  = 4
mpl.rcParams.update({
    "axes.titlesize": TITLE_FS,
    "axes.labelsize": LABEL_FS,
    "xtick.labelsize": TICK_FS,
    "ytick.labelsize": TICK_FS,
})

# labels
TARGET_UTIL_LABEL   = "target util (%)"
NODES_LABEL         = "# of nodes"
INSTANCES_LABEL     = "% of instances"
PODS_PER_NODE_LABEL = "pods/node"

# Legend formatting
LEGEND_HANDLE_LENGTH=1.0  # length of color boxes
LEGEND_TEXT_PAD=0.3       # space between box and text
LEGEND_COLUMN_SPACING=0.8 # space between entries

# figure saving
FIGURE_DPI = 300 # default = 100
FIGURE_FORMATS = ["pdf", "png"]

# 2d sizes
FIGSIZE_2D = (3.5, 2.5)
CELL_FIGSIZE_2D = (2.2, 1.6)
BAR_WIDTH_2D = 0.9

# 3d sizes
FIGSIZE_3D = (8, 4)
BAR_WIDTH_3D = 0.13
ELEV_3D, AZIM_3D = 20.0, -54.0

# colors / series (stack order = bottom -> top)
set2, set3 = plt.get_cmap("Set2").colors, plt.get_cmap("Set3").colors
CATEGORIES = [
    {"key": "other",             "label": "Other",          "col": "other_rate",               "color": set2[3]},
    {"key": "solver_optimal",    "label": "Better&Optimal", "col": "solver_optimal_rate",      "color": set2[0]},
    {"key": "solver_feasible",   "label": "Better",         "col": "solver_feasible_rate",     "color": set2[1]},
    {"key": "default_optimal",   "label": "KWOK Optimal",   "col": "default_optimal_rate",     "color": set2[2]},
    {"key": "default_all",       "label": "No Calls",       "col": "default_all_running_rate", "color": set3[11]},
    {"key": "solver_failed",     "label": "Failures",       "col": "solver_failed_rate",       "color": set2[7]},
]

#################################################################
# Global table settings
#################################################################
TABLES_PPNS     = [4, 8]
TABLES_UTILS    = [90.0, 95.0, 100.0, 105.0]
TABLES_NODES    = [4, 8, 16, 32]
TABLES_PRIORITY = 4
TABLES_TIMEOUT  = 10
TABLES_DECIMALS = 1

#################################################################
# Plotting helpers
#################################################################
def safe_div(num, den):
    return num.div(den.replace(0, np.nan)).fillna(0.0)

def save_figure(fig: mpl.figure.Figure, out_path: Path):
    """
    Save a Matplotlib figure to all formats in FORMATS using the global DPI.
    out_path should be given without extension.
    """
    out_path.parent.mkdir(parents=True, exist_ok=True)
    for ext in FIGURE_FORMATS:
        fname = out_path.with_suffix(f".{ext}")
        fig.savefig(fname, dpi=FIGURE_DPI, bbox_inches="tight")
    plt.close(fig)
    print(f"[ok] saved figure: {out_path} ({', '.join(FIGURE_FORMATS)})")

#################################################################
# 2D bar plot as a grid of ppn vs priorities with util aggregated
#################################################################
def aggregate_over_util(per_combo_df: pd.DataFrame) -> pd.DataFrame:
    keys = ["pods_per_node","priorities","timeout_s","nodes"]
    
    # Sum counts & sums over util
    g = (
        per_combo_df
        .groupby(keys, as_index=False)
        .agg({
            "n_seeds": "sum",
            "n_seeds_not_all_running": "sum",
            "n_default_all_running": "sum",
            "n_solver_called": "sum",
            "n_solver_failed": "sum",
            "n_default_optimal": "sum",
            "n_solver_optimal": "sum",
            "n_solver_feasible": "sum",
            "n_solver_improve": "sum",
            "n_other": "sum",
            "solver_duration_ms_sum": "sum",
            "cpu_delta_sum": "sum",
            "mem_delta_sum": "sum",
        })
    )
    
    # Rates from summed counts
    g["default_all_running_rate"] = safe_div(g["n_default_all_running"], g["n_seeds"])
    g["solver_called_rate"]       = safe_div(g["n_solver_called"],        g["n_seeds"])
    g["solver_failed_rate"]       = safe_div(g["n_solver_failed"],        g["n_seeds"])
    g["default_optimal_rate"]     = safe_div(g["n_default_optimal"],      g["n_seeds"])
    g["solver_optimal_rate"]      = safe_div(g["n_solver_optimal"],       g["n_seeds"])
    g["solver_feasible_rate"]     = safe_div(g["n_solver_feasible"],      g["n_seeds"])
    g["solver_improve_rate"]      = safe_div(g["n_solver_improve"],       g["n_seeds"])
    g["other_rate"]               = safe_div(g["n_other"],                g["n_seeds"])
    g["solver_duration_ms_mean"]  = safe_div(g["solver_duration_ms_sum"], g["n_solver_called"])
    g["cpu_delta_mean"]           = safe_div(g["cpu_delta_sum"],          g["n_seeds"])
    g["mem_delta_mean"]           = safe_div(g["mem_delta_sum"],          g["n_seeds"])
    return g.copy()

def plot_2d_grid_ppn_prio_with_aggregated_util(
    df_util_agg: pd.DataFrame,
    ppns: list[int],
    priorities: list[int],
    out_path: Path,
    cell_figsize: tuple[float, float]
):
    nrows, ncols = len(ppns), len(priorities)
    fig, axes = plt.subplots(
        nrows, ncols,
        figsize=(ncols * cell_figsize[0], nrows * cell_figsize[1]),
        sharex=True, sharey=True, squeeze=False
    )

    # Shared y-label for the whole figure
    fig.supylabel("% of instances", fontsize=LABEL_FS, x=0.03)

    seen_keys = set()
    
    for r, ppn in enumerate(ppns):
        
        for c, prio in enumerate(priorities):
            ax = axes[r][c]
            panel = df_util_agg[(df_util_agg["pods_per_node"] == ppn) & (df_util_agg["priorities"] == prio)].copy()
            
            # if no data for this (ppn, prio), skip
            if panel.empty:
                ax.axis("off")
                continue
            
            # one row per (nodes, timeout_s)
            panel = panel.groupby(["nodes", "timeout_s"], as_index=False)[[s["col"] for s in CATEGORIES if s["col"].endswith("_rate")]].mean()
            nodes_vals = sorted(panel["nodes"].unique().tolist())
            ts = sorted(panel["timeout_s"].unique().tolist())
            bars_per_group = max(1, len(ts))
            width = BAR_WIDTH_2D / bars_per_group
            x = np.arange(len(nodes_vals))

            for j, timeout in enumerate(ts):
                sub = panel[panel["timeout_s"] == timeout].set_index("nodes")
                xj = x + (j - (bars_per_group - 1) / 2.0) * width
                bar_height = np.zeros(len(nodes_vals), dtype=float)

                for category in CATEGORIES:
                    vals = (sub.get(category["col"], pd.Series(0.0, index=sub.index)).reindex(nodes_vals).fillna(0.0).values)
                    h = vals * 100.0
                    if np.any(h > EPS): # if any height > small threshold (epsilon)
                        ax.bar(
                            xj, h, width=width, bottom=bar_height,
                            color=category["color"], edgecolor="black", linewidth=0.35
                        )
                        bar_height += h
                        seen_keys.add(category["key"])

                # annotate timeout above each bar
                for xi, top in zip(xj, bar_height):
                    ax.text(float(xi), float(top) + 1.0, f"{int(timeout)}s", ha="center", va="bottom", fontsize=ANNOT_FS)

            ax.set_xticks(x)
            ax.set_xticklabels([str(n) for n in nodes_vals], fontsize=TICK_FS)
            ax.set_ylim(0, 110)
            ax.set_yticks([0, 20, 40, 60, 80, 100])
            ax.yaxis.set_major_formatter(mtick.PercentFormatter(100.0))
            ax.tick_params(axis='y', labelsize=TICK_FS)

            # columns (priorities)
            if r == 0:
                ax.set_title(f"priority={prio}", fontsize=TITLE_FS)

            # rows (pods per node)
            if c == 0:
                if nrows % 2 == 1 and r == nrows // 2:
                    axis_text = f"{INSTANCES_LABEL}\n{PODS_PER_NODE_LABEL}={ppn}"
                else:
                    axis_text = f"\n{PODS_PER_NODE_LABEL}={ppn}"
                lbl = ax.set_ylabel(axis_text, fontsize=LABEL_FS, labelpad=10)
                lbl.set_va('center')
                lbl.set_ha('center')
                lbl.set_linespacing(1.8)

            # bottom row (nodes)
            if r == nrows - 1:
                ax.set_xlabel(NODES_LABEL, fontsize=LABEL_FS)

    # legend
    legends = [s for s in CATEGORIES if s["key"] in seen_keys][::-1]
    legend_handles = [mpatches.Rectangle((0, 0), 1, 1, fc=s["color"]) for s in legends]
    legend_labels  = [s["label"] for s in legends]
    fig.legend(
        legend_handles, legend_labels,
        loc="upper center",
        bbox_to_anchor=(0.53, 1.02),
        prop={"size": LEGEND_FS},
        ncol=len(legend_labels),
        handlelength=LEGEND_HANDLE_LENGTH,
        handletextpad=LEGEND_TEXT_PAD,
        columnspacing=LEGEND_COLUMN_SPACING,
    )
    
    # save figure
    save_figure(fig, out_path)

plot_2d_grid_ppn_prio_with_aggregated_util(
    df_util_agg=aggregate_over_util(df_per_combo),
    ppns=PLOT_PPNS,
    priorities=PLOT_PRIORITIES,
    out_path=OUT_DIR / "2d_grid_ppn_prio",
    cell_figsize=CELL_FIGSIZE_2D,
)

###############################################################
# 3D bar plot of ppn vs priorities vs timeout
###############################################################
def plot_3d_ppn_prio_timeout(df: pd.DataFrame, title: str, out_path: Path):
    utils = sorted(df["util"].unique().tolist())
    nodes = sorted(int(n) for n in df["nodes"].unique().tolist())

    def get_rate(df: pd.DataFrame, util_val, nodes_val, col):
        df_idx = df.set_index(["util","nodes"])
        try:
            return float(df_idx.loc[(util_val, nodes_val), col])
        except KeyError:
            return 0.0

    x_index = {r: i for i, r in enumerate(utils)}
    y_index = {n: j for j, n in enumerate(nodes)}
    
    fig, ax = plt.subplots(
        figsize=FIGSIZE_3D,
        subplot_kw={"projection": "3d"}
    )
    ax.view_init(elev=ELEV_3D, azim=AZIM_3D)
    ax.set_proj_type("ortho")

    ax.set_xlim(0, len(utils))
    ax.set_ylim(0, len(nodes))
    ax.set_zlim(0, 100)

    ax.set_xticks([x_index[r] + 0.5 for r in utils])
    ax.set_yticks([y_index[n] + 0.5 for n in nodes])
    ax.set_zticks([0, 20, 40, 60, 80, 100])
    ax.zaxis.set_major_formatter(mtick.PercentFormatter(100.0))
    
    ax.set_xticklabels([f"{int(round(r))}%" for r in utils], fontsize=TICK_FS)
    ax.set_yticklabels([str(n) for n in nodes], fontsize=TICK_FS)
    
    ax.tick_params(axis='x', labelsize=TICK_FS, pad=-2)
    ax.tick_params(axis='y', labelsize=TICK_FS, pad=-2)
    ax.tick_params(axis='z', labelsize=TICK_FS, pad=-1)
    
    ax.set_xlabel(TARGET_UTIL_LABEL, fontsize=LABEL_FS, labelpad=-3)
    ax.set_ylabel(NODES_LABEL, fontsize=LABEL_FS, labelpad=-3)
    ax.set_zlabel(INSTANCES_LABEL, fontsize=LABEL_FS, labelpad=-4)
    
    ax.set_title(title, fontsize=TITLE_FS, y=1.01, pad=0)

    dx = max(0.05, min(1.0, BAR_WIDTH_3D))
    dy = max(0.05, min(1.0, BAR_WIDTH_3D))

    seen_keys = set()
    for u in utils:
        for n in nodes:
            x0 = x_index[u] + (1 - dx)/2
            y0 = y_index[n] + (1 - dy)/2
            z = 0.0

            rate_solver_opt    = get_rate(df, u, n, "solver_optimal_rate")
            rate_solver_feas   = get_rate(df, u, n, "solver_feasible_rate")
            rate_solver_fail   = get_rate(df, u, n, "solver_failed_rate")
            rate_default_opt   = get_rate(df, u, n, "default_optimal_rate")
            rate_default_all   = get_rate(df, u, n, "default_all_running_rate")
            rate_other         = get_rate(df, u, n, "other_rate")

            for key, rate in [
                ("other",           rate_other),
                ("solver_optimal",  rate_solver_opt),
                ("solver_feasible", rate_solver_feas),
                ("default_optimal", rate_default_opt),
                ("default_all",     rate_default_all),
                ("solver_failed",   rate_solver_fail),
            ]:
                h = rate * 100.0
                if h > EPS: # if height > small threshold (epsilon)
                    color = next(s["color"] for s in CATEGORIES if s["key"] == key)
                    ax.bar3d(
                        x0, y0, z, dx, dy, h,
                        color=color, edgecolor="black", linewidth=0.35, shade=False,
                    )
                    z += h
                    seen_keys.add(key)

    # legend
    legends = [s for s in CATEGORIES if s["key"] in seen_keys][::-1]
    legend_handles = [mpatches.Rectangle((0,0),1,1, fc=s["color"]) for s in legends]
    legend_labels  = [s["label"] for s in legends]
    fig.legend(
        legend_handles, legend_labels,
        loc="upper center",
        bbox_to_anchor=(0.545, 0.92),
        prop={"size": LEGEND_FS},
        ncol=len(legend_labels),  # one row
        handlelength=LEGEND_HANDLE_LENGTH,  # shorter color boxes
        handletextpad=LEGEND_TEXT_PAD, # space between box and text
        columnspacing=LEGEND_COLUMN_SPACING, # space between entries
    )

    # save figure
    save_figure(fig, out_path)

# one 3D bar plot per (ppn, prio, timeout)
for ppn in PLOT_PPNS:
    for prio in PLOT_PRIORITIES:
        for t in PLOT_TIMEOUTS:
            sub = df_per_combo[(df_per_combo["pods_per_node"] == ppn) & (df_per_combo["priorities"] == prio) & (df_per_combo["timeout_s"] == t)]
            if sub.empty:
                print(f"[skip] no per-combo rows for ppn={ppn}, prio={prio}, t={t}")
                continue
            title = f"{PODS_PER_NODE_LABEL}={ppn}, priorities={prio}, timeout={t}s"
            out_file = OUT_DIR / f"3d_ppn{ppn}_prio{prio}_timeout{t:02d}"
            plot_3d_ppn_prio_timeout(sub, title, out_file)


###############################################################
# Tables
###############################################################
# filter data for table
df_solver_stats = df_per_combo[
    (df_per_combo["priorities"] == TABLES_PRIORITY) &
    (df_per_combo["timeout_s"] == TABLES_TIMEOUT) &
    (df_per_combo["pods_per_node"].isin(TABLES_PPNS)) &
    (df_per_combo["util"].isin(TABLES_UTILS)) &
    (df_per_combo["nodes"].isin(TABLES_NODES))
].copy()

# helper: metric pivot (util x (ppn,nodes)) → formatted strings
def metric_pivot(values: pd.Series) -> pd.DataFrame:
    p = (pd.concat([df_solver_stats[["util","pods_per_node","nodes"]], values], axis=1).pivot_table(index="util", columns=["pods_per_node","nodes"], values=values.name, aggfunc="mean"))
    p = p.reindex(index=TABLES_UTILS, columns=pd.MultiIndex.from_product([TABLES_PPNS, TABLES_NODES], names=["ppn","nodes"]))
    p = p.astype(float).round(TABLES_DECIMALS)
    return p.map(lambda x: f"{x:.{TABLES_DECIMALS}f}" if pd.notna(x) else "—")

# build metric tables
solver_sec   = (df_solver_stats["solver_duration_ms_mean"] / 1000.0).rename("solver_sec")
cpu_delta_pc = (df_solver_stats["cpu_delta_mean"] * 100.0).rename("cpu_delta_pc")
mem_delta_pc = (df_solver_stats["mem_delta_mean"] * 100.0).rename("mem_delta_pc")

tab_dur = metric_pivot(solver_sec)
tab_cpu = metric_pivot(cpu_delta_pc)
tab_mem = metric_pivot(mem_delta_pc)

# assemble console DataFrame (rows=(util, metric), cols=(ppn,nodes))
rows = []
for u in TABLES_UTILS:
    rows.append(pd.Series(tab_dur.loc[u], name=(f"{int(u)}%", "solver duration (s)")))
    rows.append(pd.Series(tab_cpu.loc[u], name=(f"{int(u)}%", "Δ cpu util (%)")))
    rows.append(pd.Series(tab_mem.loc[u], name=(f"{int(u)}%", "Δ mem util (%)")))

# build and print LaTeX tabular to console
def fmt_cell(s: str) -> str:
    return r"\multicolumn{1}{c}{—}" if s == "—" else s

lines = []
lines.append(r"\begin{tabular}{@{}c l *{8}{c}@{}}")
lines.append(r"\toprule")
lines.append(r"\multirow{2}{*}{\textbf{util}} & \multirow{2}{*}{\textbf{metric}} &")
lines.append(r"\multicolumn{4}{c}{\textbf{ppn = 4}} & \multicolumn{4}{c}{\textbf{ppn = 8}}\\")
lines.append(r"\cmidrule(lr){3-6}\cmidrule(lr){7-10}")
lines.append(r" &  & \textbf{4} & \textbf{8} & \textbf{16} & \textbf{32} & \textbf{4} & \textbf{8} & \textbf{16} & \textbf{32}\\")
lines.append(r"\midrule")

for u in TABLES_UTILS:
    util_label = f"{int(u)}\\%"
    lines.append(rf"\multirow{{{3 if int(u)==90 else 2}}}{{*}}{{{util_label}}}")
    def emit_row(metric_label: str, tab: pd.DataFrame):
        left  = [fmt_cell(tab.loc[u, (4, n)]) for n in TABLES_NODES]
        right = [fmt_cell(tab.loc[u, (8, n)]) for n in TABLES_NODES]
        lines.append("& " + metric_label + " & " + " & ".join(left + right) + r" \\")
    emit_row(r"solver\,duration\,(s)", tab_dur)
    emit_row(r"$\Delta$\,cpu\,util\,(\%)", tab_cpu)
    emit_row(r"$\Delta$\,mem\,util\,(\%)", tab_mem)
    lines.append(r"\midrule" if u != TABLES_UTILS[-1] else r"\bottomrule")
lines.append(r"\end{tabular}")

latex_block = "\n".join(lines)

print("\n# === LaTeX tabular ===\n")
print(latex_block)