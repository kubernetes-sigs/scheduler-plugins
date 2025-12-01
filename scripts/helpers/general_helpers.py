#!/usr/bin/env python3
# general_helpers.py

import sys, time, random, subprocess, csv, re, logging, hashlib, shlex, yaml
from typing import List, Dict, Tuple, Optional, Any
from pathlib import Path
from decimal import Decimal

MEM_UNIT_TABLE = {
    # bytes
    "": 1, "b": 1, "byte": 1, "bytes": 1,
    # SI (10^3)
    "k": 10**3, "kb": 10**3,
    "m": 10**6, "mb": 10**6,
    "g": 10**9, "gb": 10**9,
    # IEC (2^10)
    "ki": 1024, "kib": 1024,
    "mi": 1024**2, "mib": 1024**2,
    "gi": 1024**3, "gib": 1024**3,
}

##############################################
# ------------ Time helpers----------------
##############################################
def get_timestamp() -> str:
    """
    Get the current timestamp as a string.
    """
    return time.strftime("%Y/%m/%d/%H:%M:%S", time.localtime())

def format_hms(seconds: int) -> str:
    """
    Format seconds into a human-readable string.
    """
    seconds = max(0, int(seconds))
    h, r = divmod(seconds, 3600)
    m, s = divmod(r, 60)
    parts = []
    if h: parts.append(f"{h}h")
    if m: parts.append(f"{m}m")
    if s or not parts: parts.append(f"{s}s")
    return "".join(parts)

##############################################
# ------------ Logging helpers----------------
##############################################
class PrefixFilter(logging.Filter):
    """
    Injects a static 'prefix' field into each LogRecord.
    """
    def __init__(self, prefix: str):
        super().__init__()
        self.prefix = prefix
    def filter(self, record: logging.LogRecord) -> bool:
        record.prefix = self.prefix
        return True

def setup_logging(name: str, prefix: str, level: str = "INFO") -> logging.Logger:
    """
    Configure the module logger.
    - prefix: shown before every message (e.g. '[worker 2/5 cluster=kwok1] ').
    - level:  DEBUG/INFO/WARNING/ERROR/CRITICAL (case-insensitive).
    """
    lvl = getattr(logging, str(level).upper(), logging.INFO)
    logger = logging.getLogger(name)
    logger.propagate = False
    logger.setLevel(lvl)
    logger.handlers.clear()
    h = logging.StreamHandler(stream=sys.stdout)
    fmt = logging.Formatter("%(asctime)s %(prefix)s%(message)s", datefmt="%H:%M:%S")
    h.setFormatter(fmt)
    h.addFilter(PrefixFilter(prefix))
    logger.addHandler(h)
    logging.captureWarnings(True)
    return logger

def make_header_footer(msg: str, width: int = 100, border: str = "=") -> Tuple[str, str]:
    """
    Build a centered header line with `msg` between border chars and a matching-width footer line.
    If `msg` is longer than `width`, the width expands to fit it.
    Example:
    >>> h, f = get_header_footer("Running tests")
    >>> print(h, " ... stuff ... ", f, sep="")
    """
    msg = str(msg).strip().replace("\n", " ")
    inner = f" {msg} "                       # space padding around the message
    w = max(width, len(inner))               # ensure width fits the message
    left = (w - len(inner)) // 2
    right = w - len(inner) - left
    header = f"{border * left}{inner}{border * right}"
    footer = f"{border * w}"
    return header, footer

def get_git_info(cwd: Optional[Path] = None) -> Dict[str, Any]:
    """
    Collect basic git info for reproducibility. Best effort; returns empty on failure.
    """
    info: Dict[str, Any] = {}
    def _run(cmd: List[str]) -> Optional[str]:
        try:
            r = subprocess.run(cmd, cwd=str(cwd) if cwd else None, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, check=False)
            if r.returncode == 0:
                return (r.stdout or b"").decode("utf-8", errors="replace").strip()
        except Exception:
            pass
        return None
    commit = _run(["git", "rev-parse", "HEAD"])
    branch = _run(["git", "rev-parse", "--abbrev-ref", "HEAD"])
    r = subprocess.run(["git", "status", "--porcelain"], cwd=str(cwd) if cwd else None,
                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, check=False)
    if commit is not None: info["commit"] = commit
    if branch is not None: info["branch"] = branch
    return info

def build_cli_cmd() -> str:
    """
    Return the full CLI command used to invoke the current script,
    prefixed with 'python3 ' (for reproducible info bundles).
    """
    return "python3 " + " ".join(shlex.quote(a) for a in sys.argv)

def write_info_file(
    out_path: Path | str,
    *,
    meta_extra: dict | None = None,
    inputs: dict | None = None,
    sections: dict | None = None,
    logger: logging.Logger | None = None,
) -> None:
    """
    Generic helper to write an info YAML bundle.

    Structure:
      meta:
        timestamp: ...
        git: ...
        ...meta_extra...
      inputs:  (optional)
        ...inputs...
      <section-name>:
        ... (from sections) ...

    - out_path: target YAML path.
    - meta_extra: additional keys to merge into 'meta'.
    - inputs: dictionary to be stored under 'inputs'.
    - sections: mapping section_name -> dict, stored at top level.
    """
    try:
        p = Path(out_path)
        p.parent.mkdir(parents=True, exist_ok=True)

        from scripts.helpers.general_helpers import get_git_info, get_timestamp  # if not in same file, adjust

        git_info = get_git_info(Path.cwd())
        meta = {
            "timestamp": get_timestamp(),
            "git": git_info or {},
        }
        if meta_extra:
            meta.update(meta_extra)

        payload: dict = {"meta": meta}

        if inputs is not None:
            payload["inputs"] = inputs

        if sections:
            for k, v in sections.items():
                if v is not None:
                    payload[k] = v

        with open(p, "w", encoding="utf-8") as fh:
            yaml.safe_dump(payload, fh, sort_keys=False)

        if logger:
            logger.info("wrote info bundle to %s", p)
    except Exception as e:
        if logger:
            logger.warning("failed to write info bundle to %s: %s", out_path, e)

def log_field_fmt(v):
    return "<unset>" if v in (None, "") else str(v)

##############################################
# ------------ Parser helpers----------------
##############################################
def normalize_interval(doc: Dict[str, Any], key_combo: Tuple[str, str, str], *, allow_none: bool = True) -> Optional[str]:
    """
    Normalize a (single) or (lo, hi) interval from the document.
    It first checks for 'single' key; if not found, it looks for 'lo_key' and 'hi_key'.
    Returns "lo,hi" or "" if not found (or None if allow_none and not found).
    """
    single, lo_key, hi_key = key_combo
    if single in doc and doc[single] is not None:
        v = doc[single]
        if isinstance(v, (int, float)):
            return str(int(v))
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

def parse_duration_to_seconds(s: str) -> float:
    """
    Parse a duration string into seconds.
    Accepted forms:
    - Plain number: "3600"  -> 3600 seconds
    - With unit:
        "10s" -> 10 seconds
        "30m" -> 30 minutes
        "1h"  -> 1 hour
        "1d"  -> 1 day
    Units: s = seconds, m = minutes, h = hours, d = days.
    """
    s = s.strip().lower()
    if not s:
        raise ValueError("trace-time duration string cannot be empty")

    unit_multipliers = {
        "s": 1.0,
        "m": 60.0,
        "h": 3600.0,
        "d": 86400.0,
    }

    # If last char is a letter, treat it as unit
    if s[-1].isalpha():
        unit = s[-1]
        if unit not in unit_multipliers:
            raise ValueError(f"Unknown duration unit in trace-time: {unit!r}")
        number_part = s[:-1]
        if not number_part:
            raise ValueError(f"Missing numeric value before unit in trace-time: {s!r}")
        value = float(number_part)
        return value * unit_multipliers[unit]
    else:
        # No unit -> seconds
        return float(s)

def parse_int_interval(s: Optional[str], *, min_lo: int = 1) -> Optional[Tuple[int, int]]:
    """
    Parse a string interval "lo,hi" or "x" into a (lo, hi) tuple.
    Returns None if s is None or empty.
    Ensures lo >= min_lo and hi >= lo.
    """
    if not s:
        return None
    parts = [x.strip() for x in str(s).split(",", 1)]
    if len(parts) == 1:
        lo = hi = int(parts[0])
    else:
        lo, hi = int(parts[0]), int(parts[1])
    lo = max(min_lo, lo)
    hi = max(lo, hi)
    return lo, hi

def parse_qty_interval(s: Optional[str]) -> Optional[Tuple[str, str]]:
    if not s:
        return None
    parts = [x.strip() for x in s.split(",", 1)]
    if len(parts) == 1:
        return (parts[0], parts[0])
    return (parts[0], parts[1])

def parse_timeout_s(t:str | None, default: int = 60) -> int:
    """
    Parse a timeout string into seconds.
    """
    if not t:
        return default
    try:
        if t.endswith("ms"): return max(1, int(int(t[:-2]) / 1000))
        if t.endswith("s"):  return int(t[:-1])
        if t.endswith("m"):  return int(t[:-1]) * 60
        if t.endswith("h"):  return int(t[:-1]) * 3600
        return int(t)
    except Exception:
        return default

def coerce_bool(v, default=False):
    if v is None: return default
    if isinstance(v, bool): return v
    s = str(v).strip().lower()
    if s in ("1","true","yes","y","on"): return True
    if s in ("0","false","no","n","off"): return False
    return default

def get_str(v):
    if v is None: return None
    s = str(v).strip()
    return s if s else None

def get_int_from_dict(doc: Dict[str, Any], key: str, default: int) -> int:
    """
    Get an integer value from the document, returning a default if not found or invalid.
    """
    v = doc.get(key, default)
    try:
        return int(v)
    except Exception:
        return default

def get_float_from_dict(doc: Dict[str, Any], key: str, default: float) -> float:
    """
    Get a float value from the document, returning a default if not found or invalid.
    """
    v = doc.get(key, default)
    try:
        return float(v)
    except Exception:
        return default

def get_str_from_dict(doc: Dict[str, Any], key: str, default: Optional[str]) -> Optional[str]:
    """
    Get a string value from the document, returning a default if not found or empty.
    """
    v = doc.get(key, default)
    if v is None:
        return default
    s = str(v).strip()
    return s if s else default

##############################################
# ------------ Quantity helpers----------------
##############################################
def qty_to_mcpu_int(token: str) -> int:
    """
    Convert any CPU quantity to millicores.
    Accepts: '250m', '0.25', '1.5', '2cpu', '2 cores', etc.
    No unit => cores.
    """
    t = (token or "").strip().lower()
    m = re.fullmatch(r'\s*([0-9]+(?:\.[0-9]+)?)\s*([a-z ]*)\s*', t)
    if not m:
        raise ValueError(f"Invalid CPU quantity: {token!r}")
    val = Decimal(m.group(1))
    unit = (m.group(2) or "").replace(" ", "")
    if unit in ("m", "mcpu", "millicpu"):
        milli = val
    elif unit in ("", "c", "cpu", "core", "cores"):
        milli = val * Decimal(1000)
    else:
        raise ValueError(f"Unsupported CPU unit: {unit!r}")
    return max(1, int(milli))

def qty_to_bytes_int(token: str) -> int:
    
    """
    Convert any Kubernetes-like memory quantity to integer bytes.
    Accepts: '1536Mi', '1.5Gi', '500MB', '4G', '1024', '42 kib', etc.
    No unit => bytes.
    """
    t = (token or "").strip().lower()
    m = re.fullmatch(r'\s*([0-9]+(?:\.[0-9]+)?)\s*([a-z]+)?\s*', t)
    if not m:
        raise ValueError(f"Invalid memory quantity: {token!r}")
    val = Decimal(m.group(1))
    unit = (m.group(2) or "")
    mult = MEM_UNIT_TABLE.get(unit)
    if mult is None:
        raise ValueError(f"Unsupported memory unit: {unit!r}")
    bytes_int = int(val * mult)
    return max(1, bytes_int)  # keep >0 for downstream constraints

def qty_to_mcpu_str(m: int) -> str:
    """
    Convert milli CPU integer (e.g. 100, 1000) to CPU string (e.g. "100m", "1").
    """
    return str(m // 1000) if m % 1000 == 0 else f"{m}m"

def qty_to_bytes_str(b: int) -> str:
    """Return bytes as a decimal quantity string for K8s."""
    return str(int(max(1, b)))

##############################################
# ------------ CSV helpers----------------
##############################################
def csv_read_header(path: Path) -> list[str] | None:
    """Return the header row for a CSV file, or None if unreadable/empty."""
    try:
        with open(path, "r", encoding="utf-8", newline="") as fh:
            rdr = csv.reader(fh)
            row = next(rdr, None)
            if row is None:
                return None
            # normalize by stripping surrounding spaces
            return [c.strip() for c in row]
    except Exception:
        return None

def csv_append_row(
    file_path: str | Path,
    header: list[str],
    row: dict,
) -> None:
    """
    Append a row to CSV, writing the header if the file is new.
    Rules:
      - Reject if 'row' contains any keys not in 'header'.
      - Missing header fields are written as empty strings.
      - File column order always follows 'header' (incoming row order ignored).
    """
    p = Path(file_path)
    p.parent.mkdir(parents=True, exist_ok=True)
    header_set = set(header)
    row_keys = set(row.keys())
    extra = row_keys - header_set
    if extra:
        raise ValueError(f"Row contains fields not in header: {sorted(extra)}")
    # Build in header order; fill missing with ""
    safe_row = {k: ("" if row.get(k) is None else row.get(k, "")) for k in header}
    with open(p, "a", encoding="utf-8", newline="") as f:
        wr = csv.DictWriter(f, fieldnames=header)
        if f.tell() == 0:
            wr.writeheader()
        wr.writerow(safe_row)
        f.flush()

######################################################
# ---------- Seed helpers --------------
######################################################
def derive_seed(base_seed: int, *labels: object, nbytes: int = 16) -> int:
    """
    Deterministically derive a child seed from a base seed and a sequence of labels.
    This is to make sure that changes in one part of the code do not affect RNG streams in other parts.
    """
    h = hashlib.blake2b(digest_size=nbytes)
    # fixed-width encode base to keep derivations stable
    h.update(int(base_seed).to_bytes(16, "big", signed=False))
    for lab in labels:
        h.update(b"\x00")  # separator
        h.update(str(lab).encode("utf-8"))
    return int.from_bytes(h.digest(), "big")

def seeded_random(base_seed: int, *labels: object) -> random.Random:
    """
    Convenience: Random() seeded from _derive_seed(base_seed, *labels).
    """
    return random.Random(derive_seed(base_seed, *labels))

def generate_seeds(gen_seeds_to_file: Optional[List[str]]) -> None:
    """
    Generate random seeds and if requested, write them to one or multiple files.
    Modes:
    --generate-seeds-to-file PATH NUM
    --generate-seeds-to-file PATH NUM PARTS
    If PARTS provided and PATH contains '{i}', substitute it with 1..PARTS.
    Else, create PATH_part-<i>(.ext)
    """
    argsv = gen_seeds_to_file
    if not argsv or len(argsv) not in (2, 3):
        raise SystemExit("--gen-seeds-to-file requires PATH NUM [PARTS]")
    path_str, num_str = argsv[0], argsv[1]
    try:
        total = int(num_str)
    except Exception:
        raise SystemExit("--gen-seeds-to-file: NUM must be an integer")
    if total < 1:
        raise SystemExit("--gen-seeds-to-file: NUM must be >= 1")
    parts = 1
    if len(argsv) == 3:
        try:
            parts = int(argsv[2])
        except Exception:
            raise SystemExit("--gen-seeds-to-file: PARTS must be an integer")
        if parts < 1:
            raise SystemExit("--gen-seeds-to-file: PARTS must be >= 1")
        if parts > total:
            raise SystemExit("--gen-seeds-to-file: PARTS cannot exceed NUM")
    # RNG + seeds
    now_ns = int(time.time_ns())
    r = seeded_random(now_ns, "base")
    seeds = [(r.getrandbits(63) or 1) for _ in range(total)]
    # Split even-ish across parts
    base = total // parts
    rem = total % parts
    def _resolve_out(i: int) -> Path:
        if "{i}" in path_str:
            return Path(path_str.replace("{i}", str(i)))
        # No template; if multiple parts, add _part-i before extension inline
        p = Path(path_str)
        if parts > 1:
            if p.suffix:
                return p.with_name(f"{p.stem}-{i}{p.suffix}")
            else:
                return p.with_name(f"{p.name}-{i}")
        return p  # single file
    # Ensure parent dir of first output exists
    first_out = _resolve_out(1)
    first_out.parent.mkdir(parents=True, exist_ok=True)
    offset = 0
    written_total = 0
    for i in range(1, parts + 1):
        size = base + (1 if i <= rem else 0)
        chunk = seeds[offset:offset + size]
        offset += size
        outp = _resolve_out(i)
        outp.parent.mkdir(parents=True, exist_ok=True)
        with open(outp, "w", encoding="utf-8", newline="") as f:
            for s in chunk:
                f.write(f"{s}\n")
        print(f"wrote {len(chunk)} seeds to {outp}")
        written_total += len(chunk)
    print(f"wrote {written_total} seeds across {parts} file(s)")