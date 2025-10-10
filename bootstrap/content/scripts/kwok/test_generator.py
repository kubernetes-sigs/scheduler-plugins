#!/usr/bin/env python3
# test_generator.py

import sys, os, shutil, argparse, math, time, random, csv, json, logging, yaml, subprocess, tempfile, traceback, contextlib, fcntl, shlex
from dataclasses import dataclass, field
from typing import Optional, Tuple, List, Dict, Any
from collections import namedtuple
from pathlib import Path
from urllib import request as _urlreq, error as _urlerr

from helpers import (
    seeded_random,
    get_timestamp, setup_logging,
    stat_snapshot,
    file_exists, csv_append_row, ensure_csv_with_header, csv_read_header,
    qty_to_mcpu_str, qty_to_bytes_str, qty_to_bytes_int, qty_to_mcpu_int,
    yaml_kwok_node, yaml_priority_class, yaml_kwok_pod, yaml_kwok_rs,
    normalize_interval, parse_int_interval, parse_qty_interval, parse_timeout_s, get_int_from_dict, get_float_from_dict, get_str_from_dict, split_interval
)

# ===============================================================
# Constants
# ===============================================================
RESULTS_HEADER = [
    "timestamp", "kwok_config", "seed_file", "seed",
    "error", "baseline", "best_name","best_score", "best_duration_us", "best_status",
    "num_nodes", "num_pods", "num_priorities", "num_replicaset",
    "num_replicas_per_rs_lo", "num_replicas_per_rs_hi",
    "node_cpu_m", "node_mem_b",
    "cpu_per_pod_m_lo", "cpu_per_pod_m_hi",
    "mem_per_pod_b_lo", "mem_per_pod_b_hi",
    "util", "util_run_cpu", "util_run_mem",
    "cpu_m_run", "mem_b_run",
    "wait_pod_mode", "wait_pod_timeout_s", "settle_timeout_min_s", "settle_timeout_max_s",
    "running_count", "unscheduled_count",
    "pods_run_by_node",
    "running_placed_by_prio", "unschedulable_by_prio",
    "unscheduled", "running",
    "pod_node",
    "solver_attempts",
]

# Module-level logger handle (handlers configured later by setup_logging in main())
LOG = logging.getLogger("kwok")

CM_SOLVER_STATS_NAME = "solver-stats"
CM_SOLVER_STATS_NAMESPACE = "kube-system"

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

    source_file: Optional[Path] = field(default=None, repr=False)

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

    source_file: Optional[Path] = field(default=None, repr=False)

_Failure = namedtuple("_Failure", "category cfg seed phase message details")
# ===============================================================
# KwokTestGenerator
# ===============================================================
class KwokTestGenerator:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.ctx: Optional[str] = None
        self.results_dir: Path | None = None
        self.failed_f: Path | None = None
        self.last_optimize_json: dict[str, Any] | None = None
        self._results_csv_path: Path | None = None
        self._suppress_fail_log: bool = False
        self._failure: _Failure | None = None
        self._saved_not_all_running: int = 0
        self._skipped_all_running_seeds_csv_path: Path | None = None

    ######################################################
    # ---------- ETA helpers ----------
    ######################################################
    @staticmethod
    def _fmt_hms(seconds: int) -> str:
        """
        Format seconds into a human-readable string.
        """
        seconds = max(0, int(seconds))
        h, r = divmod(seconds, 3600)
        m, s = divmod(r, 60)
        parts = []
        if h: parts.append(f"{h}h")
        if m: parts.append(f"{m}m")
        if s or not parts: parts.append(f"{s}s")
        return "".join(parts)
    
    def _eta_init(self):
        """Call once per config to reset the seed durations."""
        self._seed_durations: list[float] = []

    def _eta_record_seed_duration(self, started_at: float):
        """Called after a seed finishes (success or fail)."""
        try:
            self._seed_durations.append(max(0.0, time.time() - started_at))
        except Exception:
            pass

    def _eta_estimation(self, cfg_idx: int, cfgs_total: int, seed_idx: int, seeds_total: int) -> float | None:
        """
        Return an epoch seconds ETA, or None if not enough info.
        """
        if not self._seed_durations:
            return None
        avg = sum(self._seed_durations) / max(1, len(self._seed_durations))
        if seeds_total is None or seeds_total <= 0: # unknown/infinite
            return None
        seeds_done = max(0, seed_idx - 1)
        seeds_left_current = max(0, seeds_total - seeds_done)
        cfgs_done = max(0, cfg_idx - 1)
        cfgs_left = max(0, cfgs_total - cfgs_done)
        future_configs_left = max(0, cfgs_left - 1)  # exclude current
        seeds_left_future = future_configs_left * seeds_total
        remaining_seeds_overall = seeds_left_current + seeds_left_future
        return time.time() + remaining_seeds_overall * avg

    def _eta_summary(self, cfg_idx: int, cfgs_total: int,
                        next_seed_idx: int, seeds_total: int) -> None:
        """
        Log an ETA summary block.
        """
        eta_epoch = self._eta_estimation(cfg_idx, cfgs_total, next_seed_idx, seeds_total)
        header = "\n================================================ ETA ================================================\n"
        footer = "\n====================================================================================================="

        # Show "not-all-running" segment only if a limit is set
        if self.args.seeds_not_all_running > 0:
            left = max(0, int(self.args.seeds_not_all_running) - int(getattr(self, "_saved_not_all_running", 0)))
            not_all_seg = f" | seeds-not-all-running-left={left}"
        else:
            not_all_seg = ""

        # Handle unlimited/unknown seed count
        if seeds_total is None or seeds_total <= 0:
            block = "ETA: unknown (seed count is unlimited)."
            LOG.info("%s%s%s", header, block, footer)
            return

        # Handle no samples yet
        if not self._seed_durations:
            block = "ETA: collecting first duration sample..." + not_all_seg
            LOG.info("%s%s%s", header, block, footer)
            return

        # Compute averages and counts
        avg = sum(self._seed_durations) / max(1, len(self._seed_durations))
        seeds_done = max(0, next_seed_idx - 1)
        seeds_left_current = max(0, seeds_total - seeds_done)
        cfgs_done = max(0, cfg_idx - 1)
        cfgs_left = max(0, cfgs_total - cfgs_done)
        # exclude current cfg from "future" so current seeds_left_current stand alone
        future_configs_left = max(0, cfgs_left - 1)
        seeds_left_future = future_configs_left * seeds_total

        # If not enough data to estimate ETA, show progress only
        if eta_epoch is None:
            block = (
                "ETA: not enough data yet (done %d/%d seeds in cfg %d/%d)"
            ) % (seeds_done, seeds_total, cfg_idx, cfgs_total)
            block += not_all_seg + "."
            LOG.info("%s%s%s", header, block, footer)
            return

        # Create the ETA string and log
        now = time.time()
        left_s = max(0, int(round(eta_epoch - now)))
        eta_str = time.strftime("%Y-%m-%d %H:%M:%S", time.localtime(eta_epoch))
        total_seeds_left = seeds_left_current + seeds_left_future
        block = (
            "ETA %s (in %s) | cfgs left: cur=%d, fut=%d | "
            "seeds left: cur=%d, fut=%d, total=%d%s | "
            "avg/seed=%.1fs (%d sample%s)"
        ) % (
            eta_str, self._fmt_hms(left_s),
            cfgs_left, future_configs_left,
            seeds_left_current, seeds_left_future, total_seeds_left,
            not_all_seg,
            avg, len(self._seed_durations), "" if len(self._seed_durations) == 1 else "s",
        )
        LOG.info("%s%s%s", header, block, footer)

    def _eta_write_file(self, eta_epoch: float | None, cfg_idx: int, cfgs_total: int, seed_idx: int, seeds_total: int):
        """
        Delete stale eta_* and write a new one:
        eta_<time>_configs<at-of-total>_seeds<at-of-total>
        """
        if not self.results_dir:
            return
        try:
            # delete any prior eta_* markers
            for p in self.results_dir.glob("eta_*"):
                try:
                    p.unlink()
                except OSError:
                    pass
            
            # what we're currently AT
            cfgs_at = max(0, int(cfg_idx))
            cfgs_total = max(0, int(cfgs_total))
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
                f"_configs-{cfgs_at}-of-{cfgs_total}"
                f"_seeds-{seeds_at}-of-{seeds_total}"
            )
            fpath = self.results_dir / fname
            
            # write a tiny payload
            with open(fpath, "w", encoding="utf-8") as fh:
                payload = {
                    "eta_epoch": int(eta_epoch) if isinstance(eta_epoch, (int, float)) else None,
                    "eta_iso": (time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime(eta_epoch)) if eta_epoch is not None else None),
                    "configs_at": cfgs_at, "configs_total": cfgs_total,
                    "seeds_at": seeds_at,   "seeds_total": seeds_total,
                }
                fh.write(json.dumps(payload, separators=(",", ":"), sort_keys=True) + "\n")
        except Exception:  # best-effort; don't break the run for ETA issues
            pass

    def _eta_update_marker(self, cfg_idx: int, cfgs_total: int, seed_idx: int, seeds_total: int):
        """
        Update the eta_* marker file.
        """
        eta_epoch = self._eta_estimation(cfg_idx, cfgs_total, seed_idx, seeds_total)
        self._eta_write_file(eta_epoch, cfg_idx, cfgs_total, seed_idx, seeds_total)

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
        # Make settle_timeout_max optional in configs: if missing/empty -> 0 (disabled)
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
    # ---------- CSV helpers & results helpers -----------
    ######################################################
    @staticmethod
    def _extract_best_attempt_fields(best_name: str, attempts: list[dict]) -> tuple[float | None, int | None, str]:
        """
        Prefer the attempt with name == best_name; otherwise use the one with the
        highest numeric 'score'. Return (best_score, best_duration_us, best_status).
        """
        if not isinstance(attempts, list) or not attempts:
            return None, None, ""
        best_attempt = None
        if best_name:
            for a in attempts:
                if str(a.get("name")) == best_name:
                    best_attempt = a
                    break
        if best_attempt is None:
            return None, None, ""
        # parse fields
        best_score = None
        try:
            v = best_attempt.get("score")
            if v is not None:
                best_score = float(v)
        except Exception:
            pass
        best_duration_us = None
        try:
            v = best_attempt.get("duration_us")
            if v is not None:
                best_duration_us = int(v)
        except Exception:
            # allow float-like strings
            try:
                best_duration_us = int(float(best_attempt.get("duration_us")))
            except Exception:
                pass
        best_status = str(best_attempt.get("status") or "")
        return best_score, best_duration_us, best_status

    def _skipped_all_running_csv(self) -> Path:
        assert self.results_dir is not None, "results_dir must be set"
        if self._skipped_all_running_seeds_csv_path is None:
            self._skipped_all_running_seeds_csv_path = self.results_dir / "skipped_all_running_seeds.csv"
            header = ["timestamp", "kwok_config", "seed_file", "seed", "running_count"]
            if not self._skipped_all_running_seeds_csv_path.exists():
                with open(self._skipped_all_running_seeds_csv_path, "w", encoding="utf-8", newline="") as fh:
                    w = csv.writer(fh)
                    w.writerow(header)
        return self._skipped_all_running_seeds_csv_path

    def _record_skipped_all_running_seed(self, cfg: Path, seed: int, seed_file: str | None, running_count: int) -> None:
        try:
            path = self._skipped_all_running_csv()
            with open(path, "a", encoding="utf-8", newline="") as fh:
                w = csv.writer(fh)
                w.writerow([get_timestamp(), str(cfg), seed_file or "", str(seed), str(running_count)])
            LOG.info("all pods running; skipped seed added to %s: seed=%s", path.name, seed)
        except Exception as e:
            LOG.warning("failed to record skipped seed: %s", e)

    def _purge_mismatched_csv(self, path: Path, expected_header: list[str]) -> bool:
        """
        If file exists and header != expected_header and --overwrite is set,
        delete the file and return True. Otherwise, return False.
        """
        if not path.exists():
            return False
        actual = csv_read_header(path) or []
        exp_norm = [c.strip() for c in expected_header]
        if actual != exp_norm and getattr(self.args, "overwrite", False):
            try:
                path.unlink()
                LOG.info("deleted CSV with mismatched header (overwrite=true): %s", path.name)
                return True
            except OSError as e:
                LOG.warning("failed to delete mismatched CSV %s: %s", path.name, e)
        return False

    def _append_result_csv(self, row: dict) -> None:
        assert self.results_dir is not None, "results_dir must be set"
        # If header mismatch and --overwrite: delete the old file
        self._purge_mismatched_csv(self.results_f, RESULTS_HEADER)
        ensure_csv_with_header(self.results_f, RESULTS_HEADER)
        csv_append_row(self.results_f, RESULTS_HEADER, row)

    def _load_seen_results(self) -> set[int]:
        seen: set[int] = set()
        assert self.results_dir is not None, "results_dir must be set"
        self.results_f = self.results_dir / "results.csv"
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

    def _remove_seed_from_results(self, seed: int) -> int:
        assert self.results_dir is not None, "results_dir must be set"
        if not self.results_f.exists():
            return 0
        try:
            with open(self.results_f, "r", encoding="utf-8", newline="") as fh:
                rows = list(csv.DictReader(fh))
        except Exception:
            return 0
        keep = [r for r in rows if (r.get("seed") or "").strip() != str(seed)]
        removed = len(rows) - len(keep)
        if removed > 0:
            with open(self.results_f, "w", encoding="utf-8", newline="") as fh:
                w = csv.DictWriter(fh, fieldnames=RESULTS_HEADER)
                w.writeheader()
                for r in keep:
                    w.writerow({k: r.get(k, "") for k in RESULTS_HEADER})
            LOG.info("pruned %d row(s) for seed=%s from %s", removed, seed, self.results_f)
        return removed
    
    def _clean_results_dir(self) -> None:
        """
        Delete all files and subdirectories in results_dir (but not the dir itself).
        """
        rd = self.results_dir
        if not rd:
            return
        try:
            rd = rd.resolve()
        except Exception:
            LOG.warning("could not resolve results_dir; skipping clean")
            return
        # Safety checks
        if not rd.exists():
            LOG.info("results_dir %s does not exist; nothing to clean", rd)
            return
        if not rd.is_dir():
            LOG.warning("results_dir %s is not a directory; skipping clean", rd)
            return
        # Guard against root ("/") just in case
        if rd == rd.anchor:
            LOG.error("refusing to clean the filesystem root (%s)", rd)
            return
        deleted = 0
        for entry in rd.iterdir():
            try:
                if entry.is_dir():
                    shutil.rmtree(entry)
                else:
                    entry.unlink(missing_ok=True)
                deleted += 1
            except Exception as e:
                LOG.warning("failed to delete %s: %s", entry, e)
        LOG.info("cleaned %d item(s) from %s", deleted, rd)
    
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
        if self.last_optimize_json and isinstance(self.last_optimize_json, dict):
            try:
                error = str(self.last_optimize_json.get("error", "") or "")
                best_name = str(self.last_optimize_json.get("best_name", "") or "")
                raw_attempts = self.last_optimize_json.get("attempts") or []
                if isinstance(raw_attempts, list):
                    attempts = raw_attempts
                bl = self.last_optimize_json.get("baseline") or {}
                if isinstance(bl, dict):
                    baseline = bl
            except Exception:
                pass

        if attempts and best_name:
            return baseline, best_name, attempts, error

        # (2) Fallback to ConfigMap runs.json
        cm = KwokTestGenerator._get_latest_configmap(
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
        cm_obj = KwokTestGenerator._get_latest_configmap(
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

        # Ensure output dir and write verbatim JSON blob
        assert self.results_dir is not None, "results_dir must be resolved before saving stats"
        out_dir = (self.results_dir / "solver-stats")
        out_dir.mkdir(parents=True, exist_ok=True)
        out_path = out_dir / f"{CM_SOLVER_STATS_NAME}_seed-{seed}_run-{run_idx}.json"

        try:
            # Always overwrite the per-run file
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
        Save scheduler logs for the current KWOK cluster to results_dir/scheduler-logs.
        File: <results-dir>/scheduler-logs/scheduler-logs_seed-<seed>_run-<run_idx>.log
        """
        assert self.results_dir is not None, "results_dir must be resolved before saving logs"
        out_dir = (self.results_dir / "scheduler-logs")
        out_dir.mkdir(parents=True, exist_ok=True)
        # No timestamp in filename; include run index. Always prune on collision.
        out_path = out_dir / f"scheduler-logs_seed-{seed}_run-{run_idx}.log"
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
        Run `kubectl --context <ctx> <args...>` and stream its combined output into LOG (DEBUG).
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
        Run `kwokctl <args...>` and stream its combined output into LOG (INFO).
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
    @contextlib.contextmanager
    def _kwok_cache_lock():
        """
        Take an exclusive lock so only one process runs 'kwokctl create cluster'
        at a time, avoiding races in ~/.kwok/cache when downloading binaries.
        Override lock path via env KWOK_CACHE_LOCK if desired.
        """
        lock_path = os.environ.get("KWOK_CACHE_LOCK", "/tmp/kwokctl-cache.lock")
        os.makedirs(os.path.dirname(lock_path), exist_ok=True)
        fd = os.open(lock_path, os.O_CREAT | os.O_RDWR, 0o666)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX)
            yield
        finally:
            try:
                fcntl.flock(fd, fcntl.LOCK_UN)
            finally:
                os.close(fd)

    @staticmethod
    def _prepare_kwokctl_config_file(src: Path) -> tuple[Path, Path | None]:
        """
        Prepare a KWOK configuration file for kwokctl.
        If the source file contains multiple documents, pick the one with kind=KwokctlConfiguration
        and apiVersion starting with config.kwok.x-k8s.io/. If not found, return (src, None).
        Otherwise, write the chosen document to a temp file and return (tempfile, tempfile).
        The caller is responsible for cleaning up the temp file if returned.
        """
        try:
            with open(src, "r", encoding="utf-8") as f:
                docs = list(yaml.safe_load_all(f))
        except Exception:
            return src, None
        chosen = None
        for d in docs:
            if isinstance(d, dict) and d.get("kind") == "KwokctlConfiguration":
                apiver = str(d.get("apiVersion", ""))
                if not apiver.startswith("config.kwok.x-k8s.io/"):
                    continue
                chosen = d
                break
        if chosen is None:
            return src, None
        tf = tempfile.NamedTemporaryFile("w", delete=False, suffix=".yaml")
        try:
            yaml.safe_dump(chosen, tf, sort_keys=False)
        finally:
            tf.close()
        return Path(tf.name), Path(tf.name)

    @staticmethod
    def _ensure_kwok_cluster(name_or_ctx: str, kwok_runtime: str, kwok_config: str | Path | None, recreate: bool = True) -> None:
        """
        Ensure a KWOK cluster with the given name or context exists.
        """
        cluster_name = name_or_ctx[len("kwok-"):] if name_or_ctx.startswith("kwok-") else name_or_ctx
        cfg_path = Path(kwok_config) if kwok_config else None

        if recreate:
            LOG.info(f"recreating kwok cluster '{cluster_name}'")
            with KwokTestGenerator._kwok_cache_lock():
                KwokTestGenerator._run_kwokctl_logged("delete", "cluster", "--name", cluster_name, check=False)

        if cfg_path and cfg_path.exists():
            cfg_for_kwokctl, tmp_to_cleanup = KwokTestGenerator._prepare_kwokctl_config_file(cfg_path)
            try:
                with KwokTestGenerator._kwok_cache_lock():
                    KwokTestGenerator._run_kwokctl_logged("create", "cluster", "--name", cluster_name, "--config", str(cfg_for_kwokctl), "--runtime", kwok_runtime)
            finally:
                if tmp_to_cleanup and tmp_to_cleanup.exists():
                    try: os.unlink(tmp_to_cleanup)
                    except OSError: pass
        else:
            if cfg_path and not cfg_path.exists():
                LOG.warning(f"kwok-config='{cfg_path}' not found; creating with defaults")
            with KwokTestGenerator._kwok_cache_lock():
                KwokTestGenerator._run_kwokctl_logged("create", "cluster", "--name", cluster_name, "--runtime", kwok_runtime)

    @staticmethod
    def _create_kwok_nodes(ctx: str, num_nodes: int, node_cpu: str, node_mem: str, pods_cap: int) -> None:
        """
        Create KWOK nodes kwok-node-1..kwok-node-N with the given capacity.
        """
        node_yaml = "".join(yaml_kwok_node(f"kwok-node-{i}", node_cpu, node_mem, pods_cap) for i in range(1, num_nodes + 1))
        if node_yaml:
            KwokTestGenerator._apply_yaml(ctx, node_yaml)

    @staticmethod
    def _ensure_namespace(ctx: str, ns: str) -> None:
        """
        Ensure the namespace exists in the given context.
        If recreate=True, delete it first, then (re)create if missing.
        """
        LOG.info(f"ensuring namespace '{ns}' in ctx={ctx}")
        rns = subprocess.run(["kubectl", "--context", ctx, "get", "ns", ns], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if rns.returncode != 0:
            KwokTestGenerator._run_kubectl_logged(ctx, "create", "ns", ns, check=True)
        LOG.info(f"namespace '{ns}' exists in ctx={ctx}")

    @staticmethod
    def _ensure_service_account(ctx: str, ns: str, sa: str, retries: int = 20, delay: float = 0.5) -> None:
        """
        Ensure the ServiceAccount exists in the given namespace.
        If recreate=True, delete it first, then (re)create if missing.
        """
        LOG.info(f"ensuring service account '{sa}' in ns='{ns}'")
        for _ in range(1, retries + 1):
            r = subprocess.run(["kubectl", "--context", ctx, "-n", ns, "get", "sa", sa], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if r.returncode == 0:
                LOG.info(f"found service-account '{sa}' in ns '{ns}'")
                break
            time.sleep(delay)

    @staticmethod
    def _ensure_priority_classes(ctx: str, num_priorities: int, *, prefix: str = "p", start: int = 1) -> None:
        """
        Ensure PriorityClasses {prefix}{i} for i in [start, start+num_priorities).
        If delete_extras=True, remove any of our prefixed PCs outside that set.
        """
        pcs_yaml = "".join(yaml_priority_class(f"{prefix}{v}", v) for v in range(start, start + num_priorities))
        LOG.info(f"ensuring {num_priorities} PriorityClasses")
        if pcs_yaml:
            KwokTestGenerator._apply_yaml(ctx, pcs_yaml)
        LOG.info(f"ensured {num_priorities} PriorityClasses")

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
    
    def _apply_standalone_pods(self, ta: TestConfigApplied, rng: random.Random) -> List[Dict[str, object]]:
        """
        Create standalone pods in the given namespace with the specified specs.
        Returns a list of pod specs created. In direct mode, just returns specs (no cluster work).
        """
        # 1) Generate specs once (single source of truth)
        specs = self._make_standalone_pod_specs_only(rng, ta)

        # 2) Direct mode? don't touch the cluster
        if self.args.direct_solving:
            return specs

        # 3) Normal mode: convert specs -> YAML & apply
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
                _ = KwokTestGenerator._wait_each(self.ctx, "pod", name, ta.namespace, ta.wait_pod_timeout_s, ta.wait_pod_mode)
        elif ta.wait_pod_mode in ("exist", "ready"):
            for name in names:
                _ = KwokTestGenerator._wait_each(self.ctx, "pod", name, ta.namespace, ta.wait_pod_timeout_s, ta.wait_pod_mode)

        LOG.info("created %d standalone pods", len(specs))
        # For compatibility with the rest of the pipeline, return a normalized list
        # (keep field names you already used elsewhere if needed)
        return specs

    def _apply_replicasets(self, ta: TestConfigApplied, rng: random.Random) -> List[Dict[str, object]]:
        """
        Create ReplicaSets in the given namespace with the specified specs.
        Returns a list of replicaset specs created. In direct mode, just returns specs (no cluster work).
        """
        # 1) Generate specs once (single source of truth)
        specs = self._make_replicaset_specs_only(rng, ta)

        # 2) Direct mode? don't touch the cluster
        if self.args.direct_solving:
            return specs

        # 3) Normal mode: convert specs -> YAML & apply
        for s in specs:
            pc = f"p{int(s['priority'])}"
            cpu_m_str = qty_to_mcpu_str(int(s["req_cpu_m"]))
            mem_b_str = qty_to_bytes_str(int(s["req_mem_bytes"]))
            self._apply_yaml(self.ctx, yaml_kwok_rs(ta.namespace, s["name"], int(s["replicas"]), cpu_m_str, mem_b_str, pc))
            _ = self._wait_rs_pods(self.ctx, s["name"], ta.namespace, ta.wait_pod_timeout_s, ta.wait_pod_mode)

        LOG.info("created %d ReplicaSets with total %d (=num_pods)",
                len(specs), sum(int(s['replicas']) for s in specs))
        return specs

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
    # ---------- Solve directly ------------------
    ######################################################
    @staticmethod
    def _preplace_running_pods(pods, nodes, run_util: float, seed: int) -> None:
        """
        Deterministic, simple pre-placement:
        - Greedy largest-first; stable shuffle for strict ties using *the same seed*
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

        # ----- Build and return utilization metrics -----
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

    def _solve_direct_from_specs(self, ta, seed, pod_specs, rs_specs) -> tuple[dict, dict]:
        # Build nodes (numeric capacities)
        nodes = [{
            "name": f"node-{j}",
            "cap_cpu_m": int(ta.node_cpu_m),
            "cap_mem_bytes": int(ta.node_mem_b),
        } for j in range(ta.num_nodes)]

        # Expand pods; keep uid -> priority map for stats
        pods = []
        uid_to_priority: dict[str,int] = {}
        uid_counter = 0

        def _emit(name, prio, cpu_m, mem_b):
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
            _emit(s["name"], s["priority"], s["req_cpu_m"], s["req_mem_bytes"])

        for rs in rs_specs:
            for r in range(int(rs["replicas"])):
                _emit(f'{rs["name"]}-{r}', rs["priority"], rs["req_cpu_m"], rs["req_mem_bytes"])

        # --- Pre-place some pods as already running (direct mode only) ---
        run_util = float(getattr(self.args, "direct_running_util", 0.0) or 0.0)
        self._preplace_running_pods(pods, nodes, run_util, seed)
        LOG.info("pre-placed pods with target_util=%.3f (seed=%d)", run_util, seed)

        instance = {
            "timeout_ms": int(self.args.solver_timeout_ms),
            "ignore_affinity": True,
            "log_progress": True, # can slow down the solver quite a bit
            "workers": 0,
            "max_trials": 0,
            "preemptor": None,
            "nodes": nodes,
            "pods": pods,
        }

        # Export input if requested
        if self.args.export_solver_input:
            Path(self.args.export_solver_input).parent.mkdir(parents=True, exist_ok=True)
            with open(self.args.export_solver_input, "w", encoding="utf-8") as f:
                json.dump(instance, f, indent=2)

        LOG.info("solving directly with %d nodes and %d pods (seed=%d)", len(nodes), len(pods), seed)
        cmd = shlex.split(self.args.solver_cmd)
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

        # Show raw IO once after process end
        if self.args.show_solver_exitlog:
            if err:
                print("[solver:stderr]", file=sys.stderr)
                print(err, file=sys.stderr)
            if out:
                print("[solver:stdout]")
                print(out)

        # Parse JSON from stdout
        try:
            resp = json.loads(out) if out else {}
        except Exception:
            resp = {"status": "PY_SOLVER_STDOUT_PARSE_ERROR", "raw": out}

        # Export output if requested
        if self.args.export_solver_output:
            Path(self.args.export_solver_output).parent.mkdir(parents=True, exist_ok=True)
            with open(self.args.export_solver_output, "w", encoding="utf-8") as f:
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

    ######################################################
    # ---------- Optimizer HTTP helpers ------------------
    ######################################################
    def _trigger_optimizer_http(self, url: str, *, timeout: float = 120.0) -> tuple[int, str]:
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
            req = _urlreq.Request(url, data=data, headers=headers, method="POST")
            with _urlreq.urlopen(req, timeout=timeout) as resp:
                body = resp.read().decode("utf-8", errors="replace")
                return getattr(resp, "status", 200), body
        except _urlerr.HTTPError as e:
            # HTTP-level error (server responded with e.code)
            try:
                body = e.read().decode("utf-8", errors="replace")
            except Exception:
                body = str(e)
            return e.code, body
        except Exception as e:
            LOG.info(f"optimizer POST failed: {e}")
            # give a synthetic status if we never reached the server
            return 0, f"connect-failed: {e}"

    def _wait_optimizer_inactive_http(self, url: str, timeout_s: int, *, poll_initial_s: float = 0.5, poll_interval_s: float = 1.5) -> bool:
        """
        Poll the /active endpoint until it reports Active=false or until timeout.
        Returns True if became inactive; False if timed out or endpoint unreachable.
        poll_initial_s: initial poll interval in seconds (default 0.5s)
        poll_interval_s: maximum poll interval in seconds (default 1.5s)
        """

        def get_optimizer_active_status_http(url: str, *, timeout: float = 3.0) -> tuple[int, str]:
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
            code, body = get_optimizer_active_status_http(url)
            inactive_now = None
            if code == 200 and body:
                try:
                    data = json.loads(body)
                    # server struct: { Active: bool, ... }
                    active = bool(data.get("Active", data.get("active", True)))
                    inactive_now = (active is False)
                except Exception:
                    inactive_now = None

            if inactive_now is True:
                LOG.info(f"optimizer is inactive; proceeding")
                return True

            now = time.time()
            if now - last_log > 2:
                LOG.info("waiting for optimizer to become inactive... code=%s body=%s", code, (body[:200] + ("..." if len(body) > 200 else "")) if isinstance(body, str) else body)
                last_log = now

            time.sleep(interval)
            interval = poll_interval_s

        LOG.warning("wait_for_inactive timed out after %ss; continuing", timeout_s)
        return False

    ##############################################
    # ------------ Logging helpers -----------------
    ##############################################
    @staticmethod
    def _format_testconfigraw_block(tr: "TestConfigRaw") -> str:
        """
        Produce a stable, readable block of the raw config values as parsed from YAML.
        """
        def _fmt_interval(v):
            if v is None:
                return "<unset>"
            if isinstance(v, (tuple, list)) and len(v) == 2:
                return f"{v[0]},{v[1]}"
            return str(v)

        fields = [
            ("source_file", getattr(tr, "source_file", None)),
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
        return "\n".join(lines)

    @staticmethod
    def _log_config_raw(tr: "TestConfigRaw", *, use_logger: bool = True) -> None:
        block = KwokTestGenerator._format_testconfigraw_block(tr)
        header = "\n================================================ CONFIG (RAW) ================================================\n"
        footer = "\n=============================================================================================================="
        if use_logger:
            LOG.info("%s%s%s", header, block, footer)
        else:
            print(f"{header}{block}{footer}", flush=True)

    @staticmethod
    def _format_args_block(args) -> str:
        # Keep a stable, explicit order
        fields = [
            ("cluster_name", args.cluster_name),
            ("kwok_runtime", args.kwok_runtime),
            ("config_file", args.config_file),
            ("results_dir", args.results_dir),
            ("overwrite", args.overwrite),
            ("clean_results", args.clean_results),
            ("seed", args.seed),
            ("seed_file", args.seed_file),
            ("gen_seeds_to_file", args.gen_seeds_to_file),
            ("count", args.count),
            ("retries", args.retries),
            ("repeats", args.repeats),
            ("seeds_not_all_running", args.seeds_not_all_running),
            ("matrix_file", args.matrix_file),
            ("save_solver_stats", args.save_solver_stats),
            ("save_scheduler_logs", args.save_scheduler_logs),
            ("trigger_optimizer", args.trigger_optimizer),
            ("optimizer_url", (args.optimizer_url if getattr(args, "trigger_optimizer", False) else "<unused>")),
            ("active_url", args.active_url),
            ("pause", args.pause),
            ("log_level", args.log_level),
        ]
        pad = max(len(k) for k, _ in fields)
        def _fmt(v):
            return "<unset>" if v in (None, "") else str(v)
        lines = [f"{k.rjust(pad)} = {_fmt(v)}" for k, v in fields]
        return "\n".join(lines)

    @staticmethod
    def _log_args(args, *, use_logger: bool = True) -> None:
        block = KwokTestGenerator._format_args_block(args)
        header = "\n================================================ ARGS ================================================\n"
        footer = "\n======================================================================================================"
        if use_logger:
            LOG.info("%s%s%s", header, block, footer)
        else:
            print(f"{header}{block}{footer}", flush=True)

    ##############################################
    # ------------ Seed helpers -----------------
    ##############################################
    @staticmethod
    def _read_seeds_file(path: Path) -> List[int]:
        """
        TXT:
          - One integer per line (whitespace allowed).
          - Empty lines and lines starting with '#' are ignored.
          - Non-integer lines are skipped (logged at DEBUG).
        CSV:
          - Requires a 'seed' column (case-insensitive) -> else raise ValueError.
          - Each non-empty seed cell must be a positive integer -> else raise ValueError.
          - Empty cells are ignored.
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
        if not path.exists():
            raise ValueError(f"seed-file not found: {path}")
        if path.suffix.lower() == ".csv":
            try:
                with open(path, "r", encoding="utf-8", newline="") as f:
                    rdr = csv.DictReader(f)
                    if rdr.fieldnames is None:
                        raise ValueError(f"CSV seed-file has no header: {path}")
                    seed_key = next((c for c in rdr.fieldnames if (c or "").strip().lower() == "seed"), None)
                    if not seed_key:
                        raise ValueError(f"CSV seed-file missing required 'seed' column: {path}")
                    line_no = 1  # header
                    for row in rdr:
                        line_no += 1
                        raw = (row.get(seed_key) or "").strip()
                        if raw == "":
                            continue  # allow blank seed cells
                        try:
                            val = int(raw)
                        except Exception:
                            raise ValueError(f"Invalid integer in 'seed' at {path}:{line_no}: {raw!r}")
                        if val <= 0:
                            raise ValueError(f"Non-positive seed at {path}:{line_no}: {val}")
                        _add(val)
            except ValueError:
                raise
            except Exception as e:
                raise ValueError(f"Failed reading CSV seed-file {path}: {e}")
        else:
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
    # ------------ Config I/O helpers -----------------
    ##############################################
    @staticmethod
    def _get_kwok_config_file(path: str) -> Path:
        """
        Validate and return the single KWOK configuration file.
        """
        p = Path(path)
        if not p.exists() or not p.is_file():
            raise SystemExit(f"--config-file not found: {p}")
        if p.suffix.lower() not in (".yaml", ".yml"):
            raise SystemExit(f"--config-file must be a YAML file: {p}")
        return p

    @staticmethod
    def _pick_runner_doc(docs: List[Any]) -> Dict[str, Any]:
        """
        Pick the KwokRunConfiguration document from the list of YAML documents.
        """
        for d in docs:
            if isinstance(d, dict) and (str(d.get("kind","")) == "KwokRunConfiguration"):
                return d
        raise KeyError("No Kwok runner settings found in YAML (expected 'kind: KwokRunConfiguration').")

    def _load_run_config(self, cfg_path: Path) -> TestConfigRaw:
        """
        Load the runner configuration from the given YAML file.
        """
        with open(cfg_path, "r", encoding="utf-8") as f:
            docs = list(yaml.safe_load_all(f)) or []
        runner_doc = KwokTestGenerator._pick_runner_doc(docs)

        tr = TestConfigRaw(source_file=cfg_path)
        tr.namespace      = get_str_from_dict(runner_doc, "namespace", tr.namespace)
        tr.num_nodes      = get_int_from_dict(runner_doc, "num_nodes", tr.num_nodes)
        tr.num_pods       = get_int_from_dict(runner_doc, "num_pods", tr.num_pods)

        tr.util           = get_float_from_dict(runner_doc, "util", tr.util)

        tr.wait_pod_mode      = self._get_wait_pod_mode_from_dict(runner_doc, "wait_pod_mode", tr.wait_pod_mode)
        tr.wait_pod_timeout   = get_str_from_dict(runner_doc, "wait_pod_timeout", tr.wait_pod_timeout)
        tr.settle_timeout_min = get_str_from_dict(runner_doc, "settle_timeout_min", tr.settle_timeout_min)
        tr.settle_timeout_max = get_str_from_dict(runner_doc, "settle_timeout_max", tr.settle_timeout_max)

        raw_np = normalize_interval(runner_doc, ("num_priorities","num_priorities_lo","num_priorities_hi"))
        if raw_np:
            tr.num_priorities = parse_int_interval(raw_np, min_lo=1)

        raw_cpu = normalize_interval(runner_doc, ("cpu_per_pod","cpu_per_pod_lo","cpu_per_pod_hi"))
        if raw_cpu:
            tr.cpu_per_pod = parse_qty_interval(raw_cpu)

        raw_mem = normalize_interval(runner_doc, ("mem_per_pod","mem_per_pod_lo","mem_per_pod_hi"))
        if raw_mem:
            tr.mem_per_pod = parse_qty_interval(raw_mem)

        raw_rps = normalize_interval(runner_doc, ("num_replicas_per_rs","num_replicas_per_rs_lo","num_replicas_per_rs_hi"))
        if raw_rps:
            tr.num_replicas_per_rs = parse_int_interval(raw_rps, min_lo=1)

        return tr

    @staticmethod
    def _validate_config(tr: TestConfigRaw) -> tuple[bool, str]:
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

    def _validate_all_configs(self, cfgs: List[Path]) -> None:
        """
        Validate all provided configuration files.
        """
        any_fail = False
        for cfg in cfgs:
            try:
                raw = self._load_run_config(cfg)
                ok, msg = KwokTestGenerator._validate_config(raw)
                if not ok:
                    LOG.error(f"config-failed {cfg}: {msg}")
                    any_fail = True
            except Exception as e:
                LOG.error(f"config-failed {cfg}: {e}")
                any_fail = True
                continue
        if any_fail:
            raise SystemExit("One or more configs failed validation; see messages above and fix them.")
        LOG.info(f"all configs validated successfully.")
    
    def _resolve_config_for_seed(self, tr: TestConfigRaw, seed_int: int) -> TestConfigApplied:
        """
        Resolve the raw config into a fully specified applied config for the given seed.
        """
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
        if tr.num_replicas_per_rs is not None:  # RS mode
            rng_rs_sizes = seeded_random(seed_int, "rs-sizes")
            rng_rs_cpu   = seeded_random(seed_int, "rs-cpu")
            rng_rs_mem   = seeded_random(seed_int, "rs-mem")
            rs_sets = self._gen_rs_sizes(rng_rs_sizes, tr.num_pods, tr.num_replicas_per_rs)
            rs_cpu = [rng_rs_cpu.randint(cpu_m[0], cpu_m[1]) for _ in rs_sets]
            rs_mem = [rng_rs_mem.randint(mem_b[0], mem_b[1]) for _ in rs_sets]
            cpu_parts = [v for i, v in enumerate(rs_cpu) for _ in range(rs_sets[i])]
            mem_parts = [v for i, v in enumerate(rs_mem) for _ in range(rs_sets[i])]
        else:  # Standalone
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
            node_cpu_m=per_node_m,
            node_mem_b=per_node_b,
            num_priorities=num_prios,
            num_replicaset=num_rs_sets,
            num_replicas_per_rs=tr.num_replicas_per_rs,
            util=tr.util,
            cpu_per_pod_m=cpu_m_used,
            mem_per_pod_b=mem_b_used,
            wait_pod_mode=wait_pod_mode,
            wait_pod_timeout_s=wait_pod_timeout_s,
            settle_timeout_min_s=settle_timeout_min_s,
            settle_timeout_max_s=settle_timeout_max_s,
            source_file=tr.source_file,
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
        if not getattr(self.args, "pause", False): return
        if not next_exists: return
        if not sys.stdin.isatty():
            LOG.info("[pause] requested but stdin is not a TTY; continuing.")
            return
        try:
            input("[pause] Press Enter to continue (Ctrl+C to abort)... ")
        except KeyboardInterrupt:
            raise SystemExit("Aborted by user during pause.")

    def _write_fail(self, category: str, cfg: Path, seed: int | None, phase: str, message: str, details: str = "") -> None:
        """
        Append a structured row to the failed-file so silent failures are visible.
        Format: ts  category  cfg  seed  phase  message  details
        """
        if self._suppress_fail_log:
            self._failure = _Failure(category, cfg, seed, phase, message, details)
            return
        ts = time.strftime("%Y/%m/%d/%H:%M:%S", time.localtime())
        cfg_s = str(cfg); sd = "-" if seed is None else str(seed)
        line = f"{ts}\t{category}\t{cfg_s}\t{sd}\t{phase}\t{message}"
        if details:
            compact = details.replace("\n", "\\n")[-1500:]
            line += f"\t{compact}"
        with open(self.failed_f, "a", encoding="utf-8") as f:
            f.write(line + "\n")

    def _print_run_header(self, s, cfg_name, seed_idx, seeds_total, cfg_idx, cfgs_total):
        """
        Print a header for the seed run.
        """
        seed_str = "unlimited" if seeds_total <= -1 else f"{seed_idx}/{seeds_total}"
        LOG.info(
            "\n------------------------------------------------- SEED RUN %s -------------------------------------------------\n"
            "seed=%s config=%s (%s/%s)\n"
            "----------------------------------------------------------------------------------------------------------------",
            seed_str, s, cfg_name, cfg_idx, cfgs_total
        )

    def _print_seed_summary(self, cfg: Path, seed: int | None, running: int | None, unscheduled: int | None, note: str = "") -> None:
        """
        Print a summary line for the run.
        """
        note = f" note='{note}'" if note else ""
        running_str = "-" if running is None else str(running)
        unscheduled_str = "-" if unscheduled is None else str(unscheduled)
        LOG.info("\n------------------------------------------------- SEED SUMMARY -------------------------------------------------\n"
                 f"{cfg.name}  seed={seed}  running={running_str}  unscheduled={unscheduled_str}  {note}\n"
                 "----------------------------------------------------------------------------------------------------------------")

    ##############################################
    # ------------ Run Paths ----------------
    ##############################################
    def _run_gen_seeds(self):
        """
        Generate random seeds and write them to one or multiple files.
        Modes:
        --generate-seeds-to-file PATH NUM
        --generate-seeds-to-file PATH NUM PARTS
        If PARTS provided and PATH contains '{i}', substitute it with 1..PARTS.
        Else, create PATH_part-<i>(.ext)
        """
        argsv = self.args.gen_seeds_to_file
        if not argsv or len(argsv) not in (2, 3):
            raise SystemExit("--gen-seeds-to-file requires PATH NUM [PARTS]")
        path_str, num_str = argsv[0], argsv[1]
        try:
            total = int(num_str)
        except Exception:
            raise SystemExit("--gen-seeds-to-file: NUM must be an integer")
        if total < 1:
            raise SystemExit("--gen-seeds-to-file: NUM must be >= 1")
        parts = 1
        if len(argsv) == 3:
            try:
                parts = int(argsv[2])
            except Exception:
                raise SystemExit("--gen-seeds-to-file: PARTS must be an integer")
            if parts < 1:
                raise SystemExit("--gen-seeds-to-file: PARTS must be >= 1")
            if parts > total:
                raise SystemExit("--gen-seeds-to-file: PARTS cannot exceed NUM")
        # RNG + seeds
        now_ns = int(time.time_ns())
        r = seeded_random(now_ns, "base")
        seeds = [(r.getrandbits(63) or 1) for _ in range(total)]
        # Split even-ish across parts
        base = total // parts
        rem = total % parts
        def _resolve_out(i: int) -> Path:
            if "{i}" in path_str:
                return Path(path_str.replace("{i}", str(i)))
            # No template; if multiple parts, add _part-i before extension inline
            p = Path(path_str)
            if parts > 1:
                if p.suffix:
                    return p.with_name(f"{p.stem}-{i}{p.suffix}")
                else:
                    return p.with_name(f"{p.name}-{i}")
            return p  # single file
        # Ensure parent dir of first output exists
        first_out = _resolve_out(1)
        first_out.parent.mkdir(parents=True, exist_ok=True)
        offset = 0
        written_total = 0
        for i in range(1, parts + 1):
            size = base + (1 if i <= rem else 0)
            chunk = seeds[offset:offset + size]
            offset += size
            outp = _resolve_out(i)
            outp.parent.mkdir(parents=True, exist_ok=True)
            with open(outp, "w", encoding="utf-8", newline="") as f:
                for s in chunk:
                    f.write(f"{s}\n")
            LOG.info("wrote %d seeds to %s", len(chunk), outp)
            written_total += len(chunk)
        LOG.info("wrote %d seeds across %d file(s)", written_total, parts)

    def _run_count_path(self, cfg: Path, cfg_idx: int, cfgs_total: int, tr: TestConfigRaw, seen: set[int]) -> None:
        """
        Run the generator for configuration(s) files and a seed count. If count=-1, run indefinitely.
        """
        to_make = int(self.args.count)  # target number of SAVED rows; -1 = infinite
        now_ns = int(time.time_ns())
        rng = seeded_random(now_ns, "base")
        made = 0

        while to_make == -1 or self._saved_not_all_running < to_make:
            s = rng.getrandbits(63) or 1
            seed_idx = made + 1
            seeds_total = to_make  # may be -1 (for ETA cosmetics)
            self._print_run_header(s, cfg.name, seed_idx, to_make, cfg_idx, cfgs_total)
            self._eta_update_marker(cfg_idx - 1, cfgs_total, made, seeds_total)

            started_at = time.time()
            try:
                rc = self._resolve_config_for_seed(tr, s)
                target_repeats = max(1, int(getattr(self.args, "repeats", 1)))
                successes = 0
                while successes < target_repeats and (to_make == -1 or self._saved_not_all_running < to_make):
                    run_idx = successes + 1
                    ok = self._run_single_seed_with_retries(cfg, seen, s, rc, run_idx=run_idx)
                    if ok:
                        successes += 1
                    if successes < target_repeats and (to_make == -1 or self._saved_not_all_running < to_make):
                        self._pause(next_exists=True)
            finally:
                self._eta_record_seed_duration(started_at)
                made += 1
                self._eta_update_marker(cfg_idx - 1, cfgs_total, made, seeds_total)
                self._eta_summary(cfg_idx, cfgs_total, seed_idx + 1, seeds_total)

            if to_make == -1:
                # keep going forever
                self._pause(next_exists=True)
            else:
                # stop will happen via while condition
                self._pause(next_exists=(self._saved_not_all_running < to_make))

        # finalize ETA marker
        if to_make == -1:
            self._eta_update_marker(cfg_idx, cfgs_total, made, -1)
        else:
            self._eta_update_marker(cfg_idx, cfgs_total, to_make, to_make)

    def _run_seed_single_path(self, cfg: Path, cfg_idx: int, cfgs_total: int, tr: TestConfigRaw, seen: set[int]) -> None:
        """
        Run the generator for configuration(s) files and a single seed.
        """
        s = int(self.args.seed)
        self._print_run_header(s, cfg.name, 1, 1, cfg_idx, cfgs_total)
        self._eta_update_marker(cfg_idx - 1, cfgs_total, 0, 1)
        started_at = time.time()
        try:
            rc = self._resolve_config_for_seed(tr, s)
            target_repeats = max(1, int(getattr(self.args, "repeats", 1)))
            successes = 0
            while successes < target_repeats:
                run_idx = successes + 1
                ok = self._run_single_seed_with_retries(cfg, seen, s, rc, run_idx=run_idx)
                if ok:
                    successes += 1
                if successes < target_repeats:
                    self._pause(next_exists=True)
        finally:
            self._eta_record_seed_duration(started_at)
            self._eta_update_marker(cfg_idx - 1, cfgs_total, 1, 1)
            self._eta_summary(cfg_idx, cfgs_total, 2, 1)
        self._eta_update_marker(cfg_idx, cfgs_total, 1, 1)
        return

    def _run_seed_file_path(self, cfg: Path, cfg_idx: int, cfgs_total: int, tr: TestConfigRaw, seen: set[int]) -> None:
        """
        Run the generator for a specific configuration and seeds from a file.
        """
        seeds_list = self._read_seeds_file(Path(self.args.seed_file))
        seeds_total = len(seeds_list)
        for seed_idx, s in enumerate(seeds_list, start=1):
            self._print_run_header(s, cfg.name, seed_idx, seeds_total, cfg_idx, cfgs_total)
            self._eta_update_marker(cfg_idx - 1, cfgs_total, seed_idx - 1, seeds_total)
            s = int(s)
            started_at = time.time()
            try:
                rc = self._resolve_config_for_seed(tr, s)
                target_repeats = max(1, int(getattr(self.args, "repeats", 1)))
                successes = 0
                while successes < target_repeats:
                    # If we already met the quota, stop
                    if self.args.seeds_not_all_running > 0 and self._saved_not_all_running >= self.args.seeds_not_all_running:
                        break
                    run_idx = successes + 1
                    ok = self._run_single_seed_with_retries(cfg, seen, s, rc, self.args.seed_file, run_idx=run_idx)
                    if ok:
                        successes += 1
                    if successes < target_repeats:
                        self._pause(next_exists=True)
            finally:
                self._eta_record_seed_duration(started_at)
                self._eta_update_marker(cfg_idx - 1, cfgs_total, seed_idx, seeds_total)
                self._eta_summary(cfg_idx, cfgs_total, seed_idx + 1, seeds_total)

            # Stop if we hit the saved quota
            if self.args.seeds_not_all_running > 0 and self._saved_not_all_running >= self.args.seeds_not_all_running:
                LOG.info("reached save quota (%d); moving to next config", self.args.seeds_not_all_running)
                break

            self._pause(next_exists=(seed_idx < seeds_total))

        self._eta_update_marker(cfg_idx, cfgs_total, min(seed_idx, seeds_total), seeds_total)
        return

    ##############################################
    # ------------ Seed Run ----------------
    ##############################################
    def _execute_seed_direct(self, cfg: Path, seed: int, ta: "TestConfigApplied") -> bool:
        """
        Direct solving path (no cluster). Generates specs, calls the Python solver,
        writes a compact CSV row, prints a summary with solver status + stats.
        """
        from collections import Counter

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
        resp, meta = self._solve_direct_from_specs(ta, seed, pod_specs, rs_specs)

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
        from collections import Counter
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
        self._print_seed_summary(
            cfg, seed,
            running=running_count,
            unscheduled=unscheduled_total,
            note=(
                f"direct-solving status={status} exec_time_ms={elapsed_ms} "
                f"evicted={len(evicted_uids)} moved={len(moved_from_running_uids)} new={len(new_from_pending_uids)} running_by_priority={{ {per_prio_str} }} "
                f"util_cpu={util_run_cpu:.3f} (old_cpu_util={old_cpu_util:.3f}) util_mem={util_run_mem:.3f} (old_mem_util={old_mem_util:.3f})"
            ),
        )
        return True


    def _execute_seed_on_cluster(
        self,
        cfg: Path,
        seen: set[int],
        seed: int,
        ta: "TestConfigApplied",
        seed_file: Optional[str],
        *,
        run_idx: int = 1,
    ) -> bool:
        """
        Full KWOK path: cluster creation, nodes, namespace/PC, apply workload,
        optional optimizer trigger, settle, snapshot, CSV row, artifacts.
        Mirrors the logic you had in _run_single_seed before.
        """
        phase = "start"
        LOG.info(f"phase={phase}  cfg={cfg.stem}  seed={seed}")
        start_time = time.time()
        rng = seeded_random(seed, "base")

        # cluster
        phase = "ensure_cluster"
        LOG.info(f"phase={phase}")
        try:
            KwokTestGenerator._ensure_kwok_cluster(self.ctx, self.args.kwok_runtime, cfg, recreate=True)
        except Exception as e:
            tb = traceback.format_exc()
            self._write_fail("seed", cfg, seed, phase, str(e), tb)
            LOG.error(f"seed-failed; ensure cluster: {e}")
            self._print_seed_summary(cfg, seed, None, None, "ensure cluster failed")
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
            self._write_fail("seed", cfg, seed, "nodes", str(e), tb)
            LOG.error(f"seed-failed; nodes setup: {e}")
            self._print_seed_summary(cfg, seed, None, None, "nodes setup failed")
            return False

        # namespace & PCs
        phase = "ensure_namespace"
        LOG.info(f"phase={phase}")
        self._ensure_namespace(self.ctx, ta.namespace)
        self._ensure_service_account(self.ctx, ta.namespace, "default")

        phase = "ensure_priority_classes"
        LOG.info(f"phase={phase}")
        self._ensure_priority_classes(self.ctx, ta.num_priorities, prefix="p", start=1)

        # workload
        phase = "apply_workload"
        LOG.info(f"phase={phase}  mode={'RS' if ta.num_replicaset>0 else 'standalone'}")
        pod_specs: list[dict] = []
        rs_specs:  list[dict] = []
        if ta.num_replicaset > 0:
            rs_specs = self._apply_replicasets(ta, rng)
        else:
            pod_specs = self._apply_standalone_pods(ta, rng)

        # optionally trigger the optimizer
        if self.args.trigger_optimizer:
            phase = "wait_settle_before_optimizer"
            LOG.info(f"phase={phase} timeout_min={ta.settle_timeout_min_s}s")
            time.sleep(ta.settle_timeout_min_s)
            phase = "trigger_optimizer"
            LOG.info(f"phase={phase} url={self.args.optimizer_url}")
            code, body = self._trigger_optimizer_http(self.args.optimizer_url)
            body_compact = (body or "").replace("\n", "\\n")
            if len(body_compact) > 600:
                body_compact = body_compact[:600] + "...(truncated)"
            LOG.info(f"optimizer_response code={code} body={body_compact}")
            self.last_optimize_json = None
            if code and isinstance(body, str) and body.strip().startswith("{"):
                try:
                    self.last_optimize_json = json.loads(body)
                except Exception:
                    self.last_optimize_json = None

        # wait & settle
        phase = "wait_settle_before_check"
        LOG.info(f"phase={phase} timeout_min={ta.settle_timeout_min_s}s")
        time.sleep(ta.settle_timeout_min_s)
        if ta.settle_timeout_max_s > 0 and getattr(self.args, "active_url", None):
            LOG.info(f"waiting for inactive scheduler (max {ta.settle_timeout_max_s}s)")
            _ = self._wait_optimizer_inactive_http(self.args.active_url, ta.settle_timeout_max_s)

        # status snapshot
        phase = "status_snapshot"
        LOG.info(f"phase={phase}")
        snap = stat_snapshot(self.ctx, ta.namespace, expected=ta.num_pods)

        # validate counts
        running_count = len(snap.pods_running)
        unsched_count = len(snap.pods_unscheduled)
        if running_count + unsched_count != ta.num_pods:
            phase = "snapshot_validation"
            self._write_fail("seed", cfg, seed, phase,
                            f"pod count mismatch: expected {ta.num_pods}, got {running_count}+{unsched_count}={running_count+unsched_count}")
            LOG.warning("treating seed as failure: pod count mismatch: expected %d, got %d+%d=%d",
                        ta.num_pods, running_count, unsched_count, running_count+unsched_count)
            self._print_seed_summary(cfg, seed, running_count, unsched_count, "pod count mismatch")
            return False

        # optionally skip “all running”
        if unsched_count == 0 and self.args.seeds_not_all_running > 0:
            single_seed_mode = (self.args.seed is not None and self.args.seed_file is None and self.args.count is None)
            under_limit = (self._saved_not_all_running < self.args.seeds_not_all_running)
            if single_seed_mode or under_limit:
                self._record_skipped_all_running_seed(cfg, seed, seed_file, running_count)
                self._print_seed_summary(cfg, seed, running_count, unsched_count, "skipped (all pods running)")
                LOG.info("skipped saving seed=%s (all pods running)", seed)
                return True

        # attempts / baseline / stages
        baseline, best_name, attempts, error = self._get_solver_attempts()
        best_score, best_duration_us, best_status = self._extract_best_attempt_fields(best_name, attempts)

        result_row = {
            "timestamp": get_timestamp(),
            "kwok_config": str(cfg),
            "seed_file": seed_file,
            "seed": str(seed),
            "error": error,
            "baseline": json.dumps(baseline, separators=(",", ":"), sort_keys=True),
            "best_name": best_name,           
            "best_score": (best_score if best_score is not None else ""),
            "best_duration_us": (int(best_duration_us) if best_duration_us is not None else ""),
            "best_status": (best_status or ""),
            "num_nodes": ta.num_nodes,
            "num_pods": ta.num_pods,
            "num_priorities": ta.num_priorities,
            "num_replicaset": ta.num_replicaset,
            "num_replicas_per_rs_lo": split_interval(ta.num_replicas_per_rs)[0],
            "num_replicas_per_rs_hi": split_interval(ta.num_replicas_per_rs)[1],
            "node_cpu_m": int(ta.node_cpu_m),
            "node_mem_b": int(ta.node_mem_b),
            "cpu_per_pod_m_lo": split_interval(ta.cpu_per_pod_m)[0],
            "cpu_per_pod_m_hi": split_interval(ta.cpu_per_pod_m)[1],
            "mem_per_pod_b_lo": split_interval(ta.mem_per_pod_b)[0],
            "mem_per_pod_b_hi": split_interval(ta.mem_per_pod_b)[1],
            "util": f"{ta.util:.3f}",
            "util_run_cpu": f"{snap.cpu_run_util:.3f}",
            "util_run_mem": f"{snap.mem_run_util:.3f}",
            "cpu_m_run": int(sum(snap.cpu_req_by_node.values())),
            "mem_b_run": int(sum(snap.mem_req_by_node.values())),
            "wait_pod_mode": (ta.wait_pod_mode or ""),
            "wait_pod_timeout_s": ta.wait_pod_timeout_s,
            "settle_timeout_min_s": ta.settle_timeout_min_s,
            "settle_timeout_max_s": ta.settle_timeout_max_s,
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
            "solver_attempts": json.dumps(attempts, separators=(",", ":"), sort_keys=True),
        }

        phase = "write_results"
        LOG.info(f"phase={phase}")
        exists = seed in seen
        if self.args.overwrite and exists and (self.args.repeats <= 1):
            removed = self._remove_seed_from_results(seed)
            LOG.info("overwrite: removed %d existing row(s) for seed=%s", removed, seed)
        self._append_result_csv(result_row)
        LOG.info("appended to %s", self.results_f)

        self._saved_not_all_running += 1

        if self.args.save_solver_stats:
            phase = "solver_stats"
            LOG.info(f"phase={phase}")
            self._write_solver_stats_json(seed, run_idx)

        if self.args.save_scheduler_logs:
            phase = "save_scheduler_logs"
            LOG.info(f"phase={phase}")
            self._save_scheduler_logs(seed, run_idx=run_idx)

        LOG.info(f"seed run done; took {time.time()-start_time:.1f}s")
        self._print_seed_summary(cfg, seed, running_count, unsched_count)
        return True

    def _run_single_seed(
        self,
        cfg: Path,
        seen: set[int],
        seed: int,
        ta: "TestConfigApplied",
        seed_file: Optional[str] = None,
        *,
        run_idx: int = 1
    ) -> bool:
        """
        Dispatcher that picks the execution path and handles overwrite/exists
        skip semantics common to both paths.
        """
        # existing-row skip: keep same behavior
        exists = seed in seen
        if exists and (not self.args.overwrite) and (self.args.repeats <= 1):
            self._print_seed_summary(cfg, seed, None, None, "skip (exists; use --overwrite to replace)")
            LOG.info("skip seed=%s because it already exists and --overwrite is not set", seed)
            return True

        if self.args.direct_solving:
            return self._execute_seed_direct(cfg, seed, ta)
        else:
            return self._execute_seed_on_cluster(cfg, seen, seed, ta, seed_file, run_idx=run_idx)

    # ------ Single seed run with retries ---------------------
    def _run_single_seed_with_retries(self, cfg: Path, seen: set[int], seed: int, ta: TestConfigApplied, seed_file: Optional[str] = None, *, run_idx: int = 1) -> bool:
        # Short-circuit: skip existing when not overwriting.
        # Allow repeats to append additional rows even if the seed already exists.
        if (seed in seen) and (not self.args.overwrite) and (self.args.repeats <= 1):
            self._print_seed_summary(cfg, seed, None, None, "skip (exists; use --overwrite to replace)")
            LOG.info("skip seed=%s because it already exists and --overwrite is not set", seed)
            return True
        max_attempts = max(1, self.args.retries, 0) + 1
        overall_started = time.time()
        last_attempt = 0
        for attempt in range(1, max_attempts + 1):
            last_attempt = attempt
            LOG.info("attempt %d/%d for seed=%s", attempt, max_attempts, seed)
            # Suppress fail-file writes for all but the final attempt
            self._suppress_fail_log = (attempt < max_attempts)
            self._failure = None
            # If overwriting, remove old rows before the first *successful* save; do it right before we save
            # We handle removal inside the success block below.
            ok = self._run_single_seed(cfg, seen, seed, ta, seed_file, run_idx=run_idx)
            if ok:
                # If overwrite=true and there were rows, prune then append (we append inside _run_single_seed already)
                LOG.info("seed=%s succeeded on attempt %d; total %.1fs", seed, attempt, time.time() - overall_started)
                self._suppress_fail_log = False
                self._failure = None
                return True
            else:
                LOG.warning("seed=%s failed on attempt %d/%d", seed, attempt, max_attempts)
        # If we get here, all attempts failed. Flush the last deferred fail line now.
        self._suppress_fail_log = False
        if self._failure:
            df = self._failure
            self._failure = None
            self._write_fail(df.category, df.cfg, df.seed, df.phase, df.message, df.details)
        LOG.error("seed=%s failed after %d attempt(s); total %.1fs", seed, last_attempt, time.time() - overall_started)
        return False

    ##############################################
    # ------------ Main Runner -------------------
    ##############################################
    def run(self) -> None:
        """
        Main runner function.
        """
        # --- generate-and-exit mode ---
        if self.args.gen_seeds_to_file is not None:
            self._run_gen_seeds()
            return

        # Check arguments
        if not self.args.matrix_file and not self.args.config_file: # matrix mode doesn't need --config-file here
            raise SystemExit("--config-file is required (unless --matrix-file is used)")
        if self.args.seed is not None and self.args.seed < 1:
            raise SystemExit("--seed must be a positive integer")
        if self.args.seed_file and self.args.count is not None:
            raise SystemExit("--seed-file cannot be used with --count")
        if self.args.seed is not None and self.args.count is not None:
            raise SystemExit("--seed cannot be used with --count")
        if self.args.count is not None and self.args.count < -1:
            raise SystemExit("--count must be -1 (infinite) or a positive integer")

        # --- check seed file and config file ---
        if self.args.seed_file:
            file_exists(self.args.seed_file)
        if not self.args.matrix_file:
            file_exists(self.args.config_file)

        # --- check kwok runtime vs. trigger-optimizer ---
        if self.args.trigger_optimizer and not self.args.optimizer_url:
            raise SystemExit("--trigger-optimizer requires --optimizer-url")
        if self.args.trigger_optimizer and self.args.kwok_runtime != "binary":
            raise SystemExit("--trigger-optimizer requires --kwok-runtime=binary")

        seed_file_str = ", seed-file=" + self.args.seed_file if self.args.seed_file else ""
        LOG.info(f"starting; cluster={self.args.cluster_name}  runtime={self.args.kwok_runtime}, config-file={self.args.config_file}{seed_file_str}")

        cfg = self._get_kwok_config_file(self.args.config_file)
        self._validate_all_configs([cfg])
        cfg_idx = getattr(self.args, "matrix_idx", 1)
        cfgs_total = getattr(self.args, "matrix_total", 1)
        LOG.info(f"\n================================================ CONFIG RUN {cfg_idx}/{cfgs_total} ===================================================\n"
                    f"config={cfg}\n"
                    f"----------------------------------------------------------------------------------------------------------------")
        try:
            raw = self._load_run_config(cfg)
            KwokTestGenerator._log_config_raw(raw, use_logger=True)
        except Exception as e:
            LOG.error(f"config-failed {cfg}: {e}")
            with open(self.failed_f, "a", encoding="utf-8") as f:
                f.write(f"config\t{cfg}\t-\t{e}\n")
            self._print_seed_summary(cfg, self.args.seed, None, None, "config load failed")
            return

        ok, msg = KwokTestGenerator._validate_config(raw)
        if not ok:
            LOG.error(f"config-failed {cfg}: {msg}")
            with open(self.failed_f, "a", encoding="utf-8") as f:
                f.write(f"config\t{cfg}\t-\t{msg}\n")
            self._print_seed_summary(cfg, self.args.seed, None, None, "validation failed")
            return

        self.ctx = f"kwok-{self.args.cluster_name}"

        # Resolve results dir:
        # - If --results-dir is provided, use it exactly.
        # - If omitted, use ./results relative to CWD.
        if self.args.results_dir:
            rd = Path(self.args.results_dir).resolve()
        else:
            rd = Path("./results").resolve()

        rd.mkdir(parents=True, exist_ok=True)
        self.results_dir = rd
        if getattr(self.args, "clean_results", False):
            LOG.info("clean-results requested: deleting all contents of %s", self.results_dir)
            self._clean_results_dir()
        self.results_f = self.results_dir / "results.csv"

        # failures file lives in results dir
        self.failed_f = self.results_dir / "failed.csv"

        LOG.info(f"results-dir resolved to: {self.results_dir}")
        
        self._eta_init()
        
        seen = self._load_seen_results()
        
        # enter count mode when only --seeds-not-all-running is provided
        if self.args.seeds_not_all_running not in (-1, 0) and self.args.seeds_not_all_running < 1:
            raise SystemExit("--seeds-not-all-running must be -1 (infinite), 0 (disabled), or >= 1")
        if (self.args.seed is None and not self.args.seed_file and self.args.count is None
            and self.args.seeds_not_all_running > 0):
            self.args.count = self.args.seeds_not_all_running
            LOG.info("no --seed/--seed-file/--count provided; defaulting --count=%d from --seeds-not-all-running", self.args.count)

        # single-seed path
        if self.args.seed is not None:
            self._run_seed_single_path(cfg, cfg_idx, cfgs_total, raw, seen)
            return
        # count path
        if self.args.count is not None and int(self.args.count) >= -1:
            self._run_count_path(cfg, cfg_idx, cfgs_total, raw, seen)
            return
        # seed-file path
        if self.args.seed_file:
            self._run_seed_file_path(cfg, cfg_idx, cfgs_total, raw, seen)
            return

        LOG.error(f"no seeds provided for cfg={cfg.name} (use --seed / --seed-file / --count).")

##############################################
# ------------ Matrix runner -----------------
##############################################
MATRIX_REQUIRED_COLS = ["config-file", "results-dir"]

def run_matrix(args) -> int:
    def _read_csv_matrix(path: str):
        rows = []
        with open(path, "r", encoding="utf-8") as f:
            rdr = csv.DictReader(f)
            if rdr.fieldnames is None:
                raise SystemExit(f"[matrix] CSV {path} has no header")
            # normalize column names we care about
            missing_required = [c for c in MATRIX_REQUIRED_COLS if c not in rdr.fieldnames]
            if missing_required:
                raise SystemExit(
                    f"[matrix] CSV {path} missing required columns: {', '.join(missing_required)}"
                )
            for i, row in enumerate(rdr, 2):
                cleaned = {k: (row.get(k, "") or "").strip() for k in rdr.fieldnames}
                # enforce required columns non-empty
                for c in MATRIX_REQUIRED_COLS:
                    if not cleaned.get(c):
                        raise SystemExit(f"[matrix] {path}:{i} column '{c}' is empty")
                # optional controls
                seed_file = cleaned.get("seed-file", "")
                snar_raw  = cleaned.get("seeds-not-all-running", "")
                have_seed_file = bool(seed_file)
                have_snar      = bool(snar_raw)
                # must have at least one of the two
                if not have_seed_file and not have_snar:
                    raise SystemExit(
                        f"[matrix] {path}:{i} requires either 'seed-file' or 'seeds-not-all-running' (or both)."
                    )
                # validate files if present
                cf = Path(cleaned["config-file"])
                if not cf.exists():
                    raise SystemExit(f"[matrix] {path}:{i} config-file not found: {cf}")
                if have_seed_file:
                    sf = Path(seed_file)
                    if not sf.exists():
                        raise SystemExit(f"[matrix] {path}:{i} seed-file not found: {sf}")
                # validate seeds-not-all-running if present
                snar_val = None
                if have_snar:
                    try:
                        snar_val = int(snar_raw)
                    except ValueError:
                        raise SystemExit(f"[matrix] {path}:{i} seeds-not-all-running must be an integer")
                    if snar_val < 1 and snar_val != -1:
                        raise SystemExit(
                            f"[matrix] {path}:{i} seeds-not-all-running must be -1 (infinite) or >= 1"
                        )
                cleaned["_have_seed_file"] = have_seed_file
                cleaned["_have_snar"] = have_snar
                cleaned["_snar_val"] = snar_val
                rows.append(cleaned)

        if not rows:
            raise SystemExit(f"[matrix] CSV {path} is empty")
        return rows

    def _run_one_row(row: dict, idx: int, total: int) -> int:
        # Clone a shallow args namespace
        ns = argparse.Namespace(**vars(args))
        ns.config_file  = row["config-file"]
        ns.results_dir  = row["results-dir"]
        ns.matrix_file  = None  # prevent recursion
        ns.matrix_idx   = idx
        ns.matrix_total = total
        # Clear seed/count/seed_file; set based on row content
        ns.seed       = None
        ns.seed_file  = None
        ns.count      = None
        # Carry over seeds-not-all-running if present
        if row.get("_have_snar"):
            ns.seeds_not_all_running = row["_snar_val"]
        # else keep whatever CLI had (default or explicit)
        # If a seed-file is provided, use it.
        if row.get("_have_seed_file"):
            ns.seed_file = row["seed-file"]
            # In seed-file mode we do NOT force count; runner already
            # caps _seeds_not_all_running_limit to the file size.
        else:
            # No seed-file: if we have seeds-not-all-running, run in count mode with that number
            if row.get("_have_snar"):
                ns.count = row["_snar_val"]  # -1 = infinite also supported
            # else: (shouldn’t happen due to validation) — leave as is
        print(
            f"\n======================= MATRIX RUN {idx}/{total} =========================\n"
            f"[matrix] config-file={ns.config_file}\n"
            f"[matrix] results-dir={ns.results_dir}\n"
            f"[matrix] seed-file={ns.seed_file or '<none>'}\n"
            f"[matrix] seeds-not-all-running={getattr(ns, 'seeds_not_all_running', 0)}\n"
            f"[matrix] count={ns.count if ns.count is not None else '<unset>'}\n"
            "------------------------------------------------------------------",
            flush=True,
        )
        # Run directly
        runner = KwokTestGenerator(ns)
        KwokTestGenerator._log_args(ns, use_logger=False)
        try:
            runner.run()
            return 0
        except SystemExit as e:
            return int(e.code) if isinstance(e.code, int) else 1
    rows = _read_csv_matrix(args.matrix_file)
    total = len(rows)
    overall_rc = 0
    for idx, row in enumerate(rows, start=1):
        rc = _run_one_row(row, idx, total)
        if rc != 0:
            overall_rc = rc  # remember last non-zero status
    return overall_rc

##############################################
# ------------ CLI ---------------------------
##############################################
def build_argparser() -> argparse.ArgumentParser:
    """
    Build the argument parser for the KWOK test generator.
    """
    ap = argparse.ArgumentParser(description="Generator of KWOK test clusters and workloads.")
    
    # general
    ap.add_argument("--cluster-name", dest="cluster_name", default="kwok1", help="A unique KWOK cluster name (default: kwok1).")
    ap.add_argument("--kwok-runtime", dest="kwok_runtime", default="docker",
                    help="KWOK runtime 'binary' or 'docker' (default: docker).")
    ap.add_argument("--config-file", dest="config_file", help="Path to a single KWOK config YAML")
    ap.add_argument("--results-dir", dest="results_dir", default=None,
                    help=("Directory to store results. If omitted, results are written to ./results"))
    ap.add_argument("--overwrite", action="store_true", help="Replace any existing results for the same seed.")
    ap.add_argument("--clean-results", dest="clean_results", action="store_true",
                    help="Before running, delete all contents of --results-dir.")

    # seeds
    ap.add_argument("--gen-seeds-to-file", "--gen-seeds-to-file", dest="gen_seeds_to_file", nargs="+", metavar="PATH NUM [PARTS]",
                    help=("Write NUM random seeds to PATH, then exit. "
                        "Optionally split into PARTS files. If PATH contains '{i}', it will be "
                        "replaced by 1..PARTS; otherwise files are suffixed with _part-<i>.")
    )
    ap.add_argument("--seed", type=int, default=None, help="Run exactly this seed (per kwok-config)")
    ap.add_argument("--seed-file", dest="seed_file", default=None, help="Path to seeds file (CSV with 'seed' col or newline list).")
    ap.add_argument("--count", type=int, default=None, help="Generate the specified number of random seeds; -1=infinite.")
    ap.add_argument("--retries", type=int, default=5,
                    help="If a seed fails, retry this many times before recording as failed (default: 5).")
    ap.add_argument("--repeats", type=int, default=1,
                    help=("Number of successful runs to collect per seed (default: 1). "
                            "Each successful repeat writes per-run artifacts named with _run-<idx> and prunes "
                            "any existing file that collides with that name. Failed repeats are retried per --retries "
                            "and do not count toward this number."))
    ap.add_argument("--seeds-not-all-running", dest="seeds_not_all_running", type=int, default=0,
                    help=("If >0, save save up to this many seeds where not all pods are running. "
                            "In --count mode the value is capped to --count (except -1), "
                            "and in --seed-file mode it's capped to the number of seeds in the file. "
                            "In single-seed mode, the seed is saved only if not all pods are running."))
    
    # matrix
    ap.add_argument("--matrix-file", help="CSV with columns: config-file,seed-file,results-dir.")

    # solver stats
    ap.add_argument("--save-solver-stats", dest="save_solver_stats", action="store_true",
                    help="Save solver stats for each seed under <results-dir>/solver-stats")
    
    # scheduler logs
    ap.add_argument("--save-scheduler-logs", dest="save_scheduler_logs", action="store_true",
                    help="After saving stats for each seed, save 'kwokctl logs kube-scheduler --name <cluster>' under <results-dir>/scheduler-logs as scheduler-logs_seed-<seed>.log")
    
    # logging
    ap.add_argument("--log-level", dest="log_level", default=os.environ.get("KWOK_LOG_LEVEL", "INFO"),
                    help="Logging level (DEBUG, INFO, WARNING, ERROR) (default: INFO)")
    
    # pause
    ap.add_argument("--pause", action="store_true", help="Pause for Enter between seeds and between configs.")
    
    # manual HTTP optimizer trigger
    ap.add_argument("--trigger-optimizer", dest="trigger_optimizer", action="store_true",
                    help="After applying all pods for a seed, POST the manual optimizer endpoint.")
    ap.add_argument("--optimizer-url", dest="optimizer_url", default="http://localhost:18080/optimize",
                    help="URL to POST for manual optimizer trigger (default: http://localhost:18080/optimize).")
    ap.add_argument("--active-url", dest="active_url", default="http://localhost:18080/active",
                    help="URL to GET for optimizer active state (default: http://localhost:18080/active)")
    
    # Direct solver
    ap.add_argument("--direct-solving", dest="direct_solving", action="store_true",
                    help="Bypass cluster use; directly call the Python solver with generated nodes/pods.")
    ap.add_argument("--solver-timeout-ms", dest="solver_timeout_ms", type=int, default=10000,
                    help="Timeout for the Python solver (milliseconds).")
    ap.add_argument("--solver-cmd", dest="solver_cmd", type=str, default="python3 scripts/python_solver/main.py",
                    help="Command to run the Python solver. It must read JSON from stdin and write JSON to stdout.")
    ap.add_argument("--export-solver-input", dest="export_solver_input", type=Path, default=None,
                    help="If set, write the exact JSON sent to the solver to this path.")
    ap.add_argument("--export-solver-output", dest="export_solver_output", type=Path, default=None,
                    help="If set, write the solver JSON response to this path.")
    ap.add_argument("--show-solver-exitlog", action="store_true",
                    help="After the solver exits, print its raw stdout/stderr once (useful if not streaming).")
    ap.add_argument("--direct-running-util", dest="direct_running_util", type=float, default=1.0,
                    help="Direct mode only: pre-place pods as already running up to this per-node utilization (0..1).")

    return ap

def main():
    """
    Main entry point for the KWOK test generator.
    """
    args = build_argparser().parse_args()
    
    _ = setup_logging(prefix=f"[cluster={args.cluster_name}] ", level=args.log_level)
    
    # Execute matrix mode if requested
    if args.matrix_file and args.kwok_runtime:
        sys.exit(run_matrix(args))

    # Print args
    KwokTestGenerator._log_args(args, use_logger=True)
    
    # Run the generator
    KwokTestGenerator(args).run()
    
    print("done.")

if __name__ == "__main__":
    main()
