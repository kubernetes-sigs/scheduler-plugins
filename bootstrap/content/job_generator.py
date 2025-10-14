#!/usr/bin/env python3
# generate_job_yamls.py
import argparse, itertools, os, pathlib
from typing import Any, Dict, Iterable, List, Optional
import yaml

# --------------------------- Helpers ---------------------------

def dump_yaml(path: str, data: dict) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        yaml.safe_dump(data, f, sort_keys=False)

def util_tag(util: float) -> str:
    # 0.90 -> "090", 1.05 -> "105"
    return f"{int(round(util * 100)):03d}"

def time_tag(seconds: int) -> str:
    # 1 -> "01", 12 -> "12"
    return f"{int(seconds):02d}"

def ensure_trailing_s(seconds: int) -> str:
    return f"{int(seconds)}s"

def make_filename(prefix: str, nodes: int, pods: int, prio: int, util: float, timeout_s: Optional[int]) -> str:
    base = f"{prefix}nodes{nodes}_pods{pods}_prio{prio}_util{util_tag(util)}"
    if timeout_s is not None:
        base += f"_timeout{time_tag(timeout_s)}"
    return base + ".yaml"

def make_output_dir(root: str, prefix: str, nodes: int, pods: int, prio: int, util: float, timeout_s: Optional[int]) -> str:
    leaf = f"{prefix}nodes{nodes}_pods{pods}_prio{prio}_util{util_tag(util)}"
    if timeout_s is not None:
        leaf += f"_timeout{time_tag(timeout_s)}"
    return str(pathlib.Path(root) / leaf)

def prune_nones(obj: Any) -> Any:
    """Recursively drop keys with None values from dicts; clean lists too."""
    if isinstance(obj, dict):
        cleaned = {}
        for k, v in obj.items():
            v2 = prune_nones(v)
            if v2 is not None:
                cleaned[k] = v2
        return cleaned
    if isinstance(obj, list):
        out = []
        for v in obj:
            pv = prune_nones(v)
            if pv is not None:
                out.append(pv)
        return out
    return obj

def normalize_to_data_jobs(path_str: str) -> str:
    """
    If the path contains 'data/jobs' anywhere, strip everything before it so it starts with 'data/jobs'.
    Otherwise, return it unchanged.
    """
    s = path_str.replace("\\", "/")
    needle = "data/jobs"
    idx = s.find(needle)
    return s[idx:] if idx != -1 else path_str

# --------------------------- Main ---------------------------

def main():
    p = argparse.ArgumentParser(description="Generate YAML job files from all parameter combinations (no base file).")
    # Where to write generated YAML files
    p.add_argument("--out-dir", required=True, help="Directory on disk to store the generated YAML files.")
    # The value to put into each YAML under 'output-dir'
    p.add_argument("--output-dir", required=True, help="The base path used inside YAML 'output-dir' fields.")
    p.add_argument("--prefix", default="", help="Filename/output-dir prefix (include trailing underscore yourself if desired).")
    p.add_argument("--overwrite", action="store_true", default=False, help="Overwrite existing YAML files if they exist.")

    # Top-level defaults
    p.add_argument("--workload-config-file", type=str, required=True, help="Default workload config file.")
    p.add_argument("--kwokctl-config-file", type=str, required=True, help="Default kwokctl config file.")
    p.add_argument("--seed-file", type=str, required=True, help="Default seed file.")

    # Default None so they are omitted unless passed (become True)
    p.add_argument("--save-scheduler-logs", action="store_true", default=None)
    p.add_argument("--save-solver-stats", action="store_true", default=None)
    p.add_argument("--solver-trigger", action="store_true", default=None)

    # Parameter grids (space-separated lists)
    p.add_argument("--num-nodes", type=int, nargs="+", required=True)
    p.add_argument("--avg-pods-per-node", type=int, nargs="+", required=True)
    p.add_argument("--num-priorities", type=int, nargs="+", required=True)
    p.add_argument("--utils", type=float, nargs="+", required=True, help="e.g. 0.90 1.00 1.05")
    # Optional timeouts: if omitted, no timeout segment in filename/output-dir and no env is written
    p.add_argument("--timeouts", type=int, nargs="*", default=None, help="Seconds, e.g. 1 5 10 (optional)")

    # Extra top-level numeric field
    p.add_argument("--seeds-not-all-running", type=int, default=None,
                   help="If set, writes 'seeds-not-all-running: <int>' at top-level.")

    args = p.parse_args()

    # Build the iterable for timeouts (None if not provided)
    timeouts: Iterable[Optional[int]] = args.timeouts if (args.timeouts and len(args.timeouts) > 0) else [None]

    # Filesystem destination for YAMLs
    fs_root = pathlib.Path(args.out_dir)
    fs_root.mkdir(parents=True, exist_ok=True)

    total, skipped = 0, 0
    for nodes, pods_per_node, prio, u, t in itertools.product(
        args.num_nodes, args.avg_pods_per_node, args.num_priorities, args.utils, timeouts
    ):
        num_pods = int(nodes) * int(pods_per_node)

        # Filenames on disk
        fname = make_filename(args.prefix, nodes, num_pods, prio, u, t)
        yaml_path = fs_root / fname

        if yaml_path.exists() and not args.overwrite:
            print(f"[skip] {yaml_path} exists (use --overwrite to replace)")
            skipped += 1
            continue

        # 'output-dir' inside YAML (based on --output-dir)
        per_file_output_dir_yaml = make_output_dir(args.output_dir, args.prefix, nodes, num_pods, prio, u, t)
        per_file_output_dir_yaml = normalize_to_data_jobs(per_file_output_dir_yaml)

        # Build YAML document
        override_workload_cfg: Dict[str, Any] = {
            "num_nodes": int(nodes),
            "num_pods": int(num_pods),
            "num_priorities": int(prio),
            "util": float(u),
        }

        doc: Dict[str, Any] = {
            "workload-config-file": args.workload_config_file,
            "kwokctl-config-file": args.kwokctl_config_file,
            "seed-file": args.seed_file,
            "output-dir": per_file_output_dir_yaml,
            # raw argparse values; prune_nones will remove None entries
            "save-scheduler-logs": args.save_scheduler_logs,
            "save-solver-stats": args.save_solver_stats,
            "solver-trigger": args.solver_trigger,
            "seeds-not-all-running": args.seeds_not_all_running,
            "override-workload-config": override_workload_cfg,
        }

        # Only include envs key if we actually have timeouts
        if t is not None:
            doc["override-kwokctl-envs"] = [
                {"name": "SOLVER_PYTHON_TIMEOUT", "value": ensure_trailing_s(t)}
            ]

        # Drop any None-valued keys anywhere in the structure
        doc = prune_nones(doc)

        dump_yaml(str(yaml_path), doc)
        print(f"[ok] wrote {yaml_path}")
        total += 1

    print(f"[done] generated {total} YAML file(s), skipped {skipped}, into {args.out_dir}")

if __name__ == "__main__":
    main()
