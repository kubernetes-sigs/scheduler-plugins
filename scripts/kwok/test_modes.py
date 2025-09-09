#!/usr/bin/env python3
# kwok_case_runner.py
#
# Python version of the bash script:
# - Recreates a KWOK cluster per case using a solver/mode/at-specific config
# - Runs the generator (7 nodes x 6 pods => 42 pods)
# - Waits for baseline Running pods
# - Applies a high-priority RS and waits for its Ready replicas
# - Records PASS/FAIL per case and prints a tabulated summary
#
# Env overrides:
#   CLUSTER_NAME (default: kwok1)
#   NS           (default: crossnode-test)

import os
import sys
import time
from pathlib import Path
from typing import List, Tuple

from tabulate import tabulate

from kwok_shared import (
    run, get_json_ctx,  # subprocess helpers
)

# -------------------- Config --------------------

CLUSTER_NAME = os.environ.get("CLUSTER_NAME", "kwok1")
NS = os.environ.get("NS", "crossnode-test")
CTX = f"kwok-{CLUSTER_NAME}"

# File locations (kept identical to the bash layout)
REPO_ROOT = Path.cwd()
GENERATOR_PY = REPO_ROOT / "scripts/kwok/kwok_test_generator.py"
HIGH_PRIO_RS_YAML = REPO_ROOT / "scripts/kwok/test-high-prio-rs.yaml"

def config_yaml_path(solver: str, mode: str, at: str) -> Path:
    return REPO_ROOT / f"scripts/kwok/test-{solver}-{mode}-{at}.yaml"

# -------------------- Wait helpers --------------------

def wait_for_rs_ready_replicas(ns: str, rs_name: str, replicas: int, timeout_s: int) -> bool:
    """
    Polls the ReplicaSet's .status.readyReplicas in the namespace until it reaches 'replicas',
    or times out. Prints progress lines like the bash function.
    """
    start = time.time()
    while True:
        ready = 0
        try:
            rs = get_json_ctx(CTX, ["-n", ns, "get", "replicaset", rs_name, "-o", "json"])
            ready = int((rs.get("status") or {}).get("readyReplicas") or 0)
        except Exception:
            # Treat as 0 ready while RS is not yet visible/created
            ready = 0

        print(f"[wait] RS/{rs_name}: {ready}/{replicas} Ready in ns/{ns}")
        if ready >= replicas:
            print(f"[wait] RS/{rs_name} reached target: {ready}/{replicas} Ready")
            return True

        if time.time() - start > timeout_s:
            print(f"[wait] timeout after {timeout_s}s waiting for RS/{rs_name} to have {replicas} Ready replicas")
            return False

        time.sleep(2)


def wait_for_running_count(ns: str, expected: int, timeout_s: int) -> bool:
    """
    Counts pods with phase == Running in the given namespace until at least 'expected' are Running,
    or times out. Mirrors the bash behavior/printing.
    """
    start = time.time()
    while True:
        running = 0
        try:
            pods = get_json_ctx(CTX, ["-n", ns, "get", "pods", "-o", "json"])
            items = pods.get("items", []) or []
            for p in items:
                phase = (p.get("status") or {}).get("phase") or ""
                if phase == "Running":
                    running += 1
        except Exception:
            running = 0

        print(f"[wait] {running}/{expected} Running in ns/{ns}")
        if running >= expected:
            print(f"[wait] reached target: {running} pods Running")
            return True

        if time.time() - start > timeout_s:
            print(f"[wait] timeout after {timeout_s}s waiting for {expected} pods Running")
            return False

        time.sleep(2)

# -------------------- Runner --------------------

def run_case(solver: str, mode: str, at: str, expected_param: int) -> Tuple[str, str, str]:
    """
    Mirrors bash 'run_case'. Returns (label, status, note).
    Note: expected_param is kept for parity with the bash signature (not used in logic).
    """
    label = f"{solver}-{mode}-{at}"
    ok = True
    notes: List[str] = []

    print(f"===== Running case: {label} =====")

    # Fresh cluster
    run(["kwokctl", "delete", "cluster", "--name", CLUSTER_NAME], check=False)
    cfg = config_yaml_path(solver, mode, at)
    r_create = run(
        ["kwokctl", "create", "cluster", "--name", CLUSTER_NAME, "--config", str(cfg)],
        check=False
    )
    if r_create.returncode != 0:
        ok = False
        notes.append("kwokctl create failed")

    # Ensure context string (same as bash)
    global CTX
    CTX = f"kwok-{CLUSTER_NAME}"

    # Build generator args
    pyargs = [
        sys.executable, str(GENERATOR_PY),
        CLUSTER_NAME, "7", "6", "0",
        "--namespace", NS,  # keep generator's ns aligned with our waits
    ]
    if (mode == "for_every") or (at == "postfilter"):
        pyargs.append("--wait-each")

    # Run generator (do not abort whole script on failure)
    r_gen = run(pyargs, check=False)
    if r_gen.returncode != 0:
        ok = False
        notes.append("generator failed")

    # ---- Only the waits decide timing outcomes (but we still record other failures like bash) ----

    # Baseline: wait for 42 Running (7 nodes * 6 pods)
    if not wait_for_running_count(NS, 42, 80):
        ok = False
        notes.append("baseline wait timed out")

    time.sleep(3)

    # Apply the high-priority RS
    r_apply = run(["kubectl", "--context", CTX, "apply", "-f", str(HIGH_PRIO_RS_YAML)], check=False)
    if r_apply.returncode != 0:
        ok = False
        notes.append("apply RS failed")

    # Wait for the RS to have 7 Ready replicas (matches the bash)
    if not wait_for_rs_ready_replicas(NS, "high-priority-rs", 7, 80):
        ok = False
        notes.append("RS wait timed out")

    if ok:
        print(f"===== Case {label} PASS =====")
        return label, "PASS", "; ".join(notes)
    else:
        print(f"===== Case {label} FAIL =====")
        return label, "FAIL", "; ".join(notes)

# -------------------- Test Matrix --------------------
# Keep parity with the bash defaults (two active cases).
CASES_TO_RUN = [
    ("local_search", "for_every", "preenqueue", 34),
    ("py",           "for_every", "preenqueue", 34),
    # Uncomment/extend as desired:
    # ("bfs", "for_every",    "preenqueue", 34),
    # ("bfs", "for_every",    "postfilter", 34),
    # ("bfs", "in_batches",   "preenqueue", 34),
    # ("bfs", "in_batches",   "postfilter", 34),
    # ("bfs", "continuously", "postfilter", 34),
    # ("local_search", "for_every",    "postfilter", 34),
    # ("local_search", "in_batches",   "preenqueue", 34),
    # ("local_search", "in_batches",   "postfilter", 34),
    # ("local_search", "continuously", "postfilter", 34),
    # ("py", "in_batches",   "preenqueue", 34),
    # ("py", "for_every",    "postfilter", 34),
    # ("py", "in_batches",   "postfilter", 34),
    # ("py", "continuously", "postfilter", 34),
]

def main() -> int:
    rows: List[Tuple[str, str, str]] = []
    passed = 0
    failed = 0

    for solver, mode, at, expected in CASES_TO_RUN:
        label, status, note = run_case(solver, mode, at, expected)
        rows.append((label, status, note))
        if status == "PASS":
            passed += 1
        else:
            failed += 1

    # -------------------- Summary (tabulate) --------------------
    print()
    print("==================== TEST SUMMARY ====================")
    print(tabulate(rows, headers=["CASE", "RESULT", "NOTE"], tablefmt="fancy_grid", stralign="left"))
    print("------------------------------------------------------")
    print(f"Passed: {passed}  Failed: {failed}")
    print("======================================================")

    return 0 if failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
