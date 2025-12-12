#!/usr/bin/env python3
# kwok_helpers.py

import tempfile
import os, subprocess, yaml, shlex, logging, textwrap, contextlib, copy
from pathlib import Path
from typing import Iterable, Mapping, Any
from scripts.helpers.kubectl_helpers import kubectl_apply_yaml
try:
    import fcntl  # type: ignore
except Exception:  # pragma: no cover
    fcntl = None

# ====================================================================
# YAML helpers.
# Due to proper indentation, we keep them outside class
# ====================================================================
def yaml_kwok_node(name: str, cpu: str, mem: str, pods_cap: int) -> str:
    return textwrap.dedent(f"""\
    apiVersion: v1
    kind: Node
    metadata:
      name: {name}
      annotations:
        kwok.x-k8s.io/node: "fake"
      labels:
        kubernetes.io/hostname: "{name}"
        kubernetes.io/os: "linux"
        kubernetes.io/arch: "amd64"
        node-role.kubernetes.io/agent: ""
        type: "kwok"
    spec:
      taints:
      - key: kwok.x-k8s.io/node
        value: "fake"
        effect: NoSchedule
    status:
      capacity:
        cpu: "{cpu}"
        memory: "{mem}"
        pods: {pods_cap}
      allocatable:
        cpu: "{cpu}"
        memory: "{mem}"
        pods: {pods_cap}
      nodeInfo:
        architecture: amd64
        kubeletVersion: fake
        kubeProxyVersion: fake
        operatingSystem: linux
      phase: Running
    ---
    """)

def yaml_kwok_rs(ns: str, rs_name: str, replicas: int, cpu: str, mem: str, pc: str) -> str:
    return textwrap.dedent(f"""\
    apiVersion: apps/v1
    kind: ReplicaSet
    metadata:
      name: {rs_name}
      namespace: {ns}
    spec:
      replicas: {replicas}
      selector:
        matchLabels:
          app: {rs_name}
      template:
        metadata:
          labels:
            app: {rs_name}
        spec:
          priorityClassName: {pc}
          restartPolicy: Always
          tolerations:
          - key: "kwok.x-k8s.io/node"
            operator: "Exists"
            effect: "NoSchedule"
          containers:
          - name: filler
            image: registry.k8s.io/pause:3.9
            resources:
              requests: {{cpu: "{cpu}", memory: "{mem}"}}
              limits:   {{cpu: "{cpu}", memory: "{mem}"}}
    ---
    """)

def yaml_kwok_pod(ns: str, name: str, cpu: str, mem: str, pc: str) -> str:
    return textwrap.dedent(f"""\
    apiVersion: v1
    kind: Pod
    metadata:
      name: {name}
      namespace: {ns}
    spec:
      restartPolicy: Always
      priorityClassName: {pc}
      tolerations:
      - key: "kwok.x-k8s.io/node"
        operator: "Exists"
        effect: "NoSchedule"
      containers:
      - name: filler
        image: registry.k8s.io/pause:3.9
        resources:
          requests: {{cpu: "{cpu}", memory: "{mem}"}}
          limits:   {{cpu: "{cpu}", memory: "{mem}"}}
    ---
    """)

##############################################
# ------------ KWOK helpers --------
##############################################
def run_kwokctl_logged(logger: logging.Logger, *args: str, input_bytes: bytes | None = None, check: bool = True) -> subprocess.CompletedProcess:
    """
    Run `kwokctl <args...>` and stream its combined output into LOG.
    """
    cmd = ["kwokctl", *args]
    logger.debug("exec: %s", " ".join(shlex.quote(c) for c in cmd))
    r = subprocess.run(cmd, input=input_bytes, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
    if r.stdout:
        for line in r.stdout.decode(errors="replace").splitlines():
            logger.info("kwokctl> %s", line)
    if check and r.returncode != 0:
        raise subprocess.CalledProcessError(r.returncode, cmd, r.stdout)
    return r

def ensure_kwok_cluster(logger: logging.Logger, cluster_name: str, kwok_runtime: str, config_doc: dict | None = None, recreate: bool = True) -> None:
    """
    Create a kwok cluster.
    """
    if recreate:
        logger.info("recreating kwok cluster '%s'", cluster_name)
        with kwok_cache_lock():
            run_kwokctl_logged(logger, "delete", "cluster", "--name", cluster_name, check=False)
    tf = tempfile.NamedTemporaryFile("w", delete=False, suffix=".yaml")
    try:
        yaml.safe_dump(config_doc, tf, sort_keys=False)
    finally:
        tf.close()
    cfg_for_kwokctl = Path(tf.name)
    try:
        with kwok_cache_lock():
            run_kwokctl_logged(logger, "create", "cluster", "--name", cluster_name, "--config", str(cfg_for_kwokctl), "--runtime", kwok_runtime)
    finally:
        if cfg_for_kwokctl.exists():
            try: os.unlink(cfg_for_kwokctl)
            except OSError: pass

def create_kwok_nodes(logger: logging.Logger, ctx: str, num_nodes: int, node_cpu: str, node_mem: str, pods_cap: int) -> None:
    """
    Create KWOK nodes kwok-node-1..kwok-node-N with the given capacity.
    """
    kubectl_apply_yaml(logger, ctx, "".join(yaml_kwok_node(f"kwok-node-{i}", node_cpu, node_mem, pods_cap) for i in range(1, num_nodes + 1)))

def kwok_pods_cap(total_pods: int | None = None) -> int:
    """
    Compute a reasonable per-node pod capacity for KWOK nodes.
    We use max(512, total_pods * 3) to ensure that the pod capacity is not a limiting factor.
    """
    return max(512, (total_pods * 3)) if total_pods is not None else 512

@contextlib.contextmanager
def kwok_cache_lock():
    """
    Take an exclusive lock so only one process runs 'kwokctl create cluster'
    at a time, avoiding races in ~/.kwok/cache when downloading binaries.
    Override lock path via env KWOK_CACHE_LOCK if desired.
    """
    default_lock_path = "/tmp/kwokctl-cache.lock"
    if os.name == "nt":
      default_lock_path = str(Path(tempfile.gettempdir()) / "kwokctl-cache.lock")

    lock_path = os.environ.get("KWOK_CACHE_LOCK", default_lock_path)
    os.makedirs(os.path.dirname(lock_path), exist_ok=True)
    fd = os.open(lock_path, os.O_CREAT | os.O_RDWR, 0o666)
    try:
      if fcntl is not None:
        fcntl.flock(fd, fcntl.LOCK_EX)
      yield
    finally:
      try:
        if fcntl is not None:
          fcntl.flock(fd, fcntl.LOCK_UN)
      finally:
        os.close(fd)

def merge_kwokctl_envs(doc: dict, add_envs: Iterable[Mapping[str, Any]] | None, component: str = "kube-scheduler") -> dict:
    """
    Return a copy of `doc` with extraEnvs for `component` merged by env 'name'.
    - Items in `add_envs` override/insert existing envs.
    - Ensures componentsPatches and the component entry exist.
    - Sorts components and envs by name for stability.
    """
    d = copy.deepcopy(doc) if isinstance(doc, dict) else {}
    # Normalize inputs
    add = [
        e for e in (add_envs or [])
        if isinstance(e, Mapping) and e.get("name")
    ]
    if not add:
        return d
    # Ensure componentsPatches is a list of dicts
    cps = [c for c in (d.get("componentsPatches") or []) if isinstance(c, dict)]
    d["componentsPatches"] = cps  # normalize back on the copy
    # Get or create the target component patch
    comp = next((c for c in cps if c.get("name") == component), None)
    if comp is None:
        comp = {"name": component}
        cps.append(comp)
    # Merge by name
    cur = [e for e in (comp.get("extraEnvs") or []) if isinstance(e, dict) and e.get("name")]
    env_by_name = {e["name"]: copy.deepcopy(e) for e in cur}
    env_by_name.update({e["name"]: copy.deepcopy(e) for e in add})
    comp["extraEnvs"] = [env_by_name[k] for k in sorted(env_by_name)]
    cps.sort(key=lambda c: c.get("name", ""))
    return d