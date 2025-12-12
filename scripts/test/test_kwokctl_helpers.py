import logging
import os
import subprocess
import pytest

from scripts.helpers import kwokctl_helpers as kh
from scripts.test.test_utils import make_logger_stream, null_lock


# ---------------------------------------------------------------------------
# YAML helpers
# ---------------------------------------------------------------------------


def test_yaml_kwok_node_contains_expected_fields():
	y = kh.yaml_kwok_node("kwok-node-1", cpu="4", mem="8Gi", pods_cap=123)
	assert "kind: Node" in y
	assert "name: kwok-node-1" in y
	assert "kwok.x-k8s.io/node" in y
	assert "cpu: \"4\"" in y
	assert "memory: \"8Gi\"" in y
	assert "pods: 123" in y
	assert y.strip().endswith("---")


def test_yaml_kwok_rs_contains_expected_fields():
	y = kh.yaml_kwok_rs("ns1", "rs1", replicas=3, cpu="250m", mem="64Mi", pc="p10")
	assert "kind: ReplicaSet" in y
	assert "namespace: ns1" in y
	assert "name: rs1" in y
	assert "replicas: 3" in y
	assert "matchLabels:" in y
	assert "app: rs1" in y
	assert "priorityClassName: p10" in y
	assert "requests: {cpu: \"250m\", memory: \"64Mi\"}" in y
	assert y.strip().endswith("---")


def test_yaml_kwok_pod_contains_expected_fields():
	y = kh.yaml_kwok_pod("ns1", "pod1", cpu="100m", mem="32Mi", pc="p5")
	assert "kind: Pod" in y
	assert "namespace: ns1" in y
	assert "name: pod1" in y
	assert "priorityClassName: p5" in y
	assert "requests: {cpu: \"100m\", memory: \"32Mi\"}" in y
	assert y.strip().endswith("---")


# ---------------------------------------------------------------------------
# KWOK helpers
# ---------------------------------------------------------------------------


def test_run_kwokctl_logged_success_logs_output(monkeypatch):
	logger, stream = make_logger_stream("kwokctl-helpers")

	def fake_run(cmd, input=None, stdout=None, stderr=None, check=False):
		assert cmd[0] == "kwokctl"
		return subprocess.CompletedProcess(cmd, 0, stdout=b"line1\nline2\n")

	monkeypatch.setattr(kh.subprocess, "run", fake_run)
	r = kh.run_kwokctl_logged(logger, "get", "clusters")
	assert r.returncode == 0
	out = stream.getvalue()
	assert "INFO kwokctl> line1" in out
	assert "INFO kwokctl> line2" in out


def test_run_kwokctl_logged_raises_on_error_when_check_true(monkeypatch):
	logger, _ = make_logger_stream("kwokctl-helpers")

	def fake_run(cmd, input=None, stdout=None, stderr=None, check=False):
		return subprocess.CompletedProcess(cmd, 7, stdout=b"boom")

	monkeypatch.setattr(kh.subprocess, "run", fake_run)
	with pytest.raises(subprocess.CalledProcessError):
		kh.run_kwokctl_logged(logger, "get", "clusters", check=True)


def test_ensure_kwok_cluster_calls_delete_then_create_when_recreate_true(monkeypatch, tmp_path):
	logger, _ = make_logger_stream("kwokctl-helpers")
	calls: list[tuple[list[str], bool]] = []

	def fake_run(logger2, *args, input_bytes=None, check=True):
		calls.append((list(args), check))
		return subprocess.CompletedProcess(["kwokctl"], 0, stdout=b"")

	unlinked: list[str] = []
	real_unlink = os.unlink

	def fake_unlink(p: str):
		unlinked.append(str(p))
		try:
			real_unlink(p)
		except OSError:
			pass

	monkeypatch.setattr(kh, "kwok_cache_lock", null_lock)
	monkeypatch.setattr(kh, "run_kwokctl_logged", fake_run)
	monkeypatch.setattr(kh.os, "unlink", fake_unlink)

	kh.ensure_kwok_cluster(logger, "c1", kwok_runtime="docker", config_doc={"kind": "Cluster"}, recreate=True)

	# First call should be delete cluster with check=False
	assert calls[0][0][:3] == ["delete", "cluster", "--name"]
	assert calls[0][0][3] == "c1"
	assert calls[0][1] is False
	# Second call should be create cluster with config/runtime
	assert calls[1][0][:4] == ["create", "cluster", "--name", "c1"]
	assert "--config" in calls[1][0]
	assert "--runtime" in calls[1][0]
	assert "docker" in calls[1][0]
	# Temp config file should be cleaned up
	assert len(unlinked) == 1


def test_ensure_kwok_cluster_no_delete_when_recreate_false(monkeypatch):
	logger, _ = make_logger_stream("kwokctl-helpers")
	calls: list[list[str]] = []

	def fake_run(logger2, *args, input_bytes=None, check=True):
		calls.append(list(args))
		return subprocess.CompletedProcess(["kwokctl"], 0, stdout=b"")

	monkeypatch.setattr(kh, "kwok_cache_lock", null_lock)
	monkeypatch.setattr(kh, "run_kwokctl_logged", fake_run)
	monkeypatch.setattr(kh.os, "unlink", lambda p: None)

	kh.ensure_kwok_cluster(logger, "c1", kwok_runtime="docker", config_doc={}, recreate=False)
	assert len(calls) == 1
	assert calls[0][0] == "create"


def test_create_kwok_nodes_calls_kubectl_apply_yaml(monkeypatch):
	logger, _ = make_logger_stream("kwokctl-helpers")
	called = {}

	def fake_apply(logger2, ctx, yaml_text: str):
		called["ctx"] = ctx
		called["yaml"] = yaml_text
		return subprocess.CompletedProcess(["kubectl"], 0, stdout=b"")

	monkeypatch.setattr(kh, "kubectl_apply_yaml", fake_apply)
	kh.create_kwok_nodes(logger, "ctx1", num_nodes=2, node_cpu="2", node_mem="1Gi", pods_cap=50)
	assert called["ctx"] == "ctx1"
	assert "name: kwok-node-1" in called["yaml"]
	assert "name: kwok-node-2" in called["yaml"]


def test_kwok_pods_cap_defaults_and_scales():
	assert kh.kwok_pods_cap() == 512
	# 10*3=30 -> max(512,30)=512
	assert kh.kwok_pods_cap(10) == 512
	# 300*3=900
	assert kh.kwok_pods_cap(300) == 900


def test_kwok_cache_lock_locks_and_unlocks(monkeypatch, tmp_path):
	lock_file = tmp_path / "locks" / "kwok.lock"
	monkeypatch.setenv("KWOK_CACHE_LOCK", str(lock_file))

	calls: list[tuple[str, int]] = []

	def fake_open(path, flags, mode):
		assert str(path) == str(lock_file)
		return 123

	def fake_close(fd):
		assert fd == 123
		calls.append(("close", fd))

	def fake_flock(fd, op):
		calls.append(("flock", op))

	monkeypatch.setattr(kh.os, "open", fake_open)
	monkeypatch.setattr(kh.os, "close", fake_close)
	# On Windows, kh.fcntl may be None; then kwok_cache_lock should be best-effort.
	if kh.fcntl is not None:
		monkeypatch.setattr(kh.fcntl, "flock", fake_flock)

	with kh.kwok_cache_lock():
		pass

	# Ensure it took EX lock then UN lock (when available), and always closed the fd.
	if kh.fcntl is not None:
		assert ("flock", kh.fcntl.LOCK_EX) in calls
		assert ("flock", kh.fcntl.LOCK_UN) in calls
	assert ("close", 123) in calls


def test_merge_kwokctl_envs_merges_by_name_and_sorts_stable():
	base = {
		"componentsPatches": [
			{"name": "kube-scheduler", "extraEnvs": [{"name": "A", "value": "1"}]},
			{"name": "zzz", "extraEnvs": [{"name": "Z", "value": "9"}]},
		]
	}
	add = [{"name": "B", "value": "2"}, {"name": "A", "value": "override"}, {"nope": True}]

	merged = kh.merge_kwokctl_envs(base, add, component="kube-scheduler")
	# Must not mutate original
	assert base["componentsPatches"][0]["extraEnvs"][0]["value"] == "1"

	cps = merged["componentsPatches"]
	assert [c.get("name") for c in cps] == sorted([c.get("name") for c in cps])

	sched = next(c for c in cps if c.get("name") == "kube-scheduler")
	envs = sched["extraEnvs"]
	assert [e["name"] for e in envs] == ["A", "B"]
	assert next(e for e in envs if e["name"] == "A")["value"] == "override"


def test_merge_kwokctl_envs_creates_components_patch_when_missing():
	merged = kh.merge_kwokctl_envs({}, [{"name": "X", "value": "1"}], component="kube-scheduler")
	cps = merged["componentsPatches"]
	sched = next(c for c in cps if c.get("name") == "kube-scheduler")
	assert sched["extraEnvs"] == [{"name": "X", "value": "1"}]
