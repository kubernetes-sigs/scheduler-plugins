import argparse
import math
from pathlib import Path

import pytest


pytest.importorskip("numpy")
pytest.importorskip("matplotlib")

from scripts.kwok_trace_replayer import trace_generator as tg


def test_build_arg_parser_requires_means_and_has_defaults(tmp_path: Path):
    p = tg.build_arg_parser()
    args = p.parse_args(
        [
            "--output-dir",
            str(tmp_path),
            "--mean-arrival",
            "1.0",
            "--mean-life",
            "10.0",
            "--mean-cpu",
            "0.2",
            "--mean-mem",
            "0.3",
        ]
    )

    assert args.output_dir == str(tmp_path)
    assert args.seed == 42
    assert args.num_nodes == 8
    assert args.trace_time == "3600s"
    assert args.priority_min == 1
    assert args.priority_max == 3
    assert args.replicas_min == 1
    assert args.replicas_max == 1


def test_round_float_args_rounds_only_floats():
    ns = argparse.Namespace(a=1.23456, b=2, c="x", d=3.14159)
    tg.round_float_args(ns, ndigits=2)
    assert ns.a == 1.23
    assert ns.b == 2
    assert ns.c == "x"
    assert ns.d == 3.14


def test_sample_pareto_validates_and_respects_xmax():
    rng = tg.np.random.default_rng(123)

    with pytest.raises(ValueError):
        tg.TraceGenerator._sample_pareto(rng, alpha=0.0, x_min=1.0, x_max=None, size=10)

    with pytest.raises(ValueError):
        tg.TraceGenerator._sample_pareto(rng, alpha=1.0, x_min=0.0, x_max=None, size=10)

    x = tg.TraceGenerator._sample_pareto(rng, alpha=2.0, x_min=1.0, x_max=2.0, size=1000)
    assert (x >= 1.0).all()
    assert (x <= 2.0 + 1e-9).all()


def test_build_geometric_support_validates_and_returns_probs():
    values, probs = tg.TraceGenerator._build_geometric_support(min_val=3, max_val=3, ratio=0.5)
    assert values.tolist() == [3]
    assert probs is None

    values, probs = tg.TraceGenerator._build_geometric_support(min_val=1, max_val=3, ratio=1.0)
    assert values.tolist() == [1, 2, 3]
    assert probs is None

    values, probs = tg.TraceGenerator._build_geometric_support(min_val=1, max_val=4, ratio=0.5)
    assert values.tolist() == [1, 2, 3, 4]
    assert probs is not None
    assert math.isclose(float(probs.sum()), 1.0, rel_tol=1e-9, abs_tol=1e-9)
    assert probs[0] >= probs[-1]

    with pytest.raises(ValueError):
        tg.TraceGenerator._build_geometric_support(min_val=1, max_val=2, ratio=0.0)


def test_delete_completed_pods_updates_cluster_state():
    state = tg.ClusterState(num_nodes=1, live_cpu=3.0, live_mem=4.0, live_pods=3)

    # heap entries: (end_time, cpu, mem, replicas)
    end_heap = [
        (1.0, 1.0, 1.0, 1),
        (2.0, 0.5, 0.5, 2),
        (5.0, 1.0, 2.0, 1),
    ]
    tg.heapq.heapify(end_heap)

    tg.TraceGenerator._delete_completed_pods(state, end_heap, t=2.0)

    # First two entries should be popped.
    assert len(end_heap) == 1
    assert state.live_cpu == pytest.approx(3.0 - 1.0 * 1 - 0.5 * 2)
    assert state.live_mem == pytest.approx(4.0 - 1.0 * 1 - 0.5 * 2)
    assert state.live_pods == 0


def test_solve_alpha_validates_target_mean_ranges():
    rng = tg.np.random.default_rng(0)

    with pytest.raises(ValueError):
        tg.TraceGenerator._solve_alpha_of_pareto_for_mean(rng, x_min=0.0, x_max=1.0, target_mean=0.5)

    with pytest.raises(ValueError):
        tg.TraceGenerator._solve_alpha_of_pareto_for_mean(rng, x_min=1.0, x_max=None, target_mean=1.0)

    with pytest.raises(ValueError):
        tg.TraceGenerator._solve_alpha_of_pareto_for_mean(rng, x_min=1.0, x_max=2.0, target_mean=3.0)


def test_solve_alpha_bracket_failure_raises(monkeypatch):
    rng = tg.np.random.default_rng(0)

    # Force MC mean to be constant regardless of alpha, so bracketing fails.
    def constant_sample(_rng, _alpha, _x_min, _x_max, size=1):
        return tg.np.full(size, 1.0, dtype=float)

    monkeypatch.setattr(tg.TraceGenerator, "_sample_pareto", staticmethod(constant_sample))
    monkeypatch.setattr(tg, "SOLVE_ALPHA_SAMPLES", 10)
    monkeypatch.setattr(tg, "SOLVE_ALPHA_MAX_ITERATIONS", 10)

    with pytest.raises(ValueError):
        tg.TraceGenerator._solve_alpha_of_pareto_for_mean(rng, x_min=0.5, x_max=2.0, target_mean=1.5)


def test_solve_alpha_converges_quickly_with_stubbed_sampling(monkeypatch):
    rng = tg.np.random.default_rng(0)

    # Deterministic decreasing "mean" as alpha increases.
    # Mean(alpha) = x_min + 1/alpha, clamped to x_max.
    def sample_for_alpha(_rng, alpha, x_min, x_max, size=1):
        v = float(x_min + (1.0 / float(alpha)))
        if x_max is not None:
            v = min(v, float(x_max))
        return tg.np.full(size, v, dtype=float)

    monkeypatch.setattr(tg.TraceGenerator, "_sample_pareto", staticmethod(sample_for_alpha))
    monkeypatch.setattr(tg, "SOLVE_ALPHA_SAMPLES", 1)
    monkeypatch.setattr(tg, "SOLVE_ALPHA_MAX_ITERATIONS", 200)
    monkeypatch.setattr(tg, "SOLVE_ALPHA_TOLERANCE", 1e-6)
    monkeypatch.setattr(tg, "SOLVE_ALPHA_LOWER_BOUND", 0.1)
    monkeypatch.setattr(tg, "SOLVE_ALPHA_UPPER_BOUND", 10.0)

    # target_mean is between endpoint means, so it should converge.
    # For x_min=0.5, mean(alpha)=0.5+1/alpha. Target 0.7 => alpha ~ 5.
    alpha = tg.TraceGenerator._solve_alpha_of_pareto_for_mean(
        rng,
        x_min=0.5,
        x_max=10.0,
        target_mean=0.7,
    )
    assert 0.1 <= alpha <= 10.0



def test_generate_trace_write_trace_and_run(tmp_path: Path, monkeypatch):
    p = tg.build_arg_parser()
    args = p.parse_args(
        [
            "--output-dir",
            str(tmp_path),
            "--trace-time",
            "5s",
            "--num-nodes",
            "2",
            "--mean-arrival",
            "1.0",
            "--mean-life",
            "2.0",
            "--mean-cpu",
            "0.2",
            "--mean-mem",
            "0.3",
            "--priority-min",
            "1",
            "--priority-max",
            "2",
            "--replicas-min",
            "1",
            "--replicas-max",
            "2",
        ]
    )

    # Keep init fast and deterministic.
    monkeypatch.setattr(tg.TraceGenerator, "_write_info_file", lambda self: None)

    def fake_fit(self):
        # Attach alphas both on self and args, because _generate_trace uses args.alpha_*.
        self.alpha_cpu = 2.0
        self.alpha_mem = 2.0
        self.alpha_arrival = 2.0
        self.alpha_life = 2.0
        self.args.alpha_cpu = 2.0
        self.args.alpha_mem = 2.0
        self.args.alpha_arrival = 2.0
        self.args.alpha_life = 2.0

    monkeypatch.setattr(tg.TraceGenerator, "_fit_alphas", fake_fit)

    # Deterministic sampling so the loop is predictable.
    def fixed_sample(_rng, _alpha, x_min, _x_max, size=1):
        # Return something >= x_min.
        return tg.np.full(size, float(x_min), dtype=float)

    monkeypatch.setattr(tg.TraceGenerator, "_sample_pareto", staticmethod(fixed_sample))

    # Avoid plotting paths.
    monkeypatch.setattr(tg.TraceGenerator, "_plot_trace_utilization", lambda self: None)
    monkeypatch.setattr(tg.TraceGenerator, "_plot_generated_histograms", lambda self: None)

    gen = tg.TraceGenerator(args)
    gen._generate_trace()
    assert gen.pods
    gen._write_trace()
    assert (Path(args.output_dir) / "trace.json").exists()

    # Also cover run() end-to-end without plotting.
    gen.run()
    assert (Path(args.output_dir) / "trace.json").exists()


def test_summarize_utilization_handles_empty_snapshots():
    gen = tg.TraceGenerator.__new__(tg.TraceGenerator)
    gen.times = []
    gen.u_cpu_hist = []
    gen.u_mem_hist = []
    # Should not raise.
    tg.TraceGenerator._summarize_utilization(gen)


def test_fit_alphas_sets_fields_and_attaches_to_args(monkeypatch):
    # Make the solver deterministic and fast.
    def sample_for_alpha(_rng, alpha, x_min, x_max, size=1):
        v = float(x_min + (1.0 / float(alpha)))
        if x_max is not None:
            v = min(v, float(x_max))
        return tg.np.full(size, v, dtype=float)

    monkeypatch.setattr(tg.TraceGenerator, "_sample_pareto", staticmethod(sample_for_alpha))
    monkeypatch.setattr(tg, "SOLVE_ALPHA_SAMPLES", 1)
    monkeypatch.setattr(tg, "SOLVE_ALPHA_MAX_ITERATIONS", 100)
    monkeypatch.setattr(tg, "SOLVE_ALPHA_TOLERANCE", 1e-6)

    args = argparse.Namespace(
        xmin_cpu=0.1,
        xmax_cpu=1.0,
        mean_cpu=0.2,
        xmin_mem=0.1,
        xmax_mem=1.0,
        mean_mem=0.2,
        xmin_arrival=0.1,
        xmax_arrival=10.0,
        mean_arrival=0.3,
        xmin_life=0.1,
        xmax_life=10.0,
        mean_life=0.3,
    )

    gen = tg.TraceGenerator.__new__(tg.TraceGenerator)
    gen.args = args
    gen.rng = tg.np.random.default_rng(0)

    tg.TraceGenerator._fit_alphas(gen)

    assert gen.alpha_cpu is not None
    assert gen.alpha_mem is not None
    assert gen.alpha_arrival is not None
    assert gen.alpha_life is not None
    assert hasattr(args, "alpha_cpu")
    assert hasattr(args, "alpha_mem")
    assert hasattr(args, "alpha_arrival")
    assert hasattr(args, "alpha_life")
