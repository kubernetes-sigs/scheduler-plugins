import numpy as np
import pytest


pytest.importorskip("matplotlib")
pytest.importorskip("scipy")

from scripts.kwok_trace_replayer import trace_helpers as th


def test_trace_record_defaults():
    rec = th.TraceRecord(id=1, start_time=0.0, end_time=1.0, cpu=0.1, mem=0.2, priority=3)
    assert rec.replicas == 1


def test_estimate_pareto_params_returns_positive_params():
    # Use a small synthetic positive dataset; just validate we get sane positive estimates.
    data = np.array([1.0, 1.2, 1.5, 2.0, 3.0, 5.0], dtype=float)
    est = th.estimate_pareto_params(data)
    assert est is not None
    alpha, x_min = est
    assert alpha > 0
    assert x_min > 0
