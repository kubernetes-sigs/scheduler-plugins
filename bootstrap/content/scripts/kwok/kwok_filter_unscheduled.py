#!/usr/bin/env python3
import csv, argparse, sys
from pathlib import Path

def to_int_safe(s) -> int:
    try:
        return int(str(s).strip() or 0)
    except Exception:
        return 0

def filter_one_csv(inp: Path) -> int:
    """
    Filter a single CSV to rows with unscheduled_count > 0.
    Writes <inp.stem>_filtered.csv next to the original.
    Returns the number of kept rows (or -1 if skipped due to missing header/column).
    """
    # Don’t reprocess already-filtered files
    if inp.stem.endswith("_filtered"):
        return -1

    outp = inp.with_name(f"{inp.stem}_filtered.csv")

    with inp.open("r", encoding="utf-8", newline="") as fin:
        rdr = csv.DictReader(fin)
        if rdr.fieldnames is None:
            print(f"[skip] {inp} has no header", file=sys.stderr)
            return -1
        if "unscheduled_count" not in rdr.fieldnames:
            print(f"[skip] {inp} missing 'unscheduled_count' column", file=sys.stderr)
            return -1

        kept = 0
        with outp.open("w", encoding="utf-8", newline="") as fout:
            w = csv.DictWriter(fout, fieldnames=rdr.fieldnames)
            w.writeheader()
            for row in rdr:
                if to_int_safe(row.get("unscheduled_count")) > 0:
                    w.writerow(row)
                    kept += 1

    print(f"[ok] {inp} → {outp}  (kept {kept} rows)")
    return kept

def main():
    ap = argparse.ArgumentParser(description="Recursively filter CSVs to rows with unscheduled_count>0.")
    ap.add_argument("folder", help="Folder to scan (recursively) for *.csv files")
    args = ap.parse_args()

    root = Path(args.folder)
    if not root.exists() or not root.is_dir():
        sys.exit(f"Not a directory: {root}")

    total_files = 0
    total_kept = 0
    for inp in root.rglob("*.csv"):
        total_files += 1
        kept = filter_one_csv(inp)
        if kept and kept > 0:
            total_kept += kept

    print(f"\nDone. Scanned {total_files} CSV file(s). Total kept rows across outputs: {total_kept}")

if __name__ == "__main__":
    main()
