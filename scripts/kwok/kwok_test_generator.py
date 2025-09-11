#!/usr/bin/env python3

# kwok_test_generator.py

import csv, json, time, random, argparse, re, subprocess, tempfile, os, textwrap, hashlib, math, yaml
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional, Tuple, List, Dict, Any

from kwok_shared import (
    get_scheduled_and_unscheduled, get_json_ctx, stat_snapshot, dir_exists, file_exists,
    csv_append_row, cpu_m_str_to_int, mem_str_to_mib_int, cpu_m_int_to_str, mem_mi_int_to_str,
)

# ===============================================================
# Constants
# ===============================================================
RESULTS_HEADER = [
    "timestamp","kwok_config","seed",
    "num_nodes","pods_per_node","num_replicaset","num_priorities",
    "node_cpu","node_mem",
    "cpu_interval_per_pod","mem_interval_per_pod",
    "rs_replicas_per_set_interval",
    "util","util_tolerance",
    "util_run_cpu","util_run_mem","cpu_m_run","mem_b_run",
    "wait_mode","wait_timeout","settle_timeout",
    "scheduled_count","unscheduled_count",
    "pods_run_by_node","placed_by_priority",
    "unscheduled","scheduled",
    "pod_node",
]

# ====================================================================
# YAML builders. Due to identation problems we keep them outside class
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

def yaml_kwok_rs(ns: str, rs_name: str, replicas: int, qcpu: str, qmem: str, pc: str) -> str:
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
              requests: {{cpu: {qcpu}, memory: {qmem}}}
              limits:   {{cpu: {qcpu}, memory: {qmem}}}
    ---
    """)

def yaml_kwok_pod(ns: str, name: str, qcpu: str, qmem: str, pc: str) -> str:
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
          requests: {{cpu: {qcpu}, memory: {qmem}}}
          limits:   {{cpu: {qcpu}, memory: {qmem}}}
    ---
    """)

# ===============================================================
# Data classes
# ===============================================================
@dataclass
class TestConfig:
    # namespace
    namespace: str = "test"

    # topology
    num_nodes: int = 4
    pods_per_node: int = 4
    num_replicaset: Optional[Tuple[int, int]] = None  # (lo, hi) count of RS
    rs_replicas_per_set_interval: Optional[Tuple[int, int]] = None  # (lo, hi) replicas per RS
    num_priorities: int = 4

    # node capacity
    node_cpu: str = "24"      # e.g. "24" or "2500m"
    node_mem: str = "32Gi"    # e.g. "32Gi"

    # per-pod intervals (normalized to "lo,hi" strings)
    cpu_interval_per_pod: Optional[Tuple[int, int]] = (100, 1000)   # mCPU
    mem_interval_per_pod: Optional[Tuple[int, int]] = (128, 2048)    # MiB

    # utilization target
    util: float = 0.9
    util_tolerance: float = 0.01

    # waits
    wait_mode: Optional[str] = "ready"  # None/"none","exist","ready","running"
    wait_timeout: str = "5s"
    settle_timeout: str = "5s"

    # internal: remember where this came from
    source_file: Optional[Path] = field(default=None, repr=False)

# ===============================================================
# KwokTestGenerator
# ===============================================================
class KwokTestGenerator:
    def __init__(self, args: argparse.Namespace) -> None:
        self.args = args
        self.ctx: Optional[str] = None
        self.results_dir = Path(args.results_dir)
        self.failed_cfg_f = Path(args.failed_kwok_configs_file)
        self.failed_seeds_f = Path(args.failed_seeds_file)
        self.max_rows_per_file = int(args.max_rows_per_file)

        # Collect rows for summary if --test OR a single --seed is provided
        self._collect_test: bool = bool(args.test or (args.seed is not None))
        self._test_rows: List[Dict[str, Any]] = []

        # filesystem prep
        self.failed_cfg_f.parent.mkdir(parents=True, exist_ok=True)
        self.failed_seeds_f.parent.mkdir(parents=True, exist_ok=True)
        self.results_dir.mkdir(parents=True, exist_ok=True)
        self.failed_cfg_f.touch(exist_ok=True)
        self.failed_seeds_f.touch(exist_ok=True)

    ######################################################
    # ---------- Basic parsing/coercion helpers ----------
    ######################################################
    @staticmethod
    def _qty_to_mi_str(token: str) -> str:
        """
        Accepts things like: '5.2Gi', '2Gi', '3072Mi', '4GB', '512MB', '1024Ki'
        Returns a canonical string in whole Mi, e.g. '5325Mi'.
        """
        t = token.strip()
        m = re.fullmatch(r'\s*([0-9]+(?:\.[0-9]+)?)\s*([KMGTP]i?|[kmg]i?b?)?\s*', t)
        if not m:
            return t  # fall back; let mem_str_to_mib_int handle/raise

        val = float(m.group(1))
        unit = (m.group(2) or "Mi").upper()  # default to Mi if no unit

        # Map to Mi (IEC if ends with 'I', otherwise SI bytes)
        if unit in ("MI", "MIB"):
            mi = val
        elif unit in ("GI", "GIB"):
            mi = val * 1024
        elif unit in ("TI", "TIB"):
            mi = val * 1024 * 1024
        elif unit in ("KI", "KIB"):
            mi = val / 1024
        elif unit in ("KB", "K"):  # 10^3 bytes
            mi = (val * 1_000) / 1024
        elif unit in ("MB", "M"):  # 10^6 bytes
            mi = (val * 1_000_000) / 1_048_576
        elif unit in ("GB", "G"):  # 10^9 bytes
            mi = (val * 1_000_000_000) / 1_048_576
        elif unit in ("TB", "T"):  # 10^12 bytes
            mi = (val * 1_000_000_000_000) / 1_048_576
        else:
            # Unknown unit: let downstream deal with it
            return t

        return f"{int(round(mi))}Mi"

    @staticmethod
    def _qty_to_mcpu_str(token: str) -> str:
        """
        Accepts: '250m', '0.25', '0.25cpu', '1.5', '1 core', '2cores', '750 MCPU'
        Returns canonical 'Xm' (millicores), e.g. '250m', '1500m'.
        """
        t = token.strip().lower()
        # number + optional unit
        m = re.fullmatch(r'\s*([0-9]+(?:\.[0-9]+)?)\s*([a-z]*)\s*', t)
        if not m:
            return t  # let downstream raise if truly bad

        val = float(m.group(1))
        unit = m.group(2)

        # treat these as millicpu directly
        if unit in ("m", "mcpu", "millicpu"):
            milli = val
        # treat these (or empty) as whole cores
        elif unit in ("", "c", "cpu", "core", "cores"):
            milli = val * 1000.0
        else:
            # unknown unit; pass through
            return t

        return f"{int(round(milli))}m"

    @staticmethod
    def _parse_int_interval(s: Optional[str]) -> Optional[Tuple[int, int]]:
        """
        Parse a string into an integer interval (lo, hi).
        """
        if not s:
            return None
        lo_s, hi_s = [x.strip() for x in s.split(",", 1)]
        lo = max(1, int(lo_s))
        hi = max(lo, int(hi_s))
        return lo, hi

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
    # ---------- Hash-based seed derivation --------------
    ######################################################
    @staticmethod
    def _derive_seed(base_seed: int, *labels: object, nbytes: int = 16) -> int:
        """
        Deterministically derive a child seed from a base seed and a sequence of labels.
        Uses BLAKE2b for mixing + domain separation.
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
        """Convenience: Random() seeded from _derive_seed(base_seed, *labels)."""
        return random.Random(KwokTestGenerator._derive_seed(base_seed, *labels))
    
    ######################################################
    # ---------- Interval & distribution helpers ----------
    ######################################################
    @staticmethod
    def _format_interval_int(t: Optional[Tuple[int, int]]) -> str:
        """
        Format an integer interval for YAML output.
        """
        return "" if not t else f"{int(t[0])},{int(t[1])}"

    @staticmethod
    def _normalize_interval(
        doc: Dict[str, Any],
        key_combo: Tuple[str, str, str],
        *,  # supports: single, lo/hi pair, list, or dict
        allow_none: bool = True,
    ) -> Optional[str]:
        """
        Normalize an interval to the canonical "lo,hi" string.
        Accepts:
            - "interval_name": ["100m","1000m"]
            - "interval_name": {"lo":"100m","hi":"1000m"}
            - "interval_name_lo": "100m" + "interval_name_hi": "1000m"
        """
        single, lo_key, hi_key = key_combo

        if single in doc and doc[single] is not None:
            v = doc[single]
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
    def _parse_cpu_interval(s: Optional[str]) -> Optional[Tuple[int,int]]:
        """
        Parse a CPU interval string into a tuple of (min, max) millicores.
        Accepts decimals (e.g., '0.25', '250m', '1.5').
        """
        if not s:
            return None
        lo_raw, hi_raw = [x.strip() for x in s.split(",", 1)]
        lo_norm = KwokTestGenerator._qty_to_mcpu_str(lo_raw)
        hi_norm = KwokTestGenerator._qty_to_mcpu_str(hi_raw)
        return cpu_m_str_to_int(lo_norm), cpu_m_str_to_int(hi_norm)

    @staticmethod
    def _parse_mem_interval(s: Optional[str]) -> Optional[Tuple[int,int]]:
        """
        Parse a memory interval string into a tuple of (min, max) MiB.
        Accepts decimals (e.g., '5.2Gi').
        """
        if not s:
            return None
        lo_raw, hi_raw = [x.strip() for x in s.split(",", 1)]
        lo_norm = KwokTestGenerator._qty_to_mi_str(lo_raw)
        hi_norm = KwokTestGenerator._qty_to_mi_str(hi_raw)
        return mem_str_to_mib_int(lo_norm), mem_str_to_mib_int(hi_norm)
    
    @staticmethod
    def _format_interval_cpu(tup: tuple[int, int]) -> str:
        """
        Format a CPU interval for YAML output.
        """
        return f"{cpu_m_int_to_str(tup[0])},{cpu_m_int_to_str(tup[1])}"

    @staticmethod
    def _format_interval_mem(tup: tuple[int, int]) -> str:
        """
        Format a memory interval for YAML output.
        """
        return f"{mem_mi_int_to_str(tup[0])},{mem_mi_int_to_str(tup[1])}"

    @staticmethod
    def _is_interval_feasible(interval: Optional[Tuple[int,int]], n: int, total: int) -> bool:
        """
        Check if the given interval is feasible for the specified n and total.
        """
        if not interval or n <= 0: 
            return False
        lo, hi = interval
        if lo < 1 or hi < lo: 
            return False
        return n*lo <= total <= n*hi
    
    @staticmethod
    def _resolve_interval(label, interval, n, total, *, cap_each, jitter=0.5):
        """
        Validate or auto-generate a per-pod interval for the given total and n.
        """
        if KwokTestGenerator._is_interval_feasible(interval, n, total):
            return interval, False
        if interval is not None:
            raise ValueError(
                f"Infeasible {label} per-pod interval {interval} for n={n}, total={total}. "
                "Note: Intervals are expected to be per-pod. "
                f"Remove them to allow auto-generation, otherwise, adjust them - must satisfy n*lo <= total_{label} <= n*hi."
                f"One range that will works is: total_{label}/total_pods={total/n} +/- some jitter"
            )
        auto = KwokTestGenerator._gen_interval(total, n, cap_each=cap_each, jitter=jitter)
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
        # Must be able to reach the target:
        hi = max(hi, int(math.ceil(total / n)))
        # Keep lo from forcing us under the target:
        lo = min(lo, int(math.floor(total / n)))
        # Respect per-pod cap if given
        if cap_each is not None:
            if cap_each < int(math.ceil(total / n)):
                raise ValueError("Target cannot be met under per-pod cap.")
            hi = min(hi, cap_each)
            hi = max(hi, int(math.ceil(total / n)))
        if lo > hi:
            lo = max(1, min(int(math.floor(total / n)), hi))
        # Final sanity: guaranteed feasible
        assert n*lo <= total <= n*hi
        return lo, hi

    @staticmethod
    def _gen_random_parts(total: int, n: int, lo: int, hi: int, rng: random.Random) -> List[int]:
        """
        Generate n parts in [lo, hi] that sum to total.
        """
        parts: List[int] = []
        rem = total
        for i in range(n, 0, -1):
            min_i = max(lo, rem - (i-1)*hi)
            max_i = min(hi, rem - (i-1)*lo)
            if min_i > max_i:
                raise ValueError("Interval became infeasible during generation")
            x = rng.randint(min_i, max_i)
            parts.append(x)
            rem -= x
        rng.shuffle(parts)
        return parts

    @staticmethod
    def _gen_rs_specs(
        rng: random.Random,
        counts: list[int],                    # replicas per RS (will be adjusted by moving replicas)
        c_lo: int, c_hi: int,                 # per-pod CPU bounds (millicores)
        m_lo: int, m_hi: int,                 # per-pod MEM bounds (Mi)
        target_cpu: int, target_mem: int,     # desired weighted sums: sum(counts[i]*x_i)
        util: float, util_tol: float,         # from config
        *,
        max_steps: int = 10000
    ) -> tuple[list[int], list[int], list[int], bool]:
        """
        Given a list of RS replica counts, generate per-RS (per-pod) CPU and MEM requests
        such that the weighted sums are close to target_cpu and target_mem within tolerances.
        """
        n = len(counts)
        if n == 0:
            return counts, [], [], True

        cpu_x = [rng.randint(c_lo, c_hi) for _ in range(n)]
        mem_x = [rng.randint(m_lo, m_hi) for _ in range(n)]

        def wsum(vals: list[int], cnts: list[int]) -> int:
            return sum(v * c for v, c in zip(vals, cnts))

        def abs_tol(target: int) -> int:
            # target ≈ alloc*util → abs tolerance ≈ target*(util_tol/util)
            if util <= 0:
                return max(1, int(0.01 * target))
            return max(1, int(round(target * (util_tol / util))))

        tol_cpu = abs_tol(target_cpu)
        tol_mem = abs_tol(target_mem)

        S_cpu = wsum(cpu_x, counts)
        S_mem = wsum(mem_x, counts)
        
        def in_tol() -> bool:
            return abs(S_cpu - target_cpu) <= tol_cpu and abs(S_mem - target_mem) <= tol_mem

        def try_nudge(which: str, direction: int) -> bool:
            nonlocal S_cpu, S_mem  # declare before assigning
            """
            which: 'cpu' or 'mem'; direction: +1 to increase sum, -1 to decrease.
            Picks a candidate RS (weighted by replicas) that can move 1 within bounds.
            """
            if which == "cpu":
                cand = [i for i in range(n) if (direction > 0 and cpu_x[i] < c_hi) or (direction < 0 and cpu_x[i] > c_lo)]
            else:
                cand = [i for i in range(n) if (direction > 0 and mem_x[i] < m_hi) or (direction < 0 and mem_x[i] > m_lo)]
            if not cand:
                print(f"nudge: no candidates to adjust {which} in direction {direction}")
                return False
            i = rng.choices(cand, weights=[max(1, counts[j]) for j in cand], k=1)[0]
            if which == "cpu":
                if direction > 0 and cpu_x[i] < c_hi:
                    cpu_x[i] += 1
                    S_cpu += counts[i]
                    return True
                if direction < 0 and cpu_x[i] > c_lo:
                    cpu_x[i] -= 1
                    S_cpu -= counts[i]
                    return True
            else:
                if direction > 0 and mem_x[i] < m_hi:
                    mem_x[i] += 1
                    S_mem += counts[i]
                    return True
                if direction < 0 and mem_x[i] > m_lo:
                    mem_x[i] -= 1
                    S_mem -= counts[i]
                    return True
            print("nudge failed unexpectedly")
            return False

        def move_one_replica() -> bool:
            nonlocal S_cpu, S_mem  # declare before assigning
            """
            Move one replica from a 'heavier' RS to a 'lighter' RS if it reduces total L1 error.
            """
            if sum(counts) <= 1:
                return False
            order = sorted(range(n), key=lambda i: (cpu_x[i] + mem_x[i]) * max(1, counts[i]), reverse=True)
            heavy = order
            light = list(reversed(order))
            cur_err = abs(S_cpu - target_cpu) + abs(S_mem - target_mem)
            for i in heavy:
                if counts[i] <= 1:
                    continue
                for j in light:
                    if i == j:
                        continue
                    new_S_cpu = S_cpu + (cpu_x[j] - cpu_x[i])
                    new_S_mem = S_mem + (mem_x[j] - mem_x[i])
                    new_err = abs(new_S_cpu - target_cpu) + abs(new_S_mem - target_mem)
                    if new_err < cur_err:
                        counts[i] -= 1
                        counts[j] += 1
                        S_cpu, S_mem = new_S_cpu, new_S_mem
                        return True
            print("move_one_replica: no beneficial move found")
            return False

        # Pre-loop early exit: sometimes the initial draw is already within tolerance.
        if in_tol():
            print(
                f"replicasets balancing ended early; ok=True "
                f"(cpu {S_cpu}/{target_cpu}±{tol_cpu}, mem {S_mem}/{target_mem}±{tol_mem})"
            )
            return counts, cpu_x, mem_x, True

        for step in range(1, max_steps + 1):
            if in_tol():
                print(
                    f"replicasets balancing ended after {step} steps; ok=True "
                    f"(cpu {S_cpu}/{target_cpu}±{tol_cpu}, mem {S_mem}/{target_mem}±{tol_mem})"
                )
                return counts, cpu_x, mem_x, True

            wants = []
            wants.append(("cpu", +1) if S_cpu < target_cpu else ("cpu", -1) if S_cpu > target_cpu else None)
            wants.append(("mem", +1) if S_mem < target_mem else ("mem", -1) if S_mem > target_mem else None)
            wants = [w for w in wants if w]

            # Try nudges; if any change gets us within tolerance, stop immediately.
            for which, direction in rng.sample(wants, len(wants)):
                if try_nudge(which, direction):
                    if in_tol():
                        print(
                            f"replicasets balancing ended after {step} steps (after nudge); ok=True "
                            f"(cpu {S_cpu}/{target_cpu}±{tol_cpu}, mem {S_mem}/{target_mem}±{tol_mem})"
                        )
                    # Either way, go to next loop iteration to keep improving if needed.
                    break
            else:
                # No nudge possible; try moving a replica.
                if move_one_replica():
                    if in_tol():
                        print(
                            f"replicasets balancing ended after {step} steps (after move); ok=True "
                            f"(cpu {S_cpu}/{target_cpu}±{tol_cpu}, mem {S_mem}/{target_mem}±{tol_mem})"
                        )
                        return counts, cpu_x, mem_x, True
                    continue
                # Stuck: neither nudge nor move helped.
                break

            if move_one_replica():
                continue
            
            break  # stuck

        ok = in_tol()
        print(
            f"replicasets balancing ended after {max_steps} steps; ok={ok} "
            f"(cpu {S_cpu}/{target_cpu}±{tol_cpu}, mem {S_mem}/{target_mem}±{tol_mem})"
        )
        return counts, cpu_x, mem_x, ok
    
    @staticmethod
    def _gen_replicaset_counts(
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
        else:
            # Narrow auto-interval around the mean; jitter chosen to keep counts reasonable
            lo, hi = KwokTestGenerator._gen_interval(
                total_pods, num_replicaset, cap_each=None, jitter=0.4
            )
            lo = max(1, lo)

        return KwokTestGenerator._gen_random_parts(total_pods, num_replicaset, lo, hi, rng)

    ######################################################
    # ---------- CSV helpers & results rotation ----------
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
    def _append_rows(path: Path, header: List[str], rows: List[Dict[str, Any]]) -> None:
        """
        Append rows to the CSV file, ensuring the header exists.
        """
        KwokTestGenerator._ensure_csv_with_header(path, header)
        if not rows:
            return
        with open(path, "a", encoding="utf-8", newline="") as f:
            wr = csv.DictWriter(f, fieldnames=header, extrasaction="ignore")
            wr.writerows(rows)

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
    
    def _segment_list(self, stem: str) -> List[tuple[int, Path]]:
        """
        List all segments for the given stem, returning (index, path) tuples sorted by index.
        """
        segs: List[tuple[int, Path]] = []
        base = self.results_dir / f"{stem}.csv"
        if base.exists():
            segs.append((1, base))

        pat = re.compile(rf"^{re.escape(stem)}_(\d+)\.csv$")
        for p in self.results_dir.glob(f"{stem}_*.csv"):
            m = pat.match(p.name)
            if m:
                segs.append((int(m.group(1)), p))

        # de-dup idx=1 if both base and _1 exist; prefer base as #1
        uniq: dict[int, Path] = {}
        for idx, p in sorted(segs, key=lambda t: t[0]):
            if idx not in uniq:
                uniq[idx] = p
        return sorted(uniq.items(), key=lambda t: t[0])

    def _pick_results_csv_to_write(self, stem: str, rows_to_add: int = 1) -> Path:
        """
        Choose a segment to write 'rows_to_add' rows to.
        - Creates '<stem>_1.csv' if no segments exist.
        - Rotates to '<stem>_<last+1>.csv' when current_rows + rows_to_add > limit.
        Always ensures the header on the chosen file.
        """
        segs = self._segment_list(stem)
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
            print(f"[kwok-test-gen][rotate] {last_path.name} is full ({rows}/{self.max_rows_per_file}); "
                f"switching to {next_path.name}")
            return next_path

        # Otherwise use the last segment
        self._ensure_csv_with_header(last_path, RESULTS_HEADER)
        return last_path

    def _load_seen_results_all(self, stem: str) -> set[int]:
        """
        Load all seen seeds from all segments for the given stem.
        """
        seen: set[int] = set()
        for _, p in self._segment_list(stem):
            try:
                with open(p, "r", encoding="utf-8", newline="") as fh:
                    for row in csv.DictReader(fh):
                        s = (row.get("seed") or "").strip()
                        if s:
                            seen.add(int(s))
            except Exception:
                pass
        return seen
    
    def _result_file_index(self, stem: str, file: Path) -> int:
        """
        Rotation index for files strictly matching: <stem>_<N>.csv  (N >= 1)
        Returns -1 if not a rotated file.
        """
        m = re.match(rf"^{re.escape(stem)}_(\d+)\.csv$", file.name)
        return int(m.group(1)) if m else -1

    def _result_files_for_cfg(self, stem: str) -> List[Path]:
        """
        Only consider rotated files (<stem>_<N>.csv) for writing/rotation.
        """
        files = [p for p in self.results_dir.glob(f"{stem}_*.csv")
                if self._result_file_index(stem, p) >= 1]
        files.sort(key=lambda p: self._result_file_index(stem, p))
        return files
    
    ##############################################
    # ------------ KWOK / kubectl helpers --------
    ##############################################
    @staticmethod
    def _prep_kwokctl_config_file(src: Path) -> tuple[Path, Path | None]:
        """
        If `src` is a multi-doc YAML, extract the document with
        kind: KwokctlConfiguration into a temp file and return it.
        Returns (path_to_pass_to_kwokctl, tmp_file_to_cleanup_or_None).
        Falls back to (src, None) if PyYAML not present or doc not found.
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
            print(f"Recreating kwok cluster '{cluster_name}'")
            subprocess.run(["kwokctl", "delete", "cluster", "--name", cluster_name], check=False)

        if cfg_path and cfg_path.exists():
            cfg_for_kwokctl, tmp_to_cleanup = KwokTestGenerator._prep_kwokctl_config_file(cfg_path)
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
                print(f"KWOK_CONFIG='{cfg_path}' not found; creating with defaults")
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
    def _apply_yaml(ctx:str, yaml_text:str) -> subprocess.CompletedProcess:
        """
        Apply a YAML configuration to the cluster.
        """
        return subprocess.run(["kubectl","--context",ctx,"apply","-f","-"], input=yaml_text.encode(), check=True)
    
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
                print(f"Found default serviceaccount in ns '{ns}'")
                break
            time.sleep(delay)

    @staticmethod
    def _delete_namespace(ctx: str, ns: str) -> None:
        """
        Delete the specified namespace in the given context.
        """
        print(f"Deleting namespace '{ns}'...")
        try:
            subprocess.run(
                ["kubectl", "--context", ctx, "delete", "namespace", ns, "--ignore-not-found"],
                check=True,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.STDOUT
            )
            print(f"Namespace '{ns}' deleted (ctx={ctx})")
        except:
            print(f"Error deleting namespace '{ns}' in ctx={ctx}")

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
    def _scale_replicaset(ctx: str, ns: str, rs_name: str, replicas: int) -> None:
        """
        Scale a ReplicaSet to a desired replica count.
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
            time.sleep(0.5)
        print(f"Timeout waiting for pod '{name}' in ns '{ns}' to be {mode}")
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

            time.sleep(0.5)

        print(f"Timeout waiting for RS '{rs_name}' in ns '{ns}' to have desired pods ready")
        return last_count
    
    def _apply_standalone_pods(
        self, ns: str, rng: random.Random, total_pods: int,
        cpu_parts: List[int], mem_parts: List[int],
        num_priorities: int, wait_mode: Optional[str], wait_timeout_s: int
    ) -> List[Dict[str, object]]:
        """
        Apply standalone pods with the given specifications.
        """
        specs: List[Dict[str, object]] = []
        names: List[str] = []
        for i in range(total_pods):
            prio = rng.randint(1, max(1, num_priorities))
            pc = f"p{prio}"
            name = f"pod-{i+1:03d}-{pc}"
            cpu_m = max(1, int(cpu_parts[i]))
            mem_mi = max(1, int(mem_parts[i]))
            qcpu = cpu_m_int_to_str(cpu_m)
            qmem = mem_mi_int_to_str(mem_mi)
            KwokTestGenerator._apply_yaml(self.ctx, yaml_kwok_pod(ns, name, qcpu, qmem, pc))
            names.append(name)
            specs.append({"name": name, "cpu_m": cpu_m, "mem_mi": mem_mi, "priority": pc})
            if wait_mode == "running":
                _ = KwokTestGenerator._wait_each(self.ctx, "pod", name, ns, wait_timeout_s, wait_mode)
        if wait_mode in ("exist", "ready"):
            for name in names:
                _ = KwokTestGenerator._wait_each(self.ctx, "pod", name, ns, wait_timeout_s, wait_mode)
        return specs

    def _apply_replicasets(
        self, ns: str, rng: random.Random, total_pods: int,
        total_cpu: int, total_mem: int,
        num_replicaset: int, num_priorities: int,
        cpu_interval_per_pod_used: tuple[int,int] | None,
        mem_interval_per_pod_used: tuple[int,int] | None,
        wait_mode: str | None, wait_timeout_s: int,
        util: float, util_tolerance: float,
        rs_replicas_per_set_interval: Optional[Tuple[int,int]] = None,   # <— NEW
    ) -> List[Dict[str, object]]:
        """
        Apply the ReplicaSets with the given specifications.
        """
        
        specs: List[Dict[str, object]] = []
        
        # 1) RS replica counts using the configured (or auto) interval
        replicas = KwokTestGenerator._gen_replicaset_counts(
            total_pods, num_replicaset, rng, replicas_cnt_interval=rs_replicas_per_set_interval
        )
        
        # 2) Per-RS template sizes drawn under the SAME per-pod intervals,
        #    chosen so sums match the exact cluster totals from cpu_parts/mem_parts.
        c_lo, c_hi = cpu_interval_per_pod_used if cpu_interval_per_pod_used else (1, max(1, total_cpu))
        m_lo, m_hi = mem_interval_per_pod_used if mem_interval_per_pod_used else (1, max(1, total_mem))
        replicas, cpu_x, mem_x, ok = KwokTestGenerator._gen_rs_specs(
            rng, replicas, c_lo, c_hi, m_lo, m_hi, total_cpu, total_mem, util, util_tolerance,
        )
        if not ok:
            raise RuntimeError("RS template totals outside util tolerance; widen per-pod intervals or relax util/util_tolerance")

        # 3) Create & scale RSs
        for i, count in enumerate(replicas, start=1):
            pc = f"p{self._prio_for_step_desc(i, num_priorities)}"
            rsname = f"rs-{i:02d}-{pc}"
            qcpu = cpu_m_int_to_str(cpu_x[i-1])
            qmem = mem_mi_int_to_str(mem_x[i-1])

            specs.append({"name": rsname, "replicas": int(count),
                        "cpu_m": int(cpu_x[i-1]), "mem_mi": int(mem_x[i-1]), "priority": pc})

            KwokTestGenerator._apply_yaml(self.ctx, yaml_kwok_rs(ns, rsname, 0, qcpu, qmem, pc))
            for r in range(1, int(count) + 1):
                KwokTestGenerator._scale_replicaset(self.ctx, ns, rsname, r)
                if wait_mode == "running":
                    _ = KwokTestGenerator._wait_rs_pods(self.ctx, rsname, ns, wait_timeout_s, wait_mode)

        if wait_mode in ("exist", "ready"):
            for spec in specs:
                _ = KwokTestGenerator._wait_each(self.ctx, "rs", spec["name"], ns, wait_timeout_s, wait_mode)

        return specs

    @staticmethod
    def _prio_for_step_desc(step: int, num_prios: int) -> int:
        """
        Get the priority for a given step in the sequence.
        """
        if num_prios <= 1:
            return 1
        return num_prios - ((step - 1) % num_prios)

    ##############################################
    # ------------ Results helpers ---------------
    ##############################################
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
    def _pick_num_replicaset_for_seed(seed_int: int, interval: Optional[Tuple[int, int]], total_pods: int) -> int:
        """
        Pick the number of ReplicaSets to create for a given seed.
        """
        if not interval:
            return 0
        lo, hi = interval
        lo = max(0, int(lo))
        hi = max(lo, int(hi))
        hi = min(hi, int(total_pods))
        rng = KwokTestGenerator._rng(seed_int, "num_replicaset", lo, hi, total_pods)
        return rng.randint(lo, hi)

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

    ##############################################
    # ------------ Runner helpers ----------------
    ##############################################
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
    def _get_kwok_configs(dir_path: str) -> List[Path]:
        """
        Get all KWOK configuration files from the specified directory.
        """
        cfg_dir = Path(dir_path)
        if not cfg_dir.exists():
            raise SystemExit(f"--kwok-config-dir not found: {cfg_dir}")
        cfgs = sorted([p for p in cfg_dir.glob("**/*") if p.is_file() and p.suffix.lower() in (".yaml", ".yml")])
        if not cfgs:
            raise SystemExit("No KWOK configs found (must be at least one).")
        return cfgs

    @staticmethod
    def _load_run_config(cfg_path: Path) -> TestConfig:
        """
        Load the run configuration from a YAML file.
        """
        with open(cfg_path, "r", encoding="utf-8") as f:
            docs = list(yaml.safe_load_all(f)) or []
        runner_doc = KwokTestGenerator._pick_runner_doc(docs)

        rc = TestConfig(source_file=cfg_path)
        rc.namespace         = KwokTestGenerator._get_str_from_dict(runner_doc, "namespace", rc.namespace)
        rc.num_nodes         = KwokTestGenerator._get_int_from_dict(runner_doc, "num_nodes", rc.num_nodes)
        rc.pods_per_node     = KwokTestGenerator._get_int_from_dict(runner_doc, "pods_per_node", rc.pods_per_node)
        rc.num_priorities    = KwokTestGenerator._get_int_from_dict(runner_doc, "num_priorities", rc.num_priorities)
        rc.node_cpu          = KwokTestGenerator._get_str_from_dict(runner_doc, "node_cpu", rc.node_cpu)
        rc.node_mem          = KwokTestGenerator._get_str_from_dict(runner_doc, "node_mem", rc.node_mem)
        rc.util              = KwokTestGenerator._get_float_from_dict(runner_doc, "util", rc.util)
        rc.util_tolerance    = KwokTestGenerator._get_float_from_dict(runner_doc, "util_tolerance", rc.util_tolerance)
        rc.wait_mode         = KwokTestGenerator._get_wait_mode_from_dict(runner_doc, "wait_mode", rc.wait_mode)
        rc.wait_timeout      = KwokTestGenerator._get_str_from_dict(runner_doc, "wait_timeout", rc.wait_timeout)
        rc.settle_timeout    = KwokTestGenerator._get_str_from_dict(runner_doc, "settle_timeout", rc.settle_timeout)
        
        raw_nr = KwokTestGenerator._normalize_interval(
            runner_doc, ("num_replicaset", "num_replicaset_lo", "num_replicaset_hi"), allow_none=True
        )
        if raw_nr:
            rc.num_replicaset = KwokTestGenerator._parse_int_interval(raw_nr)

        raw_cpu = KwokTestGenerator._normalize_interval(
            runner_doc, ("cpu_interval_per_pod", "cpu_interval_per_pod_lo", "cpu_interval_per_pod_hi"), allow_none=True
        )
        if raw_cpu:
            rc.cpu_interval_per_pod = KwokTestGenerator._parse_cpu_interval(raw_cpu)

        raw_mem = KwokTestGenerator._normalize_interval(
            runner_doc, ("mem_interval_per_pod", "mem_interval_per_pod_lo", "mem_interval_per_pod_hi"), allow_none=True
        )
        if raw_mem:
            rc.mem_interval_per_pod = KwokTestGenerator._parse_mem_interval(raw_mem)

        raw_rs = KwokTestGenerator._normalize_interval(
            runner_doc,
            ("num_replicas_per_set", "num_replicas_per_set_lo", "num_replicas_per_set_hi"),
            allow_none=True
        )
        if raw_rs:
            rc.rs_replicas_per_set_interval = KwokTestGenerator._parse_int_interval(raw_rs)

        return rc

    def _print_test_summary(self) -> None:
        """
        Print a summary of the test results.
        """
        if not self._test_rows:
            print("\n[kwok-test-gen][summary] No test results.")
            return
        rows = sorted(self._test_rows, key=lambda r: (str(r["kwok_config"]), int(r.get("seed") or 0)))
        cfg_w = max(len("kwok_config"), *(len(Path(r["kwok_config"]).name) for r in rows))
        seed_w = max(len("seed"), *(len(str(r["seed"])) for r in rows))
        sch_w = len("scheduled")
        uns_w = len("unscheduled")

        print("\n==================== TEST SUMMARY ====================")
        header = f"{'kwok_config'.ljust(cfg_w)}  {'seed'.rjust(seed_w)}  {'scheduled'.rjust(sch_w)}  {'unscheduled'.rjust(uns_w)}  note"
        print(header)
        print("-" * len(header))
        for r in rows:
            cfg_name = Path(r["kwok_config"]).name.ljust(cfg_w)
            seed = str(r["seed"]).rjust(seed_w)
            sch = ("" if r["scheduled"] is None else str(r["scheduled"])).rjust(sch_w)
            uns = ("" if r["unscheduled"] is None else str(r["unscheduled"])).rjust(uns_w)
            note = str(r.get("note",""))
            print(f"{cfg_name}  {seed}  {sch}  {uns}  {note}")
        print("======================================================")

    def _run_one_seed(
        self,
        cfg: Path,
        namespace: str,
        seen: set[int],
        seed_int: int,
        num_nodes: int, pods_per_node: int, num_replicaset: int, num_priorities: int,
        node_cpu: str, node_mem: str,
        util: float, util_tol: float,
        node_mc: int, node_mi: int, total_pods: int,
        cpu_interval_per_pod_used: Optional[Tuple[int,int]],
        mem_interval_per_pod_used: Optional[Tuple[int,int]],
        wait_mode: Optional[str], wait_timeout_raw: str, settle_timeout_raw: str,
        wait_timeout_s: int, settle_timeout_s: int,
        rs_interval_str: str = "",
        rs_replicas_per_set_interval: Optional[Tuple[int,int]] = None,  # <— NEW
    ) -> None:
        """
        Run a single seed test.
        """
        
        exists = seed_int in seen

        # Save policy:
        # - NEVER save in --test mode
        # - If seed already exists, ALWAYS skip saving and warn
        save_allowed = (not self.args.test) and (not exists)

        if exists:
            print(f"[kwok-test-gen][warn] cfg={cfg.stem} seed={seed_int} already present; will run but NOT save")

        try:
            start_time = time.time()
            
            tgt_mc_cluster = int(node_mc * num_nodes * util)
            tgt_mi_cluster = int(node_mi * num_nodes * util)

            rng_cpu     = KwokTestGenerator._rng(seed_int, "cpu_parts", cfg.stem)
            rng_mem     = KwokTestGenerator._rng(seed_int, "mem_parts", cfg.stem)
            rng_layout  = KwokTestGenerator._rng(seed_int, "layout", cfg.stem)

            # Generate per-pod parts using the *per-pod* fixed interval
            c_lo, c_hi = cpu_interval_per_pod_used
            m_lo, m_hi = mem_interval_per_pod_used

            cpu_parts = KwokTestGenerator._gen_random_parts(
                tgt_mc_cluster, total_pods, c_lo, c_hi, rng_cpu
            )
            mem_parts = KwokTestGenerator._gen_random_parts(
                tgt_mi_cluster, total_pods, m_lo, m_hi, rng_mem
            )

            # Isolate run
            KwokTestGenerator._ensure_namespace(self.ctx, namespace, recreate=True)

            pod_specs: List[Dict[str, object]] = []
            rs_specs: List[Dict[str, object]] = []

            if num_replicaset > 0:
                total_cpu = int(sum(cpu_parts))
                total_mem = int(sum(mem_parts))
                rs_specs = self._apply_replicasets(
                    namespace, rng_layout, total_pods, total_cpu, total_mem,
                    num_replicaset, num_priorities, cpu_interval_per_pod_used, mem_interval_per_pod_used,
                    wait_mode, wait_timeout_s, util, util_tol,
                    rs_replicas_per_set_interval=rs_replicas_per_set_interval,
                )
            else: # STANDALONE
                # Verify that the cluster totals hit the utilization window
                alloc_cpu = node_mc * num_nodes
                alloc_mem = node_mi * num_nodes
                sum_cpu = int(sum(cpu_parts))
                sum_mem = int(sum(mem_parts))
                lo_cpu = int(alloc_cpu * (util - util_tol))
                hi_cpu = int(alloc_cpu * (util + util_tol))
                lo_mem = int(alloc_mem * (util - util_tol))
                hi_mem = int(alloc_mem * (util + util_tol))

                if not (lo_cpu <= sum_cpu <= hi_cpu) or not (lo_mem <= sum_mem <= hi_mem):
                    if self._collect_test:
                        self._test_rows.append({
                            "kwok_config": str(cfg),
                            "seed": seed_int,
                            "scheduled": 0,
                            "unscheduled": 0,
                            "note": "totals outside util tolerance",
                        })
                    with open(self.failed_seeds_f, "a", encoding="utf-8") as f:
                        f.write(
                            f"{cfg}\t{seed_int}\tTotals outside util tolerance "
                            f"(cpu={sum_cpu}, range=[{lo_cpu},{hi_cpu}]; "
                            f"mem={sum_mem}, range=[{lo_mem},{hi_mem}])\n"
                        )
                    print(
                        f"[kwok-test-gen][seed-failed] cfg={cfg.stem} seed={seed_int} -> "
                        f"totals outside util tolerance "
                        f"(cpu={sum_cpu}, range=[{lo_cpu},{hi_cpu}]; "
                        f"mem={sum_mem}, range=[{lo_mem},{hi_mem}])"
                    )
                    return
                
                pod_specs = self._apply_standalone_pods(
                    namespace, rng_layout, total_pods, cpu_parts, mem_parts,
                    num_priorities, wait_mode, wait_timeout_s
                )

            # settle & collect
            _, scheduled_pairs, unschedulable_names = get_scheduled_and_unscheduled(
                self.ctx, namespace, expected=total_pods, settle_timeout=settle_timeout_s
            )
            scheduled_names = sorted([name for (name, _) in scheduled_pairs])
            scheduled_braced = "{" + ",".join(scheduled_names) + "}"
            unscheduled_braced = "{" + ",".join(sorted(unschedulable_names)) + "}"

            scheduled_by_name = {name: node for (name, node) in scheduled_pairs}
            placed_by_priority = KwokTestGenerator._count_running_by_priority(self.ctx, namespace)
            snap = stat_snapshot(self.ctx, namespace, expected=total_pods, settle_timeout=settle_timeout_s)

            # pod_node aggregation
            standalone_by_name = {p["name"]: p for p in (pod_specs or [])}
            rs_by_name = {r["name"]: r for r in (rs_specs or [])}
            all_event_pods = set(scheduled_by_name.keys()) | set(unschedulable_names)
            pod_node_list: List[Dict[str, object]] = []
            for pname in sorted(all_event_pods):
                node = scheduled_by_name.get(pname, "")
                if pname in standalone_by_name:
                    spec = standalone_by_name[pname]
                    pod_node_list.append({
                        "name": pname,
                        "cpu_m": int(spec["cpu_m"]),
                        "mem_mi": int(spec["mem_mi"]),
                        "priority": str(spec["priority"]),
                        "node": node,
                    })
                else:
                    rsname = pname.rsplit("-", 1)[0] if "-" in pname else ""
                    r = rs_by_name.get(rsname, {})
                    pod_node_list.append({
                        "name": pname,
                        "cpu_m": int(r.get("cpu_m", 0)),
                        "mem_mi": int(r.get("mem_mi", 0)),
                        "priority": str(r.get("priority", "")),
                        "node": node,
                    })

            # record test row (for --test or any single-seed run)
            if self._collect_test:
                self._test_rows.append({
                    "kwok_config": str(cfg),
                    "seed": seed_int,
                    "scheduled": int(len(scheduled_pairs)),
                    "unscheduled": int(len(unschedulable_names)),
                    "note": "",
                })

            result_row = {
                "timestamp": KwokTestGenerator._get_timestamp(),
                "kwok_config": str(cfg),
                "seed": str(seed_int),
                "num_nodes": num_nodes,
                "pods_per_node": pods_per_node,
                "num_replicaset": num_replicaset,
                "num_priorities": num_priorities,
                "node_cpu": node_cpu,
                "node_mem": node_mem,
                "cpu_interval_per_pod": KwokTestGenerator._format_interval_cpu(cpu_interval_per_pod_used) if cpu_interval_per_pod_used else "",
                "mem_interval_per_pod": KwokTestGenerator._format_interval_mem(mem_interval_per_pod_used) if mem_interval_per_pod_used else "",
                "rs_replicas_per_set_interval": rs_interval_str,
                "util": util,
                "util_tolerance": util_tol,
                "util_run_cpu": f"{snap.cpu_run_util:.3f}",
                "util_run_mem": f"{snap.mem_run_util:.3f}",
                "cpu_m_run": int(sum(snap.cpu_req_by_node.values())),
                "mem_b_run": int(sum(snap.mem_req_by_node.values())),
                "wait_mode": (wait_mode or ""),
                "wait_timeout": wait_timeout_raw,
                "settle_timeout": settle_timeout_raw,
                "scheduled_count": int(len(scheduled_pairs)),
                "unscheduled_count": int(len(unschedulable_names)),
                "pods_run_by_node": json.dumps(snap.pods_run_by_node, separators=(",", ":")),
                "placed_by_priority": json.dumps(placed_by_priority, separators=(",", ":")),
                "unscheduled": unscheduled_braced,
                "scheduled": scheduled_braced,
                "pod_node": json.dumps(pod_node_list, separators=(",", ":")),
            }
            
            print(f"[kwok-test-gen] cfg={cfg.stem} seed={seed_int} -> done; took {time.time() - start_time:.1f}s, "
                f"scheduled={len(scheduled_pairs)}, unschedulable={len(unschedulable_names)}")

            # Save if allowed (rotation-aware)
            if save_allowed:
                dest_csv = self._pick_results_csv_to_write(cfg.stem, rows_to_add=1)
                # ensure header in case kwok_shared.csv_append_row doesn't add it
                self._ensure_csv_with_header(dest_csv, RESULTS_HEADER)
                csv_append_row(dest_csv, RESULTS_HEADER, result_row)
                print(f"[kwok-test-gen] cfg={cfg.stem} seed={seed_int} -> appended to {dest_csv.name}")
            else:
                print(f"[kwok-test-gen] cfg={cfg.stem} seed={seed_int} -> NOT saved")

        except Exception as e:
            if self._collect_test:
                self._test_rows.append({
                    "kwok_config": str(cfg),
                    "seed": seed_int,
                    "scheduled": None,
                    "unscheduled": None,
                    "note": f"error: {e}",
                })
            with open(self.failed_seeds_f, "a", encoding="utf-8") as f:
                f.write(f"{cfg}\t{seed_int}\t{e}\n")

    def _run_for_config(self, cfg: Path) -> None:
        """
        Run the test for a specific configuration.
        """
        try:
            rc = self._load_run_config(cfg)
        except Exception as e:
            print(f"[kwok-test-gen][config-failed] {cfg}: {e}")
            with open(self.failed_cfg_f, "a", encoding="utf-8") as f:
                f.write(str(cfg) + "\n")
            if self._collect_test:
                self._test_rows.append({
                    "kwok_config": str(cfg),
                    "seed": self.args.seed,
                    "scheduled": None,
                    "unscheduled": None,
                    "note": "config load failed",
                })
            return
        
        # ---- fast-fail: require at least one RS per priority ----
        if rc.num_replicaset is not None:
            try:
                lo_rs, _ = rc.num_replicaset  # tuple from _parse_int_interval
            except Exception:
                lo_rs = None
            if lo_rs is not None and lo_rs < rc.num_priorities:
                msg = (f"invalid config: num_replicaset lower bound ({lo_rs}) "
                       f"< num_priorities ({rc.num_priorities}). "
                       "Increase num_replicaset_lo to be >= num_priorities.")
                print(f"[kwok-test-gen][config-failed] {cfg}: {msg}")
                with open(self.failed_cfg_f, "a", encoding="utf-8") as f:
                    f.write(f"{cfg}\t{msg}\n")
                if self._collect_test:
                    self._test_rows.append({
                        "kwok_config": str(cfg),
                        "seed": self.args.seed,
                        "scheduled": None,
                        "unscheduled": None,
                        "note": "num_replicaset_lo < num_priorities",
                    })
                return

        self.ctx = f"kwok-{self.args.cluster_name}"

        try:
            KwokTestGenerator._ensure_kwok_cluster(self.ctx, cfg, recreate=True)
        except Exception as e:
            print(f"[kwok-test-gen][config-failed] ensure cluster {cfg}: {e}")
            with open(self.failed_cfg_f, "a", encoding="utf-8") as f:
                f.write(str(cfg) + "\n")
            if self._collect_test:
                self._test_rows.append({
                    "kwok_config": str(cfg),
                    "seed": self.args.seed,
                    "scheduled": None,
                    "unscheduled": None,
                    "note": "ensure cluster failed",
                })
            return

        wait_mode = None if rc.wait_mode in (None, "none", "None", "") else str(rc.wait_mode)
        wait_timeout_s = KwokTestGenerator._parse_timeout_s(rc.wait_timeout)
        settle_timeout_s = KwokTestGenerator._parse_timeout_s(rc.settle_timeout)

        # nodes & priorities (once per config)
        DEFAULT_POD_CAP = max(30, rc.pods_per_node * 3)
        KwokTestGenerator._delete_kwok_nodes(self.ctx)
        KwokTestGenerator._create_kwok_nodes(self.ctx, rc.num_nodes, rc.node_cpu, rc.node_mem, pods_cap=DEFAULT_POD_CAP)
        KwokTestGenerator._ensure_priority_classes(self.ctx, rc.num_priorities, prefix="p", start=1, delete_extras=True)

        # totals & intervals (once per config)
        total_pods = rc.num_nodes * rc.pods_per_node
        node_mc = cpu_m_str_to_int(rc.node_cpu)
        node_mi = mem_str_to_mib_int(rc.node_mem)

        tgt_mc_cluster = int(node_mc * rc.util) * rc.num_nodes
        tgt_mi_cluster = int(node_mi * rc.util) * rc.num_nodes

        cpu_interval_per_pod_used, _cpu_auto = KwokTestGenerator._resolve_interval(
            "cpu", rc.cpu_interval_per_pod, total_pods, tgt_mc_cluster, cap_each=node_mc, jitter=0.25
        )
        mem_interval_per_pod_used, _mem_auto = KwokTestGenerator._resolve_interval(
            "mem", rc.mem_interval_per_pod, total_pods, tgt_mi_cluster, cap_each=node_mi, jitter=0.25
        )
        # rotation-aware: we still pass a "stem" base path; actual writing picks rotated file
        seen = self._load_seen_results_all(cfg.stem)

        # seed-only path
        if self.args.seed is not None:
            s = int(self.args.seed)
            # Always compute these; helper returns 0 when interval is None
            num_rs = self._pick_num_replicaset_for_seed(s, rc.num_replicaset, total_pods)
            rs_interval_str = KwokTestGenerator._format_interval_int(rc.rs_replicas_per_set_interval)
            self._run_one_seed(
                cfg, rc.namespace, seen, s,
                rc.num_nodes, rc.pods_per_node, num_rs, rc.num_priorities,
                rc.node_cpu, rc.node_mem, rc.util, rc.util_tolerance,
                node_mc, node_mi, total_pods,
                cpu_interval_per_pod_used, mem_interval_per_pod_used,
                wait_mode, rc.wait_timeout, rc.settle_timeout, wait_timeout_s, settle_timeout_s,
                rs_interval_str=rs_interval_str,
                s_replicas_per_set_interval=rc.rs_replicas_per_set_interval,
            )
            return

        # count path
        if self.args.count is not None and int(self.args.count) >= -1:
            to_make = int(self.args.count)
            base = int(time.time_ns())
            rng = KwokTestGenerator._rng(base, "seed-stream", self.args.cluster_name)
            made = 0
            rs_interval_str = KwokTestGenerator._format_interval_int(rc.rs_replicas_per_set_interval)
            while to_make == -1 or made < to_make:
                s = rng.getrandbits(63) or 1  # keep positive
                num_rs = self._pick_num_replicaset_for_seed(s, rc.num_replicaset, total_pods)
                self._run_one_seed(
                    cfg, rc.namespace, seen, s,
                    rc.num_nodes, rc.pods_per_node, num_rs, rc.num_priorities,
                    rc.node_cpu, rc.node_mem, rc.util, rc.util_tolerance,
                    node_mc, node_mi, total_pods,
                    cpu_interval_per_pod_used, mem_interval_per_pod_used,
                    wait_mode, rc.wait_timeout, rc.settle_timeout, wait_timeout_s, settle_timeout_s,
                    rs_interval_str=rs_interval_str,
                    rs_replicas_per_set_interval=rc.rs_replicas_per_set_interval,
                )
                if to_make != -1:
                    made += 1
            return

        # seed-file path
        if self.args.seed_file:
            seeds_list = self._read_seeds_file(Path(self.args.seed_file))
            total_seeds = len(seeds_list)
            rs_interval_str = KwokTestGenerator._format_interval_int(rc.rs_replicas_per_set_interval)
            for idx, s in enumerate(seeds_list, start=1):
                remaining = total_seeds - idx + 1
                print("\n======================================================================================================")
                print(f"[kwok-test-gen] starting seed={s} for config={cfg.name} (remaining seeds in file: {remaining})")
                print("======================================================================================================")
                s = int(s)
                num_rs = self._pick_num_replicaset_for_seed(s, rc.num_replicaset, total_pods)
                self._run_one_seed(
                    cfg, rc.namespace, seen, s,
                    rc.num_nodes, rc.pods_per_node, num_rs, rc.num_priorities,
                    rc.node_cpu, rc.node_mem, rc.util, rc.util_tolerance,
                    node_mc, node_mi, total_pods,
                    cpu_interval_per_pod_used, mem_interval_per_pod_used,
                    wait_mode, rc.wait_timeout, rc.settle_timeout, wait_timeout_s, settle_timeout_s,
                    rs_interval_str=rs_interval_str,
                    rs_replicas_per_set_interval=rc.rs_replicas_per_set_interval,
                )
            return

        print(f"[kwok-test-gen] No seeds provided for cfg={cfg.name} (use --seed / --seed-file / --count).")

    ##############################################
    # ------------ Main runner -------------------
    ##############################################
    def run(self) -> None:
        """
        Main runner function.
        """
        
        # --- early mode: just generate seeds to file and exit ---
        if getattr(self.args, "random_seeds_to_file", None):
            if self.args.count is None or self.args.count < 1:
                raise SystemExit("--random-seeds-to-file requires --count >= 1")
            if self.args.seed is not None or self.args.seed_file:
                raise SystemExit("--random-seeds-to-file cannot be combined with --seed or --seed-file")

            base = int(time.time_ns())
            rng = KwokTestGenerator._rng(base, "seed-file")
            seeds = [(rng.getrandbits(63) or 1) for _ in range(int(self.args.count))]

            outp = Path(self.args.random_seeds_to_file)
            outp.parent.mkdir(parents=True, exist_ok=True)
            with open(outp, "w", encoding="utf-8", newline="") as f:
                for s in seeds:
                    f.write(f"{s}\n")

            print(f"[kwok-test-gen] wrote {len(seeds)} seeds to {outp} (one per line, no header)")
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

        # --- FAIL FAST on provided paths ---
        if self.args.seed_file:
            file_exists(self.args.seed_file)
        dir_exists(self.args.kwok_config_dir)

        # --- test-mode constraints ---
        if self.args.test:
            if self.args.seed is None:
                raise SystemExit("--test requires exactly one --seed")
            if self.args.count is not None or self.args.seed_file:
                raise SystemExit("--test cannot be combined with --count or --seed-file")

        # proceed
        cfgs = self._get_kwok_configs(self.args.kwok_config_dir)
        total_cfgs = len(cfgs)
        print(f"[kwok-test-gen] configs={total_cfgs}")
        for idx, cfg in enumerate(cfgs, start=1):
            remaining_cfgs = total_cfgs - idx + 1
            print("\n======================================================================================================")
            print(f"[kwok-test-gen] config={cfg}  (remaining configs: {remaining_cfgs})")
            print("======================================================================================================")
            self._run_for_config(cfg)

        if self._collect_test:
            self._print_test_summary()

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
    ap.add_argument("--kwok-config-dir", dest="kwok_config_dir",
                    default="./scripts/kwok/kwok_configs",
                    help="Directory containing one or more KWOK config YAMLs")
    ap.add_argument("--results-dir", dest="results_dir", default="./scripts/kwok/results",
                    help="Directory to store results CSV files (one per KWOK config)")

    # rotation
    ap.add_argument("--max-rows-per-file", dest="max_rows_per_file", type=int, default=500000,
                    help="Maximum number of data rows per results CSV before rotating to <name>_N.csv")

    # failure logs
    ap.add_argument("--failed-kwok-configs-file", dest="failed_kwok_configs_file",
                    default="./scripts/kwok/failed/failed_kwok_configs.csv",
                    help="File path to record failed KWOK configs")
    ap.add_argument("--failed-seeds-file", dest="failed_seeds_file",
                    default="./scripts/kwok/failed/failed_seeds.csv",
                    help="File path to record failed (config, seed) runs")

    # seeds
    ap.add_argument("--seed", type=int, default=None,
                    help="Run exactly this seed (per kwok-config)")
    ap.add_argument("--seed-file", dest="seed_file", default=None,
                    help="Path to seeds file (CSV with 'seed' col or newline list).")
    ap.add_argument("--random-seeds-to-file", dest="random_seeds_to_file", default=None,
                    help="Write --count random seeds (one per line, no header) to this file, then exit. Cannot be combined with --seed/--seed-file.")
    ap.add_argument("--divide-scheduled-unscheduled", action="store_true",
                    help="Also create two per-pod CSVs named '<cfg>_scheduled.csv' and '<cfg>_unscheduled.csv'")
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
