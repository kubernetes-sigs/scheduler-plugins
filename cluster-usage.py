#!/usr/bin/env python3
import json, subprocess, sys

def cpu_to_m(v:str) -> int:
    if not v: return 0
    return int(v[:-1]) if v.endswith('m') else int(float(v) * 1000)

def mem_to_mi(v:str) -> int:
    if not v: return 0
    s = v
    if s.endswith('Ki'): return max(0, int(s[:-2]) // 1024)
    if s.endswith('Mi'): return int(s[:-2])
    if s.endswith('Gi'): return int(s[:-2]) * 1024
    if s.endswith('Ti'): return int(s[:-2]) * 1024 * 1024
    # assume Mi if raw int
    try: return int(s)
    except: return 0

def get_json(cmd:list[str]) -> dict:
    out = subprocess.check_output(cmd)
    return json.loads(out)

def sum_requests(pod:dict) -> tuple[int,int]:
    cpu = mem = 0
    spec = pod.get("spec", {})
    for group in ("containers","initContainers","ephemeralContainers"):
        for c in spec.get(group, []) or []:
            req = (c.get("resources",{}) or {}).get("requests", {}) or {}
            cpu += cpu_to_m(req.get("cpu", "0"))
            mem += mem_to_mi(req.get("memory", "0"))
    return cpu, mem

def main():
    nodes = get_json(["kubectl","get","nodes","-o","json"])
    pods  = get_json(["kubectl","get","pods","--all-namespaces","-o","json"])

    # alloc by node
    alloc = {}
    for n in nodes["items"]:
        name = n["metadata"]["name"]
        a = n.get("status",{}).get("allocatable",{}) or {}
        alloc[name] = (cpu_to_m(a.get("cpu","0")), mem_to_mi(a.get("memory","0")))

    # aggregate per-node
    cpu_req = {n:0 for n in alloc}
    mem_req = {n:0 for n in alloc}
    pods_run= {n:0 for n in alloc}

    all_run = 0
    all_notrun = 0

    for p in pods["items"]:
        phase = (p.get("status",{}) or {}).get("phase","")
        if phase == "Running": all_run += 1
        elif phase: all_notrun += 1

        node = (p.get("spec",{}) or {}).get("nodeName","")
        if not node or node not in alloc:
            continue
        c, m = sum_requests(p)
        cpu_req[node] += c
        mem_req[node] += m
        if phase == "Running":
            pods_run[node] += 1

    # header
    print(f'{"NODE":25} {"CPU_ALLOC(m)":14} {"CPU_REQ(m)":22} {"CPU_FREE(m)":22} '
          f'{"MEM_ALLOC(Mi)":14} {"MEM_REQ(Mi)":22} {"MEM_FREE(Mi)":22} {"PODS_RUN":16}')

    # rows
    for node in sorted(alloc.keys()):
        am, ai = alloc[node]
        rm = cpu_req.get(node,0)
        ri = mem_req.get(node,0)
        fm = am - rm
        fi = ai - ri
        cpu_req_pct  = (rm/am*100) if am>0 else 0.0
        cpu_free_pct = (fm/am*100) if am>0 else 0.0
        mem_req_pct  = (ri/ai*100) if ai>0 else 0.0
        mem_free_pct = (fi/ai*100) if ai>0 else 0.0
        run = pods_run.get(node,0)

        print(f'{node:25} '
              f'alloc={am:>5}m      '
              f'req={rm:>5}m ({cpu_req_pct:4.1f}%)   '
              f'free={fm:>5}m ({cpu_free_pct:4.1f}%)   '
              f'alloc={ai:>6}Mi '
              f'req={ri:>6}Mi ({mem_req_pct:4.1f}%)   '
              f'free={fi:>6}Mi ({mem_free_pct:4.1f}%)   '
              f'run={run:<6}')

    print()
    print(f"TOTAL_PODS_RUN={all_run}  TOTAL_PODS_NOTRUN={all_notrun}")

if __name__ == "__main__":
    try:
        main()
    except subprocess.CalledProcessError as e:
        sys.stderr.write(e.output.decode() if e.output else str(e))
        sys.exit(e.returncode)
