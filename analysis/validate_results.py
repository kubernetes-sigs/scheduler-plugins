#!/usr/bin/env python3
"""
check_seeds_and_results.py

What it does
============
A) Recursively find every 'info.yaml' under <root>, and for each:
   - Normal folders:
       * Extract inputs.job.seed-file
       * Expect exactly N seeds in seed-file (default N=100)
       * Check results.csv has exactly those seeds in 'seed' column
       * Check scheduler-logs/ and solver-stats/ have a file per expected seed
         (excluding rows where results.csv solver_attempts is empty/[])
       * Special tolerance: If each of the two log folders is missing EXACTLY ONE file
         (can be DIFFERENT seeds), mark as WARN (not FAIL)
       * Validate seeds-all-running.txt / seeds-not-all-running.txt if present
         (partition rules as before)
   - Folders whose path contains 'default-deterministic':
       * ONLY check that 'seeds-not-all-running.txt' exists and has exactly N lines.
         (Skip all other checks.)

B) Recursively scan 'data/jobs/' (relative to <root> or via --jobs-dir) for *.yaml/*.yml:
   - For each job file, extract 'output-dir' (top-level or under 'job.output-dir')
   - List missing output directories into 'missing_job_outputs.txt' (in CWD)

Exit code: 1 if any folder FAILs (WARNs do not fail). Missing job outputs do NOT affect exit code.

Usage:
  python3 check_seeds_and_results.py <root> [--expect-count 100] [--jobs-dir data/jobs] [-v]

Requires: PyYAML (pip install pyyaml)
"""

import argparse, csv, os, sys, yaml
from pathlib import Path
from typing import List, Set, Tuple, Dict, Optional

# =========================
# YAML / CSV helpers
# =========================

def read_yaml(path: Path) -> dict:
    with path.open("r", encoding="utf-8") as f:
        return yaml.safe_load(f) or {}


def resolve_seed_file(seed_value: str, info_dir: Path, root: Path) -> Path:
    cand = Path(seed_value)
    if cand.is_absolute() and cand.exists():
        return cand
    for base in (info_dir, root):
        cand2 = (base / seed_value).resolve()
        if cand2.exists():
            return cand2
    return Path(seed_value)  # unresolved (for error reporting)


def load_seed_list(seed_file: Path) -> List[str]:
    seeds: List[str] = []
    with seed_file.open("r", encoding="utf-8") as f:
        for line in f:
            s = line.strip()
            if not s or s.startswith("#"):
                continue
            seeds.append(s)
    return seeds


def parse_attempts_empty(raw: Optional[str]) -> bool:
    if raw is None:
        return True
    s = str(raw).strip().strip('"').strip("'").strip()
    return s == "" or s == "[]"


def read_results_info(results_csv: Path) -> Tuple[List[str], Set[str]]:
    """
    Returns (all_seeds_in_results, seeds_with_empty_attempts)
    """
    seeds: List[str] = []
    empty_attempts: Set[str] = set()
    with results_csv.open("r", encoding="utf-8", newline="") as f:
        reader = csv.DictReader(f)
        if "seed" not in (reader.fieldnames or []):
            raise ValueError(f"'seed' column not found in {results_csv}")
        attempts_col_present = "solver_attempts" in (reader.fieldnames or [])
        for row in reader:
            raw_seed = (row.get("seed") or "").strip().strip('"').strip()
            if not raw_seed:
                continue
            seeds.append(raw_seed)
            if attempts_col_present and parse_attempts_empty(row.get("solver_attempts")):
                empty_attempts.add(raw_seed)
    return seeds, empty_attempts


def collect_seedlike_from_dir(d: Path) -> Set[str]:
    """
    Collect seeds from filenames by finding 'seed-<token>' and reading up to next '_' or end.
    """
    seeds: Set[str] = set()
    if not d.exists() or not d.is_dir():
        return seeds
    for name in os.listdir(d):
        if "seed-" not in name:
            continue
        try:
            tail = name.split("seed-", 1)[1]
            token = tail.split("_", 1)[0].strip()
            if token:
                seeds.add(token)
        except Exception:
            continue
    return seeds


def compare_sets(expected: Set[str], actual: Set[str]) -> Tuple[Set[str], Set[str]]:
    return (expected - actual, actual - expected)


# =========================
# seeds-all/not-all checks
# =========================

def check_seed_partitions(info_dir: Path, seeds_set: Set[str], expect_count: int, out: Dict[str, object]):
    all_path = info_dir / "seeds-all-running.txt"
    not_path = info_dir / "seeds-not-all-running.txt"

    have_all = all_path.exists()
    have_not = not_path.exists()

    if not (have_all or have_not):
        return  # nothing to validate

    def _load(path: Path) -> List[str]:
        try:
            return load_seed_list(path)
        except Exception as e:
            out["status"] = "FAIL"
            out.setdefault("issues", []).append(f"Failed reading {path.name}: {e}")
            return []

    all_list: List[str] = _load(all_path) if have_all else []
    not_list: List[str] = _load(not_path) if have_not else []

    all_set, not_set = set(all_list), set(not_list)
    union_set = all_set | not_set
    inter_set = all_set & not_set

    # No extras
    extras = union_set - seeds_set
    if extras:
        out["status"] = "FAIL"
        out.setdefault("issues", []).append(
            f"{'Both lists' if have_all and have_not else 'Seed list'} contain {len(extras)} seed(s) not in seed-file "
            f"(e.g., {', '.join(sorted(list(extras))[:5])}{'...' if len(extras)>5 else ''})"
        )

    # Overlap
    if have_all and have_not and inter_set:
        out["status"] = "FAIL"
        out.setdefault("issues", []).append(
            f"Overlap between seeds-all-running.txt and seeds-not-all-running.txt: {len(inter_set)} "
            f"(e.g., {', '.join(sorted(list(inter_set))[:5])}{'...' if len(inter_set)>5 else ''})"
        )

    # Exact partition if both exist
    if have_all and have_not:
        missing_from_union = seeds_set - union_set
        if missing_from_union:
            out["status"] = "FAIL"
            out.setdefault("issues", []).append(
                f"Union of the two seed lists is missing {len(missing_from_union)} seed(s) from seed-file "
                f"(e.g., {', '.join(sorted(list(missing_from_union))[:5])}{'...' if len(missing_from_union)>5 else ''})"
            )
        if len(union_set) != expect_count:
            out["status"] = "FAIL"
            out.setdefault("issues", []).append(
                f"Combined seeds-all-running.txt + seeds-not-all-running.txt has {len(union_set)} unique seeds; expected {expect_count} "
                f"(all-running={len(all_set)}, not-all-running={len(not_set)})"
            )
    else:
        # Only one exists -> must be subset
        present_set = all_set if have_all else not_set
        if not present_set.issubset(seeds_set):
            bad = present_set - seeds_set
            out["status"] = "FAIL"
            out.setdefault("issues", []).append(
                f"{'seeds-all-running.txt' if have_all else 'seeds-not-all-running.txt'} contains {len(bad)} seed(s) not in seed-file "
                f"(e.g., {', '.join(sorted(list(bad))[:5])}{'...' if len(bad)>5 else ''})"
            )

    # Diagnostic duplicates -> warnings
    if have_all and len(all_list) != len(all_set):
        out.setdefault("warnings", []).append(
            f"seeds-all-running.txt contains {len(all_list) - len(all_set)} duplicate row(s)"
        )
    if have_not and len(not_list) != len(not_set):
        out.setdefault("warnings", []).append(
            f"seeds-not-all-running.txt contains {len(not_list) - len(not_set)} duplicate row(s)"
        )

    out["seeds_all_running_count"] = len(all_set) if have_all else 0
    out["seeds_not_all_running_count"] = len(not_set) if have_not else 0


# =========================
# Core folder check
# =========================

def check_folder(info_yaml: Path, expect_count: int, root: Path, verbose: bool) -> Dict[str, object]:
    info_dir = info_yaml.parent

    # --- Special-cased behavior for 'default-deterministic' paths ---
    if "default-deterministic" in {p.name for p in info_dir.resolve().parents} | {info_dir.name}:
        out = {
            "folder": str(info_dir),
            "status": "PASS",
            "issues": [],
            "warnings": [],
        }
        snr = info_dir / "seeds-not-all-running.txt"
        if not snr.exists():
            out["status"] = "FAIL"
            out["issues"].append("seeds-not-all-running.txt not found (required for default-deterministic)")
            out["reason"] = "Missing seeds-not-all-running.txt"
            return out
        try:
            seeds = load_seed_list(snr)
        except Exception as e:
            out["status"] = "FAIL"
            out["issues"].append(f"Failed reading seeds-not-all-running.txt: {e}")
            out["reason"] = "Read error"
            return out

        if len(seeds) != expect_count:
            out["status"] = "FAIL"
            out["issues"].append(
                f"seeds-not-all-running.txt has {len(seeds)} entries; expected {expect_count}"
            )
            out["reason"] = f"Count {len(seeds)} != {expect_count}"
        else:
            out["reason"] = f"seeds-not-all-running.txt has {expect_count} entries"
        return out
    # --- End special case ---

    data = read_yaml(info_yaml)

    # Extract seed-file
    try:
        seed_value = data["inputs"]["job"]["seed-file"]
    except Exception:
        return {
            "folder": str(info_dir),
            "status": "FAIL",
            "reason": "Could not find inputs.job.seed-file in info.yaml",
        }

    seed_file = resolve_seed_file(seed_value, info_dir, root)
    if not seed_file.exists():
        return {
            "folder": str(info_dir),
            "status": "FAIL",
            "reason": f"Seed file not found: {seed_file}",
        }

    # Load seeds from seed file
    seeds_list = load_seed_list(seed_file)
    seeds_set = set(seeds_list)
    out: Dict[str, object] = {
        "folder": str(info_dir),
        "seed_file": str(seed_file),
        "seed_file_count": len(seeds_list),
        "status": "PASS",
        "issues": [],
        "warnings": [],
    }

    # Expect exact count
    if len(seeds_list) != expect_count:
        out["status"] = "FAIL"
        out["issues"].append(
            f"Seed file count is {len(seeds_list)}, expected {expect_count}"
        )

    # Check results.csv
    results_csv = info_dir / "results.csv"
    results_seeds_list: List[str] = []
    empty_attempts: Set[str] = set()
    if not results_csv.exists():
        out["status"] = "FAIL"
        out["issues"].append("results.csv not found")
    else:
        try:
            results_seeds_list, empty_attempts = read_results_info(results_csv)
            res_seeds_set = set(results_seeds_list)

            missing, unexpected = compare_sets(seeds_set, res_seeds_set)
            if missing:
                out["status"] = "FAIL"
                out["issues"].append(
                    f"results.csv is missing {len(missing)} seeds (examples: {', '.join(sorted(list(missing))[:5])}{'...' if len(missing)>5 else ''})"
                )
            if unexpected:
                out["status"] = "FAIL"
                out["issues"].append(
                    f"results.csv has {len(unexpected)} unexpected seeds (examples: {', '.join(sorted(list(unexpected))[:5])}{'...' if len(unexpected)>5 else ''})"
                )

            if len(results_seeds_list) != len(res_seeds_set):
                dup_count = len(results_seeds_list) - len(res_seeds_set)
                out["warnings"].append(f"results.csv contains {dup_count} duplicate seed entries")

            out["results_csv_seed_count"] = len(results_seeds_list)
        except Exception as e:
            out["status"] = "FAIL"
            out["issues"].append(f"Failed reading results.csv: {e}")

    # Validate seeds-all-running.txt / seeds-not-all-running.txt if present
    check_seed_partitions(info_dir, seeds_set, expect_count, out)

    # Determine which seeds are expected in logs (exclude those with empty/[] solver_attempts)
    expected_in_logs: Set[str] = seeds_set - empty_attempts

    # Check scheduler-logs/
    sched_dir = info_dir / "scheduler-logs"
    sched_seen = collect_seedlike_from_dir(sched_dir)
    missing_sched, _ = compare_sets(expected_in_logs, sched_seen)

    # Check solver-stats/
    solver_dir = info_dir / "solver-stats"
    solver_seen = collect_seedlike_from_dir(solver_dir)
    missing_solver, _ = compare_sets(expected_in_logs, solver_seen)

    out["scheduler_logs_expected_count"] = len(expected_in_logs)
    out["scheduler_logs_ok_count"] = len(expected_in_logs) - len(missing_sched)
    out["solver_stats_expected_count"] = len(expected_in_logs)
    out["solver_stats_ok_count"] = len(expected_in_logs) - len(missing_solver)

    # WARN rule: allow each folder to be off by exactly one (even if different seeds)
    ms, mv = missing_sched, missing_solver
    if (
        (len(ms) == 1 and len(mv) == 0)
        or (len(ms) == 0 and len(mv) == 1)
        or (len(ms) == 1 and len(mv) == 1)
    ):
        msg_parts = []
        if len(ms) == 1:
            msg_parts.append(f"scheduler-logs missing seed {next(iter(ms))}")
        if len(mv) == 1:
            msg_parts.append(f"solver-stats missing seed {next(iter(mv))}")
        if msg_parts:
            out["warnings"].append(" & ".join(msg_parts))
    else:
        if ms:
            out["status"] = "FAIL"
            out["issues"].append(
                f"scheduler-logs is missing {len(ms)} seed file(s) "
                f"(examples: {', '.join(sorted(list(ms))[:5])}{'...' if len(ms)>5 else ''})"
            )
        if mv:
            out["status"] = "FAIL"
            out["issues"].append(
                f"solver-stats is missing {len(mv)} seed file(s) "
                f"(examples: {', '.join(sorted(list(mv))[:5])}{'...' if len(mv)>5 else ''})"
            )

    # Reason
    if out["issues"]:
        out["reason"] = f"{len(out['issues'])} issue(s) found"
    elif out["warnings"]:
        if out["status"] == "PASS":
            out["status"] = "WARN"
        out["reason"] = "; ".join(out["warnings"]) if verbose else "Minor discrepancy (allowed)"
    else:
        out["reason"] = "All checks passed"

    return out


# =========================
# Jobs directory scanning
# =========================

def extract_output_dir_from_job_yaml(job_yaml: Path) -> Optional[str]:
    """
    Accept either:
      - top-level 'output-dir'
      - or nested under 'job' -> 'output-dir'
    """
    try:
        doc = read_yaml(job_yaml)
    except Exception:
        return None
    if not isinstance(doc, dict):
        return None
    if "output-dir" in doc and isinstance(doc["output-dir"], (str, Path)):
        return str(doc["output-dir"])
    job = doc.get("job")
    if isinstance(job, dict) and isinstance(job.get("output-dir"), (str, Path)):
        return str(job["output-dir"])
    return None


def find_missing_job_outputs(jobs_dir: Path, cwd: Path) -> List[Tuple[str, str]]:
    """
    Scan jobs_dir recursively for *.yaml/*.yml, extract output-dir, and
    check if that directory exists. Treat relative paths as relative to CWD.
    Returns a list of tuples: (job_yaml_path, output_dir_path_that_is_missing)
    """
    missing: List[Tuple[str, str]] = []
    for p in jobs_dir.rglob("*"):
        if p.is_file() and p.suffix.lower() in (".yaml", ".yml"):
            outdir_str = extract_output_dir_from_job_yaml(p)
            if not outdir_str:
                continue  # no output-dir here; skip
            outdir = Path(outdir_str)
            outdir_abs = outdir if outdir.is_absolute() else (cwd / outdir).resolve()
            if not outdir_abs.exists():
                missing.append((str(p), str(outdir_abs)))
    return missing


# =========================
# Main
# =========================

def main():
    ap = argparse.ArgumentParser(description="Validate seeds/results/logs per info.yaml and check missing job outputs.")
    ap.add_argument("--root", type=str, default="G:/My Drive/Datalogi/MSc - SDU/Master Thesis/Results/results", help="Root directory to scan recursively for 'info.yaml'.")
    ap.add_argument("--expect-count", type=int, default=100, help="Expected number of seeds or entries (default: 100).")
    ap.add_argument("--jobs-dir", type=str, default="bootstrap/content/data/jobs", help="Jobs directory (scanned recursively for YAMLs).")
    ap.add_argument("--verbose", "-v", action="store_true", help="Print detailed issues/warnings.")
    args = ap.parse_args()

    root = Path(args.root).resolve()
    if not root.exists():
        print(f"ERROR: Root directory does not exist: {root}", file=sys.stderr)
        sys.exit(2)

    # A) Scan for info.yaml
    info_files: List[Path] = []
    for dirpath, dirnames, filenames in os.walk(root):
        if "info.yaml" in filenames:
            info_files.append(Path(dirpath) / "info.yaml")

    if not info_files:
        print("No info.yaml files found.", file=sys.stderr)

    total = len(info_files)
    passed = 0
    warned = 0
    failed = 0

    for info_yaml in sorted(info_files):
        result = check_folder(info_yaml, args.expect_count, root, args.verbose)
        status = result["status"]
        folder = result["folder"]
        reason = result.get("reason", "")
        print(f"[{status}] {folder} — {reason}")
        if status == "PASS":
            passed += 1
        elif status == "WARN":
            warned += 1
            if args.verbose and result.get("warnings"):
                for w in result["warnings"]:  # type: ignore
                    print(f"  ~ {w}")
        else:
            failed += 1
            if args.verbose and result.get("issues"):
                for issue in result["issues"]:  # type: ignore
                    print(f"  - {issue}")

    # B) Jobs dir scan (relative to ROOT or explicit)
    jobs_dir = (root / args.jobs_dir).resolve()
    missing_outputs: List[Tuple[str, str]] = []
    if jobs_dir.exists() and jobs_dir.is_dir():
        missing_outputs = find_missing_job_outputs(jobs_dir, Path.cwd())
    else:
        print(f"WARNING: jobs-dir '{jobs_dir}' does not exist; skipping jobs output check.")

    # Write missing outputs list in CWD
    if missing_outputs:
        out_path = Path.cwd() / "missing_job_outputs.txt"
        with out_path.open("w", encoding="utf-8") as f:
            for job_yaml, outdir in missing_outputs:
                f.write(f"{outdir}  # from {job_yaml}\n")
        print(f"\nMissing job outputs ({len(missing_outputs)}) — saved to: {out_path}")
        for job_yaml, outdir in (missing_outputs[:10]):  # preview first 10
            print(f"  - {outdir}  (from {job_yaml})")
        if len(missing_outputs) > 10:
            print(f"  ... and {len(missing_outputs) - 10} more")
    else:
        print("\nNo missing job outputs found in jobs-dir.")

    # Summary
    print("\nSummary")
    print("=======")
    print(f"info.yaml folders checked: {total}")
    print(f"PASS: {passed}")
    print(f"WARN: {warned}")
    print(f"FAIL: {failed}")

    # Exit 1 only on failures from info.yaml checks
    sys.exit(0 if failed == 0 else 1)


if __name__ == "__main__":
    main()
