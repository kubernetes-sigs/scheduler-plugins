#!/usr/bin/env python3
# trace_helpers.py

from dataclasses import dataclass
import numpy as np
from scipy.stats import pareto as pareto_dist

MC_MEAN_SAMPLES = 100_000  # number of MC samples for mean estimation
MC_MEAN_SEED = 12345       # fixed seed for reproducibility

# -------------------------------------------------------------------
# Font size configuration (single source of truth)
# -------------------------------------------------------------------
AXIS_LABEL_FONTSIZE = 5.5
TICK_LABEL_FONTSIZE = 4.0
TITLE_FONTSIZE = 6.5
LEGEND_FONTSIZE = 4.0

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