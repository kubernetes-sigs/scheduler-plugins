#!/usr/bin/env python3
# trace_replayer.py
"""
Replay a JSON trace (from trace_generator.py) on a KWOK cluster and monitor actual utilization
and running pods over time.

Expected directory layout (from trace_generator.py):

  <TRACE_DIR>/
    trace.json
    info.yaml
    util.png
    hists.png
    ...
    results/
      trace-monitor.csv   (created by this script)

This replayer:

- Takes a directory (--trace-dir) instead of a single file.
- Always loads <trace-dir>/trace.json.
- Stores the monitor CSV in <trace-dir>/results/trace-monitor.csv.

High-level behavior:

- Creates a KWOK cluster (kwokctl)
- Creates KWOK nodes with given CPU/memory capacities
- Ensures namespace and PriorityClasses
- Starts a monitor that periodically calls stat_snapshot(...) and writes a CSV timeline:
    wall_time_s, sim_time_s, cpu_run_util, mem_run_util,
    running_count, unsched_count, prio1_run_time_s, prio2_run_time_s, ...
- Replays the trace according to its start_time / end_time values.
  The earliest event is mapped to "now" (start_wall_time), and later events are offset
  by (sim_time - t_min).
"""

import argparse, csv, json, logging, threading, time, yaml, sys, shlex
from concurrent.futures import ThreadPoolExecutor, Future
from dataclasses import dataclass
from pathlib import Path
from typing import List, Dict

from scripts.helpers.general_helpers import (
    setup_logging,
    make_header_footer,
    get_timestamp,
    qty_to_mcpu_int,
    qty_to_mcpu_str,
    qty_to_bytes_int,
    qty_to_bytes_str,
    get_git_info,
)
from scripts.helpers.kubectl_helpers import (
    kubectl_apply_yaml,
    ensure_namespace,
    ensure_priority_classes,
    delete_rs,
)
from scripts.helpers.kwok_helpers import (
    yaml_kwok_rs,
    create_kwok_nodes,
    ensure_kwok_cluster,
)
from scripts.helpers.cluster_stats import (
    stat_snapshot,
)
from scripts.kwok_trace_replayer.trace_helpers import (
    TraceRecord,
)

#######################################################################
# Constants
#######################################################################

LOG = logging.getLogger("trace-replayer")
MAX_REPLAY_WORKERS = 4

#######################################################################
# Event model
#######################################################################

@dataclass
class Event:
    sim_time: float   # seconds in trace's time
    kind: str         # "create" or "delete"
    record_id: int
    cpu_str: str | None = None
    mem_str: str | None = None
    pc_name: str | None = None
    replicas: int = 1


#######################################################################
# Replayer class
#######################################################################
class TraceReplayer:
    """
    Encapsulates loading a trace, building events, replaying them against a KWOK cluster,
    and monitoring utilization over time.
    """

    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args

        # Base directory containing trace.json and other artifacts
        self.base_dir: Path = Path(args.trace_dir).resolve()
        self.trace_path: Path = self.base_dir / "trace.json"
        self.results_dir: Path = self.base_dir / "results"

        # Will be filled by load_trace
        self.pods: List[TraceRecord] = []
        self.max_prio: int = 0
        self.t_min: float = 0.0
        self.trace_time: float = 0.0
        self.meta: dict = {}

        # Derived from meta / args
        self.num_nodes: int = 0
        self.node_cpu_m: int = 0
        self.node_mem_b: int = 0

        # Monitoring fields
        self.ctx: str = f"kwok-{args.cluster_name}"
        self.events: List[Event] = []
        self.prio_by_identity: Dict[str, int] = {}

    # ---------------- Arg logging / info bundle ----------------

    @staticmethod
    def _log_field_fmt(v):
        return "<unset>" if v in (None, "") else str(v)

    @staticmethod
    def log_args(args: argparse.Namespace) -> None:
        """
        Log the main arguments (mirrors style used in trace_generator).
        """
        fields = [
            ("trace_dir", args.trace_dir),
            ("cluster_name", args.cluster_name),
            ("kwok_runtime", args.kwok_runtime),
            ("kwokctl_config_file", args.kwokctl_config_file),
            ("namespace", args.namespace),
            ("node_cpu", args.node_cpu),
            ("node_mem", args.node_mem),
            ("pods_cap", args.pods_cap),
            ("monitor_interval", args.monitor_interval),
            ("log_level", args.log_level),
        ]
        pad = max(len(k) for k, _ in fields)
        lines = [f"{k.rjust(pad)} = {TraceReplayer._log_field_fmt(v)}" for k, v in fields]
        block = "\n".join(lines)
        header, footer = make_header_footer("ARGS (TRACE REPLAYER)")
        LOG.info("\n%s\n%s\n%s", header, block, footer)

    def _write_info_file(self) -> None:
        """
        Write info_replayer.yaml in base_dir with git + CLI + args.
        Mirrors style of trace_generator's info.yaml, but replayer-specific.
        """
        try:
            git_info = get_git_info(Path.cwd())
            payload = {
                "meta": {
                    "timestamp": get_timestamp(),
                    "git": git_info or {},
                    "kind": "trace_replayer",
                },
                "inputs": {
                    "cli-cmd": "python3 " + " ".join(shlex.quote(a) for a in sys.argv),
                    "args": {k: v for k, v in vars(self.args).items()},
                },
                "trace": {
                    "trace_dir": str(self.base_dir),
                    "trace_path": str(self.trace_path),
                },
            }
            out_path = self.base_dir / "info_replayer.yaml"
            with open(out_path, "w", encoding="utf-8") as fh:
                yaml.safe_dump(payload, fh, sort_keys=False)
            LOG.info("wrote replayer info bundle to %s", out_path)
        except Exception as e:
            LOG.warning("failed to write info_replayer.yaml: %s", e)

    # ---------------- Helpers ----------------

    @staticmethod
    def _rs_name_for_record(record_id: int) -> str:
        """
        Stable ReplicaSet name derived from the trace record id.

        We add a non-numeric prefix to avoid YAML treating it as a number.
        Example: record_id=1 -> "rs-000001"
        """
        return f"rs-{record_id:06d}"

    def load_trace(self) -> None:
        """Load trace JSON and populate pods, max_prio, t_min, trace_time, meta."""
        if not self.trace_path.exists():
            raise FileNotFoundError(
                f"Trace file not found: {self.trace_path} "
                f"(expected trace.json inside --trace-dir={self.base_dir})"
            )

        with open(self.trace_path, "r", encoding="utf-8") as f:
            raw = json.load(f)

        if isinstance(raw, dict):
            meta = raw.get("meta", {}) or {}
            records = raw.get("pods", []) or []
        else:
            meta = {}
            records = raw

        pods: List[TraceRecord] = []
        max_prio = 0
        t_min = float("inf")
        trace_time = 0.0

        for rec in records:
            id_val = int(rec["id"])  # rely on id being present
            start = float(rec["start_time"])
            end = float(rec["end_time"])
            cpu = float(rec["cpu"])
            mem = float(rec["mem"])
            prio = int(rec.get("priority", 1))
            replicas = int(rec.get("replicas", 1))

            pods.append(
                TraceRecord(
                    id=id_val,
                    start_time=start,
                    end_time=end,
                    cpu=cpu,
                    mem=mem,
                    priority=prio,
                    replicas=replicas,
                )
            )

            max_prio = max(max_prio, prio)
            t_min = min(t_min, start)
            trace_time = max(trace_time, end)

        pods.sort(key=lambda p: p.start_time)
        if t_min == float("inf"):
            t_min = 0.0

        LOG.info(
            "loaded %d pods from %s (t_min=%.3f, trace_time=%.3f, max_priority=%d)",
            len(pods),
            self.trace_path,
            t_min,
            trace_time,
            max_prio,
        )

        self.pods = pods
        self.max_prio = max_prio
        self.t_min = t_min
        self.trace_time = trace_time
        self.meta = meta

        # num_nodes from meta (generator uses "num_nodes"; keep "n_nodes" as fallback)
        if "num_nodes" in meta:
            self.num_nodes = int(meta["num_nodes"])
        elif "n_nodes" in meta:
            self.num_nodes = int(meta["n_nodes"])
        else:
            raise KeyError(
                f"Trace meta does not contain 'num_nodes' or 'n_nodes': {meta}"
            )

    def _build_events(self, node_cpu_m: int, node_mem_b: int) -> None:
        """
        Turn trace pods into a sorted list of events with concrete K8s quantities.
        """
        events: List[Event] = []
        for p in self.pods:
            cpu_m = max(1, int(round(p.cpu * node_cpu_m)))
            mem_b = max(1, int(round(p.mem * node_mem_b)))
            cpu_str = qty_to_mcpu_str(cpu_m)
            mem_str = qty_to_bytes_str(mem_b)
            pc_name = f"p{int(p.priority)}"
            replicas = max(1, int(getattr(p, "replicas", 1)))
            events.append(
                Event(
                    sim_time=float(p.start_time),
                    kind="create",
                    record_id=p.id,
                    cpu_str=cpu_str,
                    mem_str=mem_str,
                    pc_name=pc_name,
                    replicas=replicas,
                )
            )
            events.append(
                Event(
                    sim_time=float(p.end_time),
                    kind="delete",
                    record_id=p.id,
                )
            )

        # sort by sim_time, then create before delete
        events.sort(key=lambda e: (e.sim_time, 0 if e.kind == "create" else 1))
        LOG.info("built %d events from %d pods", len(events), len(self.pods))
        self.events = events

    # ---------------- Replay ----------------

    def _replay_events(
        self,
        ns: str,
        start_wall_time: float,
        sim_t0: float,
    ) -> None:
        """
        Replay events against the cluster.

        - We preserve the relative timing from the trace:
            wall_time(ev) = start_wall_time + (ev.sim_time - sim_t0)
          where sim_t0 is typically the earliest event time (t_min).

        - Pod lifetimes are simulated via ReplicaSets:
            - "create" event -> create/patch ReplicaSet.
            - "delete" event -> delete the ReplicaSet.

        - For each distinct sim_time, we:
            * sleep until that time,
            * submit one kubectl apply per CREATE event at that sim_time,
            * submit one kubectl delete per DELETE event at that sim_time.

        - kubectl commands are executed asynchronously in a thread pool
          so the replay loop is not blocked by their runtime. We only
          log drift once per batch (per sim_time), after the sleep.
        """
        header, footer = make_header_footer("TRACE REPLAY")
        LOG.info(
            "\n%s\nstart_wall=%s sim_t0=%.3f\n%s",
            header,
            get_timestamp(),
            sim_t0,
            footer,
        )

        events = self.events
        i = 0
        n = len(events)

        executor = ThreadPoolExecutor(max_workers=MAX_REPLAY_WORKERS)
        futures: List[Future] = []

        try:
            while i < n:
                # Current batch timestamp (trace time)
                current_t = events[i].sim_time

                # Collect all events with exactly this sim_time
                batch_events: List[Event] = []
                while i < n and events[i].sim_time == current_t:
                    batch_events.append(events[i])
                    i += 1

                creates = [ev for ev in batch_events if ev.kind == "create"]
                deletes = [ev for ev in batch_events if ev.kind == "delete"]

                # Ideal wall clock time for this sim_t
                target_wall = start_wall_time + max(0.0, current_t - sim_t0)

                # Sleep until that wall time
                now_before = time.time()
                sleep_s = max(0.0, target_wall - now_before)
                if sleep_s > 0:
                    time.sleep(sleep_s)

                # Single drift log for the whole batch
                now_after = time.time()
                batch_drift = now_after - target_wall
                LOG.info(
                    "TIME DRIFT batch @ sim_t=%.3f: "
                    "target_wall=%.3f actual_wall=%.3f drift=%.6fs "
                    "(creates=%d deletes=%d)",
                    current_t,
                    target_wall - start_wall_time,
                    now_after - start_wall_time,
                    batch_drift,
                    len(creates),
                    len(deletes),
                )

                # ------------------------------------------------------
                # CREATE events: one kubectl_apply_yaml per RS (async)
                # ------------------------------------------------------
                for ev in creates:
                    assert (
                        ev.cpu_str is not None
                        and ev.mem_str is not None
                        and ev.pc_name is not None
                    )
                    rs_name = self._rs_name_for_record(ev.record_id)

                    yaml_text = yaml_kwok_rs(
                        ns=ns,
                        rs_name=rs_name,
                        replicas=ev.replicas,
                        cpu=ev.cpu_str,
                        mem=ev.mem_str,
                        pc=ev.pc_name,
                    )

                    LOG.info(
                        "CREATE RS @ sim_t=%.3f rs=%s (trace_record_id=%d) "
                        "replicas=%d cpu=%s mem=%s pc=%s",
                        ev.sim_time,
                        rs_name,
                        ev.record_id,
                        ev.replicas,
                        ev.cpu_str,
                        ev.mem_str,
                        ev.pc_name,
                    )

                    fut = executor.submit(kubectl_apply_yaml, LOG, self.ctx, yaml_text)
                    futures.append(fut)

                # ------------------------------------------------------
                # DELETE events: one delete_rs per RS (async)
                # ------------------------------------------------------
                for ev in deletes:
                    rs_name = self._rs_name_for_record(ev.record_id)
                    LOG.info(
                        "DELETE RS @ sim_t=%.3f rs=%s (trace_record_id=%d)",
                        ev.sim_time,
                        rs_name,
                        ev.record_id,
                    )
                    fut = executor.submit(delete_rs, LOG, self.ctx, ns, rs_name)
                    futures.append(fut)

            LOG.info("trace replay complete")
        finally:
            LOG.info("waiting for all kubectl tasks to finish...")
            for fut in futures:
                try:
                    fut.result()
                except Exception as e:
                    LOG.error("kubectl task failed: %s", e)
            executor.shutdown(wait=True)
            LOG.info("all kubectl tasks completed")

    # ---------------- Monitor helpers ----------------

    def _monitor_loop(
        self,
        ns: str,
        interval_s: float,
        max_prio: int,
        prio_by_identity: Dict[str, int],
        out_csv: Path,
        stop_event: threading.Event,
        start_wall_time: float,
        sim_t0: float,
    ) -> None:
        """
        Periodically sample cluster state and write CSV.

        CSV columns:
          wall_time_s,
          sim_time_s,
          cpu_run_util, mem_run_util,
          running_count, unsched_count,
          prio1_run_time_s, ..., prio<max_prio>_run_time_s

        The per-priority run times are *cumulative* pod-seconds:
          #running_pods_in_prio * delta_t
        """
        out_csv.parent.mkdir(parents=True, exist_ok=True)
        LOG.info(
            "monitor: writing time series to %s (interval=%.3fs)",
            out_csv,
            interval_s,
        )

        # Cumulative pod-seconds per priority level
        prio_runtime: Dict[int, float] = {
            p: 0.0 for p in range(1, max_prio + 1)
        }

        last_wall_abs = time.time()

        with open(out_csv, "w", encoding="utf-8", newline="") as f:
            writer = csv.writer(f)

            # Header
            header = [
                "wall_time_s",
                "sim_time_s",
                "cpu_run_util",
                "mem_run_util",
                "running_count",
                "unsched_count",
            ]
            header.extend([f"prio{p}_run_time_s" for p in range(1, max_prio + 1)])
            writer.writerow(header)

            while not stop_event.is_set():
                try:
                    snap = stat_snapshot(self.ctx, ns, expected=0)
                except Exception as e:
                    LOG.warning("monitor: stat_snapshot failed: %s", e)
                    time.sleep(interval_s)
                    continue

                now_abs = time.time()
                wall_time_s = now_abs - start_wall_time
                sim_time_s = sim_t0 + wall_time_s  # 1:1 mapping
                dt = max(0.0, now_abs - last_wall_abs)
                last_wall_abs = now_abs

                pods_running = getattr(snap, "pods_running", []) or []
                running_count = len(pods_running)
                unsched_dict = getattr(snap, "unschedulable_by_prio", {}) or {}
                if isinstance(unsched_dict, dict):
                    try:
                        unsched_count = sum(unsched_dict.values())
                    except TypeError:
                        unsched_count = 0
                else:
                    unsched_count = 0

                # Update per-priority cumulative runtime from running pods
                for entry in pods_running:
                    # stat_snapshot returns (pod_name, node_name)
                    try:
                        pod_name = entry[0]
                    except Exception:
                        LOG.debug(
                            "unexpected pods_running entry: %r (%s)",
                            entry,
                            type(entry),
                        )
                        continue

                    # Strip namespace if present: "ns/podname" -> "podname"
                    if "/" in pod_name:
                        pod_name = pod_name.split("/", 1)[1]

                    # RS-managed pods have names "<rs_name>-<suffix>"
                    # where rs_name == self._rs_name_for_record(record_id)
                    if "-" in pod_name:
                        identity, _suffix = pod_name.rsplit("-", 1)
                    else:
                        identity = pod_name

                    prio = prio_by_identity.get(identity)
                    if prio is None:
                        continue
                    if 1 <= prio <= max_prio:
                        prio_runtime[prio] += dt

                row = [
                    f"{wall_time_s:.3f}",
                    f"{sim_time_s:.3f}",
                    f"{snap.cpu_run_util:.6f}",
                    f"{snap.mem_run_util:.6f}",
                    running_count,
                    unsched_count,
                ]
                row.extend(
                    f"{prio_runtime[p]:.6f}" for p in range(1, max_prio + 1)
                )
                writer.writerow(row)
                f.flush()

                time.sleep(interval_s)

        LOG.info("monitor: stop signal received; exiting")

    # ---------------- High-level run logic ----------------

    def run(self) -> None:
        args = self.args

        # Logging
        setup_logging(
            name="trace-replayer",
            prefix="[trace-replayer] ",
            level=args.log_level,
        )

        # Log CLI arguments
        self.log_args(args)

        LOG.info("using trace directory: %s", self.base_dir)

        # Ensure results directory exists
        self.results_dir.mkdir(parents=True, exist_ok=True)

        # Write replayer info bundle (after base_dir/result_dir are known)
        self._write_info_file()

        # 1. Load trace
        self.load_trace()

        # Map stable identity -> priority; identity is RS name
        self.prio_by_identity = {
            self._rs_name_for_record(p.id): p.priority for p in self.pods
        }

        # 2. Convert node capacities to ints (mCPU / bytes)
        self.node_cpu_m = qty_to_mcpu_int(args.node_cpu)
        self.node_mem_b = qty_to_bytes_int(args.node_mem)
        LOG.info(
            "per-node capacity: cpu_m=%d mem_bytes=%d (num_nodes=%d from trace meta)",
            self.node_cpu_m,
            self.node_mem_b,
            self.num_nodes,
        )

        # 3. Build events (only once, before we start the clock)
        self._build_events(self.node_cpu_m, self.node_mem_b)

        # 4. Create KWOK cluster from kwokctl config file
        kwok_cfg_path = Path(args.kwokctl_config_file).resolve()
        with open(kwok_cfg_path, "r", encoding="utf-8") as f:
            config_doc = yaml.safe_load(f) or {}

        ensure_kwok_cluster(
            logger=LOG,
            cluster_name=args.cluster_name,
            kwok_runtime=args.kwok_runtime,
            config_doc=config_doc,
            recreate=True,
        )

        # 5. Create KWOK nodes with the chosen capacity
        create_kwok_nodes(
            logger=LOG,
            ctx=self.ctx,
            num_nodes=self.num_nodes,
            node_cpu=args.node_cpu,
            node_mem=args.node_mem,
            pods_cap=args.pods_cap,
        )

        # 6. Namespace + PriorityClasses
        ensure_namespace(LOG, self.ctx, args.namespace)
        ensure_priority_classes(LOG, self.ctx, self.max_prio)

        # 7. Monitor output path (always under <trace-dir>/results)
        monitor_out = self.results_dir / "trace-monitor.csv"

        # 8. Start monitor thread
        start_wall_time = time.time()
        stop_event = threading.Event()
        monitor_thread = threading.Thread(
            target=self._monitor_loop,
            args=(
                args.namespace,
                float(args.monitor_interval),
                self.max_prio,
                self.prio_by_identity,
                monitor_out,
                stop_event,
                start_wall_time,
                self.t_min,
            ),
            daemon=True,
        )
        monitor_thread.start()

        # 9. Replay trace (blocking)
        try:
            self._replay_events(
                ns=args.namespace,
                start_wall_time=start_wall_time,
                sim_t0=self.t_min,
            )
        finally:
            stop_event.set()
            monitor_thread.join(timeout=10.0)
            LOG.info("monitor thread joined; done.")

        # 10. Drift logging: how far did we drift from ideal timing?
        expected_dur = self.trace_time - self.t_min
        actual_dur = time.time() - start_wall_time
        drift = actual_dur - expected_dur
        LOG.info(
            "Replay duration: expected=%.3fs actual=%.3fs drift=%.3fs",
            expected_dur,
            actual_dur,
            drift,
        )

        LOG.info("Done. Monitor CSV written to %s", monitor_out)


#######################################################################
# CLI
#######################################################################
def build_argparser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description=(
            "Replay a JSON pod trace on a KWOK cluster and monitor utilization. "
            "Expects <trace-dir>/trace.json as produced by trace_generator.py."
        )
    )

    # Trace directory (instead of a single file)
    p.add_argument(
        "--trace-dir",
        required=True,
        help="Directory containing trace.json from trace_generator.py",
    )

    # Cluster / KWOK options
    p.add_argument(
        "--cluster-name",
        default="kwok1",
        help="KWOK cluster name (kwokctl --name) (default: kwok1)",
    )
    p.add_argument(
        "--kwok-runtime",
        choices=["binary", "docker"],
        default="binary",
        help="KWOK runtime (default: binary)",
    )
    p.add_argument(
        "--kwokctl-config-file",
        required=True,
        help="KwokctlConfiguration YAML used to create the KWOK cluster.",
    )
    p.add_argument(
        "--namespace",
        default="trace",
        help="Kubernetes namespace in which to create pods (default: trace)",
    )
    p.add_argument(
        "--node-cpu",
        default="1000m",
        help="Per-node CPU capacity (K8s quantity, default: 1000m = 1 core).",
    )
    p.add_argument(
        "--node-mem",
        default="1Gi",
        help="Per-node memory capacity (K8s quantity, default: 1Gi).",
    )
    p.add_argument(
        "--pods-cap",
        type=int,
        default=512,
        help="Per-node pod capacity (status.capacity.pods) (default: 512).",
    )

    # Monitoring options
    p.add_argument(
        "--monitor-interval",
        type=float,
        default=1.0,
        help="Monitor sampling interval in seconds (default: 1.0).",
    )

    # Logging
    p.add_argument(
        "--log-level",
        default="INFO",
        help="Logging level (DEBUG, INFO, WARNING, ERROR) (default: INFO).",
    )

    return p


def main() -> None:
    args = build_argparser().parse_args()
    replayer = TraceReplayer(args)
    replayer.run()


if __name__ == "__main__":
    main()
