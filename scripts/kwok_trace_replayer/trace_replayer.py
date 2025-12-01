#!/usr/bin/env python3
# trace_replayer.py

import argparse, csv, json, logging, threading, time, yaml
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
    log_field_fmt,
    build_cli_cmd,
    write_info_file,
)
from scripts.helpers.kubectl_helpers import (
    kubectl_apply_yaml,
    ensure_namespace,
    ensure_priority_classes,
    delete_rs,
    get_json_ctx,
)
from scripts.helpers.kwok_helpers import (
    yaml_kwok_rs,
    create_kwok_nodes,
    ensure_kwok_cluster,
    kwok_pods_cap,
)
from scripts.kwok_trace_replayer.trace_helpers import (
    TraceRecord,
)

#######################################################################
# Constants
#######################################################################
MAX_REPLAY_WORKERS = 10 # number of threads for replaying events. If more than 1, tasks run "async"

#######################################################################
# Logging setup
#######################################################################
LOGGER_NAME = "trace-replayer"
LOG = logging.getLogger(LOGGER_NAME)

#######################################################################
# Event model
#######################################################################
@dataclass
class Event:
    sim_time_s: float   # seconds in trace's time
    kind: str         # "create" or "delete"
    record_id: int
    cpu_str: str | None = None
    mem_str: str | None = None
    pc_name: str | None = None
    replicas: int = 1

#####################################################################
# Argument parser
#####################################################################
def build_argparser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=(
            "Replay a JSON pod trace on a KWOK cluster and monitor utilization. "
            "Expects <trace-dir>/trace.json as produced by trace_generator.py."
        )
    )

    # Trace directory
    p.add_argument("--trace-dir", dest="trace_dir", required=True,
        help="Directory containing trace.json from trace_generator.py",
    )

    # Cluster / KWOK options
    p.add_argument("--cluster-name", dest="cluster_name", default="kwok1",
        help="KWOK cluster name (kwokctl --name) (default: kwok1)",
    )
    p.add_argument("--kwok-runtime", dest="kwok_runtime", choices=["binary", "docker"], default="binary",
        help="KWOK runtime (default: binary)",
    )
    p.add_argument("--kwokctl-config-file", dest="kwokctl_config_file", required=True,
        help="KwokctlConfiguration YAML used to create the KWOK cluster.",
    )
    p.add_argument("--namespace", dest="namespace", default="trace",
        help="Kubernetes namespace in which to create pods (default: trace)",
    )
    p.add_argument("--node-cpu", dest="node_cpu", default="1000m",
        help=(
            "Per-node CPU capacity as a Kubernetes quantity. "
            "The trace stores CPU as a fraction of one node; this flag defines what "
            "'1.0' means when converting to pod requests "
            "(e.g., 0.25 → 250m if --node-cpu=1000m). "
            "Default: 1000m (≈1 core)."
        ),
    )
    p.add_argument("--node-mem", dest="node_mem", default="1Gi",
        help=(
            "Per-node memory capacity as a Kubernetes quantity. "
            "The trace stores memory as a fraction of one node; this flag defines what "
            "'1.0' means when converting to pod requests "
            "(e.g., 0.5 → 512Mi if --node-mem=1Gi). "
            "Default: 1Gi."
        ),
    )

    # Monitoring
    p.add_argument("--monitor-interval", dest="monitor_interval", type=float, default=1.0,
        help="Monitor sampling interval in seconds (default: 1.0).",
    )

    # Logging
    p.add_argument("--log-level", dest="log_level", default="INFO",
        help="Logging level (DEBUG, INFO, WARNING, ERROR) (default: INFO).",
    )

    return p

#######################################################################
# TraceReplayer class
#######################################################################
class TraceReplayer:
    """
    Replay a trace on a KWOK cluster and monitor utilization.
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

    ##############################################
    # ------------ Info/logging helpers ----------
    ##############################################
    @staticmethod
    def log_args(args: argparse.Namespace) -> None:
        """
        Log the main arguments.
        """
        fields = [
            ("trace_dir", args.trace_dir),
            ("cluster_name", args.cluster_name),
            ("kwok_runtime", args.kwok_runtime),
            ("kwokctl_config_file", args.kwokctl_config_file),
            ("namespace", args.namespace),
            ("node_cpu", args.node_cpu),
            ("node_mem", args.node_mem),
            ("monitor_interval", args.monitor_interval),
            ("log_level", args.log_level),
        ]
        pad = max(len(k) for k, _ in fields)
        lines = [f"{k.rjust(pad)} = {log_field_fmt(v)}" for k, v in fields]
        block = "\n".join(lines)
        header, footer = make_header_footer("ARGS")
        LOG.info("\n%s\n%s\n%s", header, block, footer)

    def _write_info_file(self) -> None:
        """
        Write info_replayer.yaml in base_dir with git + CLI + args.
        """
        try:
            out_path = self.base_dir / "info_replayer.yaml"
            meta_extra = {
                "kind": "trace_replayer",
            }
            inputs = {
                "cli-cmd": build_cli_cmd(),
                "args": {k: v for k, v in vars(self.args).items()},
            }
            sections = {
                "trace": {
                    "trace_dir": str(self.base_dir),
                    "trace_path": str(self.trace_path),
                }
            }
            write_info_file(
                out_path,
                meta_extra=meta_extra,
                inputs=inputs,
                sections=sections,
                logger=LOG,
            )
        except Exception as e:
            LOG.warning("failed to write info_replayer.yaml: %s", e)

    ##############################################
    # ------------ Replay helpers ----------------
    ##############################################
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
                    sim_time_s=float(p.start_time),
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
                    sim_time_s=float(p.end_time),
                    kind="delete",
                    record_id=p.id,
                )
            )

        # sort by sim_time, then create before delete
        events.sort(key=lambda e: (e.sim_time_s, 0 if e.kind == "create" else 1))
        LOG.info("built %d events from %d pods", len(events), len(self.pods))
        self.events = events

    ##############################################
    # ------------ Replayer ----------------------
    ##############################################
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
                current_t = events[i].sim_time_s

                # Collect all events with exactly this sim_time
                batch_events: List[Event] = []
                while i < n and events[i].sim_time_s == current_t:
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
                        ev.sim_time_s,
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
                        ev.sim_time_s,
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

    ##############################################
    # ------------ Monitor helpers ---------------
    ##############################################
    def _snapshot_from_pods(self, ns: str) -> tuple[float, float, List[tuple[str, str]], int, Dict[int, int]]:
        """
        Build a snapshot directly from pods:

        Returns:
            cpu_run_util:   fraction of total cluster CPU capacity requested by running pods
            mem_run_util:   fraction of total cluster memory capacity requested by running pods
            pods_running:   list of (pod_name, node_name) for running pods
            unsched_count:  total number of Pending pods
            unsched_by_prio: dict prio -> count of Pending pods
        """
        pods_json = get_json_ctx(self.ctx, ["-n", ns, "get", "pods", "-o", "json"])
        items = pods_json.get("items", []) or []

        total_cpu_m = 0
        total_mem_b = 0

        pods_running: List[tuple[str, str]] = []
        unsched_by_prio: Dict[int, int] = {}

        for pod in items:
            meta = pod.get("metadata", {}) or {}
            spec = pod.get("spec", {}) or {}
            status = pod.get("status", {}) or {}

            pod_name = meta.get("name", "")
            node_name = spec.get("nodeName", "") or ""
            phase = status.get("phase", "")

            # Try to get priority from PriorityClass name "p<N>"
            pc_name = spec.get("priorityClassName")
            prio: int | None = None
            if isinstance(pc_name, str) and pc_name.startswith("p"):
                try:
                    prio = int(pc_name[1:])
                except ValueError:
                    prio = None

            # Fallback: derive from RS identity -> priority mapping
            if prio is None and pod_name:
                identity = pod_name
                # strip namespace if any (should not be present in name itself, but be safe)
                if "/" in identity:
                    identity = identity.split("/", 1)[1]
                # RS-managed pods: "<rs_name>-<suffix>"
                if "-" in identity:
                    identity, _suffix = identity.rsplit("-", 1)
                prio = self.prio_by_identity.get(identity)

            if phase == "Running":
                pods_running.append((pod_name, node_name))

                # Sum resource requests of all containers
                containers = spec.get("containers", []) or []
                for c in containers:
                    res = (c.get("resources") or {}).get("requests", {}) or {}
                    cpu_q = res.get("cpu")
                    mem_q = res.get("memory")
                    if cpu_q:
                        try:
                            total_cpu_m += qty_to_mcpu_int(cpu_q)
                        except Exception:
                            pass
                    if mem_q:
                        try:
                            total_mem_b += qty_to_bytes_int(mem_q)
                        except Exception:
                            pass

            elif phase == "Pending" and prio is not None:
                unsched_by_prio[prio] = unsched_by_prio.get(prio, 0) + 1

        # Compute utilization relative to known cluster capacity
        cpu_capacity_m = self.num_nodes * self.node_cpu_m
        mem_capacity_b = self.num_nodes * self.node_mem_b

        cpu_run_util = (total_cpu_m / cpu_capacity_m) if cpu_capacity_m > 0 else 0.0
        mem_run_util = (total_mem_b / mem_capacity_b) if mem_capacity_b > 0 else 0.0

        unsched_count = sum(unsched_by_prio.values())
        return cpu_run_util, mem_run_util, pods_running, unsched_count, unsched_by_prio

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

        Per-priority run times are cumulative pod-seconds, counted
        **only while pods are actually Running**.
        """
        out_csv.parent.mkdir(parents=True, exist_ok=True)
        LOG.info(
            "monitor: writing time series to %s (interval=%.3fs)",
            out_csv,
            interval_s,
        )

        # Cumulative pod-seconds per priority level
        prio_runtime: Dict[int, float] = {p: 0.0 for p in range(1, max_prio + 1)}

        # Last timestamp at which we saw THIS pod in Running phase
        last_seen_running_ts: Dict[str, float] = {}

        with open(out_csv, "w", encoding="utf-8", newline="") as f:
            writer = csv.writer(f)

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
                loop_start = time.time()

                try:
                    (
                        cpu_run_util,
                        mem_run_util,
                        pods_running,      # [(pod_name, node_name)]
                        unsched_count,
                        _unsched_by_prio,
                    ) = self._snapshot_from_pods(ns)

                except Exception as e:
                    LOG.warning("monitor: snapshot_from_pods failed: %s", e)
                    time.sleep(interval_s)
                    continue

                now_abs = time.time()
                wall_time_s = now_abs - start_wall_time
                sim_time_s = sim_t0 + wall_time_s

                running_count = len(pods_running)
                live_running_pods: set[str] = set()

                # Integrate runtime *only* during intervals where pod is Running
                for pod_name, _node_name in pods_running:
                    live_running_pods.add(pod_name)

                    identity = pod_name
                    if "/" in identity:
                        identity = identity.split("/", 1)[1]
                    if "-" in identity:
                        identity, _suffix = identity.rsplit("-", 1)

                    prio = prio_by_identity.get(identity)
                    if prio is None or not (1 <= prio <= max_prio):
                        continue

                    prev_ts = last_seen_running_ts.get(pod_name)
                    if prev_ts is None:
                        # First time we see this pod in Running:
                        # just remember the timestamp; don't backfill.
                        last_seen_running_ts[pod_name] = now_abs
                        continue

                    delta = max(0.0, now_abs - prev_ts)
                    last_seen_running_ts[pod_name] = now_abs
                    prio_runtime[prio] += delta

                # Drop pods that are no longer Running from the cache
                for name in list(last_seen_running_ts.keys()):
                    if name not in live_running_pods:
                        del last_seen_running_ts[name]

                row = [
                    f"{wall_time_s:.3f}",
                    f"{sim_time_s:.3f}",
                    f"{cpu_run_util:.6f}",
                    f"{mem_run_util:.6f}",
                    running_count,
                    unsched_count,
                ]
                row.extend(f"{prio_runtime[p]:.6f}" for p in range(1, max_prio + 1))
                writer.writerow(row)
                f.flush()

                elapsed = time.time() - loop_start
                sleep_s = max(0.0, interval_s - elapsed)
                if sleep_s > 0:
                    time.sleep(sleep_s)

        LOG.info("monitor: stop signal received; exiting")

    ##############################################
    # ------------ Runner ------------------------
    ##############################################
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
            pods_cap=kwok_pods_cap(),
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

###############################################
# ------------ Main entry point ---------------
###############################################
def main() -> None:
    args = build_argparser().parse_args()
    replayer = TraceReplayer(args)
    replayer.run()

if __name__ == "__main__":
    main()
