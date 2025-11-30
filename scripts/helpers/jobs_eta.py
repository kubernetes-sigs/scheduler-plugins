#!/usr/bin/env python3
# jobs_eta.py

"""
eta.py — list KWOK run ETA summaries (new format only).

- Recurses into subdirectories of the current working directory
- Prints plain text
- Sorts by ETA (fallback: mtime, then path)

File name format (NO backward compatibility):
eta_<YYYYMMDD-HHMMSS|eta-unknown>_seeds-<at>-of-<total>[_snar-<left>]

Each file may contain a one-line JSON payload with any of:
{
  "eta_epoch": 1739576573,
  "eta_iso": "2025-02-14T10:22:53",
  "seeds_at": 12,
  "seeds_total": 50,   # -1 means unlimited
  "snar_left": 7       # optional; seeds-not-all-running left
}
"""

import json, re, sys, time
from pathlib import Path
from typing import Any, Dict, List, Optional

FNAME_RE = re.compile(
    r"^eta_(?P<timestr>(eta-unknown|[0-9]{8}-[0-9]{6}))"
    r"_seeds-(?P<seed_at>-?\d+)-of-(?P<seed_total>-?\d+)"
    r"(?:_snar-(?P<snar_left>\d+))?$"
)

def human_delta(seconds: Optional[int]) -> str:
    if seconds is None:
        return "unknown"
    s = max(0, int(seconds))
    h, s = divmod(s, 3600)
    m, s = divmod(s, 60)
    parts = []
    if h: parts.append(f"{h}h")
    if m: parts.append(f"{m}m")
    if s or not parts: parts.append(f"{s}s")
    return "".join(parts)

def parse_filename(p: Path) -> Dict[str, Any]:
    m = FNAME_RE.match(p.name)
    out: Dict[str, Any] = {
        "eta_epoch": None,
        "eta_iso": None,
        "seeds_at": None,
        "seeds_total": None,
        "snar_left": None,
    }
    if not m:
        return out
    gd = m.groupdict()
    out["seeds_at"] = int(gd["seed_at"])
    out["seeds_total"] = int(gd["seed_total"])
    if gd.get("snar_left") is not None:
        out["snar_left"] = int(gd["snar_left"])
    t = gd["timestr"]
    if t != "eta-unknown":
        try:
            eta_struct = time.strptime(t, "%Y%m%d-%H%M%S")
            out["eta_epoch"] = int(time.mktime(eta_struct))
            out["eta_iso"] = time.strftime("%Y-%m-%dT%H:%M:%S", eta_struct)
        except Exception:
            pass
    return out

def read_json_payload(p: Path) -> Dict[str, Any]:
    try:
        with open(p, "r", encoding="utf-8") as fh:
            line = fh.readline().strip()
        if line:
            obj = json.loads(line)
            return {
                "eta_epoch": obj.get("eta_epoch"),
                "eta_iso": obj.get("eta_iso"),
                "seeds_at": obj.get("seeds_at"),
                "seeds_total": obj.get("seeds_total"),
                "snar_left": obj.get("snar_left"),
            }
    except Exception:
        pass
    return {}

def merge_info(a: Dict[str, Any], b: Dict[str, Any]) -> Dict[str, Any]:
    out = dict(a)
    for k, v in b.items():
        if v not in (None, "", {}):
            out[k] = v
    return out

def find_eta_files(root: Path) -> List[Path]:
    return [p for p in root.rglob("eta_*") if p.is_file()]

def collect(root: Path) -> List[Dict[str, Any]]:
    entries: List[Dict[str, Any]] = []
    for p in find_eta_files(root):
        base = parse_filename(p)
        if base["seeds_at"] is None or base["seeds_total"] is None:
            continue
        js = read_json_payload(p)
        info = merge_info(base, js)
        try:
            mtime = int(p.stat().st_mtime)
        except Exception:
            mtime = None
        try:
            relpath = str(p.relative_to(root))
        except Exception:
            relpath = str(p)
        info.update({"path": str(p), "relpath": relpath, "mtime": mtime})
        entries.append(info)
    return entries

def sort_entries(entries: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
    def key(e):
        eta = e.get("eta_epoch")
        mt  = e.get("mtime")
        return (
            (eta is None, eta if eta is not None else float("inf")),
            (mt is None, mt if mt is not None else float("inf")),
            e.get("path","")
        )
    return sorted(entries, key=key)

def fmt_plain(e: Dict[str, Any]) -> str:
    eta_epoch = e.get("eta_epoch")
    eta_iso = e.get("eta_iso")
    now = int(time.time())
    delta = None if eta_epoch is None else max(0, eta_epoch - now)
    seeds_total = e.get("seeds_total")
    seeds_total_str = "unlimited" if seeds_total == -1 else (str(seeds_total) if seeds_total is not None else "?")
    seed_at = e.get("seeds_at")
    snar_left = e.get("snar_left")

    parts = [e.get("path","")]
    parts.append("ETA: unknown" if eta_epoch is None else f"ETA: {eta_iso} (in {human_delta(delta)})")
    parts.append(f"seeds {seed_at}/{seeds_total_str}")
    if snar_left is not None:
        parts.append(f"snar left: {snar_left}")
    return "  |  ".join(parts)

def main():
    root = Path(".").resolve()
    entries = sort_entries(collect(root))
    if not entries:
        print("No eta_* files found.", file=sys.stderr)
        return
    for e in entries:
        print(fmt_plain(e))

if __name__ == "__main__":
    main()
