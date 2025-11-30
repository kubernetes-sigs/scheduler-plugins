#!/usr/bin/env python3
# bootstrap/content/scripts/kwok/trace_generator.py
"""
Generate a pod trace for Kubernetes scheduling experiments using Pareto Type I distributions for:
  - CPU requests
  - MEM requests
  - Inter-arrival times
  - Lifetimes
- Each node has normalized capacity CPU = 1.0, MEM = 1.0 and each pod's `cpu` and `mem` are in (0, 1], meaning "fraction of one node".
- Cluster total capacity is N_nodes * 1.0 (for both CPU and MEM).

Method:
  - Start at time 0 (in seconds), keep generating pods.
  - For each pod, sample inter-arrival time, lifetime, CPU/MEM fractions, and priority from Pareto Type I distributions.
  - Stop once the next pod's start_time would exceed trace_time.
Output is a JSON list of pods.
"""

import argparse, heapq, json
from dataclasses import dataclass, asdict
from typing import List, Tuple
import matplotlib.pyplot as plt
import numpy as np
from scripts.helpers.trace_helpers import TraceRecord, plot_histogram
from scripts.helpers.kwok_helpers import parse_duration_to_seconds

@dataclass
class ClusterState:
    num_nodes: int
    live_cpu: float = 0.0
    live_mem: float = 0.0
    live_pods: int = 0

    def utilization(self) -> float:
        """Return effective utilization U(t) = max(U_cpu, U_mem)."""
        cap = float(self.num_nodes)
        u_cpu = self.live_cpu / cap
        u_mem = self.live_mem / cap
        return max(u_cpu, u_mem)


# -------------------------------------------------------------------
# Core generator class
# -------------------------------------------------------------------
class ParetoTraceGenerator:
    """
    Encapsulates Pareto-based trace generation, statistics, and plotting.
    """

    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.rng = np.random.default_rng(args.seed)

        # These will be filled by fit_alphas()
        self.alpha_cpu: float | None = None
        self.alpha_mem: float | None = None
        self.alpha_arrival: float | None = None
        self.alpha_life: float | None = None

        # Results of generate()
        self.pods: List[TraceRecord] = []
        self.times: List[float] = []
        self.u_cpu_hist: List[float] = []
        self.u_mem_hist: List[float] = []
        self.max_runnable_pods_hist: List[int] = []

        self.trace_time_s: float = 0.0

    # ---------------- Pareto helpers ----------------

    @staticmethod
    def _sample_pareto(
        rng: np.random.Generator,
        alpha: float,
        x_min: float,
        x_max: float | None = None,
        size: int = 1,
    ) -> np.ndarray:
        """
        Sample from a Pareto Type I distribution with parameters (alpha, x_min):
            f(x) = alpha * x_min^alpha / x^(alpha + 1),  x >= x_min > 0
        using the inverse CDF:
            X = x_min / U^(1/alpha),  U ~ Uniform(0,1).
        If x_max is given, we avoid generating values > x_max by enforcing a lower bound on U:
            U >= (x_min / x_max)^alpha.
        """
        if alpha <= 0.0 or x_min <= 0.0:
            raise ValueError("Pareto alpha and x_min must be > 0.")
        u = rng.random(size)
        if x_max is not None:  # Lower bound on U so that x <= x_max
            u_min_tail = (x_min / x_max) ** alpha  # in (0, 1)
            u_min = max(1e-12, float(u_min_tail))
            u = np.clip(u, u_min, 1.0 - 1e-12)
        else:  # No upper clamp: keep the generic numerical safety bound
            u = np.clip(u, 1e-12, 1.0 - 1e-12)
        x = x_min / (u ** (1.0 / alpha))  # Inverse CDF
        return x

    @classmethod
    def _mc_mean_for_alpha(
        cls,
        rng: np.random.Generator,
        alpha: float,
        x_min: float,
        x_max: float | None,
        n_samples: int = 50_000,
    ) -> float:
        """Monte Carlo estimate of E[X] for truncated Pareto with given alpha."""
        samples = cls._sample_pareto(rng, alpha, x_min, x_max, size=n_samples)
        return float(samples.mean())

    @classmethod
    def _solve_alpha_for_mean(
        cls,
        rng: np.random.Generator,
        x_min: float,
        x_max: float | None,
        target_mean: float,
        *,
        n_samples: int = 50_000,
        alpha_lo: float = 0.1,
        alpha_hi: float = 10.0,
        tol_rel: float = 1e-3,
        max_iter: int = 10_000,
    ) -> float:
        """
        Find alpha such that E[X | alpha, x_min, x_max] ≈ target_mean using
        Monte Carlo + bisection. Assumes target_mean is between x_min and x_max.
        """
        if x_min <= 0.0:
            raise ValueError("x_min must be > 0.")
        if x_max is not None and not (x_min < target_mean < x_max):
            raise ValueError(
                f"target_mean={target_mean} must lie between x_min={x_min} and x_max={x_max}"
            )
        if x_max is None and target_mean <= x_min:
            raise ValueError(
                f"target_mean={target_mean} must be > x_min={x_min} for unbounded Pareto."
            )

        # Initial bracket
        m_lo = cls._mc_mean_for_alpha(rng, alpha_lo, x_min, x_max, n_samples=n_samples)
        m_hi = cls._mc_mean_for_alpha(rng, alpha_hi, x_min, x_max, n_samples=n_samples)

        # Ensure m_lo >= m_hi (mean decreases with alpha)
        if m_lo < m_hi:
            alpha_lo, alpha_hi = alpha_hi, alpha_lo
            m_lo, m_hi = m_hi, m_lo

        if not (m_hi <= target_mean <= m_lo):
            raise ValueError(
                f"Target mean {target_mean} not bracketed by MC means "
                f"[{m_hi:.4f}, {m_lo:.4f}] for alpha in [{alpha_hi}, {alpha_lo}]. "
                "Adjust xmin/xmax or alpha bounds."
            )

        # Bisection
        for _ in range(max_iter):
            alpha_mid = 0.5 * (alpha_lo + alpha_hi)
            m_mid = cls._mc_mean_for_alpha(
                rng, alpha_mid, x_min, x_max, n_samples=n_samples
            )

            # We maintain m_lo >= target >= m_hi.
            if m_mid >= target_mean:
                alpha_lo, m_lo = alpha_mid, m_mid
            else:
                alpha_hi, m_hi = alpha_mid, m_mid

            if abs(m_mid - target_mean) <= tol_rel * target_mean:
                return alpha_mid

        # Fallback: return mid of final bracket
        return 0.5 * (alpha_lo + alpha_hi)

    # ---------------- Utilization bookkeeping ----------------

    @staticmethod
    def _remove_completed_pods(
        state: ClusterState,
        end_heap: List[Tuple[float, float, float, int]],
        t: float,
    ) -> None:
        """
        Remove completed pods (with end_time <= t) from the heap and update live_cpu/mem.

        end_heap elements are (end_time, cpu, mem, replicas).
        """
        while end_heap and end_heap[0][0] <= t:
            end_time, cpu, mem, replicas = heapq.heappop(end_heap)
            total_cpu = cpu * replicas
            total_mem = mem * replicas
            state.live_cpu = max(0.0, state.live_cpu - total_cpu)
            state.live_mem = max(0.0, state.live_mem - total_mem)
            state.live_pods = max(0, state.live_pods - replicas)

    # ---------------- Trace generation ----------------

    def _generate(
        self,
        n_nodes: int,
        trace_time: float,
        # CPU
        alpha_cpu: float,
        xmin_cpu: float,
        xmax_cpu: float,
        # MEM
        alpha_mem: float,
        xmin_mem: float,
        xmax_mem: float,
        # Inter-arrival times
        alpha_arrival: float,
        xmin_arrival: float,
        xmax_arrival: float | None,
        # Lifetimes
        alpha_life: float,
        xmin_life: float,
        xmax_life: float | None,
        # Priority (discrete geometric on [priority_min, priority_max])
        priority_min: int,
        priority_max: int,
        priority_geom_r: float = 1.0,
        # Replicas (discrete geometric on [replicas_min, replicas_max])
        replicas_min: int = 1,
        replicas_max: int = 1,
        replicas_geom_r: float = 1.0,
    ) -> None:
        """
        Generate the trace and store results on the instance.

        See original docstring for details on distributions.
        """
        rng = self.rng

        # Sanity for x_min > 0
        if xmin_arrival <= 0.0:
            raise ValueError("xmin_arrival must be > 0 for Pareto I.")
        if xmin_life <= 0.0:
            raise ValueError("xmin_life must be > 0 for Pareto I.")

        # Normalize replica bounds
        r_min = max(1, int(replicas_min))
        r_max = max(r_min, int(replicas_max))

        # Normalize priority bounds
        p_min = int(priority_min)
        p_max = int(priority_max)
        if p_max < p_min:
            p_max = p_min  # ensure at least one priority value

        # Priority distribution: discrete geometric on {p_min, ..., p_max}
        if priority_geom_r <= 0.0:
            raise ValueError("priority_geom_r must be > 0.")

        prio_values = np.arange(p_min, p_max + 1, dtype=int)
        if np.isclose(priority_geom_r, 1.0):
            prio_probs = None  # uniform over prio_values
        else:
            # weight(v) = r**(v - p_min)
            exponents = np.arange(len(prio_values), dtype=float)
            prio_weights = priority_geom_r ** exponents
            prio_probs = prio_weights / prio_weights.sum()

        # Replica distribution: discrete geometric on {r_min, ..., r_max}
        if replicas_geom_r <= 0.0:
            raise ValueError("replicas_geom_r must be > 0.")

        replica_values = np.arange(r_min, r_max + 1, dtype=int)
        if len(replica_values) == 1 or np.isclose(replicas_geom_r, 1.0):
            replica_probs = None  # deterministic or uniform
        else:
            # weight(v) = r**(v - r_min)
            exponents = np.arange(len(replica_values), dtype=float)
            replica_weights = replicas_geom_r ** exponents
            replica_probs = replica_weights / replica_weights.sum()

        state = ClusterState(num_nodes=n_nodes)
        end_heap: List[Tuple[float, float, float, int]] = []  # (end_time, cpu, mem, replicas)
        pods: List[TraceRecord] = []

        # Utilization / max runnable pods history at each accepted pod
        times: List[float] = [0.0]
        u_cpu_hist: List[float] = [0.0]
        u_mem_hist: List[float] = [0.0]
        max_runnable_pods_hist: List[int] = [0]

        current_time = 0.0
        pod_id = 0

        while True:
            if current_time >= trace_time:
                break

            # Sample inter-arrival time and compute next start_time
            delta_t = float(
                self._sample_pareto(
                    rng,
                    alpha_arrival,
                    xmin_arrival,
                    xmax_arrival,
                    size=1,
                )[0]
            )
            start_time = current_time + delta_t
            # Stop, when next pod would start at time >= trace_time
            if start_time >= trace_time:
                break

            # First, remove pod completions up to this start_time
            self._remove_completed_pods(state, end_heap, start_time)

            # Sample lifetime
            lifetime = float(
                self._sample_pareto(
                    rng,
                    alpha_life,
                    xmin_life,
                    xmax_life,
                    size=1,
                )[0]
            )
            end_time = start_time + lifetime

            # Sample resources using Pareto
            cpu = float(
                self._sample_pareto(
                    rng,
                    alpha_cpu,
                    xmin_cpu,
                    xmax_cpu,
                    size=1,
                )[0]
            )
            mem = float(
                self._sample_pareto(
                    rng,
                    alpha_mem,
                    xmin_mem,
                    xmax_mem,
                    size=1,
                )[0]
            )

            # Sample priority using discrete geometric on [p_min, p_max]
            if prio_probs is None:
                priority = int(rng.choice(prio_values))
            else:
                priority = int(rng.choice(prio_values, p=prio_probs))

            # Determine replicas for this pod using discrete geometric on [r_min, r_max]
            if len(replica_values) == 1:
                replicas = int(replica_values[0])
            elif replica_probs is None:
                replicas = int(rng.choice(replica_values))
            else:
                replicas = int(rng.choice(replica_values, p=replica_probs))

            # Check utilization if we accept this pod (for logging)
            new_live_cpu = state.live_cpu + replicas * cpu
            new_live_mem = state.live_mem + replicas * mem
            u_cpu = new_live_cpu / float(n_nodes)
            u_mem = new_live_mem / float(n_nodes)

            pod_id += 1
            name = f"pod-{pod_id:06d}"
            pods.append(
                TraceRecord(
                    id=pod_id,
                    name=name,
                    start_time=start_time,
                    end_time=end_time,
                    cpu=cpu,
                    mem=mem,
                    priority=priority,
                    replicas=replicas,
                )
            )
            state.live_cpu = new_live_cpu
            state.live_mem = new_live_mem
            state.live_pods += replicas
            heapq.heappush(end_heap, (end_time, cpu, mem, replicas))
            current_time = start_time

            # Record utilization snapshot and max runnable pods (after adding)
            times.append(start_time)
            u_cpu_hist.append(u_cpu)
            u_mem_hist.append(u_mem)
            max_runnable_pods_hist.append(state.live_pods)

        # Store on instance
        self.pods = pods
        self.times = times
        self.u_cpu_hist = u_cpu_hist
        self.u_mem_hist = u_mem_hist
        self.max_runnable_pods_hist = max_runnable_pods_hist
        self.trace_time_s = trace_time

    # ---------------- Plotting helpers ----------------

    @staticmethod
    def _plot_trace_utilization(
        pods: List[TraceRecord],
        times: List[float],
        u_cpu_hist: List[float],
        u_mem_hist: List[float],
        max_runnable_pods_hist: List[int],
        out_path: str,
    ) -> None:
        """
        Plot CPU/MEM utilization and max runnable pods over time,
        including creation/deletion counts per x-tick.
        """
        times_plot = list(times)
        u_cpu_plot = list(u_cpu_hist)
        u_mem_plot = list(u_mem_hist)
        max_runnable_pods_plot = list(max_runnable_pods_hist)

        max_time = max(times_plot) if times_plot else 0.0

        # Choose x-axis units based on total duration
        # <= 7h -> minutes, <= 7d -> hours, else days
        if max_time <= 7 * 3600:
            x_scale = 1.0 / 60.0        # seconds -> minutes
            x_label = "Time (minutes)"
        elif max_time <= 7 * 24 * 3600:
            x_scale = 1.0 / 3600.0      # seconds -> hours
            x_label = "Time (hours)"
        else:
            x_scale = 1.0 / (24.0 * 3600.0)  # seconds -> days
            x_label = "Time (days)"

        fig, ax1 = plt.subplots(figsize=(9, 4))

        # CPU/MEM utilization
        ax1.plot(
            times_plot,
            u_cpu_plot,
            label="CPU utilization",
            linestyle="--",
            color="tab:blue",
            alpha=0.7,
            linewidth=0.5,
        )
        ax1.plot(
            times_plot,
            u_mem_plot,
            label="Memory utilization",
            linestyle="--",
            color="tab:orange",
            alpha=0.7,
            linewidth=0.5,
        )

        ax1.set_xlabel(x_label, labelpad=20)
        ax1.set_ylabel("Utilization (fraction of total capacity)")

        # Second axis for max runnable pods
        ax2 = ax1.twinx()
        ax2.plot(
            times_plot,
            max_runnable_pods_plot,
            linestyle="-",
            linewidth=1.0,
            color="tab:green",
            alpha=0.7,
            label="Max runnable pods",
        )
        ax2.set_ylabel("Max runnable pods")

        # Grid and legends
        ax1.grid(True, linestyle="--", alpha=0.4)

        # Combine legends from both axes
        lines1, labels1 = ax1.get_legend_handles_labels()
        lines2, labels2 = ax2.get_legend_handles_labels()
        ax1.legend(lines1 + lines2, labels1 + labels2, loc="lower right")

        # Build event list for tick annotation (C = creation, D = deletion)
        events: List[Tuple[float, str]] = []
        for p in pods:
            events.append((p.start_time, "C"))
            events.append((p.end_time, "D"))
        events.sort(key=lambda e: e[0])

        # get x ticks (in seconds) and add C/D annotations below
        xticks = ax1.get_xticks()
        xticks = [t for t in xticks if 0.0 <= t <= max_time]

        if len(events) > 0 and len(xticks) > 0:
            ax1.set_xticks(xticks)
            base_labels = [f"{t * x_scale:.0f}" for t in xticks]
            ax1.set_xticklabels(base_labels)

            idx = 0
            n_events = len(events)
            for i, tick in enumerate(xticks):
                if tick < 0:
                    continue
                left = 0.0 if i == 0 else xticks[i - 1]
                right = tick
                c_count = 0
                d_count = 0
                while idx < n_events and events[idx][0] <= right:
                    t_ev, kind = events[idx]
                    idx += 1
                    if t_ev > left:
                        if kind == "C":
                            c_count += 1
                        else:
                            d_count += 1
                if c_count or d_count:
                    ax1.text(
                        tick,
                        -0.1,  # position below axis
                        f"+{c_count} -{d_count}",
                        transform=ax1.get_xaxis_transform(),
                        ha="center",
                        va="top",
                        fontsize=8,
                    )

        plt.tight_layout()
        plt.subplots_adjust(bottom=0.22)
        plt.savefig(out_path)
        plt.close(fig)
        print(f"Saved utilization plot to {out_path}")

    @staticmethod
    def _plot_generated_histograms(
        pods: List[TraceRecord],
        args: argparse.Namespace,
        out_path: str,
    ) -> None:
        """
        Build histograms of the generated data (inter-arrival times, lifetimes,
        CPU and MEM requests) and save them as a 2x2 grid PNG.

        Uses plot_histogram from public_trace_analysis.plots.
        """
        # Sort by start_time to get sensible inter-arrivals
        pods_sorted = sorted(pods, key=lambda p: p.start_time)
        start_times = np.array([p.start_time for p in pods_sorted], dtype=float)
        cpu_vals = np.array([p.cpu for p in pods_sorted], dtype=float)
        mem_vals = np.array([p.mem for p in pods_sorted], dtype=float)
        lifetimes = np.array(
            [p.end_time - p.start_time for p in pods_sorted],
            dtype=float,
        )

        inter_arrivals = np.empty_like(start_times)
        if len(start_times) > 0:
            inter_arrivals[0] = start_times[0]  # from t=0 to first pod
        if len(start_times) > 1:
            inter_arrivals[1:] = np.diff(start_times)

        fig, axes = plt.subplots(2, 2, figsize=(8, 6))
        axes = axes.flatten()

        # Inter-arrival times
        plot_histogram(
            axes[0],
            inter_arrivals,
            title="Generated inter-arrival times",
            x_label="Δt (seconds)",
            y_label="Probability density",
            bins=80,
            log_y=True,
            x_max=args.xmax_arrival,
            scale=1.0,
            pareto_fit=True,
            pareto_alpha=args.alpha_arrival,
            pareto_xmin=args.xmin_arrival,
        )

        # Lifetimes
        plot_histogram(
            axes[1],
            lifetimes,
            title="Generated lifetimes",
            x_label="Lifetime (seconds)",
            y_label="Probability density",
            bins=80,
            log_y=True,
            x_max=args.xmax_life,
            scale=1.0,
            pareto_fit=True,
            pareto_alpha=args.alpha_life,
            pareto_xmin=args.xmin_life,
        )

        # CPU
        plot_histogram(
            axes[2],
            cpu_vals,
            title="Generated CPU requests",
            x_label="Requested CPU (fraction of node capacity)",
            y_label="Probability density",
            bins=80,
            log_y=True,
            x_max=args.xmax_cpu,
            scale=1.0,
            pareto_fit=True,
            pareto_alpha=args.alpha_cpu,
            pareto_xmin=args.xmin_cpu,
        )

        # MEM
        plot_histogram(
            axes[3],
            mem_vals,
            title="Generated memory requests",
            x_label="Requested memory (fraction of node capacity)",
            y_label="Probability density",
            bins=80,
            log_y=True,
            x_max=args.xmax_mem,
            scale=1.0,
            pareto_fit=True,
            pareto_alpha=args.alpha_mem,
            pareto_xmin=args.xmin_mem,
        )

        fig.tight_layout()
        fig.savefig(out_path, dpi=150, bbox_inches="tight")
        plt.close(fig)
        print(f"Saved generated histograms to {out_path}")

    # ---------------- High-level run logic ----------------

    def _fit_alphas(self) -> None:
        """Derive Pareto alphas from (xmin, xmax, mean) and attach them to args."""
        args = self.args

        alpha_cpu = self._solve_alpha_for_mean(
            self.rng,
            x_min=args.xmin_cpu,
            x_max=args.xmax_cpu,
            target_mean=args.mean_cpu,
        )
        alpha_mem = self._solve_alpha_for_mean(
            self.rng,
            x_min=args.xmin_mem,
            x_max=args.xmax_mem,
            target_mean=args.mean_mem,
        )
        alpha_arrival = self._solve_alpha_for_mean(
            self.rng,
            x_min=args.xmin_arrival,
            x_max=args.xmax_arrival,
            target_mean=args.mean_arrival,
        )
        alpha_life = self._solve_alpha_for_mean(
            self.rng,
            x_min=args.xmin_life,
            x_max=args.xmax_life,
            target_mean=args.mean_life,
        )

        self.alpha_cpu = alpha_cpu
        self.alpha_mem = alpha_mem
        self.alpha_arrival = alpha_arrival
        self.alpha_life = alpha_life

        # Attach them back onto args so existing plotting helpers keep working
        args.alpha_cpu = alpha_cpu
        args.alpha_mem = alpha_mem
        args.alpha_arrival = alpha_arrival
        args.alpha_life = alpha_life

        print(
            f"[pareto-fit] CPU:      mean={args.mean_cpu}  xmin={args.xmin_cpu}  "
            f"xmax={args.xmax_cpu}  alpha≈{alpha_cpu:.4f}"
        )
        print(
            f"[pareto-fit] MEM:      mean={args.mean_mem}  xmin={args.xmin_mem}  "
            f"xmax={args.xmax_mem}  alpha≈{alpha_mem:.4f}"
        )
        print(
            f"[pareto-fit] arrival:  mean={args.mean_arrival}  xmin={args.xmin_arrival}  "
            f"xmax={args.xmax_arrival}  alpha≈{alpha_arrival:.4f}"
        )
        print(
            f"[pareto-fit] lifetime: mean={args.mean_life}  xmin={args.xmin_life}  "
            f"xmax={args.xmax_life}  alpha≈{alpha_life:.4f}"
        )

    def _summarize_utilization(self) -> None:
        """Print basic utilization stats."""
        if not self.times:
            print("[utilization] No utilization snapshots recorded.")
            return

        u_cpu_arr = np.asarray(self.u_cpu_hist, dtype=float)
        u_mem_arr = np.asarray(self.u_mem_hist, dtype=float)
        u_eff_arr = np.maximum(u_cpu_arr, u_mem_arr)

        from typing import Tuple

        def _stats(arr: np.ndarray) -> Tuple[float, float, float]:
            return float(arr.mean()), float(arr.min()), float(arr.max())

        mean_cpu, min_cpu, max_cpu = _stats(u_cpu_arr)
        mean_mem, min_mem, max_mem = _stats(u_mem_arr)
        mean_eff, min_eff, max_eff = _stats(u_eff_arr)

        print(
            "[utilization] CPU: mean={:.3f}, min={:.3f}, max={:.3f}".format(
                mean_cpu, min_cpu, max_cpu
            )
        )
        print(
            "[utilization] MEM: mean={:.3f}, min={:.3f}, max={:.3f}".format(
                mean_mem, min_mem, max_mem
            )
        )
        print(
            "[utilization] EFF(max(cpu,mem)): mean={:.3f}, min={:.3f}, max={:.3f}".format(
                mean_eff, min_eff, max_eff
            )
        )

    def _write_json(self, out_path: str) -> None:
        """Write trace JSON (meta + pods) to disk."""
        args = self.args
        obj = {
            "meta": {
                "n_nodes": args.n_nodes,
                "trace_time_s": self.trace_time_s,
                "seed": args.seed,
                "cpu_params": {
                    "pareto_alpha": self.alpha_cpu,
                    "pareto_xmin": args.xmin_cpu,
                    "pareto_xmax": args.xmax_cpu,
                    "pareto_mean": args.mean_cpu,
                },
                "mem_params": {
                    "pareto_alpha": self.alpha_mem,
                    "pareto_xmin": args.xmin_mem,
                    "pareto_xmax": args.xmax_mem,
                    "pareto_mean": args.mean_mem,
                },
                "arrival_params": {
                    "pareto_alpha": self.alpha_arrival,
                    "pareto_xmin": args.xmin_arrival,
                    "pareto_xmax": args.xmax_arrival,
                    "pareto_mean": args.mean_arrival,
                },
                "lifetime_params": {
                    "pareto_alpha": self.alpha_life,
                    "pareto_xmin": args.xmin_life,
                    "pareto_xmax": args.xmax_life,
                    "pareto_mean": args.mean_life,
                },
                "priority_params": {
                    "priority_min": args.priority_min,
                    "priority_max": args.priority_max,
                    "geom_r": args.priority_geom_r,
                },
                "replica_params": {
                    "replicas_min": args.replicas_min,
                    "replicas_max": args.replicas_max,
                    "geom_r": args.replicas_geom_r,
                },
            },
            "pods": [asdict(p) for p in self.pods],
        }

        with open(out_path, "w", encoding="utf-8") as f:
            json.dump(obj, f, indent=2)
        print(f"Wrote trace to {out_path}")

    def run(self) -> None:
        """Top-level entry point used by main()."""
        args = self.args

        # Fit alphas from (xmin, xmax, mean)
        self._fit_alphas()

        # Parse trace_time duration string into seconds
        trace_time = parse_duration_to_seconds(args.trace_time)

        # Generate pods and utilization history
        self._generate(
            n_nodes=args.n_nodes,
            trace_time=trace_time,
            alpha_cpu=self.alpha_cpu,
            xmin_cpu=args.xmin_cpu,
            xmax_cpu=args.xmax_cpu,
            alpha_mem=self.alpha_mem,
            xmin_mem=args.xmin_mem,
            xmax_mem=args.xmax_mem,
            alpha_arrival=self.alpha_arrival,
            xmin_arrival=args.xmin_arrival,
            xmax_arrival=args.xmax_arrival,
            alpha_life=self.alpha_life,
            xmin_life=args.xmin_life,
            xmax_life=args.xmax_life,
            priority_min=args.priority_min,
            priority_max=args.priority_max,
            priority_geom_r=args.priority_geom_r,
            replicas_min=args.replicas_min,
            replicas_max=args.replicas_max,
            replicas_geom_r=args.replicas_geom_r,
        )

        print(
            f"Total pods {len(self.pods)} generated before stopping at t={trace_time} seconds."
        )
        if self.pods:
            t_start = min(p.start_time for p in self.pods)
            t_end = max(p.end_time for p in self.pods)
            print(f"Simulated time span: [{t_start:.3f}, {t_end:.3f}] seconds.")
        else:
            t_end = 0.0

        self._summarize_utilization()
        self._write_json(args.output)

        if args.util_plot:
            self._plot_trace_utilization(
                pods=self.pods,
                times=self.times,
                u_cpu_hist=self.u_cpu_hist,
                u_mem_hist=self.u_mem_hist,
                max_runnable_pods_hist=self.max_runnable_pods_hist,
                out_path=args.util_plot,
            )

        if args.hist_plot:
            self._plot_generated_histograms(
                pods=self.pods,
                args=args,
                out_path=args.hist_plot,
            )


# -------------------------------------------------------------------
# CLI
# -------------------------------------------------------------------
def build_arg_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=("Generate pod traces."))

    # General
    p.add_argument("--output", "-o", required=True, help="Output JSON file for the trace.")
    p.add_argument("--seed", type=int, default=42, help="Random seed.")

    # Optional plots
    p.add_argument("--util-plot", help="If set, save a PNG of CPU/MEM utilization over time to this path.")
    p.add_argument("--hist-plot", help=("If set, save a PNG with histograms of the generated types to this path."))

    # Cluster / utilization
    p.add_argument(
        "--n-nodes",
        type=int,
        default=8,
        help="Number of nodes (each has capacity 1.0 CPU, 1.0 MEM).",
    )
    p.add_argument(
        "--trace-time",
        type=str,
        default="3600s",
        help="Trace time; seconds or suffixed like '1h', '30m', ...",
    )

    # Priority (min/max like replicas)
    p.add_argument(
        "--priority-min",
        type=int,
        default=0,
        help="Minimum priority value (inclusive).",
    )
    p.add_argument(
        "--priority-max",
        type=int,
        default=3,
        help="Maximum priority value (inclusive).",
    )
    p.add_argument(
        "--priority-geom-r",
        type=float,
        default=1.0,
        help=(
            "Geometric decay factor r in (0,1]; "
            "priority value v in [priority-min, priority-max] gets weight r**(v - priority-min). "
            "Use r=1.0 for uniform."
        ),
    )

    # Replica counts
    p.add_argument(
        "--replicas-min",
        type=int,
        default=1,
        help="Minimum replicas per trace pod (default: 1).",
    )
    p.add_argument(
        "--replicas-max",
        type=int,
        default=1,
        help="Maximum replicas per trace pod (inclusive, default: 1).",
    )
    p.add_argument(
        "--replicas-geom-r",
        type=float,
        default=1.0,
        help=(
            "Geometric decay factor r in (0,1]; "
            "replica value v in [replicas-min, replicas-max] gets weight "
            "r**(v - replicas-min). Use r=1.0 for uniform."
        ),
    )

    # CPU distribution params
    p.add_argument(
        "--xmin-cpu",
        type=float,
        default=0.01,
        help="Pareto I x_min (lower bound) for CPU requests.",
    )
    p.add_argument(
        "--xmax-cpu",
        type=float,
        default=1.0,
        help="Maximum CPU request (upper clamp).",
    )
    p.add_argument(
        "--mean-cpu",
        type=float,
        required=True,
        help="Target mean CPU request (fraction of node capacity).",
    )

    # MEM distribution params
    p.add_argument(
        "--xmin-mem",
        type=float,
        default=0.01,
        help="Pareto I x_min (lower bound) for MEM requests.",
    )
    p.add_argument(
        "--xmax-mem",
        type=float,
        default=1.0,
        help="Maximum MEM request (upper clamp).",
    )
    p.add_argument(
        "--mean-mem",
        type=float,
        required=True,
        help="Target mean MEM request (fraction of node capacity).",
    )

    # Inter-arrival times (seconds)
    p.add_argument(
        "--xmin-arrival",
        type=float,
        default=0.01,
        help="Pareto I x_min for inter-arrival times (seconds).",
    )
    p.add_argument(
        "--xmax-arrival",
        type=float,
        default=None,
        help="Maximum inter-arrival time (seconds, upper clamp).",
    )
    p.add_argument(
        "--mean-arrival",
        type=float,
        required=True,
        help="Target mean inter-arrival time (seconds).",
    )

    # Lifetimes (seconds)
    p.add_argument(
        "--xmin-life",
        type=float,
        default=30.0,
        help="Pareto I x_min for lifetimes (seconds).",
    )
    p.add_argument(
        "--xmax-life",
        type=float,
        default=None,
        help="Maximum lifetime (seconds, upper clamp).",
    )
    p.add_argument(
        "--mean-life",
        type=float,
        required=True,
        help="Target mean lifetime (seconds).",
    )

    return p


def main() -> None:
    args = build_arg_parser().parse_args()
    generator = ParetoTraceGenerator(args)
    generator.run()


if __name__ == "__main__":
    main()
