#!/usr/bin/env python3
import csv, argparse, sys
from pathlib import Path

def to_int_safe(s) -> int:
    try:
        return int(str(s).strip() or 0)
    except Exception:
        return 0

def filter_one_csv(inp: Path, overwrite: bool=False) -> int:
    """
    Filter a single CSV to rows with unscheduled_count > 0.
    Writes next to the input:
      - <stem>_filtered.csv
      - <stem>_filtered_seeds.txt  (one seed per line, no header)
    Returns kept row count, or -1 if skipped.
    """
    out_csv   = inp.with_name(f"{inp.stem}_filtered.csv")
    out_seeds = inp.with_name(f"{inp.stem}_filtered_seeds.txt")

    # Decide which outputs need work (independently), unless --overwrite
    need_csv = True
    need_seeds = True
    try:
        in_mtime = inp.stat().st_mtime
        if not overwrite and out_csv.exists() and out_csv.stat().st_mtime >= in_mtime:
            need_csv = False
        if not overwrite and out_seeds.exists() and out_seeds.stat().st_mtime >= in_mtime:
            need_seeds = False
    except Exception:
        pass

    if not need_csv and not need_seeds:
        print(f"[skip] csv and seeds exists → {inp}", file=sys.stderr)
        return -1

    with inp.open("r", encoding="utf-8", newline="") as fin:
        rdr = csv.DictReader(fin)
        if rdr.fieldnames is None:
            print(f"[skip] {inp} has no header", file=sys.stderr); return -1
        if "unscheduled_count" not in rdr.fieldnames:
            print(f"[skip] {inp} missing 'unscheduled_count' column", file=sys.stderr); return -1

        have_seed_col = "seed" in rdr.fieldnames
        kept = 0
        seeds: list[str] = []

        # Open writer for filtered CSV only if needed
        if need_csv:
            fout = out_csv.open("w", encoding="utf-8", newline="")
            w = csv.DictWriter(fout, fieldnames=rdr.fieldnames)
            w.writeheader()
        else:
            fout = None
            w = None

        try:
            for row in rdr:
                if to_int_safe(row.get("unscheduled_count")) > 0:
                    if w is not None:
                        w.writerow(row)
                    kept += 1
                    if need_seeds and have_seed_col:
                        s = str(row.get("seed", "")).strip()
                        if s:
                            try:
                                seeds.append(str(int(s)))
                            except Exception:
                                seeds.append(s)
        finally:
            if fout is not None:
                fout.close()

    # Ensure seeds file (even if 0 lines) when a seed column exists
    if need_seeds and have_seed_col:
        with out_seeds.open("w", encoding="utf-8") as fs:
            for s in seeds:
                fs.write(s + "\n")

    # Status
    bits = []
    bits.append(f"→ [csv-created] kept {kept} rows" if need_csv else "→ [csv exists]")
    if have_seed_col:
        bits.append(f"→ [seeds-created] {len(seeds)} lines" if need_seeds else "→ [seeds exists]")
    else:
        bits.append("no 'seed' column")

    print(f"[ok] {inp} " + ", ".join(bits))
    return kept

def main():
    ap = argparse.ArgumentParser(description="Recursively filter CSVs to rows with unscheduled_count>0 and emit seed lists.")
    ap.add_argument("folder", help="Folder to scan (recursively) for *.csv files")
    ap.add_argument("--overwrite", action="store_true", help="Rewrite outputs even if up-to-date")
    args = ap.parse_args()

    root = Path(args.folder)
    if not root.exists() or not root.is_dir():
        sys.exit(f"Not a directory: {root}")
    
    total_files = 0
    total_kept = 0
    for inp in root.rglob("*.csv"):
        # Skip failed.csv and anything file already filtered
        if inp.name.lower() == "failed.csv" or inp.name.lower().endswith("_filtered.csv") or inp.name.lower().endswith("_filtered_seeds.txt"):
            continue
        total_files += 1
        kept = filter_one_csv(inp, overwrite=args.overwrite)
        if kept and kept > 0:
            total_kept += kept

    print(f"\nDone. Scanned {total_files} CSV file(s). Total kept rows across outputs: {total_kept}")

if __name__ == "__main__":
    main()
