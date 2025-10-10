#!/usr/bin/env python3
"""
eta.py — list KWOK run ETA summaries (simplified).

- Always recurses into subdirectories
- Always prints plain text
- Always sorts by ETA (fallback: mtime, then path)
- No limit on entries
- Accepts at most one directory argument (defaults to ".")

Looks for files named like:
  eta_<YYYYMMDD-HHMMSS|eta-unknown>_configs-<at>-of-<total>_seeds-<at>-of-<total>
and also reads one-line JSON payloads inside those files if present.
"""

from __future__ import annotations
import json, re, sys, time
from pathlib import Path
from typing import Any, Dict, List, Optional

FNAME_RE = re.compile(
    r"^eta_(?P<timestr>(eta-unknown|[0-9]{8}-[0-9]{6}))"
    r"_configs-(?P<cfg_at>\d+)-of-(?P<cfg_total>\d+)"
    r"_seeds-(?P<seed_at>-?\d+)-of-(?P<seed_total>-?\d+)$"
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
        "configs_at": None,
        "configs_total": None,
        "seeds_at": None,
        "seeds_total": None,
    }
    if not m:
        return out
    gd = m.groupdict()
    out["configs_at"] = int(gd["cfg_at"])
    out["configs_total"] = int(gd["cfg_total"])
    out["seeds_at"] = int(gd["seed_at"])
    out["seeds_total"] = int(gd["seed_total"])
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
                "configs_at": obj.get("configs_at"),
                "configs_total": obj.get("configs_total"),
                "seeds_at": obj.get("seeds_at"),
                "seeds_total": obj.get("seeds_total"),
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
    files = find_eta_files(root)
    for p in files:
        base = parse_filename(p)
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
        # Sort: known ETA first (ascending), then known mtime, then path
        return ((eta is None, eta if eta is not None else float("inf")),
                (mt is None, mt if mt is not None else float("inf")),
                e.get("path",""))
    return sorted(entries, key=key)

def fmt_plain(e: Dict[str, Any]) -> str:
    eta_epoch = e.get("eta_epoch")
    eta_iso = e.get("eta_iso")
    now = int(time.time())
    delta = None if eta_epoch is None else max(0, eta_epoch - now)
    seeds_total = e.get("seeds_total")
    seeds_total_str = "unlimited" if seeds_total == -1 else (str(seeds_total) if seeds_total is not None else "?")
    cfg_at = e.get("configs_at"); cfg_total = e.get("configs_total")
    seed_at = e.get("seeds_at")

    parts = [e.get("path","")]
    parts.append("ETA: unknown" if eta_epoch is None else f"ETA: {eta_iso} (in {human_delta(delta)})")
    parts.append(f"cfg {cfg_at}/{cfg_total}")
    parts.append(f"seeds {seed_at}/{seeds_total_str}")
    return "  |  ".join(parts)

def main():
    # One optional directory argument (default ".")
    if len(sys.argv) > 2:
        print("Usage: eta.py [DIR]", file=sys.stderr)
        sys.exit(2)
    root = Path(sys.argv[1] if len(sys.argv) == 2 else ".").resolve()

    entries = collect(root)
    entries = sort_entries(entries)

    if not entries:
        print("No eta_* files found.", file=sys.stderr)
        return

    for e in entries:
        print(fmt_plain(e))

if __name__ == "__main__":
    main()
