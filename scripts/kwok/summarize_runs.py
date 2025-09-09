#!/usr/bin/env python3
# scripts/kwok/summarize_matrix.py

import argparse, csv, sys, time
from collections import defaultdict
from pathlib import Path
import statistics as S

def quantiles(vals, qs):
    if not vals: return [0.0]*len(qs)
    v = sorted(vals)
    out = []
    for q in qs:
        if q <= 0: out.append(v[0]); continue
        if q >= 1: out.append(v[-1]); continue
        idx = q * (len(v) - 1)
        lo = int(idx); hi = min(len(v)-1, lo+1)
        frac = idx - lo
        out.append(v[lo]*(1-frac) + v[hi]*frac)
    return out

def load_runs(in_dir: Path):
    """Read all runs_n*_ppn*.csv files into grouped stats."""
    groups = defaultdict(lambda: {"scheduled": [], "cpu": [], "mem": [], "total": []})

    for f in sorted(in_dir.glob("runs_n*_ppn*.csv")):
        with f.open(newline="") as fh:
            r = csv.DictReader(fh)
            # Expect headers:
            # timestamp,cluster,num_nodes,pods_per_node,util,repeat,scheduled,total_pods,cpu_run_util,mem_run_util
            for row in r:
                try:
                    key = (
                        row["cluster"],
                        int(row["num_nodes"]),
                        int(row["pods_per_node"]),
                        float(row["util"]),
                    )
                    groups[key]["scheduled"].append(int(row["scheduled"]))
                    groups[key]["cpu"].append(float(row["cpu_run_util"]))
                    groups[key]["mem"].append(float(row["mem_run_util"]))
                    groups[key]["total"].append(int(row["total_pods"]))
                except Exception:
                    # Skip malformed rows silently
                    continue
    return groups

def write_summary(groups, out_csv: Path):
    out_csv.parent.mkdir(parents=True, exist_ok=True)
    with out_csv.open("w", newline="") as f:
        w = csv.writer(f)
        w.writerow([
            "cluster","num_nodes","pods_per_node","util",
            "repeats",
            "scheduled_mean","scheduled_median","scheduled_p25","scheduled_p75","scheduled_p90",
            "cpu_run_mean","cpu_run_median","cpu_run_p25","cpu_run_p75","cpu_run_p90",
            "mem_run_mean","mem_run_median","mem_run_p25","mem_run_p75","mem_run_p90",
            "total_pods"
        ])
        pcts = (0.25, 0.75, 0.90)
        for (cluster, nn, ppn, util), data in sorted(groups.items()):
            sch = data["scheduled"]; cpu = data["cpu"]; mem = data["mem"]; tot = data["total"]
            s25, s75, s90 = quantiles(sch, pcts)
            c25, c75, c90 = quantiles(cpu, pcts)
            m25, m75, m90 = quantiles(mem, pcts)
            w.writerow([
                cluster, nn, ppn, f"{util:.2f}",
                len(sch),
                round(S.mean(sch),3) if sch else 0, round(S.median(sch),3) if sch else 0, round(s25,3), round(s75,3), round(s90,3),
                round(S.mean(cpu),6) if cpu else 0.0, round(S.median(cpu),6) if cpu else 0.0, round(c25,6), round(c75,6), round(c90,6),
                round(S.mean(mem),6) if mem else 0.0, round(S.median(mem),6) if mem else 0.0, round(m25,6), round(m75,6), round(m90,6),
                (tot[0] if tot else 0),
            ])

def print_summary(out_csv: Path):
    print("\n==================== MATRIX SUMMARY ====================")
    with out_csv.open(newline="") as f:
        r = csv.DictReader(f)
        for row in r:
            print(
                f"[{row['cluster']}] nodes={row['num_nodes']} ppn={row['pods_per_node']} util={row['util']} "
                f"repeats={row['repeats']} | "
                f"scheduled mean/med={row['scheduled_mean']}/{row['scheduled_median']} "
                f"p25/p75/p90={row['scheduled_p25']}/{row['scheduled_p75']}/{row['scheduled_p90']} | "
                f"cpu_run mean/med={float(row['cpu_run_mean'])*100:.1f}%/{float(row['cpu_run_median'])*100:.1f}% "
                f"mem_run mean/med={float(row['mem_run_mean'])*100:.1f}%/{float(row['mem_run_median'])*100:.1f}%"
            )
    print("========================================================\n")

def main():
    ap = argparse.ArgumentParser(description="Summarize KWOK per-combo run CSVs.")
    ap.add_argument("--in-dir", default="./scripts/kwok/out")
    ap.add_argument("--out-csv", default="./scripts/kwok/out/summary.csv")
    ap.add_argument("--print", action="store_true")
    ap.add_argument("--watch", type=int, default=0, help="Recompute every N seconds (0 = one-shot)")
    args = ap.parse_args()

    in_dir = Path(args.in_dir)
    out_csv = Path(args.out_csv)

    if args.watch and args.watch > 0:
        try:
            while True:
                groups = load_runs(in_dir)
                write_summary(groups, out_csv)
                if args.print:
                    print_summary(out_csv)
                time.sleep(args.watch)
        except KeyboardInterrupt:
            print("\n[watch] stopped.")
            sys.exit(0)
    else:
        groups = load_runs(in_dir)
        write_summary(groups, out_csv)
        if args.print:
            print_summary(out_csv)

if __name__ == "__main__":
    main()
