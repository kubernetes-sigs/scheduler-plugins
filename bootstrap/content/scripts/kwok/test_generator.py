#!/usr/bin/env python3
# test_generator.py

import sys, os, copy, shutil, argparse, math, time, random, csv, json, logging, yaml, subprocess, tempfile, traceback, shlex
from argparse import BooleanOptionalAction
from dataclasses import dataclass, is_dataclass, asdict
from typing import Optional, Tuple, List, Dict, Any, Mapping, Iterable
from collections import namedtuple, Counter
from pathlib import Path
from urllib import request as _urlreq, error as _urlerr

from helpers import (
    seeded_random,
    get_timestamp, setup_logging, format_hms,
    stat_snapshot,
    csv_append_row, ensure_csv_with_header, csv_read_header,
    qty_to_mcpu_str, qty_to_bytes_str, qty_to_bytes_int, qty_to_mcpu_int,
    yaml_kwok_node, yaml_priority_class, yaml_kwok_pod, yaml_kwok_rs,
    normalize_interval, parse_int_interval, parse_qty_interval, parse_timeout_s, get_int_from_dict, get_float_from_dict, get_str, get_str_from_dict,
    coerce_bool, kwok_cache_lock,
    make_header_footer,
    generate_seeds,
)

# ===============================================================
# Constants
# ===============================================================
RESULTS_HEADER = [
    "timestamp", "seed",
    "error", "baseline", "best_name","best_score", "best_duration_ms", "best_status",
    "solver_attempts",
    "util_run_cpu", "util_run_mem",
    "cpu_m_run", "mem_b_run",
    "running_count", "unscheduled_count",
    "pods_run_by_node",
    "running_placed_by_prio", "unschedulable_by_prio",
    "unscheduled", "running",
    "pod_node",
]

LOGGER_NAME = "kwok"
LOG = logging.getLogger(LOGGER_NAME)

CM_SOLVER_STATS_NAME = "solver-stats"
CM_SOLVER_STATS_NAMESPACE = "kube-system"

RETRIES_ON_FAIL = 5

SOLVER_TRIGGER_URL = "http://localhost:18080/optimize"
SOLVER_TRIGGER_TIMEOUT_S = 60
SOLVER_ACTIVE_URL = "http://localhost:18080/active"

SOLVER_CMD = "python3 scripts/python_solver/main.py"

# ===============================================================
# Data classes
# ===============================================================
@dataclass
class TestConfigRaw:
    # namespace / topology (from YAML)
    namespace: str = ""
    num_nodes: int = 0
    num_pods: int = 0
    num_priorities: Optional[Tuple[int, int]] = None

    # replicaset (optional; if omitted, plain pods are created)
    num_replicas_per_rs: Optional[Tuple[int, int]] = None     # e.g., (3, 50)

    # per-pod intervals (K8s quantities as strings)
    cpu_per_pod: Optional[Tuple[str, str]] = None  # e.g., ("100m","1500m")
    mem_per_pod: Optional[Tuple[str, str]] = None  # e.g., ("128Mi","2048Mi")

    # utilization target
    util: float = 0.0

    # waits
    wait_pod_mode: Optional[str] = None     # None/"none","exist","ready","running"
    wait_pod_timeout: Optional[str] = None  # "5s"
    settle_timeout_min: Optional[str] = None # minimum time to wait after we have applied all pods
    settle_timeout_max: Optional[str] = None # if set, the script will call the /active endpoint until this timeout

@dataclass
class TestConfigApplied:
    namespace: str
    
    num_nodes: int
    num_pods: int
    
    node_cpu_m: int
    node_mem_b: int
    cpu_per_pod_m: Tuple[int, int]
    mem_per_pod_b: Tuple[int, int]
    
    num_priorities: int
    
    num_replicaset: int
    num_replicas_per_rs: Optional[Tuple[int, int]]
    
    util: float

    wait_pod_mode: Optional[str]
    wait_pod_timeout_s: int
    settle_timeout_min_s: int
    settle_timeout_max_s: int
    
    # Pod spec parts
    pod_parts_cpu_m: List[int] | None = None
    pod_parts_mem_b: List[int] | None = None
    rs_sets: List[int] | None = None
    rs_parts_cpu_m: List[int] | None = None
    rs_parts_mem_b: List[int] | None = None

_Failure = namedtuple("_Failure", "category seed phase message details")

##############################################
# ------------ Args --------------------------
##############################################
def build_argparser() -> argparse.ArgumentParser:
    ap = argparse.ArgumentParser(description="Generator of KWOK test clusters and workloads.")

    # general
    ap.add_argument("--cluster-name", dest="cluster_name", default=None,
                    help="A unique KWOK cluster name (default: kwok1).")
    ap.add_argument("--kwok-runtime", dest="kwok_runtime", default=None,
                    help="KWOK runtime 'binary' or 'docker'.")
    ap.add_argument("--job-file", dest="job_file", default=None,
                    help="Path to a YAML job file describing one job and optional in-memory config overrides.")
    ap.add_argument("--workload-config-file", dest="workload_config_file", required=False,
                    help="Path to a single workload YAML (WorkloadConfiguration)")
    ap.add_argument("--kwokctl-config-file", dest="kwokctl_config_file", required=False,
                    help="Path to a single KwokctlConfiguration YAML")
    ap.add_argument("--output-dir", dest="output_dir", default=None,
                    help="Directory to store outputs/results. If omitted, outputs are written to ./output")
    ap.add_argument("--re-run-seeds", dest="re_run_seeds", action=BooleanOptionalAction, default=None,
                    help="Re-run seeds that already exist in results.csv.")
    ap.add_argument("--clean-start", dest="clean_start", action=BooleanOptionalAction, default=None,
                    help="Remove old outputs and start fresh.")
    ap.add_argument("--pause", dest="pause", action=BooleanOptionalAction, default=None,
                    help="Pause for Enter between seeds.")
    ap.add_argument("--log-level", dest="log_level", default=None,
                    help="Logging level (DEBUG, INFO, WARNING, ERROR) (default: INFO)")
    ap.add_argument("--gen-seeds-to-file", dest="gen_seeds_to_file", nargs="+", metavar="PATH NUM [PARTS]",
                    help=("Write NUM random seeds to PATH, then exit. Optionally split into PARTS files..."))

    # seeds
    ap.add_argument("--seed", type=int, default=None,
                    help="Run exactly this seed (per kwok-config)")
    ap.add_argument("--seed-file", dest="seed_file", default=None,
                    help="Path to seeds file (CSV with 'seed' col or newline list).")
    ap.add_argument("--count", type=int, default=None,
                    help="Generate the specified number of random seeds; -1=infinite.")
    ap.add_argument("--repeats", type=int, default=None,
                    help="Number of successful runs to collect per seed (default: 1).")
    ap.add_argument("--seeds-not-all-running", dest="seeds_not_all_running", type=int, default=None,
                    help=("If >0, save up to this many seeds where not all pods are running..."))

    # solver stats/logs
    ap.add_argument("--save-solver-stats", dest="save_solver_stats", action=BooleanOptionalAction, default=None,
                    help="Save solver stats for each seed under <output-dir>/solver-stats")
    ap.add_argument("--save-scheduler-logs", dest="save_scheduler_logs", action=BooleanOptionalAction, default=None,
                    help="Save 'kwokctl logs kube-scheduler' under <output-dir>/scheduler-logs")

    # manual HTTP solver trigger
    ap.add_argument("--solver-trigger", dest="solver_trigger", action=BooleanOptionalAction, default=None,
                    help="After applying all pods for a seed, POST the manual solver endpoint.")

    # Direct solver
    ap.add_argument("--solver-directly", dest="solver_directly", action=BooleanOptionalAction, default=None,
                    help="Bypass cluster use; directly call the Python solver with generated nodes/pods.")
    ap.add_argument("--solver-timeout-ms", dest="solver_timeout_ms", type=int, default=None,
                    help="Timeout for the Python solver (milliseconds).")
    ap.add_argument("--solver-input-export", dest="solver_input_export", type=Path, default=None,
                    help="If set, write the exact JSON sent to the solver to this path.")
    ap.add_argument("--solver-output-export", dest="solver_output_export", type=Path, default=None,
                    help="If set, write the solver JSON response to this path.")
    ap.add_argument("--solver-directly-running-target-util", dest="solver_directly_running_target_util", type=float, default=None,
                    help="Direct mode only: pre-place pods up to this per-node utilization (0..1).")

    return ap

##############################################
# ------------ Main class --------------------
##############################################
class KwokTestGenerator:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.workload_config_doc = None
        self.workload_config: TestConfigRaw | None = None
        self.kwokctl_config_doc = None
        self.job_doc = None
        override_wl_config = None
        override_kwokctl_envs = None

        # Use getattr because args may not have job_file if not provided on CLI
        if getattr(self.args, "job_file", None):
            job_path = Path(self.args.job_file)
            if not job_path.exists():
                raise SystemExit(f"--job-file not found: {job_path}")
            try:
                with open(job_path, "r", encoding="utf-8") as f:
                    self.job_doc = yaml.safe_load(f) or {}
                if not isinstance(self.job_doc, dict):
                    raise SystemExit(f"--job-file must be a YAML mapping/object, got {type(self.job_doc).__name__}")
            except Exception as e:
                raise SystemExit(f"--job-file parse error for {job_path}: {e}")
            self.args, override = self.merge_job_fields_into_args(self.args, self.job_doc or {})
            override_wl_config = override.get("config", {})
            override_kwokctl_envs = override.get("kwokctl_envs", [])

        # Ensure defaults args
        self.args = KwokTestGenerator.ensure_default_args(self.args)
        
        # Setup logging
        setup_logging(name=LOGGER_NAME, prefix="[test-generator] ", level=self.args.log_level)
        
        self.log_args(self.args)

        wl_path = Path(self.args.workload_config_file).resolve()
        kwokctl_path = Path(self.args.kwokctl_config_file).resolve()

        # Load, merge, log and validate workload config
        with open(wl_path, "r", encoding="utf-8") as f:
            self.workload_config_doc = yaml.safe_load(f)
            if not isinstance(self.workload_config_doc, dict) or self.workload_config_doc.get("kind") != "WorkloadConfiguration":
                raise SystemExit(f"{wl_path}: expected a WorkloadConfiguration document")
        if override_wl_config:
            self.workload_config_doc = self._merge_doc(self.workload_config_doc, override_wl_config)
        self.workload_config = self._parse_config_doc(self.workload_config_doc, override_wl_config)
        self._log_workload_config(self.workload_config)
        ok, msg = self._validate_workload_config(self.workload_config)
        if not ok:
            raise SystemExit(f"config-failed {wl_path}: {msg}")
        
        # Load, merge and log kwokctl config
        with open(kwokctl_path, "r", encoding="utf-8") as f:
            self.kwokctl_config_doc = yaml.safe_load(f)
        if not isinstance(self.kwokctl_config_doc, dict) or not (self.kwokctl_config_doc.get("kind") == "KwokctlConfiguration" and
                str(self.kwokctl_config_doc.get("apiVersion", "")).startswith("config.kwok.x-k8s.io/")):
            raise SystemExit(f"{kwokctl_path}: expected a KwokctlConfiguration document")
        if override_kwokctl_envs:
            self.kwokctl_config_doc = self._merge_kwokctl_envs(self.kwokctl_config_doc, override_kwokctl_envs)
        kwokctl_envs = self._get_kwokctl_envs(self.kwokctl_config_doc)
        if kwokctl_envs:
            self.log_kwokctl_envs(kwokctl_envs)

        # Output dir and files
        self.output_dir_resolved = self._prepare_output_dir()
        self.results_f = self.output_dir_resolved / "results.csv"
        self.failed_f  = self.output_dir_resolved / "failed.csv"
        self.skipped_all_running_f = self.output_dir_resolved / "skipped_all_running.csv"
        self.solver_stats_dir = self.output_dir_resolved / "solver-stats"
        self.scheduler_logs_dir = self.output_dir_resolved / "scheduler-logs"

        # Write a bundle with metadata
        self._write_info_file()

        # General state
        self.ctx = f"kwok-{self.args.cluster_name}"
        self.seen_results: set[int] = self._load_seen_results()
        self.seed_durations: list[float] = []
        self.saved_not_all_running: int = 0
        self.last_solver_result: dict[str, Any] | None = None
        self.suppress_fail_log: bool = False
        self.failure: _Failure | None = None

    ##############################################
    # ------------ Logging helpers -----------------
    ##############################################
    @staticmethod
    def _get_git_info(cwd: Optional[Path] = None) -> Dict[str, Any]:
        """
        Collect basic git info for reproducibility. Best effort; returns empty on failure.
        """
        info: Dict[str, Any] = {}
        def _run(cmd: List[str]) -> Optional[str]:
            try:
                r = subprocess.run(cmd, cwd=str(cwd) if cwd else None, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, check=False)
                if r.returncode == 0:
                    return (r.stdout or b"").decode("utf-8", errors="replace").strip()
            except Exception:
                pass
            return None
        commit = _run(["git", "rev-parse", "HEAD"])
        branch = _run(["git", "rev-parse", "--abbrev-ref", "HEAD"])
        r = subprocess.run(["git", "status", "--porcelain"], cwd=str(cwd) if cwd else None,
                            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, check=False)
        if commit is not None: info["commit"] = commit
        if branch is not None: info["branch"] = branch
        return info

    def _write_info_file(self) -> None:
        """
        Write a single info.yaml in the output dir that includes:
        - top-level metadata (timestamp, git info, args, kwokctl envs, extra meta)
        - literal contents of workload/kwokctl configs and the job-file if provided
        """
        try:
            self.output_dir_resolved.mkdir(parents=True, exist_ok=True)
            git_info = self._get_git_info(Path.cwd())
            payload = {
                "meta": {
                    "timestamp": get_timestamp(),
                    "git": git_info or {},
                    "job_file": self.args.job_file,
                    "workload_config_file": self.args.workload_config_file,
                    "kwokctl_config_file": self.args.kwokctl_config_file,
                },
                "inputs": {
                    "cli-cmd": "python3 " + " ".join(shlex.quote(a) for a in sys.argv),
                    "job": self.job_doc if self.job_doc else {},
                    "workload": asdict(self.workload_config_doc) if is_dataclass(self.workload_config_doc) else self.workload_config_doc,
                    "kwokctl": self.kwokctl_config_doc,
                },
            }
            out_path = self.output_dir_resolved / "info.yaml"
            with open(out_path, "w", encoding="utf-8") as fh:
                yaml.safe_dump(payload, fh, sort_keys=False)
            LOG.info("wrote info bundle to %s", out_path)
        except Exception as e:
            LOG.warning("failed to write info.yaml: %s", e)

    def combined_job_config_seed_str(self) -> str:
        jf_str = f"job_file={self.args.job_file}" if self.args.job_file else ""
        wl_str = f"workload_config_file={self.args.workload_config_file}"
        kwokctl_str = f"kwokctl_config_file={self.args.kwokctl_config_file}"
        seed_file_str = f"seed_file={self.args.seed_file}" if self.args.seed_file else ""
        str_combined = "\n".join(s for s in (jf_str, wl_str, kwokctl_str, seed_file_str) if s)
        return str_combined

    @staticmethod
    def _log_workload_config(tr: "TestConfigRaw") -> None:
        def _fmt_interval(v):
            if v is None:
                return "<unset>"
            if isinstance(v, (tuple, list)) and len(v) == 2:
                return f"{v[0]},{v[1]}"
            return str(v)
        fields = [
            ("namespace", tr.namespace),
            ("num_nodes", tr.num_nodes),
            ("num_pods", tr.num_pods),
            ("num_priorities", _fmt_interval(tr.num_priorities)),
            ("num_replicas_per_rs", _fmt_interval(tr.num_replicas_per_rs)),
            ("cpu_per_pod", _fmt_interval(tr.cpu_per_pod)),
            
            ("mem_per_pod", _fmt_interval(tr.mem_per_pod)),
            ("util", tr.util),
            
            ("wait_pod_mode", tr.wait_pod_mode or "<unset>"),
            ("wait_pod_timeout", tr.wait_pod_timeout or "<unset>"),
            
            ("settle_timeout_min", tr.settle_timeout_min or "<unset>"),
            ("settle_timeout_max", tr.settle_timeout_max or "<unset>"),
        ]
        pad = max(len(k) for k, _ in fields)
        def _fmt(v):
            return "<unset>" if v in (None, "") else str(v)
        lines = [f"{k.rjust(pad)} = {_fmt(v)}" for k, v in fields]
        block = "\n".join(lines)
        header, footer = make_header_footer("CONFIG")
        LOG.info("\n%s\n%s\n%s", header, block, footer)

    @staticmethod
    def log_args(args) -> None:
        fields = [
            ("cluster_name", args.cluster_name),
            ("kwok_runtime", args.kwok_runtime),
            ("workload_config_file", args.workload_config_file),
            ("kwokctl_config_file", args.kwokctl_config_file),
            ("output_dir", args.output_dir),
            ("clean_start", args.clean_start),
            ("re_run_seeds", args.re_run_seeds),
            ("log_level", args.log_level),
            
            ("gen_seeds_to_file", args.gen_seeds_to_file),
            ("seed", args.seed),
            ("seed_file", args.seed_file),
            ("count", args.count),
            ("repeats", args.repeats),
            ("seeds_not_all_running", args.seeds_not_all_running),

            ("job_file", args.job_file),

            ("save_solver_stats", args.save_solver_stats),
            ("save_scheduler_logs", args.save_scheduler_logs),
            
            ("solver_trigger", args.solver_trigger),
        ]
        pad = max(len(k) for k, _ in fields)
        def _fmt(v):
            return "<unset>" if v in (None, "") else str(v)
        lines = [f"{k.rjust(pad)} = {_fmt(v)}" for k, v in fields]
        block = "\n".join(lines)
        header, footer = make_header_footer("ARGS")
        LOG.info("\n%s\n%s\n%s", header, block, footer)

    @staticmethod
    def log_kwokctl_envs(envs: dict[str, object]) -> None:
        header, footer = make_header_footer("KWOKCTL ENVs")
        pad = max(len(k) for k in envs.keys())
        def _fmt(v: object) -> str:
            if v in (None, ""):
                return "<unset>"
            if isinstance(v, (dict, list)):
                try:
                    return json.dumps(v, separators=(",", ":"), sort_keys=True)
                except Exception:
                    return str(v)
            return str(v)
        lines = [f"{k.rjust(pad)} = {_fmt(v)}" for k, v in envs.items()]
        LOG.info("\n%s\n%s\n%s", header, "\n".join(lines), footer)

    def _record_failure(self, category: str, seed: int, phase: str, message: str, details: str = "") -> None:
        """
        Append a structured row to the failed-file so silent failures are visible.
        Format: ts  category  seed  phase  message  details
        """
        if self.suppress_fail_log:
            self.failure = _Failure(category, seed, phase, message, details)
            return
        ts = time.strftime("%Y/%m/%d/%H:%M:%S", time.localtime())
        line = f"{ts}\t{category}\t{seed}\t{phase}\t{message}"
        if details:
            compact = details.replace("\n", "\\n")[-1500:]
            line += f"\t{compact}"
        with open(self.failed_f, "a", encoding="utf-8") as f:
            f.write(line + "\n")

    def _log_seed_run(self, seed: int | None, seed_idx: int, seeds_total: int) -> None:
        """
        Log seed run.
        """
        seed_str = "unlimited" if seeds_total <= -1 else f"{seed_idx}/{seeds_total}"
        header, footer = make_header_footer(f"SEED RUN {seed_str}")
        combined_str = self.combined_job_config_seed_str()
        LOG.info("\n%s\nseed=%s\n%s\n%s", header, str(seed), combined_str, footer)

    def _log_seed_summary(self, seed: int, note: str = "") -> None:
        """
        Log a summary for the given seed.
        """
        note = f"note='{note}'" if note else ""
        header, footer = make_header_footer(f"SEED SUMMARY")
        combined_str = self.combined_job_config_seed_str()
        LOG.info("\n%s\nseed=%s\n%s\n%s\n%s", header, str(seed), combined_str, note, footer)

    ######################################################
    # ---------- ETA helpers ----------
    ######################################################
    def _eta_record_seed_duration(self, started_at: float):
        """Called after a seed finishes (success or fail)."""
        self.seed_durations.append(max(0.0, time.time() - started_at))

    def _eta_estimation(self, seed_idx: int, seeds_total: int) -> float | None:
        """
        Return an epoch seconds ETA, or None if not enough info.
        """
        if not self.seed_durations:
            return None
        avg = sum(self.seed_durations) / max(1, len(self.seed_durations))
        if seeds_total is None or seeds_total <= 0: # unknown/infinite
            return None
        seeds_done = max(0, seed_idx - 1)
        seeds_left = max(0, seeds_total - seeds_done)
        return time.time() + seeds_left * avg

    def _eta_summary(self, next_seed_idx: int, seeds_total: int) -> None:
        """
        Log an ETA summary block.
        """
        eta_epoch = self._eta_estimation(next_seed_idx, seeds_total)
        header, footer = make_header_footer("ETA", width=100, border="=")
        # Show "not-all-running" segment only if a limit is set
        if self.args.seeds_not_all_running > 0:
            left = max(0, self.args.seeds_not_all_running - self.saved_not_all_running)
            not_all_seg = f" | seeds-not-all-running-left={left}"
        else:
            not_all_seg = ""
        # Handle unlimited/unknown seed count
        if seeds_total is None or seeds_total <= 0:
            block = "ETA: unknown (seed count is unlimited)."
            LOG.info("\n%s\n%s\n%s", header, block, footer)
            return
        # Handle no samples yet
        if not self.seed_durations:
            block = "ETA: collecting first duration sample..." + not_all_seg
            LOG.info("\n%s\n%s\n%s", header, block, footer)
            return
        # Compute averages and counts
        avg = sum(self.seed_durations) / max(1, len(self.seed_durations))
        seeds_done = max(0, next_seed_idx - 1)
        seeds_left = max(0, seeds_total - seeds_done)
        # If not enough data to estimate ETA, show progress only
        if eta_epoch is None:
            block = (
                "ETA: not enough data yet (done %d/%d seeds)"
            ) % (seeds_done, seeds_total)
            block += not_all_seg + "."
            LOG.info("\n%s\n%s\n%s", header, block, footer)
            return
        # Create the ETA string and log
        now = time.time()
        left_s = max(0, int(round(eta_epoch - now)))
        eta_str = time.strftime("%Y-%m-%d %H:%M:%S", time.localtime(eta_epoch))
        block = ("ETA %s (in %s) | seeds left: %d%s | avg/seed=%.1fs (%d sample%s)") % (
            eta_str, format_hms(left_s), seeds_left, not_all_seg, avg, len(self.seed_durations), "" if len(self.seed_durations) == 1 else "s",
        )
        LOG.info("\n%s\n%s\n%s", header, block, footer)

    def _eta_write_file(self, eta_epoch: float | None, seed_idx: int, seeds_total: int):
        """
        Delete stale eta_* and write a new one:
        eta_<time>_configs<at-of-total>_seeds<at-of-total>
        """
        try:
            # delete any prior eta_* markers
            for p in self.output_dir_resolved.glob("eta_*"):
                try:
                    p.unlink()
                except OSError:
                    pass
            # what we're currently AT
            if seeds_total and seeds_total > 0:
                seeds_at = max(0, int(seed_idx))
                seeds_total = int(seeds_total)
            else: # unknown/infinite seed count: keep total as -1, show current index
                seeds_at = max(0, int(seed_idx))
                seeds_total = -1
            if eta_epoch is None:
                time_part = "eta-unknown"
            else:
                time_part = time.strftime("%Y%m%d-%H%M%S", time.localtime(eta_epoch))
            fname = (
                f"eta_{time_part}"
                f"_seeds-{seeds_at}-of-{seeds_total}"
            )
            fpath = self.output_dir_resolved / fname
            # write a tiny payload
            with open(fpath, "w", encoding="utf-8") as fh:
                payload = {
                    "eta_epoch": int(eta_epoch) if isinstance(eta_epoch, (int, float)) else None,
                    "eta_iso": (time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime(eta_epoch)) if eta_epoch is not None else None),
                    "seeds_at": seeds_at,   "seeds_total": seeds_total,
                }
                fh.write(json.dumps(payload, separators=(",", ":"), sort_keys=True) + "\n")
        except Exception:  # best-effort; don't break the run for ETA issues
            pass

    def _eta_update_marker(self, seed_idx: int, seeds_total: int):
        """
        Update the eta_* marker file.
        """
        eta_epoch = self._eta_estimation(seed_idx, seeds_total)
        self._eta_write_file(eta_epoch, seed_idx, seeds_total)

    ######################################################
    # ---------- Parsing helpers ----------
    ######################################################
    def _parse_waits(self, tr: TestConfigRaw) -> tuple[Optional[str], int, int, int]:
        """
        Parse and default the wait parameters from the raw config.
        Returns (wait_pod_mode, wait_pod_timeout_s, settle_timeout_min_s, settle_timeout_max_s).
        """
        wait_pod_mode = None if tr.wait_pod_mode in (None, "none", "None", "") else str(tr.wait_pod_mode)
        wait_pod_timeout_s = parse_timeout_s(tr.wait_pod_timeout, default=5)
        settle_timeout_min_s = parse_timeout_s(tr.settle_timeout_min, default=2)
        settle_timeout_max_s = 0 if tr.settle_timeout_max in (None, "", "none", "None") \
            else parse_timeout_s(tr.settle_timeout_max, default=12)
        return wait_pod_mode, wait_pod_timeout_s, settle_timeout_min_s, settle_timeout_max_s

    @staticmethod
    def _get_wait_pod_mode_from_dict(doc: Dict[str, Any], key: str, default: Optional[str]) -> Optional[str]:
        """
        Get the wait mode from the document, returning None for empty values.
        """
        v = doc.get(key, default)
        if v is None:
            return None
        s = str(v).strip()
        if s.lower() in ("", "none"):
            return None
        if s not in ("exist", "ready", "running"):
            raise ValueError(f"Invalid wait_pod_mode: {s}")
        return s

    ######################################################
    # ---------- Skipped all running seeds ---------------
    ######################################################
    def _record_skipped_all_running_seed(self, seed: int, running_count: int) -> None:
        try:
            header = ["timestamp", "seed", "running_count"]
            if not self.skipped_all_running_f.exists():
                with open(self.skipped_all_running_f, "w", encoding="utf-8", newline="") as fh:
                    w = csv.writer(fh)
                    w.writerow(header)
            with open(self.skipped_all_running_f, "a", encoding="utf-8", newline="") as fh:
                w = csv.writer(fh)
                w.writerow([get_timestamp(), str(seed), str(running_count)])
            LOG.info("all pods running; skipped seed added to %s: seed=%s", self.skipped_all_running_f.name, seed)
        except Exception as e:
            LOG.warning("failed to record skipped seed: %s", e)

    ######################################################
    # ---------- CSV helpers & results helpers -----------
    ######################################################
    def _prepare_output_dir(self) -> Path:
        rd = Path(self.args.output_dir).resolve()
        if self.args.clean_start:
            if rd.exists():
                shutil.rmtree(rd)
            rd.mkdir(parents=True, exist_ok=True)
            LOG.info("clean-start=true: recreated output dir at %s", rd)
            return rd
        # clean_start=false: allow existing non-empty dir so we can resume & skip/overwrite
        rd.mkdir(parents=True, exist_ok=True)

        return rd
    
    def _append_result_csv(self, row: dict) -> None:
        """
        Append a row to results.csv. If --re-run-seeds is set and there's an
        existing row(s) with the same seed, those rows are removed first.
        """
        self._purge_mismatched_results_csv(self.results_f, RESULTS_HEADER)
        ensure_csv_with_header(self.results_f, RESULTS_HEADER)
        seed_str = str(row.get("seed") or "").strip()
        if self.args.re_run_seeds and self.results_f.exists() and seed_str:
            try:
                # Read all rows, filter out same-seed rows, rewrite file
                with open(self.results_f, "r", encoding="utf-8", newline="") as fh:
                    rows = list(csv.DictReader(fh))
                rows = [r for r in rows if (r.get("seed") or "").strip() != seed_str]
                with open(self.results_f, "w", encoding="utf-8", newline="") as fh:
                    w = csv.DictWriter(fh, fieldnames=RESULTS_HEADER)
                    w.writeheader()
                    for r in rows:
                        w.writerow(r)
                LOG.info("re-run-seeds=true: removed prior rows for seed=%s from %s", seed_str, self.results_f.name)
            except Exception as e:
                LOG.warning("re-run-seeds prune failed for seed=%s: %s", seed_str, e)
        csv_append_row(self.results_f, RESULTS_HEADER, row)

    def _purge_mismatched_results_csv(self, path: Path, expected_header: list[str]) -> bool:
        """
        If file exists and header != expected_header and --clean-start is set,
        delete the file and return True. Otherwise, return False.
        """
        if not path.exists():
            return False
        actual = csv_read_header(path) or []
        exp_norm = [c.strip() for c in expected_header]
        if actual != exp_norm and self.args.clean_start:
            try:
                path.unlink()
                LOG.info("deleted CSV with mismatched header (clean-start=true): %s", path.name)
                return True
            except OSError as e:
                LOG.warning("failed to delete mismatched CSV %s: %s", path.name, e)
        return False

    def _load_seen_results(self) -> set[int]:
        seen: set[int] = set()
        
        if not self.results_f.exists():
            return seen
        try:
            with open(self.results_f, "r", encoding="utf-8", newline="") as fh:
                for r in csv.DictReader(fh):
                    s = (r.get("seed") or "").strip()
                    if s:
                        seen.add(int(s))
        except Exception:
            pass
        return seen

    @staticmethod
    def _extract_best_attempt_fields(best_name: str, attempts: list[dict]) -> tuple[float | None, int | None, str]:
        """
        Prefer the attempt with name == best_name; otherwise use the one with the
        highest numeric 'score'. Return (best_score, best_duration_ms, best_status).
        """
        # check for empty input
        if not isinstance(attempts, list) or not attempts:
            return None, None, ""
        # get best attempt
        best_attempt = None
        if best_name:
            for a in attempts:
                if str(a.get("name")) == best_name:
                    best_attempt = a
                    break
        # if no best_name exit
        if best_attempt is None:
            return None, None, ""
        # parse fields
        best_score = None
        try:
            v = best_attempt.get("score")
            best_score = json.dumps(v, separators=(",", ":"), sort_keys=True)
        except Exception:
            pass
        best_duration_ms = None
        try:
            v = best_attempt.get("duration_ms")
            if v is not None:
                best_duration_ms = int(v)
        except Exception:
            pass
        best_status = str(best_attempt.get("status") or "")
        return best_score, best_duration_ms, best_status

    def _get_solver_attempts(self) -> tuple[dict, str, list[dict], str]:
        """
        Return (baseline, best_name, attempts, error).
        Priority:
        (1) fresh /optimize JSON if available
        (2) latest runs.json from ConfigMap
        """
        attempts: list[dict] = []
        best_name: str = ""
        error: str = ""
        baseline: dict = {}
        # (1) From the last /optimize payload
        if self.last_solver_result and isinstance(self.last_solver_result, dict):
            try:
                error = str(self.last_solver_result.get("error", "") or "")
                best_name = str(self.last_solver_result.get("best_name", "") or "")
                raw_attempts = self.last_solver_result.get("attempts") or []
                if isinstance(raw_attempts, list):
                    attempts = raw_attempts
                bl = self.last_solver_result.get("baseline") or {}
                if isinstance(bl, dict):
                    baseline = bl
            except Exception:
                pass

        if attempts and best_name:
            return baseline, best_name, attempts, error

        # (2) Fallback to ConfigMap runs.json
        cm = self._get_latest_configmap(
            self.ctx, CM_SOLVER_STATS_NAMESPACE, CM_SOLVER_STATS_NAME,
            accept_prefix=True, label_selector=None,
        )
        if cm is None:
            return baseline, best_name, attempts, error

        data = cm.get("data") or {}
        runs_raw = data.get("runs.json", "[]")
        try:
            runs = json.loads(runs_raw) or []
        except Exception:
            runs = []

        if not isinstance(runs, list) or not runs:
            return baseline, best_name, attempts, error

        last = runs[-1] or {}
        try:
            error = str(last.get("error", "") or error)
            best_name = str(last.get("best_name", "") or best_name)
            raw_attempts = last.get("attempts") or []
            if isinstance(raw_attempts, list):
                attempts = raw_attempts
            bl = last.get("baseline") or {}
            if isinstance(bl, dict):
                baseline = bl
        except Exception:
            pass

        return baseline, best_name, attempts, error

    def _write_solver_stats_json(self, seed: int, run_idx: int = 1) -> None:
        """
        Fetch runs.json from the latest solver-stats ConfigMap and dump it verbatim.
        No CSV. No splitting. Exactly what the ConfigMap has, written to a .json file.
        """
        # Find the latest matching ConfigMap
        cm_obj = self._get_latest_configmap(
            self.ctx, CM_SOLVER_STATS_NAMESPACE, CM_SOLVER_STATS_NAME,
            accept_prefix=True, label_selector=None,
        )
        if cm_obj is None:
            LOG.warning("no config map matching %r found in ns=%r; skipping solver-stats dump",
                        CM_SOLVER_STATS_NAME, CM_SOLVER_STATS_NAMESPACE)
            return
        data = cm_obj.get("data") or {}
        runs_raw = data.get("runs.json")
        if runs_raw is None:
            LOG.warning("ConfigMap %r missing 'runs.json'; skipping solver-stats dump", CM_SOLVER_STATS_NAME)
            return
        self.solver_stats_dir.mkdir(parents=True, exist_ok=True)
        out_path = self.solver_stats_dir / f"solver_stats_seed-{seed}_run-{run_idx}.json"
        try:
            # Always clean-start the per-run file
            with open(out_path, "w", encoding="utf-8") as fh:
                fh.write(runs_raw)
            LOG.info("saved solver-stats JSON to %s", out_path)
        except Exception as e:
            LOG.warning("failed writing %s: %s", out_path, e)
    
    @staticmethod
    def _get_latest_configmap(
        ctx: str,
        ns: str,
        base_name: str,
        *,
        label_selector: Optional[str] = None,
        accept_prefix: bool = True,
        retries: int = 10,
        sleep_seconds: float = 0.5,
    ) -> Optional[Dict[str, Any]]:
        """
        Get the latest ConfigMap in the given namespace matching base_name or label_selector.
        If no match is found (or kubectl fails), retry up to `retries` times with a pause in between.
        """
        def _run_once() -> Optional[Dict[str, Any]]:
            args = ["kubectl", "--context", ctx, "-n", ns, "get", "cm"]
            if label_selector:
                args += ["-l", label_selector]
            args += ["-o", "json"]

            r = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
            if r.returncode != 0:
                return None

            try:
                items = (json.loads(r.stdout.decode("utf-8", errors="replace")) or {}).get("items", [])
            except Exception:
                return None

            def _matches(item: Dict[str, Any]) -> bool:
                name = ((item.get("metadata") or {}).get("name") or "")
                if label_selector:
                    return True
                if name == base_name:
                    return True
                return accept_prefix and name.startswith(f"{base_name}-")

            cand = [it for it in items if _matches(it)]
            if not cand:
                return None

            def _key(item: Dict[str, Any]):
                md = item.get("metadata") or {}
                ts = md.get("creationTimestamp", "")
                rv = md.get("resourceVersion", "0")
                return (ts, rv)

            return max(cand, key=_key)

        # First attempt + retries
        attempts = retries + 1
        for i in range(attempts):
            result = _run_once()
            if result is not None:
                return result
            if i < attempts - 1:
                time.sleep(sleep_seconds)

        return None

    def _save_scheduler_logs(self, seed: int, run_idx: int = 1) -> None:
        """
        Save scheduler logs for the current KWOK cluster to output_dir/scheduler-logs.
        File: <output-dir>/scheduler-logs/scheduler-logs_seed-<seed>_run-<run_idx>.log
        """
        self.scheduler_logs_dir.mkdir(parents=True, exist_ok=True)
        out_path = self.scheduler_logs_dir / f"sched_logs_seed-{seed}_run-{run_idx}.log"
        if out_path.exists():
            try:
                out_path.unlink()
                LOG.info("pruned existing scheduler log (collision on run_idx): %s", out_path.name)
            except OSError as e:
                LOG.warning("failed pruning existing scheduler log %s: %s", out_path.name, e)
        try:
            # We capture the raw output for a clean log file (no prefixed 'kwokctl>' lines)
            r = subprocess.run(
                ["kwokctl", "logs", "kube-scheduler", "--name", self.args.cluster_name],
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                check=False,
            )
            data = r.stdout or b""
            with open(out_path, "wb") as fh:
                fh.write(data)
            LOG.info("saved scheduler logs to %s (rc=%d, %d bytes)", out_path, r.returncode, len(data))
        except Exception as e:
            LOG.warning("failed saving scheduler logs: %s", e)

    @staticmethod
    def _build_pod_node_list(
        running_by_name: Dict[str, str],
        unschedulable_names: List[str],
        standalone_specs: List[Dict[str, object]] | None,
        rs_specs: List[Dict[str, object]] | None,
    ) -> List[Dict[str, object]]:
        """
        Build a combined list of all pods (standalone and from RS) with their specs and node assignment.
        Each entry: {name, cpu_m, mem_b, priority, node}
        """
        by_standalone = {p["name"]: p for p in (standalone_specs or [])}
        by_rs = {r["name"]: r for r in (rs_specs or [])}
        all_names = set(running_by_name.keys()) | set(unschedulable_names)

        out: List[Dict[str, object]] = []
        for pname in sorted(all_names):
            node = running_by_name.get(pname, "")
            if pname in by_standalone:
                spec = by_standalone[pname]
                out.append({
                    "name": pname,
                    "cpu_m": int(spec["cpu_m"]),
                    "mem_b": int(spec["mem_b"]),
                    "priority": str(spec["priority"]),
                    "node": node,
                })
            else:
                # Infer RS name: pick the longest RS name that is a prefix of the pod name
                # followed by a hyphen (handles names that themselves contain hyphens).
                rsname = ""
                if by_rs:
                    for candidate in sorted(by_rs.keys(), key=len, reverse=True):
                        if pname.startswith(candidate + "-"):
                            rsname = candidate
                            break
                r = by_rs.get(rsname, {})
                out.append({
                    "name": pname,
                    "cpu_m": int(r.get("cpu_m", 0)),
                    "mem_b": int(r.get("mem_b", 0)),
                    "priority": str(r.get("priority", "")),
                    "node": node,
                })
        return out

    ##############################################
    # ------------ KWOK / kubectl helpers --------
    ##############################################
    @staticmethod
    def _run_kubectl_logged(ctx: str, *args: str, input_bytes: bytes | None = None, check: bool = True) -> subprocess.CompletedProcess:
        """
        Run `kubectl --context <ctx> <args...>` and stream its combined output into LOG.
        Returns a CompletedProcess-like object. Raises CalledProcessError if check=True and rc!=0.
        """
        cmd = ["kubectl", "--context", ctx, *args]
        LOG.debug("exec: %s", " ".join(shlex.quote(c) for c in cmd))
        r = subprocess.run(cmd, input=input_bytes, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
        if r.stdout:
            for line in r.stdout.decode(errors="replace").splitlines():
                LOG.info("kubectl> %s", line)
        if check and r.returncode != 0:
            raise subprocess.CalledProcessError(r.returncode, cmd, r.stdout)
        return r

    @staticmethod
    def _run_kwokctl_logged(*args: str, input_bytes: bytes | None = None, check: bool = True) -> subprocess.CompletedProcess:
        """
        Run `kwokctl <args...>` and stream its combined output into LOG.
        """
        cmd = ["kwokctl", *args]
        LOG.debug("exec: %s", " ".join(shlex.quote(c) for c in cmd))
        r = subprocess.run(cmd, input=input_bytes, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
        if r.stdout:
            for line in r.stdout.decode(errors="replace").splitlines():
                LOG.info("kwokctl> %s", line)
        if check and r.returncode != 0:
            raise subprocess.CalledProcessError(r.returncode, cmd, r.stdout)
        return r

    @staticmethod
    def _apply_yaml(ctx:str, yaml_text:str) -> subprocess.CompletedProcess:
        """
        Apply a YAML configuration to the cluster, logging kubectl output with worker prefix.
        """
        return KwokTestGenerator._run_kubectl_logged(ctx, "apply", "-f", "-", input_bytes=yaml_text.encode(), check=True)

    @staticmethod
    def _ensure_kwok_cluster(cluster_name: str, kwok_runtime: str, config_doc: dict | None = None, recreate: bool = True) -> None:
        if recreate:
            LOG.info(f"recreating kwok cluster '{cluster_name}'")
            with kwok_cache_lock():
                KwokTestGenerator._run_kwokctl_logged("delete", "cluster", "--name", cluster_name, check=False)
        tf = tempfile.NamedTemporaryFile("w", delete=False, suffix=".yaml")
        try:
            yaml.safe_dump(config_doc, tf, sort_keys=False)
        finally:
            tf.close()
        cfg_for_kwokctl = Path(tf.name)
        try:
            with kwok_cache_lock():
                KwokTestGenerator._run_kwokctl_logged(
                    "create", "cluster", "--name", cluster_name, "--config", str(cfg_for_kwokctl), "--runtime", kwok_runtime
                )
        finally:
            if cfg_for_kwokctl.exists():
                try: os.unlink(cfg_for_kwokctl)
                except OSError: pass

    @staticmethod
    def _create_kwok_nodes(ctx: str, num_nodes: int, node_cpu: str, node_mem: str, pods_cap: int) -> None:
        """
        Create KWOK nodes kwok-node-1..kwok-node-N with the given capacity.
        """
        KwokTestGenerator._apply_yaml(ctx, "".join(yaml_kwok_node(f"kwok-node-{i}", node_cpu, node_mem, pods_cap) for i in range(1, num_nodes + 1)))

    @staticmethod
    def _ensure_namespace(ctx: str, ns: str) -> None:
        """
        Ensure the namespace exists in the given context.
        If recreate=True, delete it first, then (re)create if missing.
        """
        rns = subprocess.run(["kubectl", "--context", ctx, "get", "ns", ns], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if rns.returncode != 0:
            KwokTestGenerator._run_kubectl_logged(ctx, "create", "ns", ns, check=True)

    @staticmethod
    def _ensure_service_account(ctx: str, ns: str, sa: str, retries: int = 20, delay: float = 0.5) -> None:
        """
        Ensure the ServiceAccount exists in the given namespace.
        If recreate=True, delete it first, then (re)create if missing.
        """
        for _ in range(1, retries + 1):
            r = subprocess.run(["kubectl", "--context", ctx, "-n", ns, "get", "sa", sa], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if r.returncode == 0:
                break
            time.sleep(delay)

    @staticmethod
    def _ensure_priority_classes(ctx: str, num_priorities: int, *, prefix: str = "p", start: int = 1) -> None:
        """
        Ensure PriorityClasses {prefix}{i} for i in [start, start+num_priorities).
        If delete_extras=True, remove any of our prefixed PCs outside that set.
        """
        KwokTestGenerator._apply_yaml(ctx, "".join(yaml_priority_class(f"{prefix}{v}", v) for v in range(start, start + num_priorities)))

    @staticmethod
    def _wait_each(ctx: str, kind: str, name: str, ns: str, timeout_sec: int, mode: str) -> int:
        """
        Wait for a specific resource to reach a desired state.
        """
        if kind == "pod":
            return KwokTestGenerator._wait_pod(ctx, name, ns, timeout_sec, mode)
        elif kind in ("replicaset", "rs"):
            return KwokTestGenerator._wait_rs_pods(ctx, name, ns, timeout_sec, mode)
        else:
            raise Exception(f"unknown kind for wait_each: {kind}")

    @staticmethod
    def _wait_pod(ctx: str, name: str, ns: str, timeout_sec: int, mode: str = "ready") -> int:
        """
        Wait until the pod meets the condition.
        mode:
        - "exist": counts pods by presence (len of pod list)
        - "ready": counts Ready=True
        - "running": counts phase == Running
        Returns 1 if the pod met the condition within timeout, else 0.
        """
        deadline = time.time() + timeout_sec
        while time.time() < deadline:
            r = subprocess.run(["kubectl", "--context", ctx, "-n", ns, "get", "pod", name, "-o", "json"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            if mode == "exist":
                if r.returncode == 0:
                    return 1
                time.sleep(0.5); continue
            if r.returncode != 0:
                time.sleep(0.5); continue
            try:
                pod = json.loads(r.stdout)
                phase = (pod.get("status") or {}).get("phase") or ""
                conditions = pod.get("status", {}).get("conditions", []) or []
                ready = any(c.get("type")=="Ready" and c.get("status")=="True" for c in conditions)
                if mode == "running" and phase == "Running":
                    return 1
                if mode == "ready" and ready:
                    return 1
            except Exception:
                pass
            LOG.info(f"waiting on pod to be {mode}")
            time.sleep(0.5)
        LOG.warning(f"timeout waiting for pod '{name}' in ns '{ns}' to be {mode}")
        return 0

    @staticmethod
    def _wait_rs_pods(ctx: str, rs_name: str, ns: str, timeout_sec: int, mode: str = "ready") -> int:
        """
        Wait until the RS has its desired number of pods meeting the condition.
        mode:
        - "exist": counts pods by presence (len of pod list)
        - "ready": counts Ready=True
        - "running": counts phase == Running
        Returns the number of pods that met the condition (possibly less than desired on timeout).
        """
        deadline = time.time() + timeout_sec
        last_count = 0
        while time.time() < deadline:
            desired = KwokTestGenerator._get_rs_spec_replicas(ctx, ns, rs_name)
            r = subprocess.run(["kubectl", "--context", ctx, "-n", ns, "get", "pods", "-l", f"app={rs_name}", "-o", "json"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            if r.returncode != 0:
                time.sleep(0.5); continue
            try:
                podlist = json.loads(r.stdout).get("items", [])
                if mode == "exist":
                    count = len(podlist)
                else:
                    count = 0
                    for p in podlist:
                        phase = (p.get("status") or {}).get("phase", "")
                        conditions = p.get("status", {}).get("conditions", []) or []
                        ready = any(c.get("type") == "Ready" and c.get("status") == "True" for c in conditions)
                        if mode == "running" and phase == "Running":
                            count += 1
                        elif mode == "ready" and ready:
                            count += 1
                last_count = count
                if desired is not None and count >= desired:
                    return count
            except Exception:
                pass
            LOG.info("waiting on rs=%s to be %s; desired=%s current=%s", rs_name, mode, desired, last_count)
            time.sleep(0.5)
        LOG.warning(f"timeout waiting for RS '{rs_name}' in ns '{ns}' to have desired pods {mode}")
        return last_count

    ##############################################
    # ------------ Workload helpers ------------
    ##############################################
    def _make_standalone_pod_specs_only(self, rng, ta) -> list[dict]:
        """
        Build standalone pod specs (name/req_cpu_m/req_mem_bytes/priority) without applying to a cluster.
        Naming mirrors _apply_standalone_pods: pod-<idx>-p<prio>.
        Uses the already resolved parts from TestConfigApplied.
        """
        specs: list[dict] = []
        n = len(ta.pod_parts_cpu_m)
        for i in range(n):
            prio = rng.randint(1, max(1, ta.num_priorities))
            name = f"pod-{i+1:03d}-p{prio}"
            specs.append({
                "name": name,
                "priority": int(prio),
                "req_cpu_m": int(ta.pod_parts_cpu_m[i]),
                "req_mem_bytes": int(ta.pod_parts_mem_b[i]),
            })
        return specs

    def _make_replicaset_specs_only(self, rng, ta) -> list[dict]:
        """
        Build ReplicaSet specs (name/req_cpu_m/req_mem_bytes/priority/replicas) without applying.
        Naming mirrors _apply_replicasets: rs-<idx>-p<prio>.
        Uses rs_sets + rs_parts_* from TestConfigApplied.
        """
        rs_specs: list[dict] = []
        if not getattr(ta, "rs_sets", None):
            return rs_specs
        for idx, replicas in enumerate(ta.rs_sets, start=1):
            prio = rng.randint(1, max(1, ta.num_priorities))
            name = f"rs-{idx:02d}-p{prio}"
            rs_specs.append({
                "name": name,
                "priority": int(prio),
                "req_cpu_m": int(ta.rs_parts_cpu_m[idx-1]),
                "req_mem_bytes": int(ta.rs_parts_mem_b[idx-1]),
                "replicas": int(replicas),
            })
        return rs_specs

    def _apply_standalone_pods(self, ta: TestConfigApplied, specs: list[dict]) -> None:
        """
        Create standalone pods in the given namespace with the specified specs.
        Returns a list of pod specs created. In direct mode, just returns specs (no cluster work).
        """
        names: List[str] = []
        for s in specs:
            pc = f"p{int(s['priority'])}"
            cpu_m_str = qty_to_mcpu_str(int(s["req_cpu_m"]))
            mem_b_str = qty_to_bytes_str(int(s["req_mem_bytes"]))
            self._apply_yaml(self.ctx, yaml_kwok_pod(ta.namespace, s["name"], cpu_m_str, mem_b_str, pc))
            names.append(s["name"])
        # Wait (as before)
        if ta.wait_pod_mode == "running":
            for name in names:
                _ = self._wait_each(self.ctx, "pod", name, ta.namespace, ta.wait_pod_timeout_s, ta.wait_pod_mode)
        elif ta.wait_pod_mode in ("exist", "ready"):
            for name in names:
                _ = self._wait_each(self.ctx, "pod", name, ta.namespace, ta.wait_pod_timeout_s, ta.wait_pod_mode)
        LOG.info("created %d standalone pods", len(specs))

    def _apply_replicasets(self, ta: TestConfigApplied, specs: list[dict]) -> None:
        """
        Create ReplicaSets in the given namespace with the specified specs.
        Returns a list of replicaset specs created. In direct mode, just returns specs (no cluster work).
        """
        for s in specs:
            pc = f"p{int(s['priority'])}"
            cpu_m_str = qty_to_mcpu_str(int(s["req_cpu_m"]))
            mem_b_str = qty_to_bytes_str(int(s["req_mem_bytes"]))
            self._apply_yaml(self.ctx, yaml_kwok_rs(ta.namespace, s["name"], int(s["replicas"]), cpu_m_str, mem_b_str, pc))
            _ = self._wait_rs_pods(self.ctx, s["name"], ta.namespace, ta.wait_pod_timeout_s, ta.wait_pod_mode)
        LOG.info("created %d ReplicaSets with total %d (=num_pods)",
                len(specs), sum(int(s['replicas']) for s in specs))

    @staticmethod
    def _gen_rs_sizes(rng: random.Random, num_pods: int, replicas_per_set: tuple[int, int]) -> list[int]:
        """
        Sequentially pick RS sizes. Each iteration = one ReplicaSet.
        Sizes are drawn uniformly in [lo, hi], capped at the remaining pods.
        The last RS may be < lo (intentional drift) to exactly hit num_pods.
        """
        lo, hi = int(replicas_per_set[0]), int(replicas_per_set[1])
        rs_sizes: list[int] = []
        remaining = num_pods
        while remaining > 0:
            # Bound the draw to what's left
            hi_now = min(hi, remaining)
            lo_now = 1 if remaining <= hi else min(lo, hi_now)
            # Draw and append
            rs_size = rng.randint(lo_now, hi_now)
            rs_sizes.append(rs_size)
            remaining -= rs_size
        return rs_sizes

    # TODO: delete if not needed
    @staticmethod
    def _scale_replicaset(ctx: str, ns: str, rs_name: str, replicas: int) -> None:
        """
        Scale a ReplicaSet to a desired replica count.
        Used to deterministically create pods one-by-one.
        """
        KwokTestGenerator._run_kubectl_logged(ctx, "-n", ns, "scale", "replicaset", rs_name, f"--replicas={replicas}", check=True)

    @staticmethod
    def _get_rs_spec_replicas(ctx: str, ns: str, rs_name: str) -> Optional[int]:
        """
        Fetch .spec.replicas for a ReplicaSet, or None if not found.
        """
        r = subprocess.run(["kubectl", "--context", ctx, "-n", ns, "get", "replicaset", rs_name, "-o", "json"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r.returncode != 0:
            return None
        try:
            return int((json.loads(r.stdout).get("spec") or {}).get("replicas") or 0)
        except Exception:
            return None

    ######################################################
    # ---------- Solver helpers ------------------
    ######################################################
    def _solver_directly(self, ta, seed, pod_specs, rs_specs) -> tuple[dict, dict]:
        def preplace_running_pods(pods, nodes, run_util: float, seed: int) -> None:
            """
            Deterministic, simple pre-placement:
            - Greedy largest-first; stable shuffle for strict ties using the same seed
            - Pack into nodes 0..N-1
            - Never exceed per-node CPU/MEM targets (cap * run_util)
            - Returns a metrics dict with per-node and total running utilization.
            """
            # Per-node target budgets (always build so we can return zeros cleanly)
            targets = []
            for nd in nodes:
                cpu_cap = int(nd["cap_cpu_m"])
                mem_cap = int(nd["cap_mem_bytes"])
                targets.append({
                    "name": nd["name"],
                    "cpu_cap": cpu_cap,
                    "mem_cap": mem_cap,
                    "cpu_target": int(cpu_cap * max(0.0, run_util)),
                    "mem_target": int(mem_cap * max(0.0, run_util)),
                    "cpu_used": 0,
                    "mem_used": 0,
                })

            if run_util > 0.0:
                # Largest-first by (cpu, mem); tie-break with SAME seed
                rng = seeded_random(seed, "preplace-simple")
                idx = list(range(len(pods)))
                idx.sort(key=lambda i: (pods[i]["req_cpu_m"], pods[i]["req_mem_bytes"]), reverse=True)

                # Break strict ties deterministically
                i = 0
                while i < len(idx):
                    j = i + 1
                    ci = pods[idx[i]]["req_cpu_m"]; mi = pods[idx[i]]["req_mem_bytes"]
                    while j < len(idx):
                        cj = pods[idx[j]]["req_cpu_m"]; mj = pods[idx[j]]["req_mem_bytes"]
                        if (ci, mi) != (cj, mj):
                            break
                        j += 1
                    if j - i > 1:
                        group = idx[i:j]
                        rng.shuffle(group)
                        idx[i:j] = group
                    i = j

                def can_fit(t, p):
                    return ((t["cpu_used"] + p["req_cpu_m"] <= t["cpu_target"]) and
                            (t["mem_used"] + p["req_mem_bytes"] <= t["mem_target"]))

                def all_met():
                    for t in targets:
                        if t["cpu_used"] < t["cpu_target"] or t["mem_used"] < t["mem_target"]:
                            return False
                    return True

                # Greedy pack
                for k in idx:
                    p = pods[k]
                    if p.get("node"):
                        continue  # respect prior assignment if any
                    placed = False
                    for t in targets:
                        if can_fit(t, p):
                            p["node"] = t["name"]  # mark as already running
                            t["cpu_used"] += p["req_cpu_m"]
                            t["mem_used"] += p["req_mem_bytes"]
                            placed = True
                            break
                    if placed and all_met():
                        break

            def safe_div(a, b):
                return (a / b) if b and b > 0 else 0.0

            per_node = []
            tot_cpu_used = tot_mem_used = 0
            tot_cpu_cap = tot_mem_cap = 0
            tot_cpu_target = tot_mem_target = 0

            for t in targets:
                tot_cpu_used   += t["cpu_used"]
                tot_mem_used   += t["mem_used"]
                tot_cpu_cap    += t["cpu_cap"]
                tot_mem_cap    += t["mem_cap"]
                tot_cpu_target += t["cpu_target"]
                tot_mem_target += t["mem_target"]

                per_node.append({
                    "name": t["name"],
                    "cpu_used": t["cpu_used"],
                    "mem_used": t["mem_used"],
                    "cpu_cap": t["cpu_cap"],
                    "mem_cap": t["mem_cap"],
                    "cpu_target": t["cpu_target"],
                    "mem_target": t["mem_target"],
                    # actual running util vs capacity
                    "cpu_util": safe_div(t["cpu_used"], t["cpu_cap"]),
                    "mem_util": safe_div(t["mem_used"], t["mem_cap"]),
                    # progress vs target (may exceed 1.0 only if targets==0 and used>0; capped)
                    "cpu_util_vs_target": min(1.0, safe_div(t["cpu_used"], t["cpu_target"])),
                    "mem_util_vs_target": min(1.0, safe_div(t["mem_used"], t["mem_target"])),
                })
        
        nodes = [{
            "name": f"node-{j}",
            "cap_cpu_m": int(ta.node_cpu_m),
            "cap_mem_bytes": int(ta.node_mem_b),
        } for j in range(ta.num_nodes)]
        pods = []
        uid_to_priority: dict[str,int] = {}
        uid_counter = 0
        def emit(name, prio, cpu_m, mem_b):
            nonlocal uid_counter
            uid_counter += 1
            uid = f"pod-{seed}-{uid_counter}"
            pods.append({
                "uid": uid,
                "namespace": ta.namespace,
                "name": name,
                "req_cpu_m": int(cpu_m),
                "req_mem_bytes": int(mem_b),
                "priority": int(prio),
                "protected": False,
                "node": "",
            })
            uid_to_priority[uid] = int(prio)

        for s in pod_specs:
            emit(s["name"], s["priority"], s["req_cpu_m"], s["req_mem_bytes"])

        for rs in rs_specs:
            for r in range(int(rs["replicas"])):
                emit(f'{rs["name"]}-{r}', rs["priority"], rs["req_cpu_m"], rs["req_mem_bytes"])

        # Pre-place some pods as already running
        run_util = self.args.solver_directly_running_target_util
        preplace_running_pods(pods, nodes, run_util, seed)
        LOG.info("pre-placed pods with target_util=%.3f (seed=%d)", run_util, seed)

        instance = {
            "timeout_ms": int(self.args.solver_timeout_ms),
            "ignore_affinity": True,
            "log_progress": True, # can slow down the solver quite a bit
            "preemptor": None,
            "nodes": nodes,
            "pods": pods,
        }

        # Export input if requested
        if self.args.solver_input_export:
            Path(self.args.solver_input_export).parent.mkdir(parents=True, exist_ok=True)
            with open(self.args.solver_input_export, "w", encoding="utf-8") as f:
                json.dump(instance, f, indent=2)

        LOG.info("solving directly with %d nodes and %d pods (seed=%d)", len(nodes), len(pods), seed)
        cmd = shlex.split(SOLVER_CMD)
        t0 = time.time()
        completed = subprocess.run(
            cmd,
            input=json.dumps(instance).encode("utf-8"),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        out_b = completed.stdout or b""
        err_b = completed.stderr or b""
        
        elapsed_ms = int((time.time() - t0) * 1000)

        out = out_b.decode("utf-8", errors="replace").strip()
        err = err_b.decode("utf-8", errors="replace").strip()

        # Parse JSON from stdout
        try:
            resp = json.loads(out) if out else {}
        except Exception:
            resp = {"status": "PY_SOLVER_STDOUT_PARSE_ERROR", "raw": out}

        # Export output if requested
        if self.args.solver_output_export:
            Path(self.args.solver_output_export).parent.mkdir(parents=True, exist_ok=True)
            with open(self.args.solver_output_export, "w", encoding="utf-8") as f:
                json.dump(resp, f, indent=2)

        LOG.info(
            "direct-solver: status=%s placements=%d duration_ms=%d",
            resp.get("status"),
            len(resp.get("placements", []) or []),
            elapsed_ms,
        )

        initial_running_uids = {p["uid"] for p in pods if (p.get("node") or "").strip()}
        uid_to_priority = {}
        uid_to_cpu = {}
        uid_to_mem = {}
        for p in pods:
            uid_to_priority[p["uid"]] = int(p.get("priority", 0))
            uid_to_cpu[p["uid"]] = int(p["req_cpu_m"])
            uid_to_mem[p["uid"]] = int(p["req_mem_bytes"])

        total_node_cpu = sum(int(n["cap_cpu_m"]) for n in nodes)
        total_node_mem = sum(int(n["cap_mem_bytes"]) for n in nodes)

        meta = {
            "uid_to_priority": uid_to_priority,
            "uid_to_cpu": uid_to_cpu,
            "uid_to_mem": uid_to_mem,
            "initial_running_uids": list(initial_running_uids),
            "total_pods": len(pods),
            "total_node_cpu": total_node_cpu,
            "total_node_mem": total_node_mem,
            "elapsed_ms": elapsed_ms,
        }
        return resp, meta

    def _solver_trigger_http(self) -> tuple[int, str]:
        """
        POST /optimize endpoint. Returns (status_code, body_str).
        Timeout should be long enough to allow the solver to run.
        """
        data = b""
        headers = {
            "Accept": "application/json",
            "Content-Length": "0",
        }
        try:
            req = _urlreq.Request(SOLVER_TRIGGER_URL, data=data, headers=headers, method="POST")
            with _urlreq.urlopen(req, timeout=SOLVER_TRIGGER_TIMEOUT_S) as resp:
                body = resp.read().decode("utf-8", errors="replace")
                return getattr(resp, "status", 200), body
        except _urlerr.HTTPError as e:
            try:
                body = e.read().decode("utf-8", errors="replace")
            except Exception:
                body = str(e)
            return e.code, body
        except Exception as e:
            LOG.info(f"solver POST failed: {e}")
            return 0, f"connect-failed: {e}"

    def _wait_solver_inactive_http(self, url: str, timeout_s: int, *, poll_initial_s: float = 0.5, poll_interval_s: float = 1.5, resp_strip_length: int = 200) -> bool:
        """
        Poll the /active endpoint until it reports Active=false or until timeout.
        Returns True if became inactive; False if timed out or endpoint unreachable.
        poll_initial_s: initial poll interval in seconds (default 0.5s)
        poll_interval_s: maximum poll interval in seconds (default 1.5s)
        """
        def get_solver_active_status_http(url: str, *, timeout: float = 3.0) -> tuple[int, str]:
            """
            GET /active endpoint. Returns (status_code, body_str).
            """
            try:
                req = _urlreq.Request(url, method="GET", headers={"Accept": "application/json"})
                with _urlreq.urlopen(req, timeout=timeout) as resp:
                    body = resp.read().decode("utf-8", errors="replace")
                    return getattr(resp, "status", 200), body
            except _urlerr.HTTPError as e:
                try:
                    body = e.read().decode("utf-8", errors="replace")
                except Exception:
                    body = str(e)
                return e.code, body
            except Exception as e:
                return 0, f"connect-failed: {e}"
        
        deadline = time.time() + max(0, int(timeout_s))
        interval = float(poll_initial_s)
        time.sleep(interval)
        last_log = 0.0
        
        while time.time() < deadline:
            code, body = get_solver_active_status_http(url)
            inactive_now = None
            if code == 200 and body:
                try:
                    data = json.loads(body)
                    active = bool(data.get("Active", data.get("active", True)))
                    inactive_now = (active is False)
                except Exception:
                    inactive_now = None
            if inactive_now is True:
                LOG.info(f"solver is inactive; proceeding")
                return True
            now = time.time()
            if now - last_log > 2:
                LOG.info("waiting for solver to become inactive... code=%s body=%s", code, (body[:resp_strip_length] + ("..." if len(body) > resp_strip_length else "")) if isinstance(body, str) else body)
                last_log = now
            time.sleep(interval)
            interval = poll_interval_s
        
        LOG.warning("wait_for_inactive timed out after %ss; continuing", timeout_s)
        return False

    ##############################################
    # ------------ Seed helpers -----------------
    ##############################################
    @staticmethod
    def _read_seeds_file(path: Path) -> List[int]:
        """
        Read seeds from a txt file. Format:
        - One integer per line (whitespace allowed).
        - Empty lines and lines starting with '#' are ignored.
        - Non-integer lines are skipped (logged at DEBUG).
        Returns: de-duplicated, order-preserving list of positive ints.
        """
        seeds: List[int] = []
        seen: set[int] = set()
        def _add(n: int):
            if n is None or n <= 0:
                return
            if n not in seen:
                seen.add(n)
                seeds.append(n)
        try:
            with open(path, "r", encoding="utf-8") as f:
                for i, line in enumerate(f, 1):
                    s = line.strip()
                    if not s or s.startswith("#"):
                        continue
                    try:
                        _add(int(s))
                    except Exception:
                        LOG.debug("seed-file %s:%d ignored non-integer line: %r", path, i, s[:120])
                        continue
        except Exception as e:
            raise ValueError(f"Failed reading seed-file {path}: {e}")
        if not seeds:
            LOG.warning("no seeds parsed from %s", path)
        else:
            LOG.info("parsed %d seed(s) from %s", len(seeds), path)
        return seeds

    ##############################################
    # ------------ Config helpers ----------------
    ##############################################
    def _parse_config_doc(self, config_doc: dict, override: dict | None = None) -> TestConfigRaw:
        """
        Parse a single test-generator config document (already loaded from YAML)
        into TestConfigRaw. Optionally apply an in-memory override first.
        """
        if override:
            config_doc = self._merge_doc(dict(config_doc), dict(override))

        tr = TestConfigRaw()

        tr.namespace          = get_str_from_dict(config_doc, "namespace", tr.namespace)
        tr.num_nodes          = get_int_from_dict(config_doc, "num_nodes", tr.num_nodes)
        tr.num_pods           = get_int_from_dict(config_doc, "num_pods", tr.num_pods)

        tr.util               = get_float_from_dict(config_doc, "util", tr.util)

        tr.wait_pod_mode      = self._get_wait_pod_mode_from_dict(config_doc, "wait_pod_mode", tr.wait_pod_mode)
        tr.wait_pod_timeout   = get_str_from_dict(config_doc, "wait_pod_timeout", tr.wait_pod_timeout)
        tr.settle_timeout_min = get_str_from_dict(config_doc, "settle_timeout_min", tr.settle_timeout_min)
        tr.settle_timeout_max = get_str_from_dict(config_doc, "settle_timeout_max", tr.settle_timeout_max)

        raw_np = normalize_interval(config_doc, ("num_priorities","num_priorities_lo","num_priorities_hi"))
        if raw_np:
            tr.num_priorities = parse_int_interval(raw_np, min_lo=1)

        raw_cpu = normalize_interval(config_doc, ("cpu_per_pod","cpu_per_pod_lo","cpu_per_pod_hi"))
        if raw_cpu:
            tr.cpu_per_pod = parse_qty_interval(raw_cpu)

        raw_mem = normalize_interval(config_doc, ("mem_per_pod","mem_per_pod_lo","mem_per_pod_hi"))
        if raw_mem:
            tr.mem_per_pod = parse_qty_interval(raw_mem)

        raw_rps = normalize_interval(config_doc, ("num_replicas_per_rs","num_replicas_per_rs_lo","num_replicas_per_rs_hi"))
        if raw_rps:
            tr.num_replicas_per_rs = parse_int_interval(raw_rps, min_lo=1)

        return tr

    @staticmethod
    def _validate_workload_config(tr: TestConfigRaw) -> tuple[bool, str]:
        """
        Validate the raw config. Do NOT raise; return (ok, aggregated_message).
        All checks live here and we accumulate *all* failures.
        """
        errors: list[str] = []
        
        # Basic checks
        if not tr.namespace or not str(tr.namespace).strip():
            errors.append("namespace must be a non-empty string")
        if tr.num_nodes < 1:
            errors.append("num_nodes must be >= 1")
        if tr.num_pods < 1:
            errors.append("num_pods must be >= 1")

        # Utilization
        if tr.util <= 0.0:
            errors.append("util must be > 0")

        # Priority check
        if not isinstance(tr.num_priorities, tuple) or len(tr.num_priorities) != 2:
            errors.append("num_priorities must be provided (fixed number or [lo,hi])")
        else:
            try:
                lo_prio = int(tr.num_priorities[0])
                hi_prio = int(tr.num_priorities[1])
                if lo_prio < 2:
                    errors.append("num_priorities_lo must be >= 2")
                if hi_prio < lo_prio:
                    errors.append("num_priorities interval must satisfy lo <= hi")
            except Exception:
                errors.append("num_priorities interval must contain integers")

        # Replicaset check
        if tr.num_replicas_per_rs is not None:
            try:
                lo_rps = int(tr.num_replicas_per_rs[0])
                hi_rps = int(tr.num_replicas_per_rs[1])
                if lo_rps < 1 or hi_rps < lo_rps:
                    errors.append("num_replicas_per_rs must satisfy 1 <= lo <= hi")
            except Exception:
                errors.append("num_replicas_per_rs interval must contain integers")
        
        # Quantities
        if tr.cpu_per_pod is None or tr.mem_per_pod is None:
            errors.append("cpu_per_pod and mem_per_pod must be provided")
        if tr.cpu_per_pod is not None:
            try:
                c_lo = qty_to_mcpu_int(tr.cpu_per_pod[0])
                c_hi = qty_to_mcpu_int(tr.cpu_per_pod[1])
                if c_lo < 1 or c_hi < c_lo:
                    errors.append("cpu_per_pod must satisfy 1m <= lo <= hi")
            except Exception:
                errors.append(f"cpu_per_pod has invalid quantities: {tr.cpu_per_pod!r}")
        if tr.mem_per_pod is not None:
            try:
                m_lo = qty_to_bytes_int(tr.mem_per_pod[0])
                m_hi = qty_to_bytes_int(tr.mem_per_pod[1])
                if m_lo < 1 or m_hi < m_lo:
                    errors.append("mem_per_pod must satisfy 1B <= lo <= hi")
            except Exception:
                errors.append(f"mem_per_pod has invalid quantities: {tr.mem_per_pod!r}")

        ok = (len(errors) == 0)
        
        return ok, ("\n".join(errors) if errors else "")
    
    def _resolve_config_for_seed(self, seed_int: int) -> TestConfigApplied:
        """
        Resolve the raw config into a fully specified applied config for the given seed.
        """
        tr = self.workload_config
        
        wait_pod_mode, wait_pod_timeout_s, settle_timeout_min_s, settle_timeout_max_s = self._parse_waits(tr)

        # num_priorities
        num_prios_lo, num_prios_hi = tr.num_priorities
        num_prios = seeded_random(seed_int, "num_priorities", num_prios_lo, num_prios_hi).randint(num_prios_lo, num_prios_hi)

        # intervals -> ints
        cpu_m = (qty_to_mcpu_int(tr.cpu_per_pod[0]), qty_to_mcpu_int(tr.cpu_per_pod[1]))
        mem_b = (qty_to_bytes_int(tr.mem_per_pod[0]), qty_to_bytes_int(tr.mem_per_pod[1]))

        # Pod sizes
        rs_sets: list[int] = []
        rs_cpu = rs_mem = None
        if tr.num_replicas_per_rs is not None: # RS mode
            rng_rs_sizes = seeded_random(seed_int, "rs-sizes")
            rng_rs_cpu   = seeded_random(seed_int, "rs-cpu")
            rng_rs_mem   = seeded_random(seed_int, "rs-mem")
            rs_sets = self._gen_rs_sizes(rng_rs_sizes, tr.num_pods, tr.num_replicas_per_rs)
            rs_cpu = [rng_rs_cpu.randint(cpu_m[0], cpu_m[1]) for _ in rs_sets]
            rs_mem = [rng_rs_mem.randint(mem_b[0], mem_b[1]) for _ in rs_sets]
            cpu_parts = [v for i, v in enumerate(rs_cpu) for _ in range(rs_sets[i])]
            mem_parts = [v for i, v in enumerate(rs_mem) for _ in range(rs_sets[i])]
        else: # standalone
            rng_pod_cpu = seeded_random(seed_int, "pod-cpu")
            rng_pod_mem = seeded_random(seed_int, "pod-mem")
            cpu_parts = [rng_pod_cpu.randint(cpu_m[0], cpu_m[1]) for _ in range(tr.num_pods)]
            mem_parts = [rng_pod_mem.randint(mem_b[0], mem_b[1]) for _ in range(tr.num_pods)]

        # Node sizes based on pods above and target utilization
        sum_cpu, sum_mem = sum(cpu_parts), sum(mem_parts)
        max_cpu_m, max_mem_b = (max(cpu_parts) if cpu_parts else 1), (max(mem_parts) if mem_parts else 1)
        util = max(1e-9, float(tr.util)) # avoid division by zero
        total_cap_cpu = int(math.ceil(sum_cpu / util))
        total_cap_mem = int(math.ceil(sum_mem / util))
        per_node_m = max(max_cpu_m, int(math.ceil(total_cap_cpu / tr.num_nodes)))
        per_node_b = max(max_mem_b, int(math.ceil(total_cap_mem / tr.num_nodes)))

        # Set final used intervals based on what we actually drew
        cpu_m_used = (min(cpu_parts) if cpu_parts else cpu_m[0], max_cpu_m)
        mem_b_used = (min(mem_parts) if mem_parts else mem_b[0], max_mem_b)

        num_rs_sets = len(rs_sets)
        
        ta = TestConfigApplied(
            namespace=tr.namespace,
            num_nodes=tr.num_nodes,
            num_pods=tr.num_pods,
            num_priorities=num_prios,
            num_replicaset=num_rs_sets,
            num_replicas_per_rs=tr.num_replicas_per_rs,
            cpu_per_pod_m=cpu_m_used,
            mem_per_pod_b=mem_b_used,
            
            util=tr.util,
            
            wait_pod_mode=wait_pod_mode,
            wait_pod_timeout_s=wait_pod_timeout_s,
            
            settle_timeout_min_s=settle_timeout_min_s,
            settle_timeout_max_s=settle_timeout_max_s,
            
            node_cpu_m=per_node_m,
            node_mem_b=per_node_b,
            
            pod_parts_cpu_m=cpu_parts,
            pod_parts_mem_b=mem_parts,
        )
        
        # Only add RS fields if RS are used
        if num_rs_sets > 0:
            ta.rs_sets = rs_sets
            ta.rs_parts_cpu_m = rs_cpu
            ta.rs_parts_mem_b = rs_mem

        return ta

    ##############################################
    # ------------ Runner helpers ----------------
    ##############################################
    def _pause(self, *, next_exists: bool = True) -> None:
        """
        Pause for Enter if --pause is set and another item is coming.
        Skips if stdin is not a TTY (e.g., CI, redirection).
        """
        if not self.args.pause: return
        if not next_exists: return
        if not sys.stdin.isatty():
            LOG.info("[pause] requested but stdin is not a TTY; continuing.")
            return
        try:
            input("[pause] Press Enter to continue (Ctrl+C to abort)... ")
        except KeyboardInterrupt:
            raise SystemExit("Aborted by user during pause.")

    def run_mode_single_seed(self) -> None:
        """
        Run the generator for configuration(s) files and a single seed.
        """
        seen = self.seen_results
        seed = int(self.args.seed)
        # Skip if already present and not overwriting
        if (not self.args.re_run_seeds) and (seed in seen):
            self._log_seed_run(seed, 1, 1)
            LOG.info("seed=%d already in %s; skipping (use --re-run-seeds to re-run seed)", seed, self.results_f.name)
            self._eta_update_marker(1, 1)
            self._eta_summary(2, 1)
            return
        self._log_seed_run(seed, 1, 1)
        self._eta_update_marker(0, 1)
        started_at = time.time()
        try:
            resolved_config = self._resolve_config_for_seed(seed)
            target_repeats = max(1, self.args.repeats)
            successes = 0
            while successes < target_repeats:
                run_idx = successes + 1
                ok = self._run_single_seed(seed, resolved_config, run_idx=run_idx)
                if ok:
                    successes += 1
                    seen.add(seed)
                if successes < target_repeats:
                    self._pause(next_exists=True)
        finally:
            self._eta_record_seed_duration(started_at)
            self._eta_update_marker(1, 1)
            self._eta_summary(2, 1)
        
        self._eta_update_marker(1, 1)

    def run_mode_count(self) -> None:
        seen = self.seen_results
        to_make = int(self.args.count)
        now_ns = int(time.time_ns())
        rng = seeded_random(now_ns, "base")
        made = 0

        def keep_running():
            if to_make == -1:
                return True
            return made < to_make

        while keep_running():
            seed = rng.getrandbits(63) or 1
            if (not self.args.re_run_seeds) and (seed in seen):
                # silently pick another seed (we're in random mode)
                continue
            seed_idx = made + 1
            seeds_total = to_make
            self._log_seed_run(seed, seed_idx, seeds_total if to_make != -1 else -1)
            self._eta_update_marker(made, seeds_total if to_make != -1 else -1)
            started_at = time.time()
            try:
                resolved_config = self._resolve_config_for_seed(seed)
                target_repeats = max(1, self.args.repeats)
                successes = 0
                while successes < target_repeats and keep_running():
                    ok = self._run_single_seed(seed, resolved_config, run_idx=successes+1)
                    if ok:
                        successes += 1
                        seen.add(seed)
                    if successes < target_repeats and keep_running():
                        self._pause(next_exists=True)
            finally:
                self._eta_record_seed_duration(started_at)
                made += 1
                self._eta_update_marker(made, seeds_total if to_make != -1 else -1)
                self._eta_summary(seed_idx + 1, seeds_total if to_make != -1 else -1)

            self._pause(next_exists=keep_running())

        self._eta_update_marker(made, to_make if to_make != -1 else -1)

    def run_mode_seed_file(self) -> None:
        """
        Run the generator for a specific configuration and seeds from a file.
        """
        seen = self.seen_results
        seeds_list = self._read_seeds_file(Path(self.args.seed_file))
        seeds_total = len(seeds_list)
        
        for seed_idx, seed in enumerate(seeds_list, start=1):
            if (not self.args.re_run_seeds) and (seed in seen):
                LOG.info("seed=%d already in %s; skipping (use --re-run-seeds to re-run seed)", seed, self.results_f.name)
                self._eta_update_marker(seed_idx, seeds_total)
                self._eta_summary(seed_idx + 1, seeds_total)
                continue
            self._log_seed_run(seed, seed_idx, seeds_total)
            self._eta_update_marker(seed_idx - 1, seeds_total)
            seed = int(seed)
            started_at = time.time()
            try:
                resolved_config = self._resolve_config_for_seed(seed)
                target_repeats = max(1, self.args.repeats)
                successes = 0
                while successes < target_repeats:
                    # If we already met the quota, stop
                    if self.args.seeds_not_all_running > 0 and self.saved_not_all_running >= self.args.seeds_not_all_running:
                        break
                    run_idx = successes + 1
                    ok = self._run_single_seed(seed, resolved_config, self.args.seed_file, run_idx=run_idx)
                    if ok:
                        successes += 1
                        seen.add(seed)
                    if successes < target_repeats:
                        self._pause(next_exists=True)
            finally:
                self._eta_record_seed_duration(started_at)
                self._eta_update_marker(seed_idx, seeds_total)
                self._eta_summary(seed_idx + 1, seeds_total)

            if self.args.seeds_not_all_running > 0 and self.saved_not_all_running >= self.args.seeds_not_all_running:
                LOG.info("reached number of seeds-not-all-running quota (%d); stopping", self.args.seeds_not_all_running)
                break

            self._pause(next_exists=(seed_idx < seeds_total))

        self._eta_update_marker(min(seed_idx, seeds_total), seeds_total)

    ##############################################
    # ------------ Seed Run ----------------
    ##############################################
    def _execute_seed_direct(self, seed: int, ta: "TestConfigApplied") -> bool:
        """
        Direct solving path (no cluster). Generates specs, calls the Python solver,
        writes a compact CSV row, prints a summary with solver status + stats.
        """
        phase = "direct_generate_specs"
        LOG.info("phase=%s (no cluster work)", phase)
        rng = seeded_random(seed, "base")

        pod_specs: list[dict] = []
        rs_specs:  list[dict] = []
        if ta.num_replicaset > 0:
            rs_specs = self._make_replicaset_specs_only(rng, ta)
        else:
            pod_specs = self._make_standalone_pod_specs_only(rng, ta)

        phase = "direct_call_solver"
        LOG.info("phase=%s", phase)
        resp, meta = self._solver_directly(ta, seed, pod_specs, rs_specs)

        placements = resp.get("placements", []) or []
        evictions  = resp.get("evictions", [])  or []

        initial_running_uids = set(meta.get("initial_running_uids", []))
        uid_to_priority = meta.get("uid_to_priority", {})
        uid_to_cpu = meta.get("uid_to_cpu", {})
        uid_to_mem = meta.get("uid_to_mem", {})
        total_pods = int(meta.get("total_pods", 0))
        total_node_cpu = int(meta.get("total_node_cpu", 0)) or 1
        total_node_mem = int(meta.get("total_node_mem", 0)) or 1

        evicted_uids = {e["pod"]["uid"] for e in evictions if "pod" in e and "uid" in e["pod"]}
        new_from_pending_uids = {
            pl["pod"]["uid"]
            for pl in placements
            if "pod" in pl and "uid" in pl["pod"] and (pl.get("from_node") or "") == ""
        }
        
        moved_from_running_uids = {
            pl["pod"]["uid"]
            for pl in placements
            if "pod" in pl and "uid" in pl["pod"] and (pl.get("from_node") or "") != ""
        }

        final_running_uids = (initial_running_uids - evicted_uids) | new_from_pending_uids
        old_run_cpu = sum(uid_to_cpu.get(u, 0) for u in initial_running_uids)
        old_run_mem = sum(uid_to_mem.get(u, 0) for u in initial_running_uids)
        old_cpu_util = old_run_cpu / total_node_cpu if total_node_cpu else 0.0
        old_mem_util = old_run_mem / total_node_mem if total_node_mem else 0.0
        running_count = len(final_running_uids)
        unscheduled_total = max(0, total_pods - running_count)

        # placed-by-priority over FINAL running set
        running_by_prio = Counter(uid_to_priority.get(u, 0) for u in final_running_uids)

        # runtime utilization over FINAL running set
        run_cpu = sum(uid_to_cpu.get(u, 0) for u in final_running_uids)
        run_mem = sum(uid_to_mem.get(u, 0) for u in final_running_uids)
        util_run_cpu = run_cpu / total_node_cpu
        util_run_mem = run_mem / total_node_mem

        per_prio_str = ", ".join(
            f"{p}:{running_by_prio.get(p,0)}"
            for p in sorted(set(uid_to_priority.values()), reverse=True)
        )
        status = resp.get("status", "UNKNOWN")
        elapsed_ms = int(meta.get("elapsed_ms", 0))
        LOG.info(
            "solver status=%s duration_ms=%d  running=%d/%d  unscheduled=%d  "
            "running_by_priority={%s}  evicted=%d  moved=%d  new=%d  "
            "old_cpu_util=%.3f old_mem_util=%.3f  util_cpu=%.3f util_mem=%.3f",
            status,
            elapsed_ms,
            running_count, total_pods, unscheduled_total,
            per_prio_str,
            len(evicted_uids),
            len(moved_from_running_uids),
            len(new_from_pending_uids),
            old_cpu_util, old_mem_util,
            util_run_cpu, util_run_mem,
        )
        phase = "direct_stats"
        LOG.info("phase=%s", phase)

        # Console summary line
        self._log_seed_summary(
            seed,
            note=(
                f"direct-solving status={status} exec_time_ms={elapsed_ms} "
                f"running={running_count}/{total_pods} unscheduled={unscheduled_total} "
                f"evicted={len(evicted_uids)} moved={len(moved_from_running_uids)} new={len(new_from_pending_uids)} running_by_priority={{ {per_prio_str} }} "
                f"util_cpu={util_run_cpu:.3f} (old_cpu_util={old_cpu_util:.3f}) util_mem={util_run_mem:.3f} (old_mem_util={old_mem_util:.3f})"
            ),
        )
        return True

    def _execute_seed_on_cluster(self, seed, ta, *, run_idx: int = 1) -> bool:
        """
        Full KWOK path: cluster creation, nodes, namespace/PC, apply workload,
        optional solver trigger, settle, snapshot, CSV row, artifacts.
        """
        phase = "start"
        LOG.info(f"phase={phase}  seed={seed}")
        start_time = time.time()
        rng = seeded_random(seed, "base")

        # cluster
        phase = "ensure_cluster"
        LOG.info(f"phase={phase}")
        try:
            self._ensure_kwok_cluster(self.args.cluster_name, self.args.kwok_runtime, self.kwokctl_config_doc, recreate=True)
        except Exception as e:
            tb = traceback.format_exc()
            self._record_failure("seed", seed, phase, str(e), tb)
            LOG.error(f"seed-failed; ensure cluster: {e}")
            self._log_seed_summary(seed, "ensure cluster failed")
            return False

        # nodes
        phase = "nodes"
        LOG.info(f"phase={phase}")
        try:
            DEFAULT_POD_CAP = max(30, ta.num_pods * 3)
            node_cpu_str = qty_to_mcpu_str(ta.node_cpu_m)
            node_mem_str = qty_to_bytes_str(ta.node_mem_b)
            LOG.info("sizing nodes: per-node cpu=%s, mem=%s", node_cpu_str, node_mem_str)
            KwokTestGenerator._create_kwok_nodes(self.ctx, ta.num_nodes, node_cpu_str, node_mem_str, pods_cap=DEFAULT_POD_CAP)
        except Exception as e:
            tb = traceback.format_exc()
            self._record_failure("seed", seed, "nodes", str(e), tb)
            LOG.error(f"seed-failed; nodes setup: {e}")
            self._log_seed_summary(seed, "nodes setup failed")
            return False

        # namespace
        phase = "ensure_namespace"
        LOG.info(f"phase={phase}")
        self._ensure_namespace(self.ctx, ta.namespace)
        
        # service account
        phase = "ensure_service_account"
        LOG.info(f"phase={phase}")
        self._ensure_service_account(self.ctx, ta.namespace, "default")

        # priority classes
        phase = "ensure_priority_classes"
        LOG.info(f"phase={phase}")
        self._ensure_priority_classes(self.ctx, ta.num_priorities, prefix="p", start=1)

        # workload
        phase = "apply_workload"
        LOG.info(f"phase={phase}  mode={'RS' if ta.num_replicaset>0 else 'standalone'}")
        pod_specs: list[dict] = []
        rs_specs:  list[dict] = []
        if ta.num_replicaset > 0:
            rs_specs = self._make_replicaset_specs_only(rng, ta)
            self._apply_replicasets(ta, rs_specs)
        else:
            pod_specs = self._make_standalone_pod_specs_only(rng, ta)
            self._apply_standalone_pods(ta, pod_specs)

        # (optionally) trigger the solver
        if self.args.solver_trigger:
            phase = "wait_settle_before_solver"
            LOG.info(f"phase={phase} timeout_min={ta.settle_timeout_min_s}s")
            time.sleep(ta.settle_timeout_min_s)
            
            phase = "solver_trigger"
            LOG.info(f"phase={phase} url={SOLVER_TRIGGER_URL}; waiting for response (timeout {SOLVER_TRIGGER_TIMEOUT_S}s)")
            code, body = self._solver_trigger_http()
            body_compact = (body or "").replace("\n", "\\n")
            if len(body_compact) > 600:
                body_compact = body_compact[:600] + "...(truncated)"
            LOG.info(f"solver_response code={code} body={body_compact}")
            self.last_solver_result = None
            if code and isinstance(body, str) and body.strip().startswith("{"):
                try:
                    self.last_solver_result = json.loads(body)
                except Exception:
                    self.last_solver_result = None

        # wait & settle
        phase = "wait_settle_before_check"
        LOG.info(f"phase={phase} timeout_min={ta.settle_timeout_min_s}s")
        time.sleep(ta.settle_timeout_min_s)
        if ta.settle_timeout_max_s > 0:
            LOG.info(f"waiting for inactive scheduler (max {ta.settle_timeout_max_s}s)")
            _ = self._wait_solver_inactive_http(SOLVER_ACTIVE_URL, ta.settle_timeout_max_s)

        # status snapshot
        phase = "status_snapshot"
        LOG.info(f"phase={phase}")
        snap = stat_snapshot(self.ctx, ta.namespace, expected=ta.num_pods)

        # validate counts
        running_count = len(snap.pods_running)
        unsched_count = len(snap.pods_unscheduled)
        if running_count + unsched_count != ta.num_pods:
            phase = "snapshot_validation"
            self._record_failure("seed", seed, phase,
                            f"pod count mismatch: expected {ta.num_pods}, got {running_count}+{unsched_count}={running_count+unsched_count}")
            LOG.warning("treating seed as failure: pod count mismatch: expected %d, got %d+%d=%d",
                        ta.num_pods, running_count, unsched_count, running_count+unsched_count)
            self._log_seed_summary(seed, note=f"pod count mismatch: expected total={ta.num_pods}, got running={running_count} unscheduled={unsched_count}; running+unscheduled={running_count+unsched_count}")
            return False

        # (optionally) skip-all-running
        if unsched_count == 0 and self.args.seeds_not_all_running > 0:
            under_limit = (self.saved_not_all_running < self.args.seeds_not_all_running)
            if under_limit:
                self.saved_not_all_running += 1
                self._record_skipped_all_running_seed(seed, running_count)
                self._log_seed_summary(seed, "skipped (all pods running)")
                LOG.info("skipped saving seed=%s (all pods running)", seed)
                return True

        # attempts / baseline / stages
        baseline, best_name, attempts, error = self._get_solver_attempts()
        best_score, best_duration_ms, best_status = self._extract_best_attempt_fields(best_name, attempts)

        result_row = {
            "timestamp": get_timestamp(),
            "seed": str(seed),
            "error": error,
            
            "baseline": json.dumps(baseline, separators=(",", ":"), sort_keys=True),
            "best_name": best_name,           
            "best_score": (best_score if best_score is not None else ""),
            "best_duration_ms": (int(best_duration_ms) if best_duration_ms is not None else ""),
            "best_status": (best_status or ""),
            
            "solver_attempts": json.dumps(attempts, separators=(",", ":"), sort_keys=True),
            
            "util_run_cpu": f"{snap.cpu_run_util:.3f}",
            "util_run_mem": f"{snap.mem_run_util:.3f}",
            "cpu_m_run": int(sum(snap.cpu_req_by_node.values())),
            "mem_b_run": int(sum(snap.mem_req_by_node.values())),
            
            "running_count": int(running_count),
            "unscheduled_count": int(unsched_count),
            
            "pods_run_by_node": json.dumps(snap.pods_run_by_node, separators=(",", ":")),
            
            "running_placed_by_prio": json.dumps(snap.running_placed_by_prio, separators=(",", ":"), sort_keys=True),
            "unschedulable_by_prio": json.dumps(snap.unschedulable_by_prio, separators=(",", ":"), sort_keys=True),
            
            "unscheduled": "{" + ",".join(sorted(snap.pods_unscheduled)) + "}",
            "running": "{" + ",".join(sorted([name for (name, _) in snap.pods_running])) + "}",
            "pod_node": json.dumps(self._build_pod_node_list(
                {name: node for (name, node) in snap.pods_running},
                snap.pods_unscheduled, pod_specs, rs_specs
            ), separators=(",", ":")),
        }

        phase = "write_results"
        LOG.info(f"phase={phase}")
        self._append_result_csv(result_row)
        LOG.info("appended to %s", self.results_f)

        if self.args.save_solver_stats:
            phase = "solver_stats"
            LOG.info(f"phase={phase}")
            self._write_solver_stats_json(seed, run_idx)

        if self.args.save_scheduler_logs:
            phase = "save_scheduler_logs"
            LOG.info(f"phase={phase}")
            self._save_scheduler_logs(seed, run_idx=run_idx)

        LOG.info(f"seed run done; took {time.time()-start_time:.1f}s")
        self._log_seed_summary(seed, note=f"running={running_count} unscheduled={unsched_count}")
        return True

    def _run_single_seed(self, seed: int, ta: TestConfigApplied, seed_file: Optional[str] = None, *, run_idx: int = 1) -> bool:
        max_attempts = max(1, RETRIES_ON_FAIL, 0) + 1
        overall_started = time.time()
        last_attempt = 0
        for attempt in range(1, max_attempts + 1):
            last_attempt = attempt
            LOG.info("attempt %d/%d for seed=%s", attempt, max_attempts, seed)
            self.suppress_fail_log = (attempt < max_attempts)
            self.failure = None

            ok = self._execute_seed_direct(seed, ta) if self.args.solver_directly \
                else self._execute_seed_on_cluster(seed, ta, run_idx=run_idx)

            if ok:
                LOG.info("seed=%s succeeded on attempt %d; total %.1fs", seed, attempt, time.time() - overall_started)
                self.suppress_fail_log = False
                self.failure = None
                return True
            else:
                LOG.warning("seed=%s failed on attempt %d/%d", seed, attempt, max_attempts)

        self.suppress_fail_log = False
        if self.failure:
            df = self.failure
            self.failure = None
            self._record_failure(df.category, df.seed, df.phase, df.message, df.details)
        LOG.error("seed=%s failed after %d attempt(s); total %.1fs", seed, last_attempt, time.time() - overall_started)
        return False

    ######################################################
    # ---------- Job file helpers ------------------------
    ######################################################
    @staticmethod
    def _merge_doc(dst: dict, src: dict) -> dict:
        """
        Recursively update dst with src.
        If dst doesn't have the key, add it, else clean-start.
        """
        for k, v in (src or {}).items():
            dst[k] = v
        return dst

    @staticmethod
    def merge_job_fields_into_args(args: argparse.Namespace, job: dict) -> tuple[argparse.Namespace, dict]:
        # Map fields from job
        jf_cluster_name          = get_str(job.get("cluster-name"))
        jf_kwok_runtime          = get_str(job.get("kwok-runtime"))
        jf_workload_config_file  = get_str(job.get("workload-config-file"))
        jf_kwokctl_config_file   = get_str(job.get("kwokctl-config-file"))
        jf_output_dir            = get_str(job.get("output-dir"))
        jf_clean_start           = coerce_bool(job.get("clean-start"), default=None)
        jf_re_run_seeds          = coerce_bool(job.get("re-run-seeds"), default=None)
        jf_log_level             = get_str(job.get("log-level"))

        jf_seed                  = job.get("seed")
        jf_seed_file             = get_str(job.get("seed-file"))
        jf_count                 = job.get("count")
        jf_repeats               = job.get("repeats")
        jf_snar                  = job.get("seeds-not-all-running")

        jf_save_solver_stats     = coerce_bool(job.get("save-solver-stats"), default=None)
        jf_save_scheduler_logs   = coerce_bool(job.get("save-scheduler-logs"), default=None)

        jf_solver_trigger        = coerce_bool(job.get("solver-trigger"), default=None)

        jf_override_cfg          = job.get("override-config") or {}
        jf_override_kwokctl_envs = job.get("override-kwokctl-envs") or []

        # CLI priority: only fill when CLI value is None
        if getattr(args, "cluster_name", None) is None and jf_cluster_name:
            args.cluster_name = jf_cluster_name
        if getattr(args, "kwok_runtime", None) is None and jf_kwok_runtime:
            args.kwok_runtime = jf_kwok_runtime
        if getattr(args, "workload_config_file", None) is None and jf_workload_config_file:
            args.workload_config_file = jf_workload_config_file
        if getattr(args, "kwokctl_config_file", None) is None and jf_kwokctl_config_file:
            args.kwokctl_config_file = jf_kwokctl_config_file
        if getattr(args, "seed_file", None) is None and jf_seed_file:
            args.seed_file = jf_seed_file
        if getattr(args, "seed", None) is None and isinstance(jf_seed, int):
            args.seed = jf_seed
        if getattr(args, "count", None) is None and isinstance(jf_count, int):
            args.count = jf_count
        if getattr(args, "repeats", None) is None and isinstance(jf_repeats, int):
            args.repeats = jf_repeats
        if getattr(args, "output_dir", None) is None and jf_output_dir:
            args.output_dir = jf_output_dir
        if getattr(args, "seeds_not_all_running", None) is None and isinstance(jf_snar, int):
            args.seeds_not_all_running = jf_snar

        if getattr(args, "save_solver_stats", None) is None and jf_save_solver_stats is not None:
            args.save_solver_stats = jf_save_solver_stats
        if getattr(args, "save_scheduler_logs", None) is None and jf_save_scheduler_logs is not None:
            args.save_scheduler_logs = jf_save_scheduler_logs
        if getattr(args, "log_level", None) is None and jf_log_level:
            args.log_level = jf_log_level
        if getattr(args, "solver_trigger", None) is None and jf_solver_trigger is not None:
            args.solver_trigger = jf_solver_trigger
        if getattr(args, "clean_start", None) is None and jf_clean_start is not None:
            args.clean_start = jf_clean_start
        if getattr(args, "re_run_seeds", None) is None and jf_re_run_seeds is not None:
            args.re_run_seeds = jf_re_run_seeds

        return args, {"config": jf_override_cfg, "kwokctl_envs": jf_override_kwokctl_envs}


    @staticmethod
    def _merge_kwokctl_envs(doc: dict, add_envs: Iterable[Mapping[str, Any]] | None, component: str = "kube-scheduler") -> dict:
        """
        Return a copy of `doc` with extraEnvs for `component` merged by env 'name'.
        - Items in `add_envs` override/insert existing envs.
        - Ensures componentsPatches and the component entry exist.
        - Sorts components and envs by name for stability.
        """
        d = copy.deepcopy(doc) if isinstance(doc, dict) else {}
        # Normalize inputs
        add = [
            e for e in (add_envs or [])
            if isinstance(e, Mapping) and e.get("name")
        ]
        if not add:
            return d
        # Ensure componentsPatches is a list of dicts
        cps = [c for c in (d.get("componentsPatches") or []) if isinstance(c, dict)]
        d["componentsPatches"] = cps  # normalize back on the copy
        # Get or create the target component patch
        comp = next((c for c in cps if c.get("name") == component), None)
        if comp is None:
            comp = {"name": component}
            cps.append(comp)
        # Current valid envs
        cur = [e for e in (comp.get("extraEnvs") or []) if isinstance(e, dict) and e.get("name")]
        # Merge by env name (incoming overrides existing)
        env_by_name = {e["name"]: copy.deepcopy(e) for e in cur}
        env_by_name.update({e["name"]: copy.deepcopy(e) for e in add})
        # Stable sort: envs and components
        comp["extraEnvs"] = [env_by_name[k] for k in sorted(env_by_name)]
        cps.sort(key=lambda c: c.get("name", ""))
        return d

    @staticmethod
    def _get_kwokctl_envs(doc: dict) -> dict[str, object] | None:
        """
        Return effective extraEnvs for kube-scheduler as a dict:
        { "<NAME>": "<value>" | {"valueFrom": {...}} | None }
        Keys are sorted for stable output. Returns None if not present.
        """
        cps = doc.get("componentsPatches") or []
        for c in cps:
            if isinstance(c, dict) and c.get("name") == "kube-scheduler":
                raw_envs = c.get("extraEnvs") or []
                env_map: dict[str, object] = {}
                for e in raw_envs:
                    if not isinstance(e, dict): 
                        continue
                    name = (e.get("name") or "").strip()
                    if not name:
                        continue
                    if "value" in e and e.get("value") not in (None, ""):
                        env_map[name] = e["value"]
                    else:
                        env_map[name] = None
                # sort by name for stability
                return dict(sorted(env_map.items(), key=lambda kv: kv[0]))
        return None

    def ensure_default_args(args: argparse.Namespace) -> argparse.Namespace:
        if getattr(args, "cluster_name", None) is None:
            args.cluster_name = "kwok1"
        if getattr(args, "kwok_runtime", None) is None:
            args.kwok_runtime = "binary"
        if getattr(args, "job_file", None) is None:
            args.job_file = None
        if getattr(args, "workload_config_file", None) is None:
            args.workload_config_file = None
        if getattr(args, "kwokctl_config_file", None) is None:
            args.kwokctl_config_file = None
        if getattr(args, "output_dir", None) is None:
            args.output_dir = "./output"

        # booleans now default to None -> set final defaults here
        if getattr(args, "clean_start", None) is None:
            args.clean_start = False
        if getattr(args, "re_run_seeds", None) is None:
            args.re_run_seeds = False
        if getattr(args, "pause", None) is None:
            args.pause = False
        if getattr(args, "log_level", None) is None:
            args.log_level = "INFO"

        if getattr(args, "seed", None) is None:
            args.seed = None
        if getattr(args, "seed_file", None) is None:
            args.seed_file = None
        if getattr(args, "count", None) is None:
            args.count = None
        if getattr(args, "repeats", None) is None:
            args.repeats = 1
        if getattr(args, "seeds_not_all_running", None) is None:
            args.seeds_not_all_running = 0

        if getattr(args, "meta", None) is None:
            args.meta = None

        if getattr(args, "save_solver_stats", None) is None:
            args.save_solver_stats = False
        if getattr(args, "save_scheduler_logs", None) is None:
            args.save_scheduler_logs = False
        if getattr(args, "solver_trigger", None) is None:
            args.solver_trigger = False

        if getattr(args, "solver_directly", None) is None:
            args.solver_directly = False
        if getattr(args, "solver_timeout_ms", None) is None:
            args.solver_timeout_ms = 10000
        if getattr(args, "solver_input_export", None) is None:
            args.solver_input_export = None
        if getattr(args, "solver_output_export", None) is None:
            args.solver_output_export = None
        if getattr(args, "solver_directly_running_target_util", None) is None:
            args.solver_directly_running_target_util = 0.0

        # check args
        if not args.workload_config_file or not args.kwokctl_config_file:
            raise SystemExit("--workload-config-file and --kwokctl-config-file are both required (CLI or job-file)")
        if args.seed is None and not args.seed_file and args.count is None:
            if args.seeds_not_all_running == 0:
                raise SystemExit("Provide --seed, --seed-file, --count, or seeds-not-all-running in job-file")
            if args.seeds_not_all_running > 0:
                args.count = args.seeds_not_all_running
                LOG.info("no --seed/--seed-file/--count provided; defaulting --count=%d from --seeds-not-all-running", args.count)
        if args.seed is not None and args.seed_file:
            raise SystemExit("--seed cannot be used with --seed-file")
        if args.seed is not None and args.count is not None:
            raise SystemExit("--seed cannot be used with --count")
        if args.seed_file and args.count is not None:
            raise SystemExit("--seed-file cannot be used with --count")
        if args.seed is not None and args.seed < 1:
            raise SystemExit("--seed must be a positive integer")
        if args.count is not None and args.count < -1:
            raise SystemExit("--count must be -1 (infinite) or a positive integer")
        if args.seeds_not_all_running not in (-1, 0) and args.seeds_not_all_running < 1:
            raise SystemExit("--seeds-not-all-running must be -1 (infinite), 0 (disabled), or >= 1")

        # --- check seed file and config file ---
        seed_path = Path(args.seed_file).resolve() if args.seed_file else None
        if args.seed_file:
            if not seed_path.exists():
                raise SystemExit(f"--seed-file not found: {seed_path}")
        wl_path = Path(args.workload_config_file).resolve()
        kwokctl_path = Path(args.kwokctl_config_file).resolve()
        if not wl_path.exists():
            raise SystemExit(f"--workload-config-file not found: {wl_path}")
        if not kwokctl_path.exists():
            raise SystemExit(f"--kwokctl-config-file not found: {kwokctl_path}")

        # --- check kwok runtime vs. solver-trigger ---
        if args.solver_trigger and args.kwok_runtime != "binary":
            raise SystemExit("--solver-trigger requires --kwok-runtime=binary")
        
        return args

def main():
    # Parse args
    args = build_argparser().parse_args()

    # If generating seeds; exit after that
    if args.gen_seeds_to_file is not None:
        generate_seeds(args.gen_seeds_to_file)
        return

    # TestGenerator instance
    test_generator = KwokTestGenerator(args)
    
    # Run the appropriate path (pass through already-seen seeds)
    if test_generator.args.seed is not None:
        test_generator.run_mode_single_seed()
    elif test_generator.args.count is not None and int(test_generator.args.count) >= -1:
        test_generator.run_mode_count()
    elif test_generator.args.seed_file:
        test_generator.run_mode_seed_file()

    print("done.")

if __name__ == "__main__":
    main()