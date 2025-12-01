#!/usr/bin/env python3
# trace_helpers.py

from dataclasses import dataclass
import numpy as np
import matplotlib.pyplot as plt
from typing import Any, List
from scipy.stats import pareto as pareto_dist

MC_MEAN_SAMPLES = 100_000 # number of MC samples for mean estimation
MC_MEAN_SEED = 12345      # fixed seed for reproducibility

# -------------------------------------------------------------------
# Data model
# -------------------------------------------------------------------
@dataclass
class TraceRecord:
    id: int
    start_time: float
    end_time: float
    cpu: float
    mem: float
    priority: int
    replicas: int = 1

# ----------------------------------------------------------------------
# Pareto I estimation helper
# ----------------------------------------------------------------------

def estimate_pareto_params(pos_data: np.ndarray) -> tuple[float, float] | None:
    """
    Estimate Pareto parameters via MLE using SciPy's pareto.
    We fix loc=0 so that the support is x >= x_min (scale_hat).
    Returns (alpha, x_min) or None if estimation fails.
    """
    b_hat, _, scale_hat = pareto_dist.fit(pos_data, floc=0.0)
    if scale_hat <= 0 or b_hat <= 0:
        return None
    return float(b_hat), float(scale_hat)  # alpha, x_min

# ----------------------------------------------------------------------
# Plot helpers
# ----------------------------------------------------------------------

def plot_histogram(
    ax: plt.Axes,
    data: np.ndarray,
    *,
    title: str,
    x_label: str,
    y_label: str,
    bins: int,
    x_max: float | None = None,
    y_min: float | None = None,
    y_max: float | None = None,
    log_y: bool = False,
    scale: float = 1.0,
    pareto_fit: bool = False,
    pareto_alpha: float | None = None,
    pareto_xmin: float | None = None,
) -> None:
    """
    Plot to an existing axes:
    - Histogram as probability density (area ≈ 1)
    - Optional Pareto PDF
    - Sample mean line
    """
    # Scale data
    data_scaled = np.asarray(data, dtype=float) * scale

    # Drop NaN and non-finite values (inf, -inf)
    finite_mask = np.isfinite(data_scaled)
    data_scaled = data_scaled[finite_mask]

    # Sample mean over all data
    mean_val = float(np.mean(data_scaled))

    # Crop data to x_max for histogram and pareto fitting
    if x_max is not None:
        data_for_hist = data_scaled[data_scaled <= x_max]
        if data_for_hist.size == 0:
            # If everything is above x_max, fall back to all data
            data_for_hist = data_scaled
    else:
        data_for_hist = data_scaled

    # Safe bin count
    n_points = data_for_hist.size
    n_unique = np.unique(data_for_hist).size
    max_bins_allowed = max(1, min(n_points, n_unique))
    bins_eff = min(bins, max_bins_allowed)

    # Histogram as PDF
    ax.hist(data_for_hist, bins=bins_eff, density=True)

    plot_min = float(np.min(data_for_hist))
    plot_max = float(np.max(data_for_hist))

    legend_handles: List[Any] = []
    legend_labels: List[str] = []

    # Fix rng for MC mean so result is deterministic
    rng_mc = np.random.default_rng(MC_MEAN_SEED)

    # --- Optional Pareto curve ---
    if pareto_fit:
        
        # Estimate pareto parameters if not provided
        if pareto_alpha is None or pareto_xmin is None:
            pareto_alpha, pareto_xmin = estimate_pareto_params(data_scaled[data_scaled > 0.0])

        lo = max(plot_min, pareto_xmin)
        hi = plot_max
        x_fit = np.linspace(lo, hi, 400)
        
        # Pareto PDF: f(x) = alpha * x_min^alpha / x^(alpha+1), x >= x_min
        pareto_pdf_vals = pareto_alpha * (pareto_xmin ** pareto_alpha) / (x_fit ** (pareto_alpha + 1.0))
        pareto_line, = ax.plot(x_fit, pareto_pdf_vals, linewidth=1.5, linestyle="-")

        # Compute an MC mean
        if x_max is not None: # Truncated/clamped case, matching sample_pareto()
            u = rng_mc.random(MC_MEAN_SAMPLES)
            u_min_tail = (pareto_xmin / x_max) ** pareto_alpha # in (0, 1)
            u_min = max(1e-12, float(u_min_tail)) # avoid zero
            u = np.clip(u, u_min, 1.0 - 1e-12) # avoid one
            samples = pareto_xmin / (u ** (1.0 / pareto_alpha)) # Inverse CDF
            samples = np.minimum(samples, x_max) # clamp at x_max
        else: # Unbounded Pareto (clipping only)
            u = rng_mc.random(MC_MEAN_SAMPLES)
            u = np.clip(u, 1e-12, 1.0 - 1e-12)
            samples = pareto_xmin / (u ** (1.0 / pareto_alpha))

        mc_mean = float(samples.mean())
        mean_str = f", MC-mean={mc_mean:.2f}"

        if pareto_line is not None:
            label = (
                r"Pareto: "
                rf"$\alpha\!=\!{pareto_alpha:.3f}$, "
                rf"$x_{{\min}}\!=\!{pareto_xmin:.3f}$"
                f"{mean_str}"
            )
            legend_handles.append(pareto_line)
            legend_labels.append(label)

    # --- Sample mean line ---
    if plot_min <= mean_val <= plot_max:
        mean_line = ax.axvline(mean_val, linestyle="--", alpha=0.8)
        legend_handles.append(mean_line)

    mean_label = f"True mean={mean_val:.3f}"
    legend_labels.append(mean_label)

    # Axis scales and labels
    if log_y:
        ax.set_yscale("log")
    if y_max is not None:
        ax.set_ylim(top=y_max)
    if y_min is not None:
        ax.set_ylim(bottom=y_min)

    ax.set_xlabel(x_label, fontsize=5.5)
    ax.set_ylabel(y_label, fontsize=5.5)
    ax.tick_params(axis="both", which="major", labelsize=5)
    ax.grid(True, axis="y", linestyle="--", alpha=0.4)
    ax.set_title(title, fontsize=7.5)

    if legend_handles:
        ax.legend(legend_handles, legend_labels, fontsize=4)