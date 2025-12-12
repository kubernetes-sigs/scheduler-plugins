import argparse
from dataclasses import dataclass
from typing import Any, Callable, Iterable, Optional

from scripts.helpers.general_helpers import get_str, coerce_bool

_UNSET = object()


@dataclass(frozen=True)
class JobField:
    job_key: str
    arg_attr: str
    parse: Optional[Callable[[Any], Any]] = None
    # If accept is None, we accept any value not in (None, "").
    accept: Optional[Callable[[Any], bool]] = None


def _accept_default(v: Any) -> bool:
    return v is not None and v != ""


def parse_optional_str(v: Any) -> str | None:
    return get_str(v)


def parse_optional_bool(v: Any) -> bool | None:
    return coerce_bool(v, default=None)


def parse_optional_int(v: Any) -> int | None:
    if v is None:
        return None
    if isinstance(v, bool):
        return None
    if isinstance(v, int):
        return int(v)
    s = str(v).strip()
    if not s:
        return None
    try:
        return int(s)
    except Exception:
        return None


def parse_optional_float(v: Any) -> float | None:
    if v is None:
        return None
    if isinstance(v, bool):
        return None
    if isinstance(v, (int, float)):
        return float(v)
    s = str(v).strip()
    if not s:
        return None
    try:
        return float(s)
    except Exception:
        return None


def merge_job_fields_into_args(
    args: argparse.Namespace,
    job: dict,
    fields: Iterable[JobField],
) -> argparse.Namespace:
    """
    Merge job-file fields into args.

    CLI priority: we only set args.<attr> when it is currently None.
    """
    job = job or {}
    for f in fields:
        raw = job.get(f.job_key, _UNSET)
        if raw is _UNSET:
            continue

        val = f.parse(raw) if f.parse else raw
        accept = f.accept or _accept_default

        if getattr(args, f.arg_attr, None) is None and accept(val):
            setattr(args, f.arg_attr, val)

    return args