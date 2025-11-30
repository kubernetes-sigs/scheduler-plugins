#!/usr/bin/env python3
# kubectl_helpers.py

import time, subprocess, shlex, json, logging, textwrap
from typing import Optional

def yaml_priority_class(name: str, value: int) -> str:
    return textwrap.dedent(f"""\
    apiVersion: scheduling.k8s.io/v1
    kind: PriorityClass
    metadata:
      name: {name}
    value: {value}
    preemptionPolicy: PreemptLowerPriority
    globalDefault: false
    description: "pod priority {value}"
    ---
    """)

def run_kubectl_logged(logger: logging.Logger, ctx: str, *args: str, input_bytes: bytes | None = None, check: bool = True) -> subprocess.CompletedProcess:
    """
    Run `kubectl --context <ctx> <args...>` and stream its combined output into LOG.
    Returns a CompletedProcess-like object. Raises CalledProcessError if check=True and rc!=0.
    """
    cmd = ["kubectl", "--context", ctx, *args]
    logger.debug("exec: %s", " ".join(shlex.quote(c) for c in cmd))
    r = subprocess.run(cmd, input=input_bytes, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
    if r.stdout:
        for line in r.stdout.decode(errors="replace").splitlines():
            logger.info("kubectl> %s", line)
    if check and r.returncode != 0:
        raise subprocess.CalledProcessError(r.returncode, cmd, r.stdout)
    return r

def kubectl_apply_yaml(logger: logging.Logger, ctx: str, yaml_text: str) -> subprocess.CompletedProcess:
    """
    Apply a YAML configuration to the cluster, logging kubectl output with worker prefix.
    """
    return run_kubectl_logged(logger, ctx, "apply", "-f", "-", input_bytes=yaml_text.encode(), check=True)

def ensure_namespace(logger: logging.Logger, ctx: str, ns: str) -> None:
    """
    Ensure the namespace exists in the given context.
    """
    rns = subprocess.run(["kubectl", "--context", ctx, "get", "ns", ns], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if rns.returncode != 0:
        run_kubectl_logged(logger, ctx, "create", "ns", ns, check=True)

def ensure_service_account(ctx: str, ns: str, sa: str, retries: int = 20, delay: float = 0.5) -> None:
    """
    Ensure the ServiceAccount exists in the given namespace.
    """
    for _ in range(1, retries + 1):
        r = subprocess.run(["kubectl", "--context", ctx, "-n", ns, "get", "sa", sa], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if r.returncode == 0:
            break
        time.sleep(delay)

def ensure_priority_classes(logger: logging.Logger, ctx: str, num_priorities: int, *, prefix: str = "p", start: int = 1) -> None:
    """
    Ensure PriorityClasses {prefix}{i} for i in [start, start+num_priorities).
    If delete_extras=True, remove any of our prefixed PCs outside that set.
    """
    kubectl_apply_yaml(logger, ctx, "".join(yaml_priority_class(f"{prefix}{v}", v) for v in range(start, start + num_priorities)))

def wait_each(logger: logging.Logger, ctx: str, kind: str, name: str, ns: str, timeout_sec: int, mode: str) -> int:
    """
    Wait for a specific resource to reach a desired state.
    """
    if kind == "pod":
        return wait_pod(logger, ctx, name, ns, timeout_sec, mode)
    elif kind in ("replicaset", "rs"):
        return wait_rs_pods(logger, ctx, name, ns, timeout_sec, mode)
    else:
        raise Exception(f"unknown kind for wait_each: {kind}")

def wait_pod(logger: logging.Logger, ctx: str, name: str, ns: str, timeout_sec: int, mode: str = "ready") -> int:
    """
    Wait until the pod meets the condition.
    mode:
    - "exist": counts pods by presence (len of pod list)
    - "ready": counts Ready=True
    - "running": counts phase == Running
    Returns 1 if the pod met the condition within timeout, else 0.
    """
    deadline = time.time() + timeout_sec
    while time.time() < deadline:
        r = subprocess.run(["kubectl", "--context", ctx, "-n", ns, "get", "pod", name, "-o", "json"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if mode == "exist":
            if r.returncode == 0:
                return 1
            time.sleep(0.5)
            continue
        if r.returncode != 0:
            time.sleep(0.5)
            continue
        try:
            pod = json.loads(r.stdout)
            phase = (pod.get("status") or {}).get("phase") or ""
            conditions = pod.get("status", {}).get("conditions", []) or []
            ready = any(c.get("type")=="Ready" and c.get("status")=="True" for c in conditions)
            if mode == "running" and phase == "Running":
                return 1
            if mode == "ready" and ready:
                return 1
        except Exception:
            pass
        logger.info("waiting on pod to be %s", mode)
        time.sleep(0.5)
    logger.warning("timeout waiting for pod '%s' in ns '%s' to be %s", name, ns, mode)
    return 0

def get_rs_spec_replicas(ctx: str, ns: str, rs_name: str) -> Optional[int]:
    """
    Fetch .spec.replicas for a ReplicaSet, or None if not found.
    """
    r = subprocess.run(["kubectl", "--context", ctx, "-n", ns, "get", "replicaset", rs_name, "-o", "json"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    if r.returncode != 0:
        return None
    try:
        return int((json.loads(r.stdout).get("spec") or {}).get("replicas") or 0)
    except Exception:
        return None

def wait_rs_pods(logger: logging.Logger, ctx: str, rs_name: str, ns: str, timeout_sec: int, mode: str = "ready") -> int:
    """
    Wait until the RS has its desired number of pods meeting the condition.
    mode:
    - "exist": counts pods by presence (len of pod list)
    - "ready": counts Ready=True
    - "running": counts phase == Running
    Returns the number of pods that met the condition (possibly less than desired on timeout).
    """
    deadline = time.time() + timeout_sec
    last_count = 0
    while time.time() < deadline:
        desired = get_rs_spec_replicas(ctx, ns, rs_name)
        r = subprocess.run(["kubectl", "--context", ctx, "-n", ns, "get", "pods", "-l", f"app={rs_name}", "-o", "json"], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        if r.returncode != 0:
            time.sleep(0.5)
            continue
        try:
            podlist = json.loads(r.stdout).get("items", [])
            if mode == "exist":
                count = len(podlist)
            else:
                count = 0
                for p in podlist:
                    phase = (p.get("status") or {}).get("phase", "")
                    conditions = p.get("status", {}).get("conditions", []) or []
                    ready = any(c.get("type") == "Ready" and c.get("status") == "True" for c in conditions)
                    if mode == "running" and phase == "Running":
                        count += 1
                    elif mode == "ready" and ready:
                        count += 1
            last_count = count
            if desired is not None and count >= desired:
                return count
        except Exception:
            pass
        logger.info("waiting on rs=%s to be %s; desired=%s current=%s", rs_name, mode, desired, last_count)
        time.sleep(0.5)
    logger.warning("timeout waiting for RS '%s' in ns '%s' to have desired pods %s", rs_name, ns, mode)
    return last_count

def delete_pod(logger: logging.Logger, ctx: str, ns: str, name: str) -> None:
    """
    Delete a pod (ignore not-found).
    """
    logger.debug("deleting pod %s/%s", ns, name)
    run_kubectl_logged(logger, ctx, "-n", ns, "delete", "pod", name, "--ignore-not-found=true", check=False)

def delete_rs(logger: logging.Logger, ctx: str, ns: str, name: str) -> None:
    """
    Delete a ReplicaSet (ignore not-found).
    This also cleans up its managed pods.
    """
    logger.debug("deleting replicaset %s/%s", ns, name)
    run_kubectl_logged(
        logger,
        ctx,
        "-n", ns,
        "delete", "replicaset", name,
        "--ignore-not-found=true",
        check=False,
    )

def get_json_ctx(ctx: str, base_cmd: list[str]) -> dict:
    """
    Get the JSON output from a kubectl command.
    """
    cmd = ["kubectl"]
    if ctx:
        cmd += ["--context", ctx]
    cmd += base_cmd
    try:
        out = subprocess.check_output(cmd, stderr=subprocess.STDOUT)
    except subprocess.CalledProcessError as e:
        msg = (e.stdout or b"").decode("utf-8", "replace")
        tail = msg[-1200:]
        raise RuntimeError(
            f"kubectl failed: rc={e.returncode} cmd={' '.join(cmd)} output_tail={tail!r}"
        ) from e
    return json.loads(out)
