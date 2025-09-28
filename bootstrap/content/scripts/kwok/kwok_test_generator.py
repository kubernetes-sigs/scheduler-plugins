#!/usr/bin/env python3
# kwok_test_generator.py

################################################################################
# Arguments:
#
#   --cluster-name <name>               KWOK cluster name (default: "kwok1")
#
#   --config-dir <dir>                  Directory containing one or more KWOK config YAMLs (default: "./kwok_configs")
#
#   --results-dir <dir>                 Directory to store results CSV files (default: "<config-dir>/results")
#
#   --max-rows-per-file <int>           Maximum rows per CSV file before rotation (default: 500000)
#
#   --failed-file <file>                File to log failed seeds (default: "./failed.csv")
#
#   --seed <int>                        Run exactly this seed (per kwok-config)
#
#   --seed-file <file>                  Path to seeds file (CSV with 'seed' col or newline list)
#
#   --generate-seeds-to-file <file>     Write --count random seeds (one per line, no header) to this file, then exit.
#   --count <int>                       Generate random seeds; -1=infinite
#
#   --test                              Test mode: requires --seed; runs each kwok-config and prints a per-config summary
#                                       of running vs unscheduled at the end. No results are saved in test mode.
################################################################################

import sys, csv, json, time, random, argparse, re, subprocess, tempfile, os, hashlib, math, yaml, traceback, contextlib, fcntl, logging, shlex, shutil
from urllib import request as _urlreq, error as _urlerr
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional, Tuple, List, Dict, Any

from kwok_helpers import (
    get_json_ctx, stat_snapshot, file_exists,
    csv_append_row, count_csv_rows, ensure_csv_with_header,
    qty_to_mcpu_str, qty_to_bytes_str, qty_to_bytes_int, qty_to_mcpu_int,
    yaml_kwok_node, yaml_kwok_pod, yaml_kwok_rs, yaml_priority_class,
    get_timestamp, setup_logging,
    normalize_interval, parse_int_interval, parse_qty_interval, parse_timeout_s, get_int_from_dict, get_float_from_dict, get_str_from_dict, coerce_int_field, split_interval
)

# ===============================================================
# Constants
# ===============================================================
RESULTS_HEADER = [
    "timestamp", "kwok_config", "seed_file", "seed",
    "error", "best_solver",
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
    "running_placed_by_priority", "unschedulable_by_priority",
    "unscheduled", "running",
    "pod_node",
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

# ===============================================================
# KwokTestGenerator
# ===============================================================
class KwokTestGenerator:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.ctx: Optional[str] = None
        self.results_dir: Path | None = None
        self.failed_f: Path | None = None
        self.max_rows_per_file = int(args.max_rows_per_file)
        self.last_optimize_json: dict[str, Any] | None = None

    ######################################################
    # ---------- ETA helpers ----------
    ######################################################
    def _init_eta_state(self):
        """Call once per config before running any seeds."""
        self._eta_started_at = time.time()
        self._seed_durations: list[float] = []

    def _record_seed_duration(self, started_at: float):
        """Call after a seed finishes (success or fail)."""
        try:
            self._seed_durations.append(max(0.0, time.time() - started_at))
        except Exception:
            pass

    def _estimate_eta_epoch(self, cfg_idx: int, cfgs_total: int,
                            seed_idx: int, seeds_total: int) -> float | None:
        """
        Return an epoch seconds ETA, or None if not enough info.
        """
        if not self._seed_durations:
            return None
        avg = sum(self._seed_durations) / max(1, len(self._seed_durations))
        if seeds_total is None or seeds_total <= 0:
            # unknown/infinite
            return None
        seeds_done = max(0, seed_idx - 1)
        seeds_left_current = max(0, seeds_total - seeds_done)
        cfgs_done = max(0, cfg_idx - 1)
        cfgs_left = max(0, cfgs_total - cfgs_done)
        future_configs_left = max(0, cfgs_left - 1)  # exclude current
        seeds_left_future = future_configs_left * seeds_total
        remaining_seeds_overall = seeds_left_current + seeds_left_future
        return time.time() + remaining_seeds_overall * avg

    def _write_completion_file(self, eta_epoch: float | None,
                            cfg_idx: int, cfgs_total: int,
                            seed_idx: int, seeds_total: int):
        """
        Delete stale completion_* and write a new one:
        completion_<time>_configs<at-of-total>_seeds<at-of-total>
        """
        if not self.results_dir:
            return
        try:
            # delete any prior completion_* markers
            for p in self.results_dir.glob("completion_*"):
                try:
                    p.unlink()
                except OSError:
                    pass
            # What we're currently AT
            cfgs_at = max(0, int(cfg_idx))
            cfgs_total = max(0, int(cfgs_total))
            if seeds_total and seeds_total > 0:
                seeds_at = max(0, int(seed_idx))
                seeds_total = int(seeds_total)
            else:
                # Unknown/infinite seed count: keep total as -1, show current index
                seeds_at = max(0, int(seed_idx))
                seeds_total = -1
            if eta_epoch is None:
                time_part = "eta-unknown"
            else:
                time_part = time.strftime("%Y%m%d-%H%M%S", time.localtime(eta_epoch))
            fname = (
                f"completion_{time_part}"
                f"_configs-{cfgs_at}-of-{cfgs_total}"
                f"_seeds-{seeds_at}-of-{seeds_total}"
            )
            fpath = self.results_dir / fname
            # write a tiny payload
            with open(fpath, "w", encoding="utf-8") as fh:
                payload = {
                    "eta_epoch": int(eta_epoch) if isinstance(eta_epoch, (int, float)) else None,
                    "eta_iso": (time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime(eta_epoch)) if eta_epoch else None),
                    "configs_at": cfgs_at, "configs_total": cfgs_total,
                    "seeds_at": seeds_at,   "seeds_total": seeds_total,
                }
                fh.write(json.dumps(payload, separators=(",", ":"), sort_keys=True) + "\n")
        except Exception:
            # best-effort; don't break the run for ETA issues
            pass

    def _update_completion_marker(self, cfg_idx: int, cfgs_total: int,
                                seed_idx: int, seeds_total: int):
        eta_epoch = self._estimate_eta_epoch(cfg_idx, cfgs_total, seed_idx, seeds_total)
        self._write_completion_file(eta_epoch, cfg_idx, cfgs_total, seed_idx, seeds_total)

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
    # ---------- Optimizer HTTP trigger ------------------
    ######################################################
    def _trigger_optimizer_http(self, url: str, *, timeout: float = 60.0) -> tuple[int, str]:
        """
        POST optimizer endpoint. Returns (status_code, body_str).
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

    def _get_active_http(self, url: str, *, timeout: float = 3.0) -> tuple[int, str]:
        """
        GET /active endpoint. Returns (status_code, body_str).
        No retries here; the wait-loop handles transient errors.
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

    @staticmethod
    def _json_get(d: dict, *keys: str, default: Any = None) -> Any:
        """Helper: try a list of key spellings (different casings)."""
        for k in keys:
            if k in d:
                return d[k]
        return default

    @staticmethod
    def _http_solver_stats_from_optimize_payload(payload: dict[str, Any]) -> dict[str, str]:
        """
        Build a dict of solver-stats columns from /optimize response JSON.
        Only includes fields we can confidently derive:
        - 'best'
        - '<best_name>_status'
        - '<best_name>_duration_us'
        - '<best_name>_placed_by_priority'
        - '<best_name>_evictions'
        - '<best_name>_moves'
        - 'optimization_error'
        """
        out: dict[str, str] = {}
        if not isinstance(payload, dict):
            return out

        # read error from several possible keys
        opt_err = KwokTestGenerator._json_get(payload, "error", default="")
        if opt_err is None:
            opt_err = ""
        out["error"] = str(opt_err).strip()

        # Go JSON likely uses camel-cased top-level fields per HttpResponse struct.
        bestSolver = KwokTestGenerator._json_get(payload, "best_solver", default=None)
        if not isinstance(bestSolver, dict):
            return out

        name = KwokTestGenerator._json_get(bestSolver, "name", default="") or ""
        if name:
            out["best_solver"] = str(name)

        if name:
            status = KwokTestGenerator._json_get(bestSolver, "status", default="")
            dur_us = KwokTestGenerator._json_get(bestSolver, "duration_us", default="")
            score  = KwokTestGenerator._json_get(bestSolver, "score", default={}) or {}
            try:
                placed = KwokTestGenerator._json_get(score, "placed_by_priority", default={}) or {}
            except Exception:
                placed = {}
            evicted = KwokTestGenerator._json_get(score, "evicted", default="")
            moved   = KwokTestGenerator._json_get(score, "moved", default="")

            try:
                placed_str = json.dumps(placed, separators=(",", ":"), sort_keys=True)
            except Exception:
                placed_str = ""
            if status is None: status = ""
            if dur_us is None: dur_us = ""
            if evicted is None: evicted = ""
            if moved is None: moved = ""

            slot = str(name)
            out[f"{slot}_status"] = str(status)
            out[f"{slot}_duration_us"] = str(dur_us)
            out[f"{slot}_placed_by_priority"] = placed_str
            out[f"{slot}_evictions"] = str(evicted)
            out[f"{slot}_moves"] = str(moved)

        return out

    ######################################################
    # ---------- Seed derivation --------------
    ######################################################
    @staticmethod
    def _derive_seed(base_seed: int, *labels: object, nbytes: int = 16) -> int:
        """
        Deterministically derive a child seed from a base seed and a sequence of labels.
        This is to make sure that changes in one part of the code do not affect RNG streams in other parts.
        """
        h = hashlib.blake2b(digest_size=nbytes)
        # fixed-width encode base to keep derivations stable
        h.update(int(base_seed).to_bytes(16, "big", signed=False))
        for lab in labels:
            h.update(b"\x00")  # separator
            h.update(str(lab).encode("utf-8"))
        return int.from_bytes(h.digest(), "big")

    @staticmethod
    def _rng(base_seed: int, *labels: object) -> random.Random:
        """
        Convenience: Random() seeded from _derive_seed(base_seed, *labels).
        """
        return random.Random(KwokTestGenerator._derive_seed(base_seed, *labels))

    ######################################################
    # ---------- RS helpers ------------------------------
    ######################################################
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

    ######################################################
    # ---------- CSV helpers & results helpers -----------
    ######################################################
    @staticmethod
    def _prune_non_values(d: dict) -> dict:
        """
        Remove keys whose values are '', None, {}, or [].
        Keep 0/False since those can be meaningful.
        """
        out = {}
        for k, v in d.items():
            if v is None:
                continue
            if v == "":
                continue
            if isinstance(v, (dict, list)) and len(v) == 0:
                continue
            out[k] = v
        return out

    @staticmethod
    def _csv_read_header(path: Path) -> list[str] | None:
        """Return the header row for a CSV file, or None if unreadable/empty."""
        try:
            with open(path, "r", encoding="utf-8", newline="") as fh:
                rdr = csv.reader(fh)
                row = next(rdr, None)
                if row is None:
                    return None
                # normalize by stripping surrounding spaces
                return [c.strip() for c in row]
        except Exception:
            return None

    def _purge_mismatched_csv(self, path: Path, expected_header: list[str]) -> bool:
        """
        If file exists and header != expected_header and --overwrite is set,
        delete the file and return True. Otherwise, return False.
        """
        if not path.exists():
            return False
        actual = self._csv_read_header(path) or []
        exp_norm = [c.strip() for c in expected_header]
        if actual != exp_norm and getattr(self.args, "overwrite", False):
            try:
                path.unlink()
                LOG.info("deleted CSV with mismatched header (overwrite=true): %s", path.name)
                return True
            except OSError as e:
                LOG.warning("failed to delete mismatched CSV %s: %s", path.name, e)
        return False
    
    def _get_best_solver(
        self,
        solver_stats_header: list[str] | None,
        solver_stats_rows: list[dict] | None,
    ) -> tuple[dict, str]:
        """
        Build a compact best-solver object for the CSV single-column, and return (obj, error).
        """
        best = {
            "name": "",
            "status": "",
            "duration_us": "",
            "placed_by_priority": {},
            "evictions": "",
            "moves": "",
        }
        error = ""

        # 1) Prefer fresh /optimize JSON if available
        if self.last_optimize_json:
            try:
                http_kv = self._http_solver_stats_from_optimize_payload(self.last_optimize_json)
                error = (http_kv.get("error") or "").strip()
                name = (http_kv.get("best_solver") or "").strip()
                if name:
                    best["name"] = name
                    best["status"] = str(http_kv.get(f"{name}_status", "") or "")
                    best["duration_us"] = coerce_int_field(http_kv.get(f"{name}_duration_us"))
                    placed_raw = http_kv.get(f"{name}_placed_by_priority", "") or "{}"
                    try:
                        best["placed_by_priority"] = json.loads(placed_raw) if isinstance(placed_raw, str) else (placed_raw or {})
                    except Exception:
                        best["placed_by_priority"] = {}
                    best["evictions"] = http_kv.get(f"{name}_evictions", "")
                    best["moves"] = http_kv.get(f"{name}_moves", "")
            except Exception:
                pass  # fall through to ConfigMap

        # 2) Fall back to latest ConfigMap row if needed
        if (not best["name"]) or (error == ""):
            if solver_stats_header is None or solver_stats_rows is None:
                h, rows = self._get_solver_stats_from_configmap()
                solver_stats_header = h or []
                solver_stats_rows = rows or []
            if solver_stats_rows:
                last = solver_stats_rows[-1]
                if not best["name"]:
                    name = (last.get("best_solver") or "").strip()
                    if name:
                        best["name"] = name
                        best["status"] = str(last.get(f"{name}_status", "") or "")
                        best["duration_us"] = coerce_int_field(last.get(f"{name}_duration_us"))
                        placed_raw = last.get(f"{name}_placed_by_priority", "") or "{}"
                        try:
                            best["placed_by_priority"] = json.loads(placed_raw) if isinstance(placed_raw, str) else (placed_raw or {})
                        except Exception:
                            best["placed_by_priority"] = {}
                        best["evictions"] = last.get(f"{name}_evictions", "")
                        best["moves"] = last.get(f"{name}_moves", "")
                if error == "":
                    error = str(last.get("error", "") or "").strip()

        best = self._prune_non_values(best)
        return best, error

    def _result_segments(self, stem: str) -> list[tuple[int, Path]]:
        """
        Get all result segments for a given stem.
        """
        segs: list[tuple[int, Path]] = []
        pat = re.compile(rf"^{re.escape(stem)}_(\d+)\.csv$")
        for p in self.results_dir.glob(f"{stem}_*.csv"):
            m = pat.match(p.name)
            if m:
                segs.append((int(m.group(1)), p))
        return sorted(segs, key=lambda t: t[0])

    def _pick_results_to_write(self, stem: str, rows_to_add: int = 1) -> Path:
        """
        Choose a segment to write 'rows_to_add' rows to.
        - Creates '<stem>_1.csv' if no segments exist.
        - Rotates to '<stem>_<last+1>.csv' when current_rows + rows_to_add > limit.
        Always ensures the header on the chosen file.
        Also: when --overwrite is set, delete any existing segment whose header
        does not match RESULTS_HEADER.
        """
        # Purge any segments with mismatched headers if overwrite=true
        segs = self._result_segments(stem)
        for _, p in list(segs):
            self._purge_mismatched_csv(p, RESULTS_HEADER)
        # Re-enumerate after potential deletions
        segs = self._result_segments(stem)

        if not segs:
            target = self.results_dir / f"{stem}_1.csv"
            ensure_csv_with_header(target, RESULTS_HEADER)
            LOG.info(f"starting new segment: {target.name}")
            return target

        last_idx, last_path = segs[-1]
        rows = count_csv_rows(last_path)

        # Rotate if we would exceed the limit with this write.
        if rows + rows_to_add > self.max_rows_per_file:
            next_path = self.results_dir / f"{stem}_{last_idx + 1}.csv"
            ensure_csv_with_header(next_path, RESULTS_HEADER)
            LOG.info(f"{last_path.name} is full ({rows}/{self.max_rows_per_file}); switching to {next_path.name}")
            return next_path

        # Otherwise use the last segment
        ensure_csv_with_header(last_path, RESULTS_HEADER)
        return last_path

    def _load_seen_results(self, stem: str) -> set[int]:
        """
        Load all seen seeds from all segments for the given stem.
        """
        seen: set[int] = set()
        for _, p in self._result_segments(stem):
            try:
                with open(p, "r", encoding="utf-8", newline="") as fh:
                    for row in csv.DictReader(fh):
                        s = (row.get("seed") or "").strip()
                        if s:
                            seen.add(int(s))
            except Exception:
                pass
        return seen

    def _get_solver_stats_from_configmap(self) -> tuple[list[str], list[dict]]:
        """
        Read latest stats ConfigMap and build (header, rows) for the solver-stats CSV.
        Pure read: no disk writes. Returns (header, rows); rows may be empty.
        """
        cm_obj = KwokTestGenerator._get_latest_configmap(
            self.ctx, CM_SOLVER_STATS_NAMESPACE, CM_SOLVER_STATS_NAME,
            accept_prefix=True, label_selector=None,
        )
        if cm_obj is None:
            LOG.warning("no config map matching %r found in ns=%r", CM_SOLVER_STATS_NAME, CM_SOLVER_STATS_NAMESPACE)
            return [], []

        data = cm_obj.get("data") or {}
        runs_raw = data.get("runs.json", "[]")
        try:
            runs = json.loads(runs_raw) or []
            if not isinstance(runs, list):
                runs = []
        except Exception:
            LOG.warning("runs.json is not valid JSON; skipping stats collect")
            return [], []

        solver_slots = ["local-search", "bfs", "python"]
        header = ["timestamp_ns", "error", "best_solver"] + solver_slots

        rows: list[dict] = []
        for run in runs:
            row = {
                "timestamp_ns": run.get("timestamp_ns") or "",
                "error": run.get("error") or "",
                "best_solver": run.get("best_solver") or "",
            }

            attempts = {(a.get("name") or ""): a for a in (run.get("attempts") or [])}

            for s in solver_slots:
                a = attempts.get(s)
                if not a:
                    # leave empty string if solver wasn't attempted in this run
                    row[s] = ""
                    continue

                sc = a.get("score") or {}
                solver_obj = {
                    "status": a.get("status", ""),
                    "duration_us": a.get("duration_us", ""),
                    "placed_by_priority": sc.get("placed_by_priority") or {},
                    "evictions": sc.get("evicted", ""),
                    "moves": sc.get("moved", ""),
                }

                solver_obj = KwokTestGenerator._prune_non_values(solver_obj)
                row[s] = json.dumps(solver_obj, separators=(",", ":"), sort_keys=True)

            rows.append(row)

        return header, rows

    def _write_solver_stats_csv(self, cfg: Path, seed: int, header: list[str], rows: list[dict]) -> None:
        """
        Write solver-stats CSV to results dir. No-op if header/rows empty.
        When --overwrite is set, delete an existing file if its header does not match 'header'.
        """
        if not header:
            LOG.warning("skip write solver-stats: empty header")
            return
        out_path = self.results_dir / f"{cfg.stem}_{CM_SOLVER_STATS_NAME}_seed-{seed}.csv"
        out_path.parent.mkdir(parents=True, exist_ok=True)

        # NEW: purge mismatched header if overwrite true
        self._purge_mismatched_csv(out_path, header)

        try:
            with open(out_path, "w", encoding="utf-8", newline="") as fh:
                w = csv.DictWriter(fh, fieldnames=header)
                w.writeheader()
                for r in rows:
                    w.writerow(r)
            LOG.info("saved %s", out_path.name)
        except Exception as e:
            LOG.warning("failed writing %s: %s", out_path.name, e)
    
    @staticmethod
    def _extract_last_from_rows(rows: list[dict], col: str) -> str:
        if not rows:
            return ""
        v = rows[-1].get(col, "")
        if v is None:
            return ""
        # If the value is structured, serialize; otherwise stringify and trim
        if isinstance(v, (dict, list)):
            return json.dumps(v, separators=(",", ":"), sort_keys=True)
        return str(v).strip()
        
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

    def _wait_for_inactive_scheduler(self, url: str, timeout_s: int, *, poll_initial_s: float = 0.5, poll_interval_s: float = 1.5) -> bool:
        """
        Poll the /active endpoint until it reports Active=false or until timeout.
        Returns True if became inactive; False if timed out or endpoint unreachable.
        poll_initial_s: initial poll interval in seconds (default 0.5s)
        poll_interval_s: maximum poll interval in seconds (default 1.5s)
        """
        deadline = time.time() + max(0, int(timeout_s))
        interval = float(poll_initial_s)
        last_log = 0.0

        while time.time() < deadline:
            code, body = self._get_active_http(url)
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


    def _save_scheduler_logs(self, cfg: Path, seed: int) -> None:
        """
        Save scheduler logs for the current KWOK cluster to results dir.
        File: <results-dir>/<cfg.stem>_scheduler-logs_seed-<seed>.log
        """
        assert self.results_dir is not None, "results_dir must be resolved before saving logs"
        out_path = self.results_dir / f"{cfg.stem}_scheduler-logs_seed-{seed}.log"
        out_path.parent.mkdir(parents=True, exist_ok=True)
        
        # only save if not exists or overwrite is enabled
        if out_path.exists() and not self.args.overwrite:
            LOG.info("scheduler log exists and overwrite disabled; skipping: %s (use --overwrite to replace)", out_path.name)
            return

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
            LOG.info("saved scheduler logs to %s (rc=%d, %d bytes)", out_path.name, r.returncode, len(data))
        except Exception as e:
            LOG.warning("failed saving scheduler logs: %s", e)

    ##############################################
    # ------------ Workload application --------
    ##############################################
    def _apply_standalone_pods(self, ta: TestConfigApplied, rng: random.Random) -> List[Dict[str, object]]:
        """
        Create standalone pods in the given namespace with the specified specs.
        Returns a list of pod specs created.
        """
        cpu_parts = ta.pod_parts_cpu_m
        mem_parts = ta.pod_parts_mem_b
        specs: List[Dict[str, object]] = []
        names: List[str] = []
        for i in range(ta.num_pods):
            prio = rng.randint(1, max(1, ta.num_priorities))
            pc = f"p{prio}"
            name = f"pod-{i+1:03d}-{pc}"
            cpu_m = max(1, int(cpu_parts[i]))
            mem_b = max(1, int(mem_parts[i]))
            cpu_m_str = qty_to_mcpu_str(cpu_m)
            mem_b_str = qty_to_bytes_str(mem_b)
            KwokTestGenerator._apply_yaml(self.ctx, yaml_kwok_pod(ta.namespace, name, cpu_m_str, mem_b_str, pc))
            names.append(name)
            specs.append({"name": name, "cpu_m": cpu_m, "mem_b": mem_b, "priority": pc})
            if ta.wait_pod_mode == "running":
                _ = KwokTestGenerator._wait_each(self.ctx, "pod", name, ta.namespace, ta.wait_pod_timeout_s, ta.wait_pod_mode)
        if ta.wait_pod_mode in ("exist", "ready"):
            for name in names:
                _ = KwokTestGenerator._wait_each(self.ctx, "pod", name, ta.namespace, ta.wait_pod_timeout_s, ta.wait_pod_mode)
        LOG.info(f"created {len(specs)} standalone pods")
        return specs

    def _apply_replicasets(self, ta: TestConfigApplied, rng: random.Random) -> List[Dict[str, object]]:
        """
        Create ReplicaSets in the given namespace with the specified specs.
        Returns a list of replicaset specs created.
        """
        specs = []
        replicas = ta.rs_sets
        cpu_x    = ta.rs_parts_cpu_m
        mem_x    = ta.rs_parts_mem_b
        for i, count in enumerate(replicas, start=1):
            prio = rng.randint(1, max(1, ta.num_priorities))
            pc = f"p{prio}"
            rsname = f"rs-{i:02d}-{pc}"
            cpu_m_str = qty_to_mcpu_str(int(cpu_x[i-1]))
            mem_b_str = qty_to_bytes_str(int(mem_x[i-1]))

            specs.append({
                "name": rsname, "replicas": int(count),
                "cpu_m": int(cpu_x[i-1]), "mem_b": int(mem_x[i-1]), "priority": pc
            })
            self._apply_yaml(self.ctx, yaml_kwok_rs(ta.namespace, rsname, 0, cpu_m_str, mem_b_str, pc))
            for r in range(1, int(count) + 1):
                self._scale_replicaset(self.ctx, ta.namespace, rsname, r)
                if ta.wait_pod_mode == "running":
                    _ = self._wait_rs_pods(self.ctx, rsname, ta.namespace, ta.wait_pod_timeout_s, ta.wait_pod_mode)
        if ta.wait_pod_mode in ("exist", "ready"):
            for spec in specs:
                _ = self._wait_each(self.ctx, "rs", spec["name"], ta.namespace, ta.wait_pod_timeout_s, ta.wait_pod_mode)
        LOG.info(f"created {len(specs)} ReplicaSets with total {sum(s['replicas'] for s in specs)} (=num_pods)")
        return specs

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

    @staticmethod
    def _count_running_and_unscheduled_by_priority(ctx: str, ns: str) -> Dict[str, int]:
        """
        Count running and unscheduled pods by their priority class in the given namespace.
        """
        running: Dict[str, int] = {}
        unscheduled: Dict[str, int] = {}
        pods = get_json_ctx(ctx, ["-n", ns, "get", "pods", "-o", "json"]).get("items", [])
        for p in pods:
            pc = (p.get("spec") or {}).get("priorityClassName") or ""
            if (p.get("status") or {}).get("phase", "") != "Running":
                unscheduled[pc] = unscheduled.get(pc, 0) + 1
            else:
                running[pc] = running.get(pc, 0) + 1
        return running, unscheduled

    ##############################################
    # ------------ Seed helpers -----------------
    ##############################################
    @staticmethod
    def _read_seeds_file(path: Path) -> List[int]:
        """
        Read seed values from a file.
        """
        seeds: List[int] = []
        if not path.exists():
            return seeds
        if path.suffix.lower() == ".csv":
            with open(path, "r", encoding="utf-8", newline="") as f:
                rd = csv.DictReader(f)
                for row in rd:
                    s = (row.get("seed") or "").strip()
                    if s:
                        seeds.append(int(s))
        else:
            with open(path, "r", encoding="utf-8") as f:
                for line in f:
                    s = line.strip()
                    if s:
                        seeds.append(int(s))
        return seeds

    # ------------------------- Config I/O -------------------------
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

    @staticmethod
    def _get_str(doc: Dict[str, Any], key: str, default: str) -> str:
        """
        Get a string value from the document, or return default if missing/empty.
        """
        v = doc.get(key, default)
        s = str(v).strip()
        return s if s else default

    def _load_run_config(self, cfg_path: Path) -> TestConfigRaw:
        """
        Load the runner configuration from the given YAML file.
        """
        with open(cfg_path, "r", encoding="utf-8") as f:
            docs = list(yaml.safe_load_all(f)) or []
        runner_doc = KwokTestGenerator._pick_runner_doc(docs)

        tr = TestConfigRaw(source_file=cfg_path)
        tr.namespace      = KwokTestGenerator._get_str(runner_doc, "namespace", tr.namespace)
        tr.num_nodes      = get_int_from_dict(runner_doc, "num_nodes", tr.num_nodes)
        tr.num_pods       = get_int_from_dict(runner_doc, "num_pods", tr.num_pods)

        tr.util           = get_float_from_dict(runner_doc, "util", tr.util)

        tr.wait_pod_mode      = self._get_wait_pod_mode_from_dict(runner_doc, "wait_pod_mode", tr.wait_pod_mode)
        tr.wait_pod_timeout   = get_str_from_dict(runner_doc, "wait_pod_timeout", tr.wait_pod_timeout)
        tr.settle_timeout_min = get_str_from_dict(runner_doc, "settle_timeout_min", tr.settle_timeout_min)
        tr.settle_timeout_max = get_str_from_dict(runner_doc, "settle_timeout_max", tr.settle_timeout_max)

        raw_np = normalize_interval(runner_doc, ("num_priorities","num_priorities_lo","num_priorities_hi"))
        if raw_np:
            tr.num_priorities = parse_int_interval(raw_np, min_lo=2)

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

    ##############################################
    # ------------ Validation & resolution -------
    ##############################################
    @staticmethod
    def _validate_config(tr: TestConfigRaw) -> tuple[bool, str]:
        """
        Validate the raw config. Do NOT raise; return (ok, aggregated_message).
        All checks live here and we accumulate *all* failures.
        """
        errors: list[str] = []

        if not tr.namespace or not str(tr.namespace).strip():
            errors.append("namespace must be a non-empty string")
        if tr.num_nodes < 1:
            errors.append("num_nodes must be >= 1")
        if tr.num_pods < 1:
            errors.append("num_pods must be >= 1")

        if tr.cpu_per_pod is None or tr.mem_per_pod is None:
            errors.append("cpu_per_pod and mem_per_pod must be provided")
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
        
        # quantities
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
        num_prios = self._rng(seed_int, "num_priorities", num_prios_lo, num_prios_hi).randint(num_prios_lo, num_prios_hi)

        # intervals -> ints
        cpu_m = (qty_to_mcpu_int(tr.cpu_per_pod[0]), qty_to_mcpu_int(tr.cpu_per_pod[1]))
        mem_b = (qty_to_bytes_int(tr.mem_per_pod[0]), qty_to_bytes_int(tr.mem_per_pod[1]))

        # Pod sizes
        rs_sets: list[int] = []
        rs_cpu = rs_mem = None
        if tr.num_replicas_per_rs is not None:  # RS mode
            rng_rs_sizes = self._rng(seed_int, "rs-sizes")
            rng_rs_cpu   = self._rng(seed_int, "rs-cpu")
            rng_rs_mem   = self._rng(seed_int, "rs-mem")
            rs_sets = self._gen_rs_sizes(rng_rs_sizes, tr.num_pods, tr.num_replicas_per_rs)
            rs_cpu = [rng_rs_cpu.randint(cpu_m[0], cpu_m[1]) for _ in rs_sets]
            rs_mem = [rng_rs_mem.randint(mem_b[0], mem_b[1]) for _ in rs_sets]
            cpu_parts = [v for i, v in enumerate(rs_cpu) for _ in range(rs_sets[i])]
            mem_parts = [v for i, v in enumerate(rs_mem) for _ in range(rs_sets[i])]
        else:  # Standalone
            rng_pod_cpu = self._rng(seed_int, "pod-cpu")
            rng_pod_mem = self._rng(seed_int, "pod-mem")
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

    # ===============================================================
    # Logging
    # ===============================================================
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

    def _log_config_raw(tr: "TestConfigRaw", *, use_logger: bool = True) -> None:
        block = KwokTestGenerator._format_testconfigraw_block(tr)
        header = "\n================================================ CONFIG (RAW) ================================================\n"
        footer = "\n=============================================================================================================="
        if use_logger:
            LOG.info("%s%s%s", header, block, footer)
        else:
            print(f"{header}{block}{footer}", flush=True)

    def _format_args_block(args) -> str:
        # Keep a stable, explicit order
        fields = [
            ("cluster_name", args.cluster_name),
            ("kwok_runtime", args.kwok_runtime),
            ("config_file", args.config_file),
            ("results_dir", args.results_dir),
            ("overwrite", args.overwrite),
            ("clean_results", args.clean_results),
            ("max_rows_per_file", args.max_rows_per_file),
            ("seed", args.seed),
            ("seed_file", args.seed_file),
            ("generate_seeds_to_file", args.generate_seeds_to_file),
            ("count", args.count),
            ("matrix_file", args.matrix_file),
            ("test", args.test),
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

    def _log_args(args, *, use_logger: bool = True) -> None:
        block = KwokTestGenerator._format_args_block(args)
        header = "\n================================================ ARGS ================================================\n"
        footer = "\n======================================================================================================"
        if use_logger:
            LOG.info("%s%s%s", header, block, footer)
        else:
            print(f"{header}{block}{footer}", flush=True)

    
    ##############################################
    # ------------ Runner helpers ----------------
    ##############################################
    def _clean_results_dir(self) -> None:
        """
        Delete all files and subdirectories in results_dir (but not the dir itself).
        Safety checks included to avoid dangerous paths.
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
        ts = time.strftime("%Y/%m/%d/%H:%M:%S", time.localtime())
        cfg_s = str(cfg); sd = "-" if seed is None else str(seed)
        line = f"{ts}\t{category}\t{cfg_s}\t{sd}\t{phase}\t{message}"
        if details:
            compact = details.replace("\n", "\\n")[-1500:]
            line += f"\t{compact}"
        with open(self.failed_f, "a", encoding="utf-8") as f:
            f.write(line + "\n")

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

    def _remove_seed_from_results(self, stem: str, seed: int) -> int:
        """
        Remove all rows for `seed` across rotated results files for this stem.
        Returns the total number of rows removed.
        """
        removed_total = 0
        for _, path in self._result_segments(stem):
            if not path.exists():
                continue
            try:
                with open(path, "r", encoding="utf-8", newline="") as fh:
                    rows = list(csv.DictReader(fh))
            except Exception:
                continue
            keep = [r for r in rows if (r.get("seed") or "").strip() != str(seed)]
            removed = len(rows) - len(keep)
            if removed > 0:
                with open(path, "w", encoding="utf-8", newline="") as fh:
                    w = csv.DictWriter(fh, fieldnames=RESULTS_HEADER)
                    w.writeheader()
                    for r in keep:
                        w.writerow({k: r.get(k, "") for k in RESULTS_HEADER})
                removed_total += removed
                LOG.info("pruned %d row(s) for seed=%s from %s", removed, seed, path.name)
        return removed_total

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

    # ------------------------- Single seed run -------------------------
    def _run_single_seed(self, cfg: Path, seen: set[int], seed: int, ta: TestConfigApplied, seed_file: Optional[str] = None) -> None:
        """
        Run a single seed with the given applied config.
        """
        exists = seed in seen
        save_allowed = (not self.args.test) and (self.args.overwrite or not exists)
        phase = "start"
        LOG.info(f"phase={phase}  cfg={cfg.stem}  seed={seed}")
        try:
            start_time = time.time()
            rng = self._rng(seed, "base")

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
                return

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
                return

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

            # Optionally trigger the optimizer
            if self.args.trigger_optimizer:
                phase = "trigger_optimizer"
                LOG.info(f"phase={phase} url={self.args.optimizer_url}")
                code, body = self._trigger_optimizer_http(self.args.optimizer_url)
                # Log a compact, single-line body to avoid bloating logs
                body_compact = (body or "").replace("\n", "\\n")
                if len(body_compact) > 600:
                    body_compact = body_compact[:600] + "...(truncated)"
                LOG.info(f"optimizer_response code={code} body={body_compact}")
                # Best-effort parse and remember JSON for later append use
                self.last_optimize_json = None
                if code and isinstance(body, str) and body.strip().startswith("{"):
                    try:
                        self.last_optimize_json = json.loads(body)
                    except Exception:
                        self.last_optimize_json = None

            phase = "wait_settle"
            LOG.info(f"phase={phase} timeout_min={ta.settle_timeout_min_s}s")
            time.sleep(ta.settle_timeout_min_s)
            if ta.settle_timeout_max_s > 0:
                LOG.info(f"waiting for inactive scheduler (max {ta.settle_timeout_max_s}s)")
                # Prefer active-wait if an endpoint is configured; otherwise fall back to sleep.
                if getattr(self.args, "active_url", None):
                    became_inactive = self._wait_for_inactive_scheduler(self.args.active_url, ta.settle_timeout_max_s)
                    if not became_inactive:
                        LOG.info("did not observe inactive within settle-timeout; proceeding")

            phase = "status_snapshot"
            LOG.info(f"phase={phase}")
            running_placed_by_priority, unscheduled_placed_by_priority = self._count_running_and_unscheduled_by_priority(self.ctx, ta.namespace)
            snap = stat_snapshot(self.ctx, ta.namespace, expected=ta.num_pods)

            phase = "solver_stats"
            solver_stats_header: list[str] | None = None
            solver_stats_rows: list[dict] | None = None

            # Collect once if needed (save and/or append)
            if self.args.save_solver_stats:
                solver_stats_header, solver_stats_rows = self._get_solver_stats_from_configmap()
                self._write_solver_stats_csv(cfg, seed, solver_stats_header, solver_stats_rows)
            
            # Best solver
            best_solver, error = self._get_best_solver(solver_stats_header, solver_stats_rows)
            result_row = {
                "timestamp": get_timestamp(),
                "kwok_config": str(cfg),
                "seed_file": seed_file,
                "seed": str(seed),
                "error": error,
                "best_solver": json.dumps(best_solver, separators=(",", ":"), sort_keys=True),
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
                "running_count": int(len(snap.pods_running)),
                "unscheduled_count": int(len(snap.pods_unscheduled)),
                "pods_run_by_node": json.dumps(snap.pods_run_by_node, separators=(",", ":")),
                "running_placed_by_priority": json.dumps(running_placed_by_priority, separators=(",", ":"), sort_keys=True),
                "unschedulable_by_priority": json.dumps(unscheduled_placed_by_priority, separators=(",", ":"), sort_keys=True),
                "unscheduled": "{" + ",".join(sorted(snap.pods_unscheduled)) + "}",
                "running": "{" + ",".join(sorted([name for (name, _) in snap.pods_running])) + "}",
                "pod_node": json.dumps(self._build_pod_node_list({name: node for (name, node) in snap.pods_running}, snap.pods_unscheduled, pod_specs, rs_specs), separators=(",", ":")),
            }

            phase = "write_results"
            LOG.info(f"phase={phase}")
            if save_allowed:
                if self.args.overwrite and exists:
                    removed = self._remove_seed_from_results(cfg.stem, seed)
                    LOG.info(f"overwrite enabled: removed {removed} existing row(s) for seed={seed}")
                dest_csv = self._pick_results_to_write(cfg.stem, rows_to_add=1)
                ensure_csv_with_header(dest_csv, RESULTS_HEADER)
                csv_append_row(dest_csv, RESULTS_HEADER, result_row)
                LOG.info(f"appended to {dest_csv.name}")
            else:
                LOG.info("not saved (already exists; use --overwrite to replace)")

            if self.args.save_scheduler_logs:
                phase = "save_scheduler_logs"
                LOG.info(f"phase={phase}")
                self._save_scheduler_logs(cfg, seed)

            LOG.info(f"done; took {time.time()-start_time:.1f}s")
            self._print_seed_summary(cfg, seed, len(snap.pods_running), len(snap.pods_unscheduled))

        except RuntimeError as e:
            tb = traceback.format_exc()
            self._write_fail("seed", cfg, seed, phase, str(e), tb)
            LOG.info(f"seed-runtime  cfg={cfg.stem} seed={seed}  phase={phase}: {e}")
            return
        except Exception as e:
            tb = traceback.format_exc()
            self._write_fail("seed", cfg, seed, phase, str(e), tb)
            LOG.error(f"seed-error  cfg={cfg.stem}  seed={seed}  phase={phase}: {e}")
            self._print_seed_summary(cfg, seed, None, None, f"error: {e}")

    # ------------------------- Multi-seed runners -------------------------
    def _run_gen_seeds(self):
        """
        Generate random seeds and write them to a file.
        """
        if self.args.count is None or self.args.count < 1:
            raise SystemExit("--generate-seeds-to-file requires --count >= 1")
        if self.args.seed is not None or self.args.seed_file:
            raise SystemExit("--generate-seeds-to-file cannot be combined with --seed or --seed-file")
        now_ns = int(time.time_ns())
        rng = self._rng(now_ns, "base")
        seeds = [(rng.getrandbits(63) or 1) for _ in range(int(self.args.count))]
        outp = Path(self.args.generate_seeds_to_file)
        outp.parent.mkdir(parents=True, exist_ok=True)
        with open(outp, "w", encoding="utf-8", newline="") as f:
            for s in seeds:
                f.write(f"{s}\n")
        LOG.info("wrote %d seeds to %s", len(seeds), outp)

    def _run_count_path(self, cfg: Path, cfg_idx: int, cfgs_total: int, tr: TestConfigRaw, seen: set[int]) -> None:
        """
        Run the generator for configuration(s) files and a seed count. If count=-1, run indefinitely.
        """
        to_make = int(self.args.count)
        now_ns = int(time.time_ns())
        rng = self._rng(now_ns, "base")
        made = 0
        while to_make == -1 or made < to_make:
            s = rng.getrandbits(63) or 1
            seed_idx = made + 1
            seeds_total = to_make  # may be -1

            self._print_run_header(s, cfg.name, seed_idx, to_make, cfg_idx, cfgs_total)
            self._update_completion_marker(cfg_idx, cfgs_total, seed_idx, seeds_total)

            started_at = time.time()
            try:
                rc = self._resolve_config_for_seed(tr, s)
                self._run_single_seed(cfg, seen, s, rc)
            finally:
                self._record_seed_duration(started_at)
                self._update_completion_marker(cfg_idx, cfgs_total, seed_idx + 1, seeds_total)

            more_coming = (to_make == -1) or (made + 1 < to_make)
            self._pause(next_exists=more_coming)
            if to_make != -1:
                made += 1
        return

    def _run_seed_single_path(self, cfg: Path, cfg_idx: int, cfgs_total: int, tr: TestConfigRaw, seen: set[int]) -> None:
        """
        Run the generator for configuration(s) files and a single seed.
        """
        s = int(self.args.seed)
        self._print_run_header(s, cfg.name, 1, 1, cfg_idx, cfgs_total)
        self._update_completion_marker(cfg_idx, cfgs_total, 1, 1)

        started_at = time.time()
        try:
            rc = self._resolve_config_for_seed(tr, s)
            self._run_single_seed(cfg, seen, s, rc)
        finally:
            # record duration and update marker AFTER seed
            self._record_seed_duration(started_at)
            # After finishing seed 1/1, seeds_left becomes 0
            self._update_completion_marker(cfg_idx, cfgs_total, 2, 1)
        return

    def _run_seed_file_path(self, cfg: Path, cfg_idx: int, cfgs_total: int, tr: TestConfigRaw, seen: set[int]) -> None:
        """
        Run the generator for a specific configuration and seeds from a file.
        """
        seeds_list = self._read_seeds_file(Path(self.args.seed_file))
        seeds_total = len(seeds_list)
        for seed_idx, s in enumerate(seeds_list, start=1):
            self._print_run_header(s, cfg.name, seed_idx, seeds_total, cfg_idx, cfgs_total)
            self._update_completion_marker(cfg_idx, cfgs_total, seed_idx, seeds_total)

            s = int(s)
            started_at = time.time()
            try:
                rc = self._resolve_config_for_seed(tr, s)
                self._run_single_seed(cfg, seen, s, rc, self.args.seed_file)
            finally:
                self._record_seed_duration(started_at)
                self._update_completion_marker(cfg_idx, cfgs_total, seed_idx + 1, seeds_total)

            self._pause(next_exists=(seed_idx < seeds_total))
        return

    def _run_for_config(self, cfg: Path, cfg_idx: int, cfgs_total: int) -> None:
        """
        Run the generator for a specific configuration file.
        """
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

        # failures file lives in results dir
        self.failed_f = self.results_dir / "failed.csv"

        LOG.info(f"results-dir resolved to: {self.results_dir}")
        
        self._init_eta_state()

        seen = self._load_seen_results(cfg.stem)

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
    # ------------ Main Runner -------------------
    ##############################################
    def run(self) -> None:
        """
        Main runner function.
        """
        # --- if generate seeds to file, do it, then exit
        if self.args.generate_seeds_to_file is not None:
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

        # --- check test mode ---
        if self.args.test:
            if self.args.seed is None:
                raise SystemExit("--test requires exactly one --seed")
            if self.args.count is not None or self.args.seed_file:
                raise SystemExit("--test cannot be combined with --count or --seed-file")

        seed_file_str = ", seed-file=" + self.args.seed_file if self.args.seed_file else ""
        LOG.info(f"starting; cluster={self.args.cluster_name}  runtime={self.args.kwok_runtime}, config-file={self.args.config_file}{seed_file_str}")

        cfg = self._get_kwok_config_file(self.args.config_file)
        self._validate_all_configs([cfg])
        cfg_idx = getattr(self.args, "matrix_idx", 1)
        cfgs_total = getattr(self.args, "matrix_total", 1)
        LOG.info(f"\n================================================ CONFIG RUN {cfg_idx}/{cfgs_total} ===================================================\n"
                 f"config={cfg}\n"
                 "----------------------------------------------------------------------------------------------------------------")
        self._run_for_config(cfg, cfg_idx, cfgs_total)

##############################################
# ------------ Matrix runner -----------------
##############################################
MATRIX_REQUIRED_COLS = ["config-file", "seed-file", "results-dir"]

def run_matrix(args) -> int:
    def _read_csv_matrix(path: str):
        rows = []
        with open(path, "r", encoding="utf-8") as f:
            rdr = csv.DictReader(f)
            if rdr.fieldnames is None:
                raise SystemExit(f"[matrix] CSV {path} has no header")
            missing = [c for c in MATRIX_REQUIRED_COLS if c not in rdr.fieldnames]
            if missing:
                raise SystemExit(f"[matrix] CSV {path} missing required columns: {', '.join(missing)}")
            for i, row in enumerate(rdr, 2):
                cleaned = {k: (row.get(k, "") or "").strip() for k in rdr.fieldnames}
                for c in MATRIX_REQUIRED_COLS:
                    if not cleaned[c]:
                        raise SystemExit(f"[matrix] {path}:{i} column '{c}' is empty")
                cleaned["_lineno"] = i
                rows.append(cleaned)
        if not rows:
            raise SystemExit(f"[matrix] CSV {path} is empty")
        return rows

    def _run_one_row(row: dict, idx: int, total: int) -> int:
        print(
            f"\n======================= MATRIX RUN {idx}/{total} =========================\n"
            f"[matrix] config-file={row['config-file']}\n"
            f"[matrix] seed-file={row['seed-file']}\n"
            f"[matrix] results-dir={row['results-dir']}\n"
            "------------------------------------------------------------------",
            flush=True,
        )
        # Clone a shallow args namespace
        ns = argparse.Namespace(**vars(args))
        ns.config_file  = row["config-file"]
        ns.seed_file    = row["seed-file"]
        ns.results_dir  = row["results-dir"]
        ns.matrix_file  = None # remove matrix_file to avoid recursion
        ns.matrix_idx   = idx
        ns.matrix_total = total
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
                    help=("Directory to store results. If omitted, results are written to ./results/<config-stem>/"))
    ap.add_argument("--overwrite", action="store_true", help="Replace any existing results for the same seed.")
    ap.add_argument("--max-rows-per-file", dest="max_rows_per_file", type=int, default=500000,
                    help="Maximum number of data rows per results CSV before rotating (default 500000).")
    ap.add_argument("--clean-results", dest="clean_results", action="store_true",
                    help="Before running, delete all contents of --results-dir.")

    # seeds
    ap.add_argument("--seed", type=int, default=None, help="Run exactly this seed (per kwok-config)")
    ap.add_argument("--seed-file", dest="seed_file", default=None, help="Path to seeds file (CSV with 'seed' col or newline list).")
    ap.add_argument("--generate-seeds-to-file", dest="generate_seeds_to_file", default=None,
                    help="Write --count random seeds (one per line, no header) to this file, then exit.")
    ap.add_argument("--count", type=int, default=None, help="Generate the specified number of random seeds; -1=infinite.")
    
    # matrix
    ap.add_argument("--matrix-file", help="CSV with columns: config-file,seed-file,results-dir.")
    
    # test
    ap.add_argument("--test", action="store_true", help="Test mode: requires --seed; prints summary only; no results are saved.")

    # solver stats
    ap.add_argument("--save-solver-stats", dest="save_solver_stats", action="store_true",
                    help="Save solver stats for each seed, saved to results dir")
    
    # scheduler logs
    ap.add_argument("--save-scheduler-logs", dest="save_scheduler_logs", action="store_true",
                    help="After save_stats for each seed, save 'kwokctl logs kube-scheduler --name <cluster>' to results dir as <config-stem>_kube-scheduler_seed-<seed>.log")
    
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

    return ap

def main():
    """
    Main entry point for the KWOK test generator.
    """
    args = build_argparser().parse_args()
    
    env_prefix = os.environ.get("KWOK_LOG_PREFIX") or f"[cluster={args.cluster_name}] "
    _ = setup_logging(env_prefix, args.log_level)
    
    if args.matrix_file and args.kwok_runtime:
        sys.exit(run_matrix(args))

    KwokTestGenerator._log_args(args, use_logger=True)
    runner = KwokTestGenerator(args)

    runner.run()

if __name__ == "__main__":
    main()
