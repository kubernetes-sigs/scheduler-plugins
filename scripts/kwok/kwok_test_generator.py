#!/usr/bin/env python3
# kwok_test_generator.py

################################################################################
# Arguments:
#
#   --cluster-name <name>               KWOK cluster name (default: "kwok1")
#
#   --config-dir <dir>                  Directory containing one or more KWOK config YAMLs (default: "./kwok_configs")
#
#   --results-dir <dir>                 Directory to store results CSV files (default: "./results")
#
#   --max-rows-per-file <int>           Maximum rows per CSV file before rotation (default: 500000)
#
#   --failed-file <file>                File to log failed seeds (default: "./failed.csv")
#
#   --seed <int>                        Run exactly this seed (per kwok-config)
#
#   --seed-file <file>                  Path to seeds file (CSV with 'seed' col or newline list)
#
#   --generate-seeds-to-file <file>     Write --count random seeds (one per line, no header) to this file, 
#                                       then exit. Cannot be combined with --seed/--seed-file.
#   --count <int>                       Generate random seeds; -1=infinite
#
#   --test                              Test mode: requires --seed; runs each kwok-config and prints a per-config summary
#                                       of scheduled vs unscheduled at the end. No results are saved in test mode.
################################################################################

import csv, json, time, random, argparse, re, subprocess, tempfile, os, textwrap, hashlib, math, yaml, traceback
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional, Tuple, List, Dict, Any
from kwok_shared import (
    get_scheduled_and_unscheduled, get_json_ctx, stat_snapshot, dir_exists, file_exists,
    csv_append_row, qty_to_mcpu_str, qty_to_bytes_str, qty_to_bytes_int, qty_to_mcpu_int,
    MEM_UNIT_TABLE
)

# ===============================================================
# Constants
# ===============================================================
RESULTS_HEADER = [
    "timestamp", "kwok_config", "seed_file", "seed",
    "num_nodes", "pods_per_node", "num_priorities", "num_replicaset", "num_replicas_per_rs_set", 
    "node_cpu_m", "node_mem_b", "cpu_per_pod_m", "mem_per_pod_b",
    "util", "util_tolerance", "util_run_cpu", "util_run_mem", "cpu_m_run", "mem_b_run",
    "wait_mode", "wait_timeout_s", "settle_timeout_s",
    "scheduled_count","unscheduled_count", "pods_run_by_node", "placed_by_priority",
    "unscheduled", "scheduled",
    "pod_node",
]

# ====================================================================
# YAML builders.
# Due to indentation problems we keep them outside class
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
    pods_per_node: int = 0

    # priorities: interval (picked per seed) – must be provided in YAML
    # Accepts fixed (becomes (x,x)) or [lo,hi]
    num_priorities: Optional[Tuple[int, int]] = None

    # ReplicaSets (how many RS objects) — OPTIONAL
    num_replicaset: Optional[Tuple[int, int]] = None           # (lo, hi)
    # Per-RS replica counts (size of each RS), if provided
    num_replicas_per_rs_set: Optional[Tuple[int, int]] = None  # (lo, hi)

    # node capacity (strings from YAML)
    node_cpu: str = ""       # "24" or "25000m"
    node_mem: str = ""       # "32Gi"

    # per-pod intervals (K8s quantities as strings)
    cpu_per_pod: Optional[Tuple[str, str]] = None  # e.g. ("100m","1500m")
    mem_per_pod: Optional[Tuple[str, str]] = None  # e.g. ("128Mi","2048Mi")

    # utilization target
    util: float = 0.0
    util_tolerance: float = 0.0

    # waits
    wait_mode: Optional[str] = None     # None/"none","exist","ready","running"
    wait_timeout: Optional[str] = None  # "5s" etc.
    settle_timeout: Optional[str] = None

    # internal
    source_file: Optional[Path] = field(default=None, repr=False)

@dataclass
class TestConfigApplied:
    namespace: str
    num_nodes: int
    pods_per_node: int
    node_m: int                       # mCPU per node
    node_b: int                       # bytes per node (kept for display)
    num_priorities: int                # chosen per seed
    num_replicaset: int                # chosen per seed
    num_replicas_per_rs_set: Optional[Tuple[int, int]] # oossibly ommited if not running with replicasets
    util: float
    util_tolerance: float
    cpu_per_pod_m: Tuple[int, int]   # millicores
    mem_per_pod_b: Tuple[int, int]   # bytes
    wait_mode: Optional[str]
    wait_timeout_s: int
    settle_timeout_s: int
    source_file: Optional[Path] = field(default=None, repr=False)

    @property
    def total_pods(self) -> int:
        return self.num_nodes * self.pods_per_node

# ===============================================================
# KwokTestGenerator
# ===============================================================
class KwokTestGenerator:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.ctx: Optional[str] = None
        self.results_dir = Path(args.results_dir)
        
        self.max_rows_per_file = int(args.max_rows_per_file)
        
        self.failed_f = Path(args.failed_file)
        self.failed_f.parent.mkdir(parents=True, exist_ok=True)
        self.failed_f.touch(exist_ok=True)

    ######################################################
    # ---------- Parsing helpers ----------
    ######################################################
    @staticmethod
    def _format_interval(tup: Optional[Tuple[int, int]]) -> str:
        """
        Format a tuple of integers as a string interval.
        """
        return "" if not tup else f"{int(tup[0])},{int(tup[1])}"

    # @staticmethod
    # def _default_timeout_str(v: Optional[str], default: str = "5s") -> str:
    #     """
    #     Return the trimmed string, or fallback to default if empty.
    #     """
    #     s = (v or "").strip()
    #     return s if s else default
 
    def _parse_waits(self, tr: TestConfigRaw) -> tuple[Optional[str], str, str, int, int]:
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
    def _split_interval_str(s: str) -> Tuple[str, str]:
        """
        Split a string interval "lo,hi" or "x" into (lo, hi).
        Returns (x,x) if only one part is given.
        """
        parts = [x.strip() for x in s.split(",", 1)]
        return (parts[0], parts[0]) if len(parts) == 1 else (parts[0], parts[1])

    @staticmethod
    def _parse_timeout_s(t:str) -> int:
        """
        Parse a timeout string into seconds.
        """
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
            return default

    @staticmethod
    def _get_str_from_dict(doc: Dict[str, Any], key: str, default: str) -> str:
        """
        Get a string value from the document, returning a default if not found or empty.
        """
        v = doc.get(key, default)
        s = str(v).strip()
        return s if s != "" else default

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
    def _is_interval_feasible(interval: Optional[Tuple[int,int]], n: int, target: int) -> bool:
        """
        Check if the given interval is feasible for the specified n and total.
        """
        if not interval or n <= 0: 
            return False
        lo, hi = interval
        if lo < 1 or hi < lo: 
            return False
        return n*lo <= target <= n*hi
    
    @staticmethod
    def _resolve_interval(num_nodes, pods_per_node, util, val, interval, *, cap_each, jitter=0.5):
        """
        Resolve the interval for the given parameters.
        """
        total_pods = num_nodes * pods_per_node
        target = int(val * util * num_nodes)
        if interval is not None:
            return interval, False # validated already; just use it
        auto = KwokTestGenerator._gen_interval(target, total_pods, cap_each=cap_each, jitter=jitter)
        return auto, True
    
    @staticmethod
    def _gen_interval(total: int, n: int, *, cap_each: Optional[int], jitter: float = 0.4) -> Tuple[int,int]:
        """
        Build a [lo, hi] around the mean so that n*lo <= total <= n*hi.
        cap_each (e.g. node capacity for that resource) is optional.
        Raises ValueError if impossible (e.g., cap_each < ceil(total/n)).
        Jitter is a fraction [0.0,1.0] to spread lo/hi around the mean.
        """
        if n <= 0:
            raise ValueError("n must be > 0")
        avg = total / n
        lo = max(1, int(math.floor(avg * (1.0 - jitter))))
        hi = int(math.ceil(avg * (1.0 + jitter)))
        hi = max(hi, int(math.ceil(total / n)))
        lo = min(lo, int(math.floor(total / n)))
        if cap_each is not None:
            if cap_each < int(math.ceil(total / n)):
                raise ValueError("Target cannot be met under per-pod cap.")
            hi = min(hi, cap_each)
            hi = max(hi, int(math.ceil(total / n)))
        if lo > hi:
            lo = max(1, min(int(math.floor(total / n)), hi))
        if not (n*lo <= total <= n*hi):
            raise ValueError("Interval generation failed to produce a feasible range")
        return lo, hi

    @staticmethod
    def _gen_random_parts(total: int, n: int, lo: int, hi: int, rng: random.Random) -> List[int]:
        """
        Generate n parts in [lo, hi] that sum to total.
        Uses a simple greedy algorithm to ensure feasibility at each step.
        First generates in order, then shuffles the result.
        """
        parts: List[int] = []
        rem = total
        for i in range(n, 0, -1):
            min_i = max(lo, rem - (i-1)*hi)
            max_i = min(hi, rem - (i-1)*lo)
            if min_i > max_i:
                raise ValueError("Interval became infeasible during generation")
            x = rng.randint(min_i, max_i) # take random in [min_i, max_i]
            parts.append(x)
            rem -= x # update remaining
        rng.shuffle(parts)
        return parts

    @staticmethod
    def _gen_rs_specs(
        rng: random.Random,
        counts: list[int],
        c_lo: int, c_hi: int,
        m_lo: int, m_hi: int,
        target_cpu: int, target_mem: int,
        util: float, util_tol: float,
        *,
        max_steps: int = 100_000
    ) -> tuple[list[int], list[int], list[int], bool]:
        """
        Choose per-RS per-pod CPU/MEM requests in [lo, hi] so that the
        weighted sums (sum_i counts[i] * x[i]) are close to the CPU/MEM targets.
        Strategy:
        1) Start with random values in bounds for each RS (for CPU and MEM).
        2) Iteratively "nudge" random RS templates up/down in small steps
            toward the target in each dimension (CPU, MEM).
        3) If nudging stalls, try moving one replica from a "heavier" RS to
            a "lighter" RS if it reduces total error across CPU+MEM.
        4) Stop when within tolerance or we hit `max_steps`.
        Returns:
        (counts, cpu_x, mem_x, ok)
            - counts: may be mutated by replica moves (same as before)
            - cpu_x/mem_x: per-RS per-pod requests
            - ok: True if inside tolerance, else best-effort False
        """
        n = len(counts)
        if n == 0:
            return counts, [], [], True

        # ---- Initial draws within bounds
        cpu_x = [rng.randint(c_lo, c_hi) for _ in range(n)]
        mem_x = [rng.randint(m_lo, m_hi) for _ in range(n)]
        x = {"cpu": cpu_x, "mem": mem_x}

        # ---- Targets / tolerances per resource
        def abs_tol(target: int) -> int:
            # If util is 0 (defensive), fall back to ~1% band.
            frac = (util_tol / util) if util > 0 else 0.01
            return max(1, int(round(target * frac)))

        resources = {
            "cpu": {"lo": c_lo, "hi": c_hi, "target": target_cpu, "tol": abs_tol(target_cpu)},
            "mem": {"lo": m_lo, "hi": m_hi, "target": target_mem, "tol": abs_tol(target_mem)},
        }

        # Weighted sums S[res] = sum_i counts[i] * x[res][i]
        def wsum(res: str) -> int:
            vals = x[res]
            return sum(v * c for v, c in zip(vals, counts))

        S = {res: wsum(res) for res in resources}

        def within() -> bool:
            # Are all resources within their individual tolerances?
            return all(abs(S[res] - resources[res]["target"]) <= resources[res]["tol"] for res in resources)

        if within():
            return counts, cpu_x, mem_x, True

        # ---- Step sizes (~2% of each range; at least 1m CPU, 1MiB MEM)
        Mi = MEM_UNIT_TABLE["mi"]
        steps = {
            "cpu": max(1, (c_hi - c_lo) // 50),
            "mem": max(Mi, ((m_hi - m_lo) // 50) // Mi * Mi),
        }

        # ---- Local adjustment of a single RS template in a given resource
        def nudge_one(res: str, want_up: bool) -> bool:
            """Try to adjust one RS's per-pod request up/down by one step."""
            lo, hi = resources[res]["lo"], resources[res]["hi"]
            vals = x[res]

            # Candidate RS indices that can move in the desired direction.
            can_move = [i for i in range(n) if (want_up and vals[i] < hi) or ((not want_up) and vals[i] > lo)]
            if not can_move:
                return False

            # Prefer RSes with more replicas (bigger effect for same step).
            i = rng.choices(can_move, weights=[max(1, counts[j]) for j in can_move], k=1)[0]
            delta = steps[res] if want_up else -steps[res]
            new_v = max(lo, min(hi, vals[i] + delta))
            actual = new_v - vals[i]
            if actual == 0:
                return False
            vals[i] = new_v
            S[res] += counts[i] * actual  # keep the running sum in sync
            return True

        # ---- Move exactly one replica (from i to j) if it reduces joint error
        def move_one_replica() -> bool:
            if sum(counts) <= 1:
                return False
            # Greedy order: heavier templates first.
            order = sorted(
                range(n),
                key=lambda i: (cpu_x[i] + mem_x[i]) * max(1, counts[i]),
                reverse=True
            )

            def total_error_after(i_from: int, i_to: int) -> int:
                # If we move one replica from i_from to i_to, each resource's S changes
                # by (x[to] - x[from]) for that resource.
                err = 0
                for res in resources:
                    new_S = S[res] + (x[res][i_to] - x[res][i_from])
                    err += abs(new_S - resources[res]["target"])
                return err

            cur_err = sum(abs(S[res] - resources[res]["target"]) for res in resources)

            for i in order:
                if counts[i] <= 1:
                    continue
                for j in reversed(order):
                    if i == j:
                        continue
                    new_err = total_error_after(i, j)
                    if new_err < cur_err:
                        # Apply the move
                        counts[i] -= 1
                        counts[j] += 1
                        for res in resources:
                            S[res] += (x[res][j] - x[res][i])
                        return True
            return False

        # ---- Main improvement loop
        for _ in range(max_steps):
            if within():
                return counts, cpu_x, mem_x, True

            changed = False
            # Randomize resource order each step to avoid bias.
            for res in rng.sample(list(resources.keys()), k=len(resources)):
                want_up = S[res] < resources[res]["target"]
                changed |= nudge_one(res, want_up)

            if changed:
                continue
            if move_one_replica():
                continue
            break  # no progress possible

        return counts, cpu_x, mem_x, within()

    @staticmethod
    def _gen_num_replicas_per_replicaset(
        total_pods: int,
        num_replicaset: int,
        rng: random.Random,
        replicas_cnt_interval: Optional[Tuple[int, int]] = None, 
    ) -> List[int]:
        """
        Partition total_pods into num_replicaset positive integers (exact sum).
        If 'interval' is provided (lo,hi), draw each RS's replica count in [lo,hi]
        while guaranteeing feasibility (n*lo <= total <= n*hi).
        Otherwise, use an auto interval around the average.
        """
        if num_replicaset <= 0:
            return []
        if num_replicaset == 1:
            return [total_pods]

        if replicas_cnt_interval:
            lo, hi = int(replicas_cnt_interval[0]), int(replicas_cnt_interval[1])
            lo = max(1, lo)
            hi = max(lo, hi)
            if not (num_replicaset * lo <= total_pods <= num_replicaset * hi):
                raise ValueError(
                    f"num_replicas_per_set infeasible: n*lo={num_replicaset*lo} "
                    f"total={total_pods} n*hi={num_replicaset*hi}"
                )
        else: # TODO: NOT SURE WE NEED THE AUTOGENERATED BY ANYMORE; AS WE CHECK ALL CONFIGS. MAYBE WE STILL NEED IT IF NOT SPECIFIED?
            lo, hi = KwokTestGenerator._gen_interval(
                total_pods, num_replicaset, cap_each=None, jitter=0.4
            )
            lo = max(1, lo)

        return KwokTestGenerator._gen_random_parts(total_pods, num_replicaset, lo, hi, rng)

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

    def _pick_results_csv_to_write(self, stem: str, rows_to_add: int = 1) -> Path:
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
            print(f"[kwok-test-gen] starting new segment: {target.name}")
            return target

        last_idx, last_path = segs[-1]
        rows = self._count_csv_rows(last_path)

        # Rotate if we would exceed the limit with this write.
        if rows + rows_to_add > self.max_rows_per_file:
            next_path = self.results_dir / f"{stem}_{last_idx + 1}.csv"
            self._ensure_csv_with_header(next_path, RESULTS_HEADER)
            print(f"[kwok-test-gen] {last_path.name} is full ({rows}/{self.max_rows_per_file}); "
                f"switching to {next_path.name}")
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
    
    ##############################################
    # ------------ KWOK / kubectl helpers --------
    ##############################################
    @staticmethod
    def _apply_yaml(ctx:str, yaml_text:str) -> subprocess.CompletedProcess:
        """
        Apply a YAML configuration to the cluster.
        """
        return subprocess.run(["kubectl","--context",ctx,"apply","-f","-"], input=yaml_text.encode(), check=True)
    
    @staticmethod
    def _prepare_kwokctl_config_file(src: Path) -> tuple[Path, Path | None]: #TODO: find a better name
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
                # optionally also check apiVersion prefix
                apiver = str(d.get("apiVersion", ""))
                if not apiver.startswith("config.kwok.x-k8s.io/"):
                    continue
                chosen = d
                break
        if chosen is None:
            return src, None

        # write chosen doc to a temp file
        tf = tempfile.NamedTemporaryFile("w", delete=False, suffix=".yaml")
        try:
            yaml.safe_dump(chosen, tf, sort_keys=False)
        finally:
            tf.close()
        return Path(tf.name), Path(tf.name)

    @staticmethod
    def _ensure_kwok_cluster(name_or_ctx: str, kwok_config: str | Path | None, recreate: bool = True) -> None:
        """
        Ensure a KWOK cluster with the given name or context exists.
        """
        cluster_name = name_or_ctx[len("kwok-"):] if name_or_ctx.startswith("kwok-") else name_or_ctx
        cfg_path = Path(kwok_config) if kwok_config else None

        if recreate:
            print(f"[kwok-test-gen] recreating kwok cluster '{cluster_name}'")
            subprocess.run(["kwokctl", "delete", "cluster", "--name", cluster_name], check=False)

        if cfg_path and cfg_path.exists():
            cfg_for_kwokctl, tmp_to_cleanup = KwokTestGenerator._prepare_kwokctl_config_file(cfg_path)
            try:
                subprocess.run(
                    ["kwokctl", "create", "cluster", "--name", cluster_name, "--config", str(cfg_for_kwokctl)],
                    check=True
                )
            finally:
                if tmp_to_cleanup and tmp_to_cleanup.exists():
                    try: os.unlink(tmp_to_cleanup)
                    except OSError: pass
        else:
            if cfg_path and not cfg_path.exists():
                print(f"[kwok-test-gen] kwok-config='{cfg_path}' not found; creating with defaults")
            subprocess.run(["kwokctl", "create", "cluster", "--name", cluster_name], check=True)

    @staticmethod
    def _create_kwok_nodes(ctx: str, num_nodes: int, node_cpu: str, node_mem: str, pods_cap: int) -> None:
        """
        Create KWOK nodes kwok-node-1..kwok-node-N with the given capacity.
        """
        node_yaml = "".join(
            yaml_kwok_node(f"kwok-node-{i}", node_cpu, node_mem, pods_cap)
            for i in range(1, num_nodes + 1)
        )
        if node_yaml:
            KwokTestGenerator._apply_yaml(ctx, node_yaml)

    @staticmethod
    def _delete_kwok_nodes(ctx: str) -> None:
        """
        Delete all KWOK nodes in the given context.
        """
        r = subprocess.run(["kubectl","--context",ctx,"get","nodes","-l","type=kwok","-o","json"],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r.returncode != 0:
            return
        items = (json.loads(r.stdout).get("items") or [])
        for n in items:
            name = (n.get("metadata") or {}).get("name")
            if name:
                subprocess.run(["kubectl","--context",ctx,"delete","node",name,"--ignore-not-found"],
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    
    @staticmethod
    def _ensure_namespace(ctx: str, ns: str, *, recreate: bool = False, retries: int = 15, delay: float = 0.5) -> None:
        """
        Ensure the namespace exists in the given context.
        If recreate=True, delete it first, then (re)create if missing.
        """
        if recreate:
            KwokTestGenerator._delete_namespace(ctx, ns)
        rns = subprocess.run(["kubectl","--context",ctx,"get","ns",ns],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if rns.returncode != 0:
            subprocess.run(["kubectl","--context",ctx,"create","ns",ns], check=True)
        
        # Ensure the default serviceaccount exists in the namespace; before we exit
        for _ in range(1, retries + 1):
            r = subprocess.run(["kubectl","--context",ctx,"-n",ns,"get","sa","default"],
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            if r.returncode == 0:
                print(f"[kwok-test-gen] found default serviceaccount in ns '{ns}'")
                break
            time.sleep(delay)

    @staticmethod
    def _delete_namespace(ctx: str, ns: str) -> None:
        """
        Delete the specified namespace in the given context.
        """
        print(f"[kwok-test-gen] deleting namespace '{ns}'...")
        try:
            subprocess.run(
                ["kubectl", "--context", ctx, "delete", "namespace", ns, "--ignore-not-found"],
                check=True,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.STDOUT
            )
            print(f"[kwok-test-gen] namespace '{ns}' deleted (ctx={ctx})")
        except:
            print(f"[kwok-test-gen] error deleting namespace '{ns}' in ctx={ctx}")

    @staticmethod
    def _ensure_priority_classes(
        ctx: str,
        num_priorities: int,
        *,
        prefix: str = "p",
        start: int = 1,
        delete_extras: bool = False,
    ) -> None:
        """
        Ensure PriorityClasses {prefix}{i} for i in [start, start+num_priorities).
        If delete_extras=True, remove any of our prefixed PCs outside that set.
        """
        pcs_yaml = "".join(
            yaml_priority_class(f"{prefix}{v}", v)
            for v in range(start, start + num_priorities)
        )
        if pcs_yaml:
            KwokTestGenerator._apply_yaml(ctx, pcs_yaml)
        if delete_extras:
            KwokTestGenerator._cleanup_priority_classes(ctx, desired_count=num_priorities, prefix=prefix, start=start)

    @staticmethod
    def _cleanup_priority_classes(ctx: str, desired_count: int, *, prefix: str = "p", start: int = 1) -> None:
        """
        Delete PriorityClasses created by us (name startswith prefix) that are not in the desired set.
        Never touches system/global defaults.
        """
        r = subprocess.run(["kubectl","--context",ctx,"get","priorityclasses.scheduling.k8s.io","-o","json"],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r.returncode != 0:
            return

        system_names = {"system-cluster-critical", "system-node-critical"}
        desired = {f"{prefix}{i}" for i in range(start, start + desired_count)}

        pcs = json.loads(r.stdout).get("items", [])
        for pc in pcs:
            name = (pc.get("metadata") or {}).get("name", "")
            global_default = bool(pc.get("globalDefault"))
            if not name:
                continue
            # Only delete our own prefixed PCs, never system/global defaults, and only if not desired
            if name.startswith(prefix) and name not in desired and name not in system_names and not global_default:
                subprocess.run(["kubectl","--context",ctx,"delete","priorityclass.scheduling.k8s.io", name],
                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

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
            r = subprocess.run(["kubectl","--context",ctx,"-n",ns,"get","pod",name,"-o","json"],
                    stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)

            if mode == "exist":
                if r.returncode == 0:
                    return 1
                time.sleep(0.5)
                continue

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
            print(f"[kwok-test-gen] waiting on pod to be {mode}")
            time.sleep(0.5)
        print(f"[kwok-test-gen] timeout waiting for pod '{name}' in ns '{ns}' to be {mode}")
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

            r = subprocess.run(["kubectl","--context",ctx,"-n",ns,"get","pods","-l",f"app={rs_name}","-o","json"],
                    stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            if r.returncode != 0:
                time.sleep(0.5); continue

            try:
                podlist = json.loads(r.stdout).get("items", [])
                if mode == "exist":
                    count = len(podlist)
                else:
                    count = 0
                    for p in podlist:
                        phase = (p.get("status") or {}).get("phase") or ""
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
            print(f"[kwok-test-gen] waiting on rs={rs_name} to be {mode}; desired={desired} current={count}")
            time.sleep(0.5)

        print(f"[kwok-test-gen] timeout waiting for RS '{rs_name}' in ns '{ns}' to have desired pods ready")
        return last_count
    
    def _apply_standalone_pods(
        self, cfg: Path, seed_int: int, ta: TestConfigApplied, rng_layout: random.Random,
    ) -> List[Dict[str, object]]:
        """
        Create standalone pods in the given namespace with the specified specs.
        Returns a list of pod specs created.
        """
        c_lo, c_hi = ta.cpu_per_pod_m
        m_lo_b, m_hi_b = ta.mem_per_pod_b

        rng_cpu = self._rng(seed_int, "cpu_parts", cfg.stem)
        rng_mem = self._rng(seed_int, "mem_parts", cfg.stem)

        alloc_cpu_m = ta.node_m * ta.num_nodes
        alloc_mem_b = ta.node_b * ta.num_nodes
        tgt_mc_cluster = int(alloc_cpu_m * ta.util)
        tgt_b_cluster  = int(alloc_mem_b * ta.util)

        cpu_parts = self._gen_random_parts(tgt_mc_cluster, ta.total_pods, c_lo, c_hi, rng_cpu)
        mem_parts = self._gen_random_parts(tgt_b_cluster,  ta.total_pods, m_lo_b, m_hi_b, rng_mem)
        
        specs: List[Dict[str, object]] = []
        names: List[str] = []
        for i in range(ta.total_pods):
            prio = rng_layout.randint(1, max(1, ta.num_priorities))
            pc = f"p{prio}"
            name = f"pod-{i+1:03d}-{pc}"
            cpu_m_str = max(1, int(cpu_parts[i]))
            mem_b_str = max(1, int(mem_parts[i]))
            cpu_m_int  = qty_to_mcpu_str(cpu_m_str)
            mem_b_int = qty_to_bytes_str(mem_b_str)
            KwokTestGenerator._apply_yaml(self.ctx, yaml_kwok_pod(ns, name, cpu_m_int, mem_b_int, pc))
            names.append(name)
            specs.append({"name": name, "cpu_m": cpu_m_str, "mem_b": mem_b_str, "priority": pc})
            if ta.wait_mode == "running":
                _ = KwokTestGenerator._wait_each(self.ctx, "pod", name, ta.namespace, ta.wait_timeout_s, ta.wait_mode)
        if ta.wait_mode in ("exist", "ready"):
            for name in names:
                _ = KwokTestGenerator._wait_each(self.ctx, "pod", name, ta.namespace, ta.wait_timeout_s, ta.wait_mode)
        return specs

    def _apply_replicasets(
        self, ns: str, rng: random.Random, total_pods: int,
        total_cpu_m: int, total_mem_b: int,
        num_replicaset: int, num_priorities: int,
        cpu_per_pod_used_m: Tuple[int,int] | None,
        mem_per_pod_used_b: Tuple[int,int] | None,
        wait_mode: str | None, wait_timeout_s: int,
        util: float, util_tolerance: float,
        num_replicas_per_rs_set: Optional[Tuple[int,int]] = None,
    ) -> List[Dict[str, object]]:
        """
        Create ReplicaSets in the given namespace with the specified specs.
        Returns a list of replicaset specs created.
        """
        specs: List[Dict[str, object]] = []

        replicas = KwokTestGenerator._gen_num_replicas_per_replicaset(
            total_pods, num_replicaset, rng, replicas_cnt_interval=num_replicas_per_rs_set
        )

        c_lo, c_hi = cpu_per_pod_used_m if cpu_per_pod_used_m else (1, max(1, total_cpu_m))
        m_lo, m_hi = mem_per_pod_used_b if mem_per_pod_used_b else (1, max(1, total_mem_b))
        replicas, cpu_x, mem_x, ok = KwokTestGenerator._gen_rs_specs(
            rng, replicas, c_lo, c_hi, m_lo, m_hi, total_cpu_m, total_mem_b, util, util_tolerance,
        )
        if not ok:
            raise RuntimeError("RS template totals outside util tolerance; widen intervals or relax util/util_tolerance")

        for i, count in enumerate(replicas, start=1):
            pc = f"p{self._prio_for_step_desc(i, num_priorities)}"
            rsname = f"rs-{i:02d}-{pc}"
            cpu_m_str = qty_to_mcpu_str(int(cpu_x[i-1]))
            mem_b_str = qty_to_bytes_str(int(mem_x[i-1]))
            specs.append({
                "name": rsname, "replicas": int(count),
                "cpu_m": int(cpu_x[i-1]), "mem_b": int(mem_x[i-1]), "priority": pc
            })
            KwokTestGenerator._apply_yaml(self.ctx, yaml_kwok_rs(ns, rsname, 0, cpu_m_str, mem_b_str, pc))
            for r in range(1, int(count) + 1):
                KwokTestGenerator._scale_replicaset(self.ctx, ns, rsname, r)
                if wait_mode == "running":
                    _ = KwokTestGenerator._wait_rs_pods(self.ctx, rsname, ns, wait_timeout_s, wait_mode)

        if wait_mode in ("exist", "ready"):
            for spec in specs:
                _ = KwokTestGenerator._wait_each(self.ctx, "rs", spec["name"], ns, wait_timeout_s, wait_mode)

        return specs

    @staticmethod
    def _scale_replicaset(ctx: str, ns: str, rs_name: str, replicas: int) -> None:
        """
        Scale a ReplicaSet to a desired replica count.
        Used to deterministically create pods one-by-one.
        """
        subprocess.run([
            "kubectl", "--context", ctx, "-n", ns,
            "scale", "replicaset", rs_name, f"--replicas={replicas}"
        ], check=True)

    @staticmethod
    def _get_rs_spec_replicas(ctx: str, ns: str, rs_name: str) -> Optional[int]:
        """
        Fetch .spec.replicas for a ReplicaSet, or None if not found.
        """
        r = subprocess.run(["kubectl","--context",ctx,"-n",ns,"get","replicaset",rs_name,"-o","json"],
                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r.returncode != 0:
            return None
        try:
            return int((json.loads(r.stdout).get("spec") or {}).get("replicas") or 0)
        except Exception:
            return None

    @staticmethod
    def _prio_for_step_desc(step: int, num_prios: int) -> int:
        """
        Get the priority for a given step in descending order.
        """
        if num_prios <= 1:
            return 1
        return num_prios - ((step - 1) % num_prios)

    @staticmethod
    def _count_running_by_priority(ctx: str, ns: str) -> Dict[str, int]:
        """
        Count running pods by their priority class in the given namespace.
        """
        out: Dict[str, int] = {}
        pods = get_json_ctx(ctx, ["-n", ns, "get", "pods", "-o", "json"]).get("items", [])
        for p in pods:
            if (p.get("status") or {}).get("phase", "") != "Running":
                continue
            pc = (p.get("spec") or {}).get("priorityClassName") or ""
            out[pc] = out.get(pc, 0) + 1
        return out

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
    def _pick_num_replicaset_for_seed(seed: int, interval: Optional[Tuple[int, int]], total_pods: int) -> int:
        """
        Pick num_replicaset for the given seed in [lo,hi] interval.
        If interval is None or empty, returns 0.
        """
        if not interval:
            return 0
        lo, hi = interval  # already validated: 0 <= lo <= hi <= total_pods
        rng = KwokTestGenerator._rng(seed, "num_replicaset", lo, hi, total_pods)
        return rng.randint(lo, hi)

    ##############################################
    # ------------ Runner helpers ----------------
    ##############################################
    def _write_fail(self, category: str, cfg: Path, seed: int | None,
                    phase: str, message: str, details: str = "") -> None:
        """
        Append a structured row to the failed-file so silent failures are visible.
        Format: ts  category  cfg  seed  phase  message  details
        """
        ts = time.strftime("%Y/%m/%d/%H:%M:%S", time.localtime())
        cfg_s = str(cfg)
        sd = "-" if seed is None else str(seed)
        line = f"{ts}\t{category}\t{cfg_s}\t{sd}\t{phase}\t{message}"
        if details:
            # keep rows single-line; trim long blobs
            compact = details.replace("\n", "\\n")[-1500:]
            line += f"\t{compact}"
        with open(self.failed_f, "a", encoding="utf-8") as f:
            f.write(line + "\n")

    @staticmethod
    def _print_run_header(s, cfg_name, seed_idx, seeds_total, cfg_idx, cfgs_total):
        """
        Print a header for the seed run.
        """
        seed_str = "unlimited" if seeds_total <=-1 else f"{seed_idx}/{seeds_total}"
        print("\n------------------------------------------------- SEED RUN -------------------------------------------------")
        print(f"[kwok-test-gen] seed={s} ({seed_str}) config={cfg_name} ({cfg_idx}/{cfgs_total})")
        print("------------------------------------------------------------------------------------------------------------")

    def _print_seed_summary(self, cfg: Path, seed: int | None, 
                            scheduled: int | None, unscheduled: int | None, 
                            note: str = "") -> None:
        """
        Print a summary line for the run.
        """
        note = f" note='{note}'" if note else ""
        scheduled_str = "-" if scheduled is None else str(scheduled)
        unscheduled_str = "-" if unscheduled is None else str(unscheduled)
        print("--------------------------------------------- SEED SUMMARY ---------------------------------------------")
        print(f"[kwok-test-gen] {cfg.name}  seed={seed}  scheduled={scheduled_str}  unscheduled={unscheduled_str}  {note}")
        print("--------------------------------------------------------------------------------------------------------")

    @staticmethod
    def _build_pod_node_list(
        scheduled_by_name: Dict[str, str],
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
        all_names = set(scheduled_by_name.keys()) | set(unschedulable_names)

        out: List[Dict[str, object]] = []
        for pname in sorted(all_names):
            node = scheduled_by_name.get(pname, "")
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
                rsname = pname.rsplit("-", 1)[0] if "-" in pname else ""
                r = by_rs.get(rsname, {})
                out.append({
                    "name": pname,
                    "cpu_m": int(r.get("cpu_m", 0)),
                    "mem_b": int(r.get("mem_b", 0)),
                    "priority": str(r.get("priority", "")),
                    "node": node,
                })
        return out

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
    def _load_run_config(cfg_path: Path) -> TestConfigRaw:
        """
        Load the runner configuration from the given YAML file.
        """
        with open(cfg_path, "r", encoding="utf-8") as f:
            docs = list(yaml.safe_load_all(f)) or []
        runner_doc = KwokTestGenerator._pick_runner_doc(docs)

        tr = TestConfigRaw(source_file=cfg_path)
        tr.namespace      = KwokTestGenerator._get_str_from_dict(runner_doc, "namespace", tr.namespace)
        tr.num_nodes      = KwokTestGenerator._get_int_from_dict(runner_doc, "num_nodes", tr.num_nodes)
        tr.pods_per_node  = KwokTestGenerator._get_int_from_dict(runner_doc, "pods_per_node", tr.pods_per_node)

        tr.node_cpu       = KwokTestGenerator._get_str_from_dict(runner_doc, "node_cpu", tr.node_cpu)
        tr.node_mem       = KwokTestGenerator._get_str_from_dict(runner_doc, "node_mem", tr.node_mem)

        tr.util           = KwokTestGenerator._get_float_from_dict(runner_doc, "util", tr.util)
        tr.util_tolerance = KwokTestGenerator._get_float_from_dict(runner_doc, "util_tolerance", tr.util_tolerance)
        tr.wait_mode      = KwokTestGenerator._get_wait_mode_from_dict(runner_doc, "wait_mode", tr.wait_mode)
        tr.wait_timeout   = KwokTestGenerator._get_str_from_dict(runner_doc, "wait_timeout", tr.wait_timeout)
        tr.settle_timeout = KwokTestGenerator._get_str_from_dict(runner_doc, "settle_timeout", tr.settle_timeout)

        raw_nr = KwokTestGenerator._normalize_interval(runner_doc, ("num_replicaset","num_replicaset_lo","num_replicaset_hi"))
        if raw_nr:
            tr.num_replicaset = KwokTestGenerator._parse_int_interval(raw_nr, min_lo=0)

        raw_cpu = KwokTestGenerator._normalize_interval(runner_doc, ("cpu_per_pod","cpu_per_pod_lo","cpu_per_pod_hi"))
        if raw_cpu:
            lo, hi = KwokTestGenerator._split_interval_str(raw_cpu)
            tr.cpu_per_pod = (lo, hi)

        raw_mem = KwokTestGenerator._normalize_interval(runner_doc, ("mem_per_pod","mem_per_pod_lo","mem_per_pod_hi"))
        if raw_mem:
            lo, hi = KwokTestGenerator._split_interval_str(raw_mem)
            tr.mem_per_pod = (lo, hi)

        raw_rs = KwokTestGenerator._normalize_interval(runner_doc, ("num_replicas_per_rs_set","num_replicas_per_rs_set_lo","num_replicas_per_rs_set_hi"))
        if raw_rs:
            tr.num_replicas_per_rs_set = KwokTestGenerator._parse_int_interval(raw_rs)

        raw_np = KwokTestGenerator._normalize_interval(runner_doc, ("num_priorities","num_priorities_lo","num_priorities_hi"))
        if raw_np:
            tr.num_priorities = KwokTestGenerator._parse_int_interval(raw_np, min_lo=1) or tr.num_priorities

        return tr

    def _resolve_config_for_seed(self, raw: TestConfigRaw, seed_int: int) -> TestConfigApplied:
        """
        Resolve the raw config into a fully specified applied config for the given seed.
        """
        wait_mode, wait_timeout_s, settle_timeout_s = self._parse_waits(raw)

        # quantities -> canonical ints
        node_mcpu  = qty_to_mcpu_int(raw.node_cpu)
        node_bytes = qty_to_bytes_int(raw.node_mem)

        # choose per-seed knobs
        num_prios  = self._pick_num_priorities_for_seed(seed_int, raw.num_priorities)
        total_pods = raw.num_nodes * raw.pods_per_node
        num_rs     = self._pick_num_replicaset_for_seed(seed_int, raw.num_replicaset, total_pods)

        # raw intervals -> ints in m / b
        cpu_interval_m = None
        if raw.cpu_per_pod:
            c_lo, c_hi = raw.cpu_per_pod
            cpu_interval_m = (qty_to_mcpu_int(c_lo), qty_to_mcpu_int(c_hi))
        mem_interval_b = None
        if raw.mem_per_pod:
            m_lo, m_hi = raw.mem_per_pod
            mem_interval_b = (qty_to_bytes_int(m_lo), qty_to_bytes_int(m_hi))
        
        cpu_used_m, _ = KwokTestGenerator._resolve_interval(
            raw.num_nodes, raw.pods_per_node, raw.util, node_mcpu,
            cpu_interval_m, cap_each=node_mcpu, jitter=0.25
        )
        mem_used_b, _ = KwokTestGenerator._resolve_interval(
            raw.num_nodes, raw.pods_per_node, raw.util, node_bytes,
            mem_interval_b, cap_each=node_bytes, jitter=0.25
        )

        return TestConfigApplied(
            namespace=raw.namespace,
            num_nodes=raw.num_nodes,
            pods_per_node=raw.pods_per_node,
            node_m=node_mcpu,
            node_b=node_bytes,
            num_priorities=num_prios,
            num_replicaset=num_rs,
            num_replicas_per_rs_set=raw.num_replicas_per_rs_set,
            util=raw.util,
            util_tolerance=raw.util_tolerance,
            cpu_per_pod_m=cpu_used_m,
            mem_per_pod_b=mem_used_b,
            wait_mode=wait_mode,
            wait_timeout_s=wait_timeout_s,
            settle_timeout_s=settle_timeout_s,
            source_file=raw.source_file,
        )

    @staticmethod
    def _pick_num_priorities_for_seed(seed: int, interval: Tuple[int, int]) -> int:
        """
        Pick num_priorities for the given seed in [lo,hi] interval.
        If interval is None or empty, returns 1.
        """
        lo, hi = interval  # already validated: 1 <= lo <= hi
        rng = KwokTestGenerator._rng(seed, "num_priorities", lo, hi)
        return rng.randint(lo, hi)

    @staticmethod
    def _validate_config(tr: TestConfigRaw) -> tuple[bool, str]:
        """
        Validate the raw config. Do NOT raise; return (ok, aggregated_message).
        All checks live here and we accumulate *all* failures.
        """
        errors: list[str] = []

        # ---------- basic presence ----------
        if not tr.namespace or not str(tr.namespace).strip():
            errors.append("namespace must be a non-empty string")

        if tr.num_nodes < 1:
            errors.append("num_nodes must be >= 1")

        if tr.pods_per_node < 1:
            errors.append("pods_per_node must be >= 1")

        # ---------- quantities (parse using helpers) ----------
        node_m: Optional[int] = None
        node_b: Optional[int] = None

        try:
            node_m = qty_to_mcpu_int(tr.node_cpu)
            if node_m < 1:
                errors.append("node_cpu must be >= 1m (e.g., '2400m' or '24')")
        except Exception:
            errors.append(f"node_cpu is not a valid quantity: {tr.node_cpu!r} (e.g., '2400m' or '24')")

        try:
            node_b = qty_to_bytes_int(tr.node_mem)
            if node_b < 1:
                errors.append("node_mem must be > 0 (e.g., '32Gi')")
        except Exception:
            errors.append(f"node_mem is not a valid quantity: {tr.node_mem!r} (e.g., '32Gi')")

        total_pods = max(0, tr.num_nodes * tr.pods_per_node)

        # ---------- util / tolerance ----------
        if not (0.0 < tr.util <= 1.0):
            errors.append(f"util must be in (0,1], got {tr.util}")

        if tr.util_tolerance < 0.0:
            errors.append(f"util_tolerance must be >= 0, got {tr.util_tolerance}")
        if tr.util_tolerance >= 1.0:
            errors.append(f"util_tolerance must be < 1, got {tr.util_tolerance}")
        if (tr.util - tr.util_tolerance) < 0.0 or (tr.util + tr.util_tolerance) > 1.0:
            errors.append(f"util ± util_tolerance must stay within [0,1] (got {tr.util} ± {tr.util_tolerance})")

        # ---------- priorities ----------
        prio_ok = False
        lo_prio = hi_prio = None
        if not isinstance(tr.num_priorities, tuple) or len(tr.num_priorities) != 2:
            errors.append("num_priorities must be provided (fixed number or [lo,hi])")
        else:
            try:
                lo_prio = int(tr.num_priorities[0])
                hi_prio = int(tr.num_priorities[1])
                if lo_prio < 1:
                    errors.append("num_priorities_lo must be >= 1")
                if hi_prio < lo_prio:
                    errors.append("num_priorities interval must satisfy lo <= hi")
                else:
                    prio_ok = (lo_prio >= 1)
            except Exception:
                errors.append("num_priorities interval must contain integers")

        # ---------- replicaset count ----------
        rs_present = tr.num_replicaset is not None
        lo_rs = hi_rs = None
        if rs_present:
            try:
                lo_rs = int(tr.num_replicaset[0])
                hi_rs = int(tr.num_replicaset[1])
                if lo_rs < 0 or hi_rs < lo_rs:
                    errors.append("num_replicaset must satisfy 0 <= lo <= hi")
                if hi_rs is not None and total_pods and hi_rs > total_pods:
                    errors.append(f"num_replicaset_hi ({hi_rs}) cannot exceed total_pods ({total_pods})")
                # If RS mode is used (hi_rs > 0), enforce lower bound >= min priorities
                if hi_rs and hi_rs > 0 and prio_ok and lo_prio is not None and lo_rs is not None:
                    if lo_rs < lo_prio:
                        errors.append(
                            f"num_replicaset_lo ({lo_rs}) < num_priorities_lo ({lo_prio}); "
                            "increase num_replicaset_lo or lower num_priorities_lo"
                        )
            except Exception:
                errors.append("num_replicaset interval must contain integers")

        # If RS mode is used, replicas/RS must be provided
        if rs_present and tr.num_replicas_per_rs_set is None:
            errors.append("num_replicas_per_rs_set is required when num_replicaset is provided")

        # ---------- replicas per RS ----------
        if tr.num_replicas_per_rs_set is not None:
            if not rs_present:
                errors.append("num_replicas_per_rs_set is set but num_replicaset is not set")
            else:
                try:
                    lo_rps = int(tr.num_replicas_per_rs_set[0])
                    hi_rps = int(tr.num_replicas_per_rs_set[1])
                    if lo_rps < 1 or hi_rps < lo_rps:
                        errors.append("num_replicas_per_rs_set must satisfy 1 <= lo <= hi")
                    if (lo_rs is not None and hi_rs is not None and total_pods > 0):
                        min_need = lo_rs * lo_rps
                        max_cap  = hi_rs * hi_rps
                        if total_pods < min_need or total_pods > max_cap:
                            errors.append(
                                "infeasible replicas-per-RS vs replicasets vs total_pods: "
                                f"with num_replicaset in [{lo_rs},{hi_rs}] and replicas/RS in [{lo_rps},{hi_rps}], "
                                f"total_pods={total_pods} must lie in [{min_need},{max_cap}]"
                            )
                except Exception:
                    errors.append("num_replicas_per_rs_set interval must contain integers")

        # ---------- per-pod CPU/MEM intervals (optional) ----------
        cpu_iv_ok = False
        mem_iv_ok = False
        c_lo = c_hi = None
        m_lo = m_hi = None

        if tr.cpu_per_pod is not None:
            try:
                c_lo = qty_to_mcpu_int(tr.cpu_per_pod[0])
                c_hi = qty_to_mcpu_int(tr.cpu_per_pod[1])
                if c_lo < 1 or c_hi < c_lo:
                    errors.append("cpu_per_pod must satisfy 1m <= lo <= hi")
                else:
                    cpu_iv_ok = True
                if node_m is not None and c_hi is not None and c_hi > node_m:
                    errors.append(f"cpu_per_pod_hi ({c_hi}m) cannot exceed node_cpu ({node_m}m)")
            except Exception:
                errors.append(f"cpu_per_pod has invalid quantities: {tr.cpu_per_pod!r}")

        if tr.mem_per_pod is not None:
            try:
                m_lo = qty_to_bytes_int(tr.mem_per_pod[0])
                m_hi = qty_to_bytes_int(tr.mem_per_pod[1])
                if m_lo < 1 or m_hi < m_lo:
                    errors.append("mem_per_pod must satisfy 1B <= lo <= hi")
                else:
                    mem_iv_ok = True
                if node_b is not None and m_hi is not None and m_hi > node_b:
                    errors.append(f"mem_per_pod_hi ({m_hi}B) cannot exceed node_mem ({node_b}B)")
            except Exception:
                errors.append(f"mem_per_pod has invalid quantities: {tr.mem_per_pod!r}")

        # ---------- feasibility of per-pod intervals vs util ----------
        # Only check if intervals are provided (otherwise they will be auto-generated later).
        if total_pods > 0 and tr.util > 0 and node_m is not None and cpu_iv_ok:
            target_cpu = int(node_m * tr.num_nodes * tr.util)
            if not KwokTestGenerator._is_interval_feasible((c_lo, c_hi), total_pods, target_cpu):
                avg_cpu = target_cpu / total_pods
                errors.append(
                    "infeasible cpu_per_pod for util target: need n*lo <= target <= n*hi "
                    f"(n={total_pods}, target={target_cpu}); consider ~{int(round(avg_cpu))}m per pod"
                )

        if total_pods > 0 and tr.util > 0 and node_b is not None and mem_iv_ok:
            target_mem = int(node_b * tr.num_nodes * tr.util)
            if not KwokTestGenerator._is_interval_feasible((m_lo, m_hi), total_pods, target_mem):
                avg_mem = int(target_mem / total_pods)
                errors.append(
                    "infeasible mem_per_pod for util target: need n*lo <= target <= n*hi "
                    f"(n={total_pods}, target={target_mem}); consider ~{avg_mem} bytes per pod"
                )

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
                    print(f"[kwok-test-gen] config-failed {cfg}: {msg}")
                    any_fail = True
            except Exception as e:
                print(f"[kwok-test-gen] config-failed {cfg}: {e}")
                any_fail = True
                continue
        if any_fail:
            raise SystemExit("One or more configs failed validation; see messages above and fix them.")
        print(f"[kwok-test-gen] all {len(cfgs)} configs validated successfully.")

    @staticmethod
    def _pick_runner_doc(docs: List[Any]) -> Dict[str, Any]:
        """
        Pick the KwokRunConfiguration document from the list of YAML documents.
        """
        for d in docs:
            if isinstance(d, dict) and (str(d.get("kind","")) == "KwokRunConfiguration"):
                return d
        raise KeyError("No Kwok runner settings found in YAML (expected 'kind: KwokRunConfiguration').")

    def _run_single_seed(
        self,
        cfg: Path,
        seen: set[int],
        seed: int,
        ta: TestConfigApplied,
        seed_file: Optional[str] = None,
    ) -> None:
        """
        Run a single seed with the given applied config.
        """
        exists = seed in seen
        save_allowed = (not self.args.test) and (not exists)
        phase = "start"
        try:
            start_time = time.time()
            rng_layout = self._rng(seed, "layout", cfg.stem)

            phase = "ensure_namespace"
            print(f"[kwok-test-gen] cfg={cfg.stem}  seed={seed}  phase={phase}")
            self._ensure_namespace(self.ctx, ta.namespace, recreate=True)

            phase = "ensure_priority_classes"
            print(f"[kwok-test-gen] cfg={cfg.stem}  seed={seed}  phase={phase}  num_p={ta.num_priorities}")
            self._ensure_priority_classes(self.ctx, ta.num_priorities, prefix="p", start=1, delete_extras=True)

            alloc_cpu_m = ta.node_m * ta.num_nodes
            alloc_mem_b = ta.node_b * ta.num_nodes
            tgt_mc_cluster = int(alloc_cpu_m * ta.util)
            tgt_b_cluster  = int(alloc_mem_b * ta.util)

            phase = "apply_workload"
            print(f"[kwok-test-gen] cfg={cfg.stem}  seed={seed}  phase={phase}  mode={'RS' if ta.num_replicaset>0 else 'standalone'}")
            pod_specs: list[dict] = []
            rs_specs:  list[dict] = []
            if ta.num_replicaset > 0:
                rs_specs = self._apply_replicasets(
                    ta.namespace, rng_layout,
                    ta.total_pods,
                    tgt_mc_cluster, tgt_b_cluster,
                    ta.num_replicaset, ta.num_priorities,
                    ta.cpu_per_pod_m, ta.mem_per_pod_b,
                    ta.wait_mode, ta.wait_timeout_s, ta.util, ta.util_tolerance,
                    num_replicas_per_rs_set=ta.num_replicas_per_rs_set,
                )
            else:
                pod_specs = self._apply_standalone_pods(cfg, seed, ta, rng_layout)

            phase = "events_snapshot"
            print(f"[kwok-test-gen] cfg={cfg.stem}  seed={seed}  phase={phase}")
            _, scheduled_pairs, unschedulable_names = get_scheduled_and_unscheduled(
                self.ctx, ta.namespace, expected=ta.total_pods, settle_timeout=ta.settle_timeout_s
            )

            phase = "stats_snapshot"
            print(f"[kwok-test-gen] cfg={cfg.stem}  seed={seed}  phase={phase}")
            placed_by_priority = self._count_running_by_priority(self.ctx, ta.namespace)
            snap = stat_snapshot(self.ctx, ta.namespace, expected=ta.total_pods, settle_timeout=ta.settle_timeout_s)

            result_row = {
                "timestamp": self._get_timestamp(),
                "kwok_config": str(cfg),
                "seed_file": seed_file,
                "seed": str(seed),
                "num_nodes": ta.num_nodes,
                "pods_per_node": ta.pods_per_node,
                "num_priorities": ta.num_priorities,
                "num_replicaset": ta.num_replicaset,
                "num_replicas_per_rs_set": self._format_interval(ta.num_replicas_per_rs_set),
                "node_cpu_m": ta.node_m,
                "node_mem_b": ta.node_b,
                "cpu_per_pod_m": self._format_interval(ta.cpu_per_pod_m),
                "mem_per_pod_b": self._format_interval(ta.mem_per_pod_b),
                "util": ta.util,
                "util_tolerance": ta.util_tolerance,
                "util_run_cpu": f"{snap.cpu_run_util:.3f}",
                "util_run_mem": f"{snap.mem_run_util:.3f}",
                "cpu_m_run": int(sum(snap.cpu_req_by_node.values())),
                "mem_b_run": int(sum(snap.mem_req_by_node.values())),
                "wait_mode": (ta.wait_mode or ""),
                "wait_timeout_s": ta.wait_timeout_s,
                "settle_timeout_s": ta.settle_timeout_s,
                "scheduled_count": int(len(scheduled_pairs)),
                "unscheduled_count": int(len(unschedulable_names)),
                "pods_run_by_node": json.dumps(snap.pods_run_by_node, separators=(",", ":")),
                "placed_by_priority": json.dumps(placed_by_priority, separators=(",", ":")),
                "unscheduled": "{" + ",".join(sorted(unschedulable_names)) + "}",
                "scheduled": "{" + ",".join(sorted([name for (name, _) in scheduled_pairs])) + "}",
                "pod_node": json.dumps(self._build_pod_node_list(
                    {name: node for (name, node) in scheduled_pairs},
                    unschedulable_names, pod_specs, rs_specs
                ), separators=(",", ":")),
            }
            phase = "write_results"
            print(f"[kwok-test-gen] cfg={cfg.stem}  seed={seed}  phase={phase}  rows=1")
            if save_allowed:
                dest_csv = self._pick_results_csv_to_write(cfg.stem, rows_to_add=1)
                self._ensure_csv_with_header(dest_csv, RESULTS_HEADER)
                csv_append_row(dest_csv, RESULTS_HEADER, result_row)
                print(f"[kwok-test-gen] cfg={cfg.stem}  seed={seed}  appended to {dest_csv.name}")
            else:
                print(f"[kwok-test-gen] cfg={cfg.stem}  seed={seed}  not saved")
            print(f"[kwok-test-gen] cfg={cfg.stem}  seed={seed}  done; took {time.time()-start_time:.1f}s")
            self._print_seed_summary(cfg, seed, len(scheduled_pairs), len(unschedulable_names))

        except RuntimeError as e:
            # previously returned silently for some RuntimeErrors
            tb = traceback.format_exc()
            self._write_fail("seed", cfg, seed, phase, str(e), tb)
            print(f"[kwok-test-gen] seed-runtime  cfg={cfg.stem} seed={seed}  phase={phase}: {e}")
            return
        except Exception as e:
            tb = traceback.format_exc()
            self._write_fail("seed", cfg, seed, phase, str(e), tb)
            print(f"[kwok-test-gen] seed-error  cfg={cfg.stem}  seed={seed}  phase={phase}: {e}")
            self._print_seed_summary(cfg, seed, None, None, f"error: {e}")

    def _run_gen_seeds(self):
        """
        Generate random seeds and write them to a file.
        """
        if self.args.count is None or self.args.count < 1:
            raise SystemExit("--random-seeds-to-file requires --count >= 1")
        if self.args.seed is not None or self.args.seed_file:
            raise SystemExit("--random-seeds-to-file cannot be combined with --seed or --seed-file")

        base = int(time.time_ns())
        rng = KwokTestGenerator._rng(base, "seed-file")
        seeds = [(rng.getrandbits(63) or 1) for _ in range(int(self.args.count))]

        outp = Path(self.args.generate_seeds_to_file)
        outp.parent.mkdir(parents=True, exist_ok=True)
        with open(outp, "w", encoding="utf-8", newline="") as f:
            for s in seeds:
                f.write(f"{s}\n")
        print(f"[kwok-test-gen] wrote {len(seeds)} seeds to {outp}")

    def _run_count_path(self, cfg: Path, cfg_idx: int, cfgs_total: int, raw: TestConfigRaw, seen: Dict[int, List[Path]]) -> None:
        """
        Run the generator for configuration(s) files and a seed count. If count=-1, run indefinitely.
        """
        to_make = int(self.args.count)
        base = int(time.time_ns())
        rng = KwokTestGenerator._rng(base, "seed-stream", self.args.cluster_name)
        made = 0
        while to_make == -1 or made < to_make:
            s = rng.getrandbits(63) or 1
            self._print_run_header(s, cfg.name, made + 1, to_make, cfg_idx, cfgs_total)
            rc = self._resolve_config_for_seed(raw, s)
            self._run_single_seed(cfg, seen, s, rc)
            if to_make != -1:
                made += 1
        return

    def _run_seed_single_path(self, cfg: Path, cfg_idx: int, cfgs_total: int, raw: TestConfigRaw, seen: Dict[int, List[Path]]) -> None:
        """
        Run the generator for configuration(s) files and a single seed.
        """
        s = int(self.args.seed)
        self._print_run_header(s, cfg.name, 1, 1, cfg_idx, cfgs_total)
        rc = self._resolve_config_for_seed(raw, s)
        self._run_single_seed(cfg, seen, s, rc)
        return

    def _run_seed_file_path(self, cfg: Path, cfg_idx: int, cfgs_total: int, raw: TestConfigRaw, seen: Dict[int, List[Path]]) -> None:
        """
        Run the generator for a specific configuration and seeds from a file.
        """
        seeds_list = self._read_seeds_file(Path(self.args.seed_file))
        seeds_total = len(seeds_list)
        for seed_idx, s in enumerate(seeds_list, start=1):
            self._print_run_header(s, cfg.name, seed_idx, seeds_total, cfg_idx, cfgs_total)
            s = int(s)
            rc = self._resolve_config_for_seed(raw, s)
            self._run_single_seed(cfg, seen, s, rc, self.args.seed_file)
        return

    def _run_for_config(self, cfg: Path, cfg_idx: int, cfgs_total: int) -> None:
        """
        Run the generator for a specific configuration file.
        """
        try:
            raw = self._load_run_config(cfg)
        except Exception as e:
            print(f"[kwok-test-gen] config-failed {cfg}: {e}")
            with open(self.failed_f, "a", encoding="utf-8") as f:
                f.write(f"config\t{cfg}\t-\t{e}\n")
            self._print_seed_summary(cfg, self.args.seed, None, None, "config load failed")
            return

        ok, msg = KwokTestGenerator._validate_config(raw)
        if not ok:
            print(f"[kwok-test-gen] config-failed {cfg}: {msg}")
            with open(self.failed_f, "a", encoding="utf-8") as f:
                f.write(f"config\t{cfg}\t-\t{msg}\n")
            self._print_seed_summary(cfg, self.args.seed, None, None, "num_replicaset_lo < num_priorities")
            return

        self.ctx = f"kwok-{self.args.cluster_name}"

        try:
            KwokTestGenerator._ensure_kwok_cluster(self.ctx, cfg, recreate=True)
        except Exception as e:
            print(f"[kwok-test-gen] config-failed; ensure cluster {cfg}: {e}")
            with open(self.failed_f, "a", encoding="utf-8") as f:
                f.write(f"config\t{cfg}\t-\t{e}\n")
            self._print_seed_summary(cfg, self.args.seed, None, None, "ensure cluster failed")
            return

        DEFAULT_POD_CAP = max(30, raw.pods_per_node * 3)
        try:
            print("[kwok-test-gen] phase=nodes: deleting old KWOK nodes …")
            KwokTestGenerator._delete_kwok_nodes(self.ctx)
            print("[kwok-test-gen] phase=nodes: creating KWOK nodes …")
            KwokTestGenerator._create_kwok_nodes(self.ctx, raw.num_nodes, raw.node_cpu, raw.node_mem, pods_cap=DEFAULT_POD_CAP)
            print("[kwok-test-gen] phase=nodes: done")
        except Exception as e:
            tb = traceback.format_exc()
            self._write_fail("config", cfg, None, "nodes", str(e), tb)
            print(f"[kwok-test-gen] config-failed; nodes setup: {e}")
            self._print_seed_summary(cfg, self.args.seed, None, None, "nodes setup failed")
            return

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

        print(f"[kwok-test-gen] no seeds provided for cfg={cfg.name} (use --seed / --seed-file / --count).")

    ##############################################
    # ------------ Main runner -------------------
    ##############################################
    def run(self) -> None:
        """
        Main runner function.
        """

        # --- if generate seeds to file, do it, then exit
        if self.args.generate_seeds_to_file is not None:
            self._run_gen_seeds()
            return

        # --- argument sanity (mutual exclusions / ranges) ---
        if self.args.seed is not None and self.args.seed < 1:
            raise SystemExit("--seed must be a positive integer")
        if self.args.seed_file and self.args.count is not None:
            raise SystemExit("--seed-file cannot be used with --count")
        if self.args.seed is not None and self.args.count is not None:
            raise SystemExit("--seed cannot be used with --count")
        if self.args.count is not None and self.args.count < -1:
            raise SystemExit("--count must be -1 (infinite) or a positive integer")

        # --- check provided paths: seed file and kwok-config dir ---
        if self.args.seed_file:
            file_exists(self.args.seed_file)
        dir_exists(self.args.config_dir)

        # --- test-mode constraints ---
        if self.args.test:
            if self.args.seed is None:
                raise SystemExit("--test requires exactly one --seed")
            if self.args.count is not None or self.args.seed_file:
                raise SystemExit("--test cannot be combined with --count or --seed-file")

        # proceed
        cfgs = self._get_kwok_configs(self.args.config_dir)
        cfgs_total = len(cfgs)
        self._validate_all_configs(cfgs)
        print(f"[kwok-test-gen] configs={cfgs_total}")
        for cfg_idx, cfg in enumerate(cfgs, start=1):
            print("\n===================================== CONFIG RUN =====================================")
            print(f"[kwok-test-gen] config={cfg}  (configs: {cfg_idx}/{cfgs_total})")
            print("======================================================================================")
            self._run_for_config(cfg, cfg_idx, cfgs_total)

##############################################
# ------------ CLI ---------------------------
##############################################
def build_argparser() -> argparse.ArgumentParser:
    """
    Build the argument parser for the KWOK test generator.
    """
    
    ap = argparse.ArgumentParser(description="Unified KWOK generator+applier runner.")
    
    # cluster
    ap.add_argument("--cluster-name", dest="cluster_name", default="kwok1",
                    help="KWOK cluster name")
    
    # configs
    ap.add_argument("--config-dir", dest="config_dir",
                    default="./scripts/kwok/kwok_configs",
                    help="Directory containing one or more KWOK config YAMLs")
    ap.add_argument("--results-dir", dest="results_dir", default="./scripts/kwok/results",
                    help="Directory to store results CSV files (one per KWOK config)")

    # rotation
    ap.add_argument("--max-rows-per-file", dest="max_rows_per_file", type=int, default=500000,
                    help="Maximum number of data rows per results CSV before rotating to <name>_N.csv")

    # failure logs
    ap.add_argument("--failed-file", dest="failed_file",
                    default="./scripts/kwok/failed/failed.csv",
                    help="File path to record all kinds of failures (configs and seeds)")

    # seeds
    ap.add_argument("--seed", type=int, default=None,
                    help="Run exactly this seed (per kwok-config)")
    ap.add_argument("--seed-file", dest="seed_file", default=None,
                    help="Path to seeds file (CSV with 'seed' col or newline list).")
    ap.add_argument("--generate-seeds-to-file", dest="generate_seeds_to_file", default=None,
                    help="Write --count random seeds (one per line, no header) to this file, then exit. Cannot be combined with --seed/--seed-file.")
    ap.add_argument("--count", type=int, default=None,
                    help="Generate random seeds; -1=infinite.")

    # Test summary mode — and note: test mode disables all saving
    ap.add_argument("--test", action="store_true",
                    help="Test mode: requires --seed; runs each kwok-config and prints a per-config summary "
                         "of scheduled vs unscheduled at the end. No results are saved in test mode.")

    # Note: other args are provided in YAML per-config
    return ap

def main():
    """
    Main entry point for the KWOK test generator.
    """
    args = build_argparser().parse_args()
    runner = KwokTestGenerator(args)
    runner.run()

if __name__ == "__main__":
    main()
