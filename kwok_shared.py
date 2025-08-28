#!/usr/bin/env python3

# kwok_shared.py

import subprocess, json
from typing import List

# ---------- subprocess helpers ----------
def run(cmd:list[str], **kw):
    return subprocess.run(cmd, **kw)

def apply_yaml(ctx:str, yaml_text:str):
    return run(["kubectl","--context",ctx,"apply","-f","-"], input=yaml_text.encode(), check=True)

def get_json(cmd: List[str]) -> dict:
    out = subprocess.check_output(cmd)
    return json.loads(out)

# ---------- quantity helpers ----------
def cpu_to_m(v: str) -> int:
    if not v: return 0
    return int(v[:-1]) if v.endswith('m') else int(float(v) * 1000)

def m_to_cpu_str(m: int) -> str:
    return str(m // 1000) if m % 1000 == 0 else f"{m}m"

def mem_to_bytes(v: str) -> int:
    if not v: return 0
    s = v.strip()
    try:
        if s.endswith("Ki"): return int(s[:-2]) * 1024
        if s.endswith("Mi"): return int(s[:-2]) * 1024 * 1024
        if s.endswith("Gi"): return int(s[:-2]) * 1024 * 1024 * 1024
        if s.endswith("Ti"): return int(s[:-2]) * 1024 * 1024 * 1024 * 1024
        return int(s)  # bytes
    except:
        return 0

def bytes_to_mib(b: int) -> int:
    return b // (1024 * 1024)

def bytes_to_kistr(b: int) -> str:
    kib = max(0, (b + 1023) // 1024)  # ceil to Ki
    return f"{kib}Ki"

def mem_to_mi(v: str) -> int:
    if not v: return 0
    s = v.strip()
    try:
        if s.endswith('Ki'): return max(0, int(s[:-2]) // 1024)
        if s.endswith('Mi'): return int(s[:-2])
        if s.endswith('Gi'): return int(s[:-2]) * 1024
        if s.endswith('Ti'): return int(s[:-2]) * 1024 * 1024
        return int(s)  # assume Mi when bare number
    except:
        return 0

def mi_to_mem_str(mi: int) -> str:
    return f"{mi}Mi"

