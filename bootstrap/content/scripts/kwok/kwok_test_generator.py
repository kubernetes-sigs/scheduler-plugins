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

import cmd
import sys, csv, json, time, random, argparse, re, subprocess, tempfile, os, textwrap, hashlib, math, yaml, traceback, contextlib, fcntl, logging, shlex
from urllib import request as _urlreq, error as _urlerr
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional, Tuple, List, Dict, Any
from concurrent.futures import ThreadPoolExecutor, as_completed

from kwok_shared import (
    get_json_ctx, stat_snapshot, dir_exists, file_exists,
    csv_append_row, qty_to_mcpu_str, qty_to_bytes_str, qty_to_bytes_int, qty_to_mcpu_int
)

# ===============================================================
# Constants
# ===============================================================
RESULTS_HEADER = [
    "timestamp", "kwok_config", "seed_file", "seed",
    "num_nodes", "num_pods", "num_priorities", "num_replicaset",
    "num_replicas_per_rs_lo", "num_replicas_per_rs_hi",
    "node_cpu_m", "node_mem_b",
    "cpu_per_pod_m_lo", "cpu_per_pod_m_hi",
    "mem_per_pod_b_lo", "mem_per_pod_b_hi",
    "util", "util_run_cpu", "util_run_mem",
    "cpu_m_run", "mem_b_run",
    "wait_mode", "wait_timeout_s", "settle_timeout_s",
    "running_count", "unscheduled_count",
    "pods_run_by_node",
    "running_placed_by_priority", "unschedulable_by_priority",
    "unscheduled", "running",
    "pod_node",
]

# Module-level logger handle (handlers configured later by setup_logging in main())
LOG = logging.getLogger("kwok")

CM_STATS_NAME = "stats"
CM_STATS_NAMESPACE = "kube-system"

# ====================================================================
# YAML builders.
# Due to proper indentation, we keep them outside class
# ====================================================================
def yaml_priority_class(name: str, value: int) -> str:
    return textwrap.dedent(f"""\
    apiVersion: scheduling.k8s.io/v1
    kind: PriorityClass
    metadata:
      name: {name}
    value: {value}
    preemptionPolicy: PreemptLowerPriority
    globalDefault: false
    description: "pod priority {value}"
    ---
    """)

def yaml_kwok_node(name: str, cpu: str, mem: str, pods_cap: int) -> str:
    return textwrap.dedent(f"""\
    apiVersion: v1
    kind: Node
    metadata:
      name: {name}
      annotations:
        kwok.x-k8s.io/node: "fake"
      labels:
        kubernetes.io/hostname: "{name}"
        kubernetes.io/os: "linux"
        kubernetes.io/arch: "amd64"
        node-role.kubernetes.io/agent: ""
        type: "kwok"
    spec:
      taints:
      - key: kwok.x-k8s.io/node
        value: "fake"
        effect: NoSchedule
    status:
      capacity:
        cpu: "{cpu}"
        memory: "{mem}"
        pods: {pods_cap}
      allocatable:
        cpu: "{cpu}"
        memory: "{mem}"
        pods: {pods_cap}
      nodeInfo:
        architecture: amd64
        kubeletVersion: fake
        kubeProxyVersion: fake
        operatingSystem: linux
      phase: Running
    ---
    """)

def yaml_kwok_rs(ns: str, rs_name: str, replicas: int, cpu: str, mem: str, pc: str) -> str:
    return textwrap.dedent(f"""\
    apiVersion: apps/v1
    kind: ReplicaSet
    metadata:
      name: {rs_name}
      namespace: {ns}
    spec:
      replicas: {replicas}
      selector:
        matchLabels:
          app: {rs_name}
      template:
        metadata:
          labels:
            app: {rs_name}
        spec:
          priorityClassName: {pc}
          restartPolicy: Always
          tolerations:
          - key: "kwok.x-k8s.io/node"
            operator: "Exists"
            effect: "NoSchedule"
          containers:
          - name: filler
            image: registry.k8s.io/pause:3.9
            resources:
              requests: {{cpu: "{cpu}", memory: "{mem}"}}
              limits:   {{cpu: "{cpu}", memory: "{mem}"}}
    ---
    """)

def yaml_kwok_pod(ns: str, name: str, cpu: str, mem: str, pc: str) -> str:
    return textwrap.dedent(f"""\
    apiVersion: v1
    kind: Pod
    metadata:
      name: {name}
      namespace: {ns}
    spec:
      restartPolicy: Always
      priorityClassName: {pc}
      tolerations:
      - key: "kwok.x-k8s.io/node"
        operator: "Exists"
        effect: "NoSchedule"
      containers:
      - name: filler
        image: registry.k8s.io/pause:3.9
        resources:
          requests: {{cpu: "{cpu}", memory: "{mem}"}}
          limits:   {{cpu: "{cpu}", memory: "{mem}"}}
    ---
    """)

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
    wait_mode: Optional[str] = None     # None/"none","exist","ready","running"
    wait_timeout: Optional[str] = None  # "5s"
    settle_timeout: Optional[str] = None

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

    wait_mode: Optional[str]
    wait_timeout_s: int
    settle_timeout_s: int
    
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
        self.kwok_runtime: str = args.kwok_runtime
        self.results_dir_arg = args.results_dir
        self.results_dir: Path | None = None
        self.failed_f: Path | None = None
        self.max_rows_per_file = int(args.max_rows_per_file)

    ######################################################
    # ---------- Parsing helpers ----------
    ######################################################
    def _parse_waits(self, tr: TestConfigRaw) -> tuple[Optional[str], int, int]:
        """
        Parse and default the wait parameters from the raw config.
        Returns (wait_mode, wait_timeout_s, settle_timeout_s).
        """
        wait_mode = None if tr.wait_mode in (None, "none", "None", "") else str(tr.wait_mode)
        wait_timeout_s = KwokTestGenerator._parse_timeout_s(tr.wait_timeout)
        settle_timeout_s = KwokTestGenerator._parse_timeout_s(tr.settle_timeout)
        return wait_mode, wait_timeout_s, settle_timeout_s

    @staticmethod
    def _parse_int_interval(s: Optional[str], *, min_lo: int = 1) -> Optional[Tuple[int, int]]:
        """
        Parse a string interval "lo,hi" or "x" into a (lo, hi) tuple.
        Returns None if s is None or empty.
        Ensures lo >= min_lo and hi >= lo.
        """
        if not s:
            return None
        parts = [x.strip() for x in str(s).split(",", 1)]
        if len(parts) == 1:
            lo = hi = int(parts[0])
        else:
            lo, hi = int(parts[0]), int(parts[1])
        lo = max(min_lo, lo)
        hi = max(lo, hi)
        return lo, hi

    @staticmethod
    def _parse_qty_interval(s: Optional[str]) -> Optional[Tuple[str, str]]:
        if not s:
            return None
        parts = [x.strip() for x in s.split(",", 1)]
        if len(parts) == 1:
            return (parts[0], parts[0])
        return (parts[0], parts[1])

    @staticmethod
    def _parse_timeout_s(t:str | None) -> int:
        """
        Parse a timeout string into seconds.
        """
        if not t:
            return 60
        try:
            if t.endswith("ms"): return max(1, int(int(t[:-2]) / 1000))
            if t.endswith("s"):  return int(t[:-1])
            if t.endswith("m"):  return int(t[:-1]) * 60
            if t.endswith("h"):  return int(t[:-1]) * 3600
            return int(t)
        except Exception:
            return 60

    @staticmethod
    def _get_timestamp() -> str:
        """
        Get the current timestamp as a string.
        """
        return time.strftime("%Y/%m/%d/%H:%M:%S", time.localtime())

    @staticmethod
    def _get_int_from_dict(doc: Dict[str, Any], key: str, default: int) -> int:
        """
        Get an integer value from the document, returning a default if not found or invalid.
        """
        v = doc.get(key, default)
        try:
            return int(v)
        except Exception:
            LOG.warning(f"invalid int for {key}: {v}, using default {default}")
            return default

    @staticmethod
    def _get_float_from_dict(doc: Dict[str, Any], key: str, default: float) -> float:
        """
        Get a float value from the document, returning a default if not found or invalid.
        """
        v = doc.get(key, default)
        try:
            return float(v)
        except Exception:
            LOG.warning(f"invalid float for {key}: {v}, using default {default}")
            return default

    @staticmethod
    def _get_str_from_dict(doc: Dict[str, Any], key: str, default: Optional[str]) -> Optional[str]:
        """
        Get a string value from the document, returning a default if not found or empty.
        """
        v = doc.get(key, default)
        if v is None:
            return default
        s = str(v).strip()
        return s if s else default

    @staticmethod
    def _get_wait_mode_from_dict(doc: Dict[str, Any], key: str, default: Optional[str]) -> Optional[str]:
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
            raise ValueError(f"Invalid wait_mode: {s}")
        return s

    ######################################################
    # ---------- Optimizer HTTP trigger ------------------
    ######################################################
    def _trigger_optimizer_http(self, url: str, *, timeout: float = 5.0, retries: int = 3, backoff_s: float = 0.5) -> tuple[int, str]:
        """
        POST optimizer endpoint. Returns (status_code, body_str).
        Retries on transient connection errors.
        """
        data = b""
        headers = {
            "Accept": "application/json",
            "Content-Length": "0",
        }
        last_exc = None
        for attempt in range(1, max(1, retries) + 1):
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
                last_exc = e
                LOG.info(f"optimizer POST failed (attempt {attempt}/{retries}): {e}")
                time.sleep(backoff_s * attempt)
        # give a synthetic status if we never reached the server
        return 0, f"connect-failed: {last_exc}"
    
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
    # ---------- Interval helpers ----------
    ######################################################
    @staticmethod
    def _split_interval(t: Optional[Tuple[int, int]]) -> tuple[str, str]:
        """Return (lo, hi) as strings; empty strings if None."""
        if not t:
            return "", ""
        return str(int(t[0])), str(int(t[1]))

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
    def _ensure_csv_with_header(path: Path, header: List[str]) -> None:
        """
        Ensure the CSV file exists with the given header.
        """
        path.parent.mkdir(parents=True, exist_ok=True)
        if not path.exists():
            with open(path, "w", encoding="utf-8", newline="") as f:
                csv.DictWriter(f, fieldnames=header).writeheader()

    @staticmethod
    def _count_csv_rows(path: Path) -> int:
        """
        Count data rows (excluding header).
        """
        if not path.exists():
            return 0
        with open(path, "r", encoding="utf-8", newline="") as f:
            rd = csv.DictReader(f)
            return sum(1 for _ in rd)

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
        """
        segs = self._result_segments(stem)
        if not segs:
            target = self.results_dir / f"{stem}_1.csv"
            self._ensure_csv_with_header(target, RESULTS_HEADER)
            LOG.info(f"starting new segment: {target.name}")
            return target

        last_idx, last_path = segs[-1]
        rows = self._count_csv_rows(last_path)

        # Rotate if we would exceed the limit with this write.
        if rows + rows_to_add > self.max_rows_per_file:
            next_path = self.results_dir / f"{stem}_{last_idx + 1}.csv"
            self._ensure_csv_with_header(next_path, RESULTS_HEADER)
            LOG.info(f"{last_path.name} is full ({rows}/{self.max_rows_per_file}); switching to {next_path.name}")
            return next_path

        # Otherwise use the last segment
        self._ensure_csv_with_header(last_path, RESULTS_HEADER)
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

    def _save_solver_stats(self, cfg: Path, seed: int) -> None:
        """
        Snapshot the latest stats ConfigMap into a CSV under results_dir.
        One wide row per run; fixed solver slots/columns for stable schema.
        Respects --overwrite; skips if file exists unless the flag is set.
        """
        out_name = f"{cfg.stem}_{CM_STATS_NAME}_seed-{seed}.csv"
        out_path = self.results_dir / out_name
        out_path.parent.mkdir(parents=True, exist_ok=True)

        if out_path.exists() and not self.args.overwrite:
            LOG.info("stats export exists and overwrite disabled; skipping: %s (use --overwrite to replace)", out_path.name)
            return

        cm_obj = KwokTestGenerator._get_latest_configmap(
            self.ctx, CM_STATS_NAMESPACE, CM_STATS_NAME,
            accept_prefix=True, label_selector=None,
        )
        if cm_obj is None:
            LOG.warning("no config map matching %r found in ns=%r", CM_STATS_NAME, CM_STATS_NAMESPACE)
            return

        md = cm_obj.get("metadata") or {}
        picked_name = md.get("name", CM_STATS_NAME)
        picked_ts = md.get("creationTimestamp", "")
        LOG.info("using config map %s (created %s) from ns=%s", picked_name, picked_ts, CM_STATS_NAMESPACE)

        data = cm_obj.get("data") or {}
        runs_raw = data.get("runs.json", "[]")
        try:
            runs = json.loads(runs_raw) or []
            if not isinstance(runs, list):
                runs = []
        except Exception:
            LOG.warning("runs.json is not valid JSON; skipping stats export for this seed")
            return
        if not runs:
            return

        solver_slots = ["local-search", "bfs", "python"]
        header = ["timestamp_ns", "plan_status", "best"]
        for s in solver_slots:
            header += [f"{s}_status", f"{s}_stage", f"{s}_duration_us", f"{s}_placed_by_prio", f"{s}_evictions", f"{s}_moves"]

        with open(out_path, "w", encoding="utf-8", newline="") as fh:
            w = csv.DictWriter(fh, fieldnames=header)
            w.writeheader()
            for run in runs:
                row = {
                    "timestamp_ns": run.get("timestamp_ns") or "",
                    "plan_status": run.get("plan_status") or "",
                    "best": run.get("best") or "",
                }
                attempts = {(a.get("name") or ""): a for a in (run.get("attempts") or [])}
                for s in solver_slots:
                    a = attempts.get(s)
                    if not a:
                        row[f"{s}_status"] = row[f"{s}_stage"] = row[f"{s}_duration_us"] = row[f"{s}_placed_by_prio"] = row[f"{s}_evictions"] = row[f"{s}_moves"] = ""
                        continue
                    sc = a.get("score") or {}
                    row[f"{s}_status"] = a.get("status", "")
                    row[f"{s}_stage"] = a.get("stage", "")
                    row[f"{s}_duration_us"] = a.get("duration_us", "")
                    row[f"{s}_placed_by_prio"] = json.dumps(sc.get("placed_by_priority") or {}, separators=(",", ":"), sort_keys=True)
                    row[f"{s}_evictions"] = sc.get("evicted", "")
                    row[f"{s}_moves"] = sc.get("moved", "")
                w.writerow(row)

        LOG.info("saved %s", out_path)

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
                LOG.info(f"found servicef account '{sa}' in ns '{ns}'")
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

    def _save_scheduler_logs(self, cfg: Path, seed: int) -> None:
        """
        Save kube-scheduler logs for the current KWOK cluster to results dir.
        File: <results-dir>/<cfg.stem>_kube-scheduler_seed-<seed>.log
        """
        assert self.results_dir is not None, "results_dir must be resolved before saving logs"
        out_path = self.results_dir / f"{cfg.stem}_kube-scheduler_seed-{seed}.log"
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
            if ta.wait_mode == "running":
                _ = KwokTestGenerator._wait_each(self.ctx, "pod", name, ta.namespace, ta.wait_timeout_s, ta.wait_mode)
        if ta.wait_mode in ("exist", "ready"):
            for name in names:
                _ = KwokTestGenerator._wait_each(self.ctx, "pod", name, ta.namespace, ta.wait_timeout_s, ta.wait_mode)
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
                if ta.wait_mode == "running":
                    _ = self._wait_rs_pods(self.ctx, rsname, ta.namespace, ta.wait_timeout_s, ta.wait_mode)
        if ta.wait_mode in ("exist", "ready"):
            for spec in specs:
                _ = self._wait_each(self.ctx, "rs", spec["name"], ta.namespace, ta.wait_timeout_s, ta.wait_mode)
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

    @staticmethod
    def _pick_num_replicaset_for_seed(seed: int, interval: Optional[Tuple[int, int]], num_pods: int) -> int:
        """
        Pick num_replicaset for the given seed in [lo,hi] interval.
        If interval is None or empty, returns 0.
        """


    @staticmethod
    def _pick_num_priorities_for_seed(seed: int, interval: Tuple[int, int]) -> int:
        """
        Pick num_priorities for the given seed in [lo,hi] interval.
        If interval is None or empty, returns 1.
        """
       

    # ------------------------- Config I/O -------------------------
    @staticmethod
    def _get_kwok_configs(dir_path: str) -> List[Path]:
        """
        Get all KWOK configuration files from the specified directory.
        """
        cfg_dir = Path(dir_path)
        if not cfg_dir.exists():
            raise SystemExit(f"--config-dir not found: {cfg_dir}")
        cfgs = sorted([p for p in cfg_dir.glob("**/*") if p.is_file() and p.suffix.lower() in (".yaml", ".yml")])
        if not cfgs:
            raise SystemExit("No KWOK configs found (must be at least one).")
        return cfgs

    @staticmethod
    def _normalize_interval(doc: Dict[str, Any], key_combo: Tuple[str, str, str], *, allow_none: bool = True) -> Optional[str]:
        """
        Normalize a (single) or (lo, hi) interval from the document.
        It first checks for 'single' key; if not found, it looks for 'lo_key' and 'hi_key'.
        Returns "lo,hi" or "" if not found (or None if allow_none and not found).
        """
        single, lo_key, hi_key = key_combo
        if single in doc and doc[single] is not None:
            v = doc[single]
            if isinstance(v, (int, float)):
                return str(int(v))
            if isinstance(v, str):
                s = v.strip()
                if s:
                    return s
            elif isinstance(v, (list, tuple)) and len(v) == 2:
                return f"{str(v[0]).strip()},{str(v[1]).strip()}"
            elif isinstance(v, dict):
                lo = str(v.get("lo", "")).strip()
                hi = str(v.get("hi", "")).strip()
                if lo and hi:
                    return f"{lo},{hi}"
        lo = str(doc.get(lo_key, "")).strip()
        hi = str(doc.get(hi_key, "")).strip()
        if lo and hi:
            return f"{lo},{hi}"
        return None if allow_none else ""

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
        tr.num_nodes      = KwokTestGenerator._get_int_from_dict(runner_doc, "num_nodes", tr.num_nodes)
        tr.num_pods       = KwokTestGenerator._get_int_from_dict(runner_doc, "num_pods", tr.num_pods)

        tr.util           = KwokTestGenerator._get_float_from_dict(runner_doc, "util", tr.util)

        tr.wait_mode      = self._get_wait_mode_from_dict(runner_doc, "wait_mode", tr.wait_mode)
        tr.wait_timeout   = self._get_str_from_dict(runner_doc, "wait_timeout", tr.wait_timeout)
        tr.settle_timeout = self._get_str_from_dict(runner_doc, "settle_timeout", tr.settle_timeout)

        raw_np = KwokTestGenerator._normalize_interval(runner_doc, ("num_priorities","num_priorities_lo","num_priorities_hi"))
        if raw_np:
            tr.num_priorities = self._parse_int_interval(raw_np, min_lo=2)

        raw_cpu = KwokTestGenerator._normalize_interval(runner_doc, ("cpu_per_pod","cpu_per_pod_lo","cpu_per_pod_hi"))
        if raw_cpu:
            tr.cpu_per_pod = KwokTestGenerator._parse_qty_interval(raw_cpu)

        raw_mem = KwokTestGenerator._normalize_interval(runner_doc, ("mem_per_pod","mem_per_pod_lo","mem_per_pod_hi"))
        if raw_mem:
            tr.mem_per_pod = KwokTestGenerator._parse_qty_interval(raw_mem)

        raw_rps = KwokTestGenerator._normalize_interval(runner_doc, ("num_replicas_per_rs","num_replicas_per_rs_lo","num_replicas_per_rs_hi"))
        if raw_rps:
            tr.num_replicas_per_rs = self._parse_int_interval(raw_rps, min_lo=1)

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
        wait_mode, wait_timeout_s, settle_timeout_s = self._parse_waits(tr)

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
            wait_mode=wait_mode,
            wait_timeout_s=wait_timeout_s,
            settle_timeout_s=settle_timeout_s,
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
                KwokTestGenerator._ensure_kwok_cluster(self.ctx, self.kwok_runtime, cfg, recreate=True)
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

            phase = "wait_settle"
            LOG.info(f"phase={phase} timeout={ta.settle_timeout_s}s")
            if ta.settle_timeout_s > 0:
                time.sleep(ta.settle_timeout_s)
            
            phase = "status_snapshot"
            LOG.info(f"phase={phase}")
            running_placed_by_priority, unscheduled_placed_by_priority = self._count_running_and_unscheduled_by_priority(self.ctx, ta.namespace)
            snap = stat_snapshot(self.ctx, ta.namespace, expected=ta.num_pods)
            result_row = {
                "timestamp": self._get_timestamp(),
                "kwok_config": str(cfg),
                "seed_file": seed_file,
                "seed": str(seed),
                "num_nodes": ta.num_nodes,
                "num_pods": ta.num_pods,
                "num_priorities": ta.num_priorities,
                "num_replicaset": ta.num_replicaset,
                "num_replicas_per_rs_lo": KwokTestGenerator._split_interval(ta.num_replicas_per_rs)[0],
                "num_replicas_per_rs_hi": KwokTestGenerator._split_interval(ta.num_replicas_per_rs)[1],
                "node_cpu_m": int(ta.node_cpu_m),
                "node_mem_b": int(ta.node_mem_b),
                "cpu_per_pod_m_lo": KwokTestGenerator._split_interval(ta.cpu_per_pod_m)[0],
                "cpu_per_pod_m_hi": KwokTestGenerator._split_interval(ta.cpu_per_pod_m)[1],
                "mem_per_pod_b_lo": KwokTestGenerator._split_interval(ta.mem_per_pod_b)[0],
                "mem_per_pod_b_hi": KwokTestGenerator._split_interval(ta.mem_per_pod_b)[1],
                "util": f"{ta.util:.3f}",
                "util_run_cpu": f"{snap.cpu_run_util:.3f}",
                "util_run_mem": f"{snap.mem_run_util:.3f}",
                "cpu_m_run": int(sum(snap.cpu_req_by_node.values())),
                "mem_b_run": int(sum(snap.mem_req_by_node.values())),
                "wait_mode": (ta.wait_mode or ""),
                "wait_timeout_s": ta.wait_timeout_s,
                "settle_timeout_s": ta.settle_timeout_s,
                "running_count": int(len(snap.pods_running)),
                "unscheduled_count": int(len(snap.pods_unscheduled)),
                "pods_run_by_node": json.dumps(snap.pods_run_by_node, separators=(",", ":")),
                "running_placed_by_priority": json.dumps(running_placed_by_priority, separators=(",", ":"), sort_keys=True),
                "unschedulable_by_priority": json.dumps(unscheduled_placed_by_priority, separators=(",", ":"), sort_keys=True),
                "unscheduled": "{" + ",".join(sorted(snap.pods_unscheduled)) + "}",
                "running": "{" + ",".join(sorted([name for (name, _) in snap.pods_running])) + "}",
                "pod_node": json.dumps(self._build_pod_node_list(
                    {name: node for (name, node) in snap.pods_running},
                    snap.pods_unscheduled, pod_specs, rs_specs
                ), separators=(",", ":")),
            }

            phase = "write_results"
            LOG.info(f"phase={phase}")
            if save_allowed:
                if self.args.overwrite and exists:
                    removed = self._remove_seed_from_results(cfg.stem, seed)
                    LOG.info(f"overwrite enabled: removed {removed} existing row(s) for seed={seed}")
                dest_csv = self._pick_results_to_write(cfg.stem, rows_to_add=1)
                self._ensure_csv_with_header(dest_csv, RESULTS_HEADER)
                csv_append_row(dest_csv, RESULTS_HEADER, result_row)
                LOG.info(f"appended to {dest_csv.name}")
            else:
                LOG.info("not saved (already exists; use --overwrite to replace)")
            
            phase = "save_solver_stats"
            LOG.info(f"phase={phase}")
            self._save_solver_stats(cfg, seed)

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
            self._print_run_header(s, cfg.name, made + 1, to_make, cfg_idx, cfgs_total)
            rc = self._resolve_config_for_seed(tr, s)
            self._run_single_seed(cfg, seen, s, rc)
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
        rc = self._resolve_config_for_seed(tr, s)
        self._run_single_seed(cfg, seen, s, rc)
        return

    def _run_seed_file_path(self, cfg: Path, cfg_idx: int, cfgs_total: int, tr: TestConfigRaw, seen: set[int]) -> None:
        """
        Run the generator for a specific configuration and seeds from a file.
        """
        seeds_list = self._read_seeds_file(Path(self.args.seed_file))
        seeds_total = len(seeds_list)
        for seed_idx, s in enumerate(seeds_list, start=1):
            self._print_run_header(s, cfg.name, seed_idx, seeds_total, cfg_idx, cfgs_total)
            s = int(s)
            rc = self._resolve_config_for_seed(tr, s)
            self._run_single_seed(cfg, seen, s, rc, self.args.seed_file)
            self._pause(next_exists=(seed_idx < seeds_total))
        return

    def _run_for_config(self, cfg: Path, cfg_idx: int, cfgs_total: int) -> None:
        """
        Run the generator for a specific configuration file.
        """
        try:
            raw = self._load_run_config(cfg)
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
        # - If omitted, place under <config-dir>/results/
        cfg_dir_path = Path(self.args.config_dir).resolve()

        if self.results_dir_arg:
            # Honor explicit --results-dir exactly as provided
            rd = Path(self.results_dir_arg).resolve()
        else:
            rd = cfg_dir_path / "results"

        rd.mkdir(parents=True, exist_ok=True)
        self.results_dir = rd

        # failures file lives inside the chosen results dir
        self.failed_f = self.results_dir / "failed.csv"
        self.failed_f.touch(exist_ok=True)

        LOG.info(f"results-dir resolved to: {self.results_dir}")

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
        if not self.args.config_dir:
            raise SystemExit("--config-dir is required")
        if self.args.seed is not None and self.args.seed < 1:
            raise SystemExit("--seed must be a positive integer")
        if self.args.seed_file and self.args.count is not None:
            raise SystemExit("--seed-file cannot be used with --count")
        if self.args.seed is not None and self.args.count is not None:
            raise SystemExit("--seed cannot be used with --count")
        if self.args.count is not None and self.args.count < -1:
            raise SystemExit("--count must be -1 (infinite) or a positive integer")

        # --- check seed file and config dir ---
        if self.args.seed_file:
            file_exists(self.args.seed_file)
        dir_exists(self.args.config_dir)
        
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
        LOG.info(f"starting; cluster={self.args.cluster_name}  runtime={self.args.kwok_runtime}, config-dir={self.args.config_dir}{seed_file_str}")

        cfgs = self._get_kwok_configs(self.args.config_dir)
        cfgs_total = len(cfgs)
        self._validate_all_configs(cfgs)

        LOG.info(f"configs={cfgs_total}")
        for cfg_idx, cfg in enumerate(cfgs, start=1):
            LOG.info(f"\n================================================ CONFIG RUN {cfg_idx}/{cfgs_total} ================================================\n"
                     f"config={cfg}\n"
                     "----------------------------------------------------------------------------------------------------------------")
            self._run_for_config(cfg, cfg_idx, cfgs_total)
            self._pause(next_exists=(cfg_idx < cfgs_total))

# ===============================================================
# Logging
# ===============================================================
def _format_args_block(args) -> str:
    # Keep a stable, explicit order
    fields = [
        ("cluster_name", args.cluster_name),
        ("kwok_runtime", args.kwok_runtime),
        ("config_dir", args.config_dir),
        ("results_dir", args.results_dir),
        ("overwrite", args.overwrite),
        ("max_rows_per_file", args.max_rows_per_file),
        ("seed", args.seed),
        ("seed_file", args.seed_file),
        ("generate_seeds_to_file", args.generate_seeds_to_file),
        ("count", args.count),
        ("matrix_file", args.matrix_file),
        ("matrix_parallel", args.matrix_parallel),
        ("test", args.test),
        ("save_scheduler_logs", args.save_scheduler_logs),
        ("trigger_optimizer", args.trigger_optimizer),
        ("optimizer_url", (args.optimizer_url if getattr(args, "trigger_optimizer", False) else "<unused>")),
        ("pause", args.pause),
        ("log_level", args.log_level),
    ]
    pad = max(len(k) for k, _ in fields)
    def _fmt(v):
        return "<unset>" if v in (None, "") else str(v)
    lines = [f"{k.rjust(pad)} = {_fmt(v)}" for k, v in fields]
    return "\n".join(lines)

def log_args_in_use(args, *, use_logger: bool = True) -> None:
    block = _format_args_block(args)
    header = "\n==================== ARGS IN USE ====================\n"
    footer = "\n====================================================="
    if use_logger:
        LOG.info("%s%s%s", header, block, footer)
    else:
        print(f"{header}{block}{footer}", flush=True)
class PrefixFilter(logging.Filter):
    """
    Injects a static 'prefix' field into each LogRecord.
    """
    def __init__(self, prefix: str):
        super().__init__()
        self.prefix = prefix
    def filter(self, record: logging.LogRecord) -> bool:
        record.prefix = self.prefix
        return True

def setup_logging(prefix: str, level: str = "INFO") -> logging.Logger:
    """
    Configure the module logger.
    - prefix: shown before every message (e.g. '[worker 2/5 cluster=kwok1] ').
    - level:  DEBUG/INFO/WARNING/ERROR/CRITICAL (case-insensitive).
    """
    lvl = getattr(logging, str(level).upper(), logging.INFO)
    logger = logging.getLogger("kwok")
    logger.propagate = False
    logger.setLevel(lvl)
    logger.handlers.clear()
    h = logging.StreamHandler(stream=sys.stdout)
    fmt = logging.Formatter("%(asctime)s %(prefix)s%(message)s", datefmt="%H:%M:%S")
    h.setFormatter(fmt)
    h.addFilter(PrefixFilter(prefix))
    logger.addHandler(h)
    logging.captureWarnings(True)
    return logger

##############################################
# ------------ Matrix runner -----------------
##############################################
MATRIX_REQUIRED_COLS = ["cluster-name", "config-dir", "seed-file", "results-dir"]

def run_matrix(args) -> int:
    log_args_in_use(args, use_logger=False)
    
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

    def _run_one_row(runtime: str, row: dict, idx: int, total: int) -> int:
        print(
            "\n==================== MATRIX RUN {}/{} ====================\n"
            "[matrix] cluster-name={}\n"
            "[matrix] config-dir={}\n"
            "[matrix] seed-file={}\n"
            "[matrix] results-dir={}\n"
            "------------------------------------------------------------------"
            .format(idx, total, row["cluster-name"], row["config-dir"], row["seed-file"], row["results-dir"]),
            flush=True,
        )
        cmd = [
            sys.executable, "-u", os.path.abspath(__file__),
            "--cluster-name", row["cluster-name"],
            "--kwok-runtime", runtime,
            "--config-dir",   row["config-dir"],
            "--seed-file",    row["seed-file"],
            "--results-dir",  row["results-dir"],
        ]
        # forward flags
        if args.overwrite:
            cmd.append("--overwrite")
        if args.trigger_optimizer:
            cmd.append("--trigger-optimizer")
        if args.save_scheduler_logs:
            cmd.append("--save-scheduler-logs")
        env = os.environ.copy()
        env["PYTHONUNBUFFERED"] = "1"
        env["KWOK_LOG_PREFIX"] = f"[worker {idx}/{total} cluster={row['cluster-name']}] "
        print("[matrix] exec:", " ".join(cmd), flush=True)
        return subprocess.run(cmd, check=False, env=env).returncode

    rows = _read_csv_matrix(args.matrix_file)
    total = len(rows)
    rcodes = []
    if args.matrix_parallel > 1:
        with ThreadPoolExecutor(max_workers=args.matrix_parallel) as pool:
            futs = [pool.submit(_run_one_row, args.kwok_runtime, r, i, total) for i, r in enumerate(rows, start=1)]
            for fut in as_completed(futs):
                rcodes.append(fut.result())
    else:
        for i, r in enumerate(rows, start=1):
            rcodes.append(_run_one_row(args.kwok_runtime, r, i, total))
    return 0 if all(r == 0 for r in rcodes) else 1

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
    ap.add_argument("--config-dir", dest="config_dir", default=None, help="Directory containing one or more KWOK config YAMLs")
    ap.add_argument("--results-dir", dest="results_dir", default=None,
                    help=("Directory to store results. If omitted, results are written to "
                          "<config-dir>/results/"))
    ap.add_argument("--overwrite", action="store_true", help="Replace any existing results for the same seed.")
    ap.add_argument("--max-rows-per-file", dest="max_rows_per_file", type=int, default=500000,
                    help="Maximum number of data rows per results CSV before rotating (default 500000).")
    
    # seeds
    ap.add_argument("--seed", type=int, default=None, help="Run exactly this seed (per kwok-config)")
    ap.add_argument("--seed-file", dest="seed_file", default=None, help="Path to seeds file (CSV with 'seed' col or newline list).")
    ap.add_argument("--generate-seeds-to-file", dest="generate_seeds_to_file", default=None,
                    help="Write --count random seeds (one per line, no header) to this file, then exit.")
    ap.add_argument("--count", type=int, default=None, help="Generate the specified number of random seeds; -1=infinite.")
    
    # matrix
    ap.add_argument("--matrix-file", help="To use this functionality stand in the root of 'content' folder. CSV with columns: cluster-name,config-dir,results-dir,seed-file.")
    ap.add_argument("--matrix-parallel", type=int, dest="matrix_parallel", default=1, help="Max concurrent runs when using --matrix-file (default 1).")
    
    # test
    ap.add_argument("--test", action="store_true", help="Test mode: requires --seed; prints summary only; no results are saved.")

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

    return ap

def main():
    """
    Main entry point for the KWOK test generator.
    """
    args = build_argparser().parse_args()

    if args.matrix_file and args.pause:
        LOG.info("Ignoring --pause because --matrix-file is in use.")
        args.pause = False

    env_prefix = os.environ.get("KWOK_LOG_PREFIX") or f"[cluster={args.cluster_name}] "
    _ = setup_logging(env_prefix, args.log_level)
    
    # Show args in use at startup (normal or matrix path)
    log_args_in_use(args, use_logger=True)

    runner = KwokTestGenerator(args)

    if args.matrix_file and args.kwok_runtime:
        sys.exit(run_matrix(args))

    runner.run()

if __name__ == "__main__":
    main()
