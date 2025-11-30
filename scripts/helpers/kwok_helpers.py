#!/usr/bin/env python3
# kwok_helpers.py

import tempfile
import os, subprocess, yaml, shlex, logging, textwrap, fcntl, contextlib
from pathlib import Path
from scripts.helpers.kubectl_helpers import kubectl_apply_yaml

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

@contextlib.contextmanager
def kwok_cache_lock():
    """
    Take an exclusive lock so only one process runs 'kwokctl create cluster'
    at a time, avoiding races in ~/.kwok/cache when downloading binaries.
    Override lock path via env KWOK_CACHE_LOCK if desired.
    """
    lock_path = os.environ.get("KWOK_CACHE_LOCK", "/tmp/kwokctl-cache.lock")
    os.makedirs(os.path.dirname(lock_path), exist_ok=True)
    fd = os.open(lock_path, os.O_CREAT | os.O_RDWR, 0o666)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)
        yield
    finally:
        try:
            fcntl.flock(fd, fcntl.LOCK_UN)
        finally:
            os.close(fd)
