#!/usr/bin/env python3
# trace_replayer.py

import argparse, csv, json, logging, threading, time, yaml
from concurrent.futures import ThreadPoolExecutor, Future
from dataclasses import dataclass
from pathlib import Path
from typing import List, Dict, Any

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
    get_str,
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
    merge_kwokctl_envs,
)
from scripts.kwok_trace_replayer.trace_helpers import (
    TraceRecord,
)

#######################################################################
# Constants
#######################################################################
MAX_REPLAY_WORKERS = 5 # number of threads for replaying events. If more than 1, tasks run "async"

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

    # Job file (optional)
    p.add_argument("--job-file", dest="job_file", default=None,
        help="Path to a YAML job file describing the trace replay job (trace-dir, kwokctl-config-file, overrides, ...).",
    )

    # Trace directory (can come from CLI or job-file)
    p.add_argument("--trace-dir", dest="trace_dir", required=False, default=None,
        help="Directory containing trace.json from trace_generator.py",
    )

    # Cluster / KWOK options
    p.add_argument("--cluster-name", dest="cluster_name", default=None,
        help="KWOK cluster name (kwokctl --name) (default: kwok1).",
    )
    p.add_argument("--kwok-runtime", dest="kwok_runtime", choices=["binary", "docker"], default=None,
        help="KWOK runtime (default: binary).",
    )
    p.add_argument("--kwokctl-config-file", dest="kwokctl_config_file", required=False, default=None,
        help="KwokctlConfiguration YAML used to create the KWOK cluster.",
    )
    p.add_argument("--namespace", dest="namespace", default=None,
        help="Kubernetes namespace in which to create pods (default: trace).",
    )
    p.add_argument("--node-cpu", dest="node_cpu", default=None,
        help=(
            "Per-node CPU capacity as a Kubernetes quantity. "
            "The trace stores CPU as a fraction of one node; this flag defines what "
            "'1.0' means when converting to pod requests "
            "(e.g., 0.25 → 250m if --node-cpu=1000m). "
            "Default: 1000m (≈1 core)."
        ),
    )
    p.add_argument("--node-mem", dest="node_mem", default=None,
        help=(
            "Per-node memory capacity as a Kubernetes quantity. "
            "The trace stores memory as a fraction of one node; this flag defines what "
            "'1.0' means when converting to pod requests "
            "(e.g., 0.5 → 512Mi if --node-mem=1Gi). "
            "Default: 1Gi."
        ),
    )

    # Monitoring
    p.add_argument("--monitor-interval", dest="monitor_interval", type=float, default=None,
        help="Monitor sampling interval in seconds (default: 1.0).",
    )

    # Logging
    p.add_argument("--log-level", dest="log_level", default=None,
        help="Logging level (DEBUG, INFO, WARNING, ERROR) (default: INFO).",
    )

    return p

def merge_job_fields_into_args(
    args: argparse.Namespace,
    job: Dict[str, Any],
) -> tuple[argparse.Namespace, List[Dict[str, Any]]]:
    """
    Merge job-file fields into args.
    CLI has priority: we only fill fields that are currently None.
    Returns (args, override_kwokctl_envs).
    """
    jf_trace_dir            = get_str(job.get("trace-dir"))
    jf_cluster_name         = get_str(job.get("cluster-name"))
    jf_kwok_runtime         = get_str(job.get("kwok-runtime"))
    jf_kwokctl_config_file  = get_str(job.get("kwokctl-config-file"))
    jf_namespace            = get_str(job.get("namespace"))
    jf_node_cpu             = get_str(job.get("node-cpu"))
    jf_node_mem             = get_str(job.get("node-mem"))
    jf_monitor_interval     = job.get("monitor-interval")
    jf_log_level            = get_str(job.get("log-level"))
    jf_override_kwokctl_envs = job.get("override-kwokctl-envs") or []

    if getattr(args, "trace_dir", None) is None and jf_trace_dir:
        args.trace_dir = jf_trace_dir
    if getattr(args, "cluster_name", None) is None and jf_cluster_name:
        args.cluster_name = jf_cluster_name
    if getattr(args, "kwok_runtime", None) is None and jf_kwok_runtime:
        args.kwok_runtime = jf_kwok_runtime
    if getattr(args, "kwokctl_config_file", None) is None and jf_kwokctl_config_file:
        args.kwokctl_config_file = jf_kwokctl_config_file
    if getattr(args, "namespace", None) is None and jf_namespace:
        args.namespace = jf_namespace
    if getattr(args, "node_cpu", None) is None and jf_node_cpu:
        args.node_cpu = jf_node_cpu
    if getattr(args, "node_mem", None) is None and jf_node_mem:
        args.node_mem = jf_node_mem
    if getattr(args, "log_level", None) is None and jf_log_level:
        args.log_level = jf_log_level
    if getattr(args, "monitor_interval", None) is None and jf_monitor_interval is not None:
        try:
            args.monitor_interval = float(jf_monitor_interval)
        except Exception:
            pass

    return args, jf_override_kwokctl_envs


def ensure_default_args(args: argparse.Namespace) -> argparse.Namespace:
    """
    Final defaults + sanity checks after we've merged job-file and CLI.
    """
    if getattr(args, "cluster_name", None) is None:
        args.cluster_name = "kwok1"
    if getattr(args, "kwok_runtime", None) is None:
        args.kwok_runtime = "binary"
    if getattr(args, "namespace", None) is None:
        args.namespace = "trace"
    if getattr(args, "node_cpu", None) is None:
        args.node_cpu = "1000m"
    if getattr(args, "node_mem", None) is None:
        args.node_mem = "1Gi"
    if getattr(args, "monitor_interval", None) is None:
        args.monitor_interval = 1.0
    if getattr(args, "log_level", None) is None:
        args.log_level = "INFO"
    if getattr(args, "job_file", None) is None:
        args.job_file = None

    # Required: trace_dir + kwokctl_config_file (from CLI or job-file)
    if not getattr(args, "trace_dir", None):
        raise SystemExit("--trace-dir (or trace-dir in job-file) is required")
    if not getattr(args, "kwokctl_config_file", None):
        raise SystemExit("--kwokctl-config-file (or kwokctl-config-file in job-file) is required")

    trace_dir = Path(args.trace_dir).resolve()
    if not trace_dir.exists():
        raise SystemExit(f"--trace-dir not found: {trace_dir}")
    kwok_cfg = Path(args.kwokctl_config_file).resolve()
    if not kwok_cfg.exists():
        raise SystemExit(f"--kwokctl-config-file not found: {kwok_cfg}")

    return args

#######################################################################
# TraceReplayer class
#######################################################################
class TraceReplayer:
    """
    Replay a trace on a KWOK cluster and monitor utilization.
    """
    def __init__(
        self,
        args: argparse.Namespace,
        job_doc: Dict[str, Any] | None = None,
        override_kwokctl_envs: List[Dict[str, Any]] | None = None,
    ) -> None:
        self.args = args
        self.job_doc: Dict[str, Any] = job_doc or {}
        self.override_kwokctl_envs: List[Dict[str, Any]] = list(override_kwokctl_envs or [])

        # Base directory containing trace.json and other artifacts
        self.base_dir: Path = Path(args.trace_dir).resolve()
        self.trace_path: Path = self.base_dir / "trace.json"
        self.results_dir: Path = self.base_dir / "results"
        self.results_dir.mkdir(parents=True, exist_ok=True) # ensure results directory exists
        self.monitor_path = self.results_dir / "trace-monitor.csv"
        
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
        self.prio_by_identity: Dict[str, int] = {} # pod identity -> priority
        
        # Write metadata bundle
        LOG.info("logging arguments and git info to trace_dir...")
        self._write_info_file()
        
        # Log args
        self.log_args()
    
    ##############################################
    # ------------ Info/logging helpers ----------
    ##############################################
    def log_args(self) -> None:
        """
        Log the main arguments.
        """
        fields = [
            ("trace_dir", self.args.trace_dir),
            ("cluster_name", self.args.cluster_name),
            ("kwok_runtime", self.args.kwok_runtime),
            ("kwokctl_config_file", self.args.kwokctl_config_file),
            ("namespace", self.args.namespace),
            ("node_cpu", self.args.node_cpu),
            ("node_mem", self.args.node_mem),
            ("monitor_interval", self.args.monitor_interval),
            ("log_level", self.args.log_level),
            ("job_file", getattr(self.args, "job_file", None)),
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
                "job_file": getattr(self.args, "job_file", None),
                "kwokctl_config_file": self.args.kwokctl_config_file,
            }
            inputs = {
                "cli-cmd": build_cli_cmd(),
                "args": {k: v for k, v in vars(self.args).items()},
                "job": self.job_doc or {},
            }
            write_info_file(
                out_path,
                meta_extra=meta_extra,
                inputs=inputs,
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
            raise FileNotFoundError(f"Trace file not found: {self.trace_path} "
                f"(expected trace.json inside --trace-dir={self.base_dir})"
            )
        with open(self.trace_path, "r", encoding="utf-8") as f:
            raw = json.load(f)
        
        meta = raw.get("meta", {}) or {}
        records = raw.get("pods", []) or []

        # Placeholder for parsed pods
        pods: List[TraceRecord] = []
        max_prio = 0
        t_min = float("inf")
        trace_time = 0.0

        # Run through records and parse them into TraceRecord objects
        for rec in records:
            
            id_val      = int(rec["id"])
            start       = float(rec["start_time"])
            end         = float(rec["end_time"])
            cpu         = float(rec["cpu"])
            mem         = float(rec["mem"])
            prio        = int(rec.get("priority"))
            replicas    = int(rec.get("replicas"))
            max_prio    = max(max_prio, prio)
            t_min       = min(t_min, start)
            trace_time  = max(trace_time, end)
            
            pods.append(TraceRecord(id=id_val, start_time=start, end_time=end, cpu=cpu, mem=mem, priority=prio, replicas=replicas))

        LOG.info("loaded %d pods from %s (t_min=%.3f, trace_time=%.3f, max_priority=%d)",
            len(pods),
            self.trace_path,
            t_min,
            trace_time,
            max_prio,
        )

        # Sort pods by start_time
        pods.sort(key=lambda p: p.start_time)

        # Update instance variables
        self.pods = pods
        self.max_prio = max_prio
        self.t_min = t_min
        self.trace_time = trace_time
        self.meta = meta
        self.num_nodes = int(meta["num_nodes"])

    def _build_events(self) -> None:
        """
        Turn trace pods into a sorted list of events with concrete K8s quantities.
        """
        events: List[Event] = []
        for p in self.pods:
            cpu_m = max(1, int(round(p.cpu * self.node_cpu_m)))
            mem_b = max(1, int(round(p.mem * self.node_mem_b)))
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
    def _replay_events(self, namespace: str, start_wall_time: float, sim_t0: float) -> None:
        """
        Replay events against the cluster.
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
        idx = 0
        num_events = len(events)
        executor = ThreadPoolExecutor(max_workers=MAX_REPLAY_WORKERS)
        futures: List[Future] = []
        try:
            while idx < num_events:
                # Current batch timestamp (trace time)
                current_t = events[idx].sim_time_s

                # Collect all events with exactly this sim_time
                batch_events: List[Event] = []
                while idx < num_events and events[idx].sim_time_s == current_t:
                    batch_events.append(events[idx])
                    idx += 1

                # Ideal wall clock time for this sim_t
                target_wall = start_wall_time + max(0.0, current_t - sim_t0)

                # Sleep until that wall time
                now_before = time.time() # now_before means "before executing batch"
                sleep_s = max(0.0, target_wall - now_before)
                if sleep_s > 0:
                    time.sleep(sleep_s)

                # Split into creates and deletes for logging
                creates = [ev for ev in batch_events if ev.kind == "create"]
                deletes = [ev for ev in batch_events if ev.kind == "delete"]

                # Logging
                now_after = time.time() # now_after means "right after sleep / just before submitting kubectl calls"
                batch_drift = now_after - target_wall
                LOG.info(
                    "TIME DRIFT batch @ sim_t=%.3f: target_wall=%.3f actual_wall=%.3f drift=%.6fs "
                    "(creates=%d deletes=%d)",
                    current_t,
                    target_wall - start_wall_time,
                    now_after - start_wall_time,
                    batch_drift,
                    len(creates),
                    len(deletes),
                )

                # ------------------------------------------------------
                # CREATE events: one kubectl_apply_yaml per RS
                # ------------------------------------------------------
                for ev in creates:
                    assert (
                        ev.cpu_str is not None
                        and ev.mem_str is not None
                        and ev.pc_name is not None
                    )
                    rs_name = self._rs_name_for_record(ev.record_id)
                    yaml_text = yaml_kwok_rs(
                        ns=namespace,
                        rs_name=rs_name,
                        replicas=ev.replicas,
                        cpu=ev.cpu_str,
                        mem=ev.mem_str,
                        pc=ev.pc_name,
                    )
                    LOG.info(
                        "CREATE @ sim_t=%.3f: rs=%s (id=%d) replicas=%d cpu=%s mem=%s pc=%s",
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
                # DELETE events: one delete_rs per RS
                # ------------------------------------------------------
                for ev in deletes:
                    rs_name = self._rs_name_for_record(ev.record_id)
                    LOG.info(
                        "DELETE @ sim_t=%.3f: rs=%s (id=%d)",
                        ev.sim_time_s,
                        rs_name,
                        ev.record_id,
                    )
                    fut = executor.submit(delete_rs, LOG, self.ctx, namespace, rs_name)
                    futures.append(fut)
                
                LOG.info("batch done @ sim_t=%.3f submitted: creates=%d deletes=%d", current_t, len(creates), len(deletes))
            
            LOG.info("all events processed; waiting for kubectl tasks to finish...")
        
        finally:
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
    def _snapshot_from_pods(self, ns: str) -> tuple[float, float, List[tuple[str, str]], int]:
        """
        Build a snapshot directly from pods:
        Returns:
            cpu_run_util:   fraction of total cluster CPU capacity requested by running pods
            mem_run_util:   fraction of total cluster memory capacity requested by running pods
            pods_running:   list of (pod_name, node_name) for running pods
            unsched_count:  total number of Pending pods
        """
        pods_json = get_json_ctx(self.ctx, ["-n", ns, "get", "pods", "-o", "json"])
        items = pods_json.get("items", []) or []
        total_cpu_m = 0
        total_mem_b = 0
        pods_running: List[tuple[str, str]] = []
        unsched_count = 0
        
        for pod in items:
            meta = pod.get("metadata", {}) or {}
            spec = pod.get("spec", {}) or {}
            status = pod.get("status", {}) or {}
            pod_name = meta.get("name", "")
            node_name = spec.get("nodeName", "") or ""
            phase = status.get("phase", "")
            
            if phase == "Running":
                pods_running.append((pod_name, node_name))
                # Sum resource requests of all containers
                containers = spec.get("containers", []) or []
                for c in containers:
                    res = (c.get("resources") or {}).get("requests", {}) or {}
                    cpu_q = res.get("cpu")
                    mem_q = res.get("memory")
                    total_cpu_m += qty_to_mcpu_int(cpu_q)
                    total_mem_b += qty_to_bytes_int(mem_q)
            
            elif phase == "Pending":
                unsched_count += 1

        # Compute utilization relative to known cluster capacity
        cpu_capacity_m = self.num_nodes * self.node_cpu_m
        mem_capacity_b = self.num_nodes * self.node_mem_b
        cpu_run_util = (total_cpu_m / cpu_capacity_m) if cpu_capacity_m > 0 else 0.0
        mem_run_util = (total_mem_b / mem_capacity_b) if mem_capacity_b > 0 else 0.0
        
        return cpu_run_util, mem_run_util, pods_running, unsched_count

    def _monitor_loop(
        self,
        namespace: str,
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
        """
        out_csv.parent.mkdir(parents=True, exist_ok=True)
        LOG.info(
            "monitor: writing time series to %s (interval=%.3fs)",
            out_csv,
            interval_s,
        )

        prio_runtime: Dict[int, float] = {p: 0.0 for p in range(1, max_prio + 1)} # cumulative pod-seconds per priority level
        last_seen_running_ts: Dict[str, float] = {} # last timestamp at which we saw this pod running

        with open(out_csv, "w", encoding="utf-8", newline="") as f:
            writer = csv.writer(f)
            header = ["wall_time_s", "sim_time_s", "cpu_run_util", "mem_run_util", "running_count", "unsched_count"]
            header.extend([f"prio{p}_run_time_s" for p in range(1, max_prio + 1)])
            writer.writerow(header)

            while not stop_event.is_set():
                loop_start = time.time()
                try:
                    cpu_run_util, mem_run_util, pods_running, unsched_cnt = self._snapshot_from_pods(namespace)
                except Exception as e:
                    LOG.warning("monitor: snapshot_from_pods failed: %s", e)
                    time.sleep(interval_s)
                    continue
                now_abs = time.time()
                wall_time_s = now_abs - start_wall_time
                sim_time_s = sim_t0 + wall_time_s
                running_cnt = len(pods_running)
                running_pods: set[str] = set()
                
                # Integrate runtime per priority level
                for pod_name, _node_name in pods_running:
                    running_pods.add(pod_name)
                    if "-" in pod_name:
                        identity, _suffix = pod_name.rsplit("-", 1)
                    prio = prio_by_identity.get(identity)
                    prev_ts = last_seen_running_ts.get(pod_name)
                    if prev_ts is None: # first time we see this pod in Running: just remember the timestamp; don't backfill.
                        last_seen_running_ts[pod_name] = now_abs
                        continue
                    delta = max(0.0, now_abs - prev_ts)
                    last_seen_running_ts[pod_name] = now_abs
                    prio_runtime[prio] += delta

                # Drop pods that are no longer Running from the cache
                for name in list(last_seen_running_ts.keys()):
                    if name not in running_pods:
                        del last_seen_running_ts[name]

                row = [f"{wall_time_s:.3f}", f"{sim_time_s:.3f}", f"{cpu_run_util:.6f}", f"{mem_run_util:.6f}", running_cnt, unsched_cnt]
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
        # Load trace
        self.load_trace()

        # self.prio_by_identity holds all pods from the trace and their priorities
        self.prio_by_identity = {
            self._rs_name_for_record(p.id): p.priority for p in self.pods
        }

        # Convert node capacities to ints (mCPU / bytes)
        self.node_cpu_m = qty_to_mcpu_int(self.args.node_cpu)
        self.node_mem_b = qty_to_bytes_int(self.args.node_mem)
        LOG.info("per-node capacity: cpu_m=%d mem_bytes=%d (num_nodes=%d from trace meta)",
            self.node_cpu_m,
            self.node_mem_b,
            self.num_nodes,
        )

        # Build events
        self._build_events()

        # Create KWOK cluster
        kwok_cfg_path = Path(self.args.kwokctl_config_file).resolve()
        with open(kwok_cfg_path, "r", encoding="utf-8") as f:
            config_doc = yaml.safe_load(f) or {}

        # Apply override-kwokctl-envs from job-file, if any
        if self.override_kwokctl_envs:
            config_doc = merge_kwokctl_envs(config_doc, self.override_kwokctl_envs)

        ensure_kwok_cluster(
            logger=LOG,
            cluster_name=self.args.cluster_name,
            kwok_runtime=self.args.kwok_runtime,
            config_doc=config_doc,
            recreate=True,
        )

        # Create KWOK nodes
        create_kwok_nodes(
            logger=LOG,
            ctx=self.ctx,
            num_nodes=self.num_nodes,
            node_cpu=self.args.node_cpu,
            node_mem=self.args.node_mem,
            pods_cap=kwok_pods_cap(),
        )

        # Namespace + PriorityClasses
        ensure_namespace(LOG, self.ctx, self.args.namespace)
        ensure_priority_classes(LOG, self.ctx, self.max_prio)

        # Start monitor thread
        start_wall_time = time.time()
        stop_event = threading.Event()
        monitor_thread = threading.Thread(
            target=self._monitor_loop,
            args=(
                self.args.namespace,
                float(self.args.monitor_interval),
                self.max_prio,
                self.prio_by_identity,
                self.monitor_path,
                stop_event,
                start_wall_time,
                self.t_min,
            ),
            daemon=True,
        )
        monitor_thread.start()

        # Replay events
        try:
            self._replay_events(
                namespace=self.args.namespace,
                start_wall_time=start_wall_time,
                sim_t0=self.t_min,
            )
        finally:
            stop_event.set()
            monitor_thread.join(timeout=10.0)
            LOG.info("monitor thread joined; done.")

        LOG.info("Done.")

###############################################
# ------------ Main entry point ---------------
###############################################
def main() -> None:
    args = build_argparser().parse_args()

    job_doc: Dict[str, Any] | None = None
    override_kwokctl_envs: List[Dict[str, Any]] = []

    # If a job-file is provided, load it and merge into args
    if getattr(args, "job_file", None):
        job_path = Path(args.job_file)
        if not job_path.exists():
            raise SystemExit(f"--job-file not found: {job_path}")
        try:
            with open(job_path, "r", encoding="utf-8") as f:
                job_doc = yaml.safe_load(f) or {}
            if not isinstance(job_doc, dict):
                raise SystemExit(f"--job-file must be a YAML mapping/object, got {type(job_doc).__name__}")
        except Exception as e:
            raise SystemExit(f"--job-file parse error for {job_path}: {e}")
        args, override_kwokctl_envs = merge_job_fields_into_args(args, job_doc)

    # Fill defaults + sanity checks (trace_dir, kwokctl_config_file, ...)
    args = ensure_default_args(args)

    # Setup logging using final log-level
    setup_logging(name="trace-replayer", prefix="[trace-replayer] ", level=args.log_level)

    # TraceReplayer instance
    replayer = TraceReplayer(args, job_doc=job_doc, override_kwokctl_envs=override_kwokctl_envs)
    replayer.run()

if __name__ == "__main__":
    main()
