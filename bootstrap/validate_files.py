#!/usr/bin/env python3
# CSV/YAML validator (prefix-agnostic). No files are written.

import re, csv, yaml, argparse
from pathlib import Path

# Accept ANY prefix; only require the segment starting at _nodes...
TOKEN = re.compile(
    r"_nodes(?P<nodes>\d+)_pods(?P<pods>\d+)_prio(?P<prio>\d+)_util(?P<util>\d{3})(?:\.(ya?ml|csv))?",
    re.IGNORECASE,
)

def parse_token(text: str):
    m = TOKEN.search(text or "")
    if not m: return None
    return {
        "nodes": int(m.group("nodes")),
        "pods":  int(m.group("pods")),
        "prio":  int(m.group("prio")),
        "util":  int(m.group("util")) / 100.0,
    }

def tokens_equal(a: dict, b: dict) -> bool:
    return (
        a["nodes"] == b["nodes"]
        and a["pods"] == b["pods"]
        and a["prio"] == b["prio"]
        and round(a["util"], 3) == round(b["util"], 3)
    )

def diff(a: dict, b: dict) -> str:
    bits=[]
    if a["nodes"]!=b["nodes"]: bits.append(f"nodes exp={a['nodes']} act={b['nodes']}")
    if a["pods"] !=b["pods"]:  bits.append(f"pods exp={a['pods']} act={b['pods']}")
    if a["prio"] !=b["prio"]:  bits.append(f"prio exp={a['prio']} act={b['prio']}")
    if round(a["util"],3)!=round(b["util"],3): bits.append(f"util exp={a['util']} act={b['util']}")
    return "; ".join(bits)

# ---------- CSV validation ----------
def validate_csv(csv_path: Path) -> list[str]:
    """If filename has a _nodes... token, each row token must match it.
       Otherwise, enforce per-row consistency between all tokens found in that row.
       Cells without tokens are ignored."""
    issues = []
    exp_from_name = parse_token(csv_path.name)

    with csv_path.open("r", encoding="utf-8-sig", newline="") as f:
        rdr = csv.DictReader(f)
        if rdr.fieldnames is None:
            return ["CSV has no header"]

        for i, row in enumerate(rdr, start=2):  # header is line 1
            # gather tokens from all cells in this row
            row_tokens = []
            for col, val in (row or {}).items():
                t = parse_token(str(val))
                if t:
                    row_tokens.append((col, t))

            if not row_tokens:
                continue  # nothing to validate on this row

            if exp_from_name:
                # every token must match the filename token
                row_mis = [
                    f"{col}: {diff(exp_from_name, t)}"
                    for col, t in row_tokens
                    if not tokens_equal(exp_from_name, t)
                ]
                if row_mis:
                    issues.append(f"row {i}: " + " | ".join(row_mis))
            else:
                # no filename token -> all tokens within the row must match each other
                first_col, first_tok = row_tokens[0]
                row_mis = [
                    f"{col} vs {first_col}: {diff(first_tok, t)}"
                    for col, t in row_tokens[1:]
                    if not tokens_equal(first_tok, t)
                ]
                if row_mis:
                    issues.append(f"row {i}: " + " | ".join(row_mis))

    return issues

# ---------- YAML validation ----------
def extract_runner_doc(docs):
    for d in docs:
        if isinstance(d, dict) and d.get("kind") == "KwokRunConfiguration":
            return d
    for d in docs:
        if isinstance(d, dict):
            return d
    return {}

def validate_yaml(yaml_path: Path) -> list[str]:
    """Compare YAML fields to the YAML filename token, if fields are present."""
    exp = parse_token(yaml_path.name)
    if not exp:
        return ["filename has no _nodes... token"]

    issues = []
    try:
        docs = list(yaml.safe_load_all(yaml_path.read_text(encoding="utf-8")))
    except Exception as e:
        return [f"YAML parse error: {e}"]
    rd = extract_runner_doc(docs)

    if "num_nodes" in rd and rd["num_nodes"] != exp["nodes"]:
        issues.append(f"num_nodes exp={exp['nodes']} act={rd['num_nodes']}")
    if "num_pods" in rd and rd["num_pods"] != exp["pods"]:
        issues.append(f"num_pods exp={exp['pods']} act={rd['num_pods']}")

    # priorities (accept scalar or equal interval)
    pr_val = None
    if "num_priorities" in rd and isinstance(rd["num_priorities"], (int, float)):
        pr_val = int(rd["num_priorities"])
    elif "num_priorities" in rd and isinstance(rd["num_priorities"], (list, tuple)) and len(rd["num_priorities"])==2 and rd["num_priorities"][0]==rd["num_priorities"][1]:
        pr_val = int(rd["num_priorities"][0])
    elif "num_priorities_lo" in rd and "num_priorities_hi" in rd and rd["num_priorities_lo"]==rd["num_priorities_hi"]:
        pr_val = int(rd["num_priorities_lo"])
    if pr_val is not None and pr_val != exp["prio"]:
        issues.append(f"num_priorities exp={exp['prio']} act={pr_val}")

    if "util" in rd:
        try:
            if round(float(rd["util"]),3) != round(float(exp["util"]),3):
                issues.append(f"util exp={exp['util']} act={rd['util']}")
        except Exception:
            issues.append(f"util not numeric: {rd['util']}")

    return issues

# ---------- CLI ----------
def main():
    ap = argparse.ArgumentParser(description="Validate CSV/YAML files against _nodes... tokens (recursive).")
    ap.add_argument("--base", default=".", help="Base directory to scan (default: .)")
    ap.add_argument("--no-yaml", action="store_true", help="Skip YAML validation")
    ap.add_argument("--no-csv", action="store_true", help="Skip CSV validation")
    ap.add_argument("-v","--verbose", action="store_true")
    args = ap.parse_args()

    base = Path(args.base).expanduser().resolve()
    if not base.exists():
        print(f"[error] base does not exist: {base}")
        raise SystemExit(2)

    reports = []

    if not args.no_csv:
        csv_paths = list(base.rglob("*.csv"))
        if args.verbose:
            print(f"[info] found {len(csv_paths)} CSV(s)")
            for p in csv_paths: print("  -", p)
        for p in csv_paths:
            issues = validate_csv(p)
            reports.append((p, "OK" if not issues else "FAIL", issues))

    if not args.no_yaml:
        yml_paths = list(base.rglob("*.yaml")) + list(base.rglob("*.yml"))
        if args.verbose:
            print(f"[info] found {len(yml_paths)} YAML(s)")
            for p in yml_paths: print("  -", p)
        for p in yml_paths:
            issues = validate_yaml(p)
            reports.append((p, "OK" if not issues else "FAIL", issues))

    ok = sum(1 for _, s, _ in reports if s == "OK")
    fail = sum(1 for _, s, _ in reports if s == "FAIL")
    print(f"\nValidated files: {len(reports)} | OK: {ok} | FAIL: {fail}")
    for path, status, issues in reports:
        print(f"\n{path}: {status}")
        for msg in issues[:50]:
            print(f"  - {msg}")

if __name__ == "__main__":
    main()
