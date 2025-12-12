import io
import json
import logging
import subprocess

import pytest

from scripts.helpers import kubectl_helpers as kh


def _make_logger_stream(level: int = logging.DEBUG):
	stream = io.StringIO()
	logger = logging.getLogger(f"test-kubectl-helpers-{id(stream)}")
	logger.handlers.clear()
	logger.setLevel(level)
	logger.propagate = False
	h = logging.StreamHandler(stream)
	h.setFormatter(logging.Formatter("%(levelname)s %(message)s"))
	logger.addHandler(h)
	return logger, stream


# ---------------------------------------------------------------------------
# YAML helpers
# ---------------------------------------------------------------------------


def test_yaml_priority_class_contains_expected_fields():
	y = kh.yaml_priority_class("p10", 10)
	assert "apiVersion: scheduling.k8s.io/v1" in y
	assert "kind: PriorityClass" in y
	assert "name: p10" in y
	assert "value: 10" in y
	assert "preemptionPolicy: PreemptLowerPriority" in y
	assert y.strip().endswith("---")


# ---------------------------------------------------------------------------
# kubectl execution helpers
# ---------------------------------------------------------------------------


def test_run_kubectl_logged_success_logs_output(monkeypatch):
	logger, stream = _make_logger_stream()

	def fake_run(cmd, input=None, stdout=None, stderr=None, check=False):
		assert cmd[:3] == ["kubectl", "--context", "ctx1"]
		return subprocess.CompletedProcess(cmd, 0, stdout=b"line1\nline2\n")

	monkeypatch.setattr(kh.subprocess, "run", fake_run)

	r = kh.run_kubectl_logged(logger, "ctx1", "get", "pods")
	assert r.returncode == 0
	out = stream.getvalue()
	assert "INFO kubectl> line1" in out
	assert "INFO kubectl> line2" in out


def test_run_kubectl_logged_raises_on_error_when_check_true(monkeypatch):
	logger, _ = _make_logger_stream()

	def fake_run(cmd, input=None, stdout=None, stderr=None, check=False):
		return subprocess.CompletedProcess(cmd, 1, stdout=b"boom")

	monkeypatch.setattr(kh.subprocess, "run", fake_run)

	with pytest.raises(subprocess.CalledProcessError):
		kh.run_kubectl_logged(logger, "ctx1", "get", "pods", check=True)


def test_kubectl_apply_yaml_calls_run_kubectl_logged(monkeypatch):
	logger, _ = _make_logger_stream()
	called = {}

	def fake_run(logger2, ctx, *args, input_bytes=None, check=True):
		called["ctx"] = ctx
		called["args"] = list(args)
		called["input"] = input_bytes
		called["check"] = check
		return subprocess.CompletedProcess(["kubectl"], 0, stdout=b"")

	monkeypatch.setattr(kh, "run_kubectl_logged", fake_run)

	kh.kubectl_apply_yaml(logger, "ctx1", "apiVersion: v1\n")
	assert called["ctx"] == "ctx1"
	assert called["args"] == ["apply", "-f", "-"]
	assert called["input"] == b"apiVersion: v1\n"
	assert called["check"] is True


# ---------------------------------------------------------------------------
# Ensure helpers
# ---------------------------------------------------------------------------


def test_ensure_namespace_creates_when_missing(monkeypatch):
	logger, _ = _make_logger_stream()
	calls = {"create": 0}

	def fake_run(cmd, stdout=None, stderr=None):
		# kubectl get ns <ns>
		assert cmd[:4] == ["kubectl", "--context", "ctx1", "get"]
		return subprocess.CompletedProcess(cmd, 1, stdout=b"")

	def fake_run_logged(logger2, ctx, *args, **kwargs):
		assert ctx == "ctx1"
		assert list(args) == ["create", "ns", "ns1"]
		calls["create"] += 1
		return subprocess.CompletedProcess(["kubectl"], 0, stdout=b"")

	monkeypatch.setattr(kh.subprocess, "run", fake_run)
	monkeypatch.setattr(kh, "run_kubectl_logged", fake_run_logged)

	kh.ensure_namespace(logger, "ctx1", "ns1")
	assert calls["create"] == 1


def test_ensure_namespace_noop_when_present(monkeypatch):
	logger, _ = _make_logger_stream()

	def fake_run(cmd, stdout=None, stderr=None):
		return subprocess.CompletedProcess(cmd, 0, stdout=b"")

	def fake_run_logged(*args, **kwargs):
		raise AssertionError("should not create namespace when it exists")

	monkeypatch.setattr(kh.subprocess, "run", fake_run)
	monkeypatch.setattr(kh, "run_kubectl_logged", fake_run_logged)
	kh.ensure_namespace(logger, "ctx1", "ns1")


def test_ensure_service_account_retries_until_found(monkeypatch):
	# Return nonzero twice then success.
	seq = [1, 1, 0]
	sleeps = []

	def fake_run(cmd, stdout=None, stderr=None):
		rc = seq.pop(0)
		return subprocess.CompletedProcess(cmd, rc, stdout=b"")

	def fake_sleep(d):
		sleeps.append(d)

	monkeypatch.setattr(kh.subprocess, "run", fake_run)
	monkeypatch.setattr(kh.time, "sleep", fake_sleep)

	kh.ensure_service_account("ctx1", "ns1", "sa1", retries=5, delay=0.01)
	# slept for the two failures
	assert sleeps == [0.01, 0.01]


def test_ensure_service_account_gives_up_after_retries(monkeypatch):
	sleeps = []

	def fake_run(cmd, stdout=None, stderr=None):
		return subprocess.CompletedProcess(cmd, 1, stdout=b"")

	def fake_sleep(d):
		sleeps.append(d)

	monkeypatch.setattr(kh.subprocess, "run", fake_run)
	monkeypatch.setattr(kh.time, "sleep", fake_sleep)

	kh.ensure_service_account("ctx1", "ns1", "sa1", retries=3, delay=0.01)
	assert sleeps == [0.01, 0.01, 0.01]


def test_ensure_priority_classes_applies_generated_yaml(monkeypatch):
	logger, _ = _make_logger_stream()
	called = {}

	def fake_apply(logger2, ctx, yaml_text: str):
		called["ctx"] = ctx
		called["yaml"] = yaml_text
		return subprocess.CompletedProcess(["kubectl"], 0, stdout=b"")

	monkeypatch.setattr(kh, "kubectl_apply_yaml", fake_apply)

	kh.ensure_priority_classes(logger, "ctx1", 3, prefix="p", start=5)
	assert called["ctx"] == "ctx1"
	y = called["yaml"]
	assert "name: p5" in y and "value: 5" in y
	assert "name: p6" in y and "value: 6" in y
	assert "name: p7" in y and "value: 7" in y


# ---------------------------------------------------------------------------
# Wait helpers
# ---------------------------------------------------------------------------


def test_wait_each_dispatches(monkeypatch):
	logger, _ = _make_logger_stream()

	monkeypatch.setattr(kh, "wait_pod", lambda *a, **k: 123)
	monkeypatch.setattr(kh, "wait_rs_pods", lambda *a, **k: 456)

	assert kh.wait_each(logger, "ctx1", "pod", "p", "ns", 1, "ready") == 123
	assert kh.wait_each(logger, "ctx1", "replicaset", "rs", "ns", 1, "ready") == 456
	assert kh.wait_each(logger, "ctx1", "rs", "rs", "ns", 1, "ready") == 456

	with pytest.raises(Exception):
		kh.wait_each(logger, "ctx1", "job", "j", "ns", 1, "ready")


def test_wait_pod_exist_success(monkeypatch):
	logger, _ = _make_logger_stream()

	def fake_run(cmd, stdout=None, stderr=None):
		return subprocess.CompletedProcess(cmd, 0, stdout=b"{}")

	monkeypatch.setattr(kh.subprocess, "run", fake_run)
	assert kh.wait_pod(logger, "ctx1", "p1", "ns1", timeout_sec=1, mode="exist") == 1


def test_wait_pod_ready_success(monkeypatch):
	logger, _ = _make_logger_stream()

	pod = {
		"status": {
			"phase": "Running",
			"conditions": [{"type": "Ready", "status": "True"}],
		}
	}

	def fake_run(cmd, stdout=None, stderr=None):
		return subprocess.CompletedProcess(cmd, 0, stdout=json.dumps(pod).encode())

	monkeypatch.setattr(kh.subprocess, "run", fake_run)
	assert kh.wait_pod(logger, "ctx1", "p1", "ns1", timeout_sec=1, mode="ready") == 1


def test_wait_pod_running_success(monkeypatch):
	logger, _ = _make_logger_stream()

	pod = {"status": {"phase": "Running", "conditions": []}}

	def fake_run(cmd, stdout=None, stderr=None):
		return subprocess.CompletedProcess(cmd, 0, stdout=json.dumps(pod).encode())

	monkeypatch.setattr(kh.subprocess, "run", fake_run)
	assert kh.wait_pod(logger, "ctx1", "p1", "ns1", timeout_sec=1, mode="running") == 1


def test_wait_pod_timeout(monkeypatch):
	logger, stream = _make_logger_stream()
	times = [0.0, 0.1, 0.2, 999.0]

	def fake_time():
		return times.pop(0)

	def fake_sleep(_d):
		return None

	def fake_run(cmd, stdout=None, stderr=None):
		# Always not found
		return subprocess.CompletedProcess(cmd, 1, stdout=b"")

	monkeypatch.setattr(kh.time, "time", fake_time)
	monkeypatch.setattr(kh.time, "sleep", fake_sleep)
	monkeypatch.setattr(kh.subprocess, "run", fake_run)

	assert kh.wait_pod(logger, "ctx1", "p1", "ns1", timeout_sec=0, mode="exist") == 0
	assert "timeout waiting for pod" in stream.getvalue().lower()


def test_get_rs_spec_replicas_parses_or_none(monkeypatch):
	def fake_run_missing(cmd, stdout=None, stderr=None):
		return subprocess.CompletedProcess(cmd, 1, stdout=b"")

	monkeypatch.setattr(kh.subprocess, "run", fake_run_missing)
	assert kh.get_rs_spec_replicas("ctx1", "ns1", "rs1") is None

	def fake_run_good(cmd, stdout=None, stderr=None):
		doc = {"spec": {"replicas": 3}}
		return subprocess.CompletedProcess(cmd, 0, stdout=json.dumps(doc).encode())

	monkeypatch.setattr(kh.subprocess, "run", fake_run_good)
	assert kh.get_rs_spec_replicas("ctx1", "ns1", "rs1") == 3

	def fake_run_bad_json(cmd, stdout=None, stderr=None):
		return subprocess.CompletedProcess(cmd, 0, stdout=b"{")

	monkeypatch.setattr(kh.subprocess, "run", fake_run_bad_json)
	assert kh.get_rs_spec_replicas("ctx1", "ns1", "rs1") is None


def test_wait_rs_pods_exist_reaches_desired(monkeypatch):
	logger, _ = _make_logger_stream()

	monkeypatch.setattr(kh, "get_rs_spec_replicas", lambda ctx, ns, rs: 2)

	def fake_run(cmd, stdout=None, stderr=None):
		doc = {"items": [{}, {}]}
		return subprocess.CompletedProcess(cmd, 0, stdout=json.dumps(doc).encode())

	monkeypatch.setattr(kh.subprocess, "run", fake_run)
	assert kh.wait_rs_pods(logger, "ctx1", "rs1", "ns1", timeout_sec=1, mode="exist") == 2


def test_wait_rs_pods_ready_reaches_desired(monkeypatch):
	logger, _ = _make_logger_stream()

	monkeypatch.setattr(kh, "get_rs_spec_replicas", lambda ctx, ns, rs: 1)

	pod = {"status": {"phase": "Running", "conditions": [{"type": "Ready", "status": "True"}]}}

	def fake_run(cmd, stdout=None, stderr=None):
		doc = {"items": [pod]}
		return subprocess.CompletedProcess(cmd, 0, stdout=json.dumps(doc).encode())

	monkeypatch.setattr(kh.subprocess, "run", fake_run)
	assert kh.wait_rs_pods(logger, "ctx1", "rs1", "ns1", timeout_sec=1, mode="ready") == 1


def test_wait_rs_pods_timeout_returns_last_count(monkeypatch):
	logger, _ = _make_logger_stream()
	times = [0.0, 0.1, 999.0]

	def fake_time():
		return times.pop(0)

	def fake_sleep(_d):
		return None

	monkeypatch.setattr(kh.time, "time", fake_time)
	monkeypatch.setattr(kh.time, "sleep", fake_sleep)
	monkeypatch.setattr(kh, "get_rs_spec_replicas", lambda ctx, ns, rs: 2)

	def fake_run(cmd, stdout=None, stderr=None):
		# invalid json -> stays in last_count=0
		return subprocess.CompletedProcess(cmd, 0, stdout=b"{")

	monkeypatch.setattr(kh.subprocess, "run", fake_run)
	assert kh.wait_rs_pods(logger, "ctx1", "rs1", "ns1", timeout_sec=0, mode="exist") == 0


# ---------------------------------------------------------------------------
# Delete helpers
# ---------------------------------------------------------------------------


def test_delete_pod_uses_ignore_not_found(monkeypatch):
	logger, _ = _make_logger_stream()
	called = {}

	def fake_run(logger2, ctx, *args, input_bytes=None, check=True):
		called["ctx"] = ctx
		called["args"] = list(args)
		called["check"] = check
		return subprocess.CompletedProcess(["kubectl"], 0, stdout=b"")

	monkeypatch.setattr(kh, "run_kubectl_logged", fake_run)
	kh.delete_pod(logger, "ctx1", "ns1", "p1")
	assert called["ctx"] == "ctx1"
	assert called["args"] == ["-n", "ns1", "delete", "pod", "p1", "--ignore-not-found=true"]
	assert called["check"] is False


def test_delete_rs_uses_ignore_not_found(monkeypatch):
	logger, _ = _make_logger_stream()
	called = {}

	def fake_run(logger2, ctx, *args, input_bytes=None, check=True):
		called["ctx"] = ctx
		called["args"] = list(args)
		called["check"] = check
		return subprocess.CompletedProcess(["kubectl"], 0, stdout=b"")

	monkeypatch.setattr(kh, "run_kubectl_logged", fake_run)
	kh.delete_rs(logger, "ctx1", "ns1", "rs1")
	assert called["ctx"] == "ctx1"
	assert called["args"] == [
		"-n",
		"ns1",
		"delete",
		"replicaset",
		"rs1",
		"--ignore-not-found=true",
	]
	assert called["check"] is False


# ---------------------------------------------------------------------------
# JSON helpers
# ---------------------------------------------------------------------------


def test_get_json_ctx_success_includes_context_when_provided(monkeypatch):
	def fake_check_output(cmd, stderr=None):
		assert cmd[:3] == ["kubectl", "--context", "ctx1"]
		return b"{\"a\": 1}"

	monkeypatch.setattr(kh.subprocess, "check_output", fake_check_output)
	assert kh.get_json_ctx("ctx1", ["get", "pods", "-o", "json"]) == {"a": 1}


def test_get_json_ctx_success_without_context(monkeypatch):
	def fake_check_output(cmd, stderr=None):
		assert cmd[0] == "kubectl"
		assert "--context" not in cmd
		return b"{\"ok\": true}"

	monkeypatch.setattr(kh.subprocess, "check_output", fake_check_output)
	assert kh.get_json_ctx("", ["version", "-o", "json"]) == {"ok": True}


def test_get_json_ctx_raises_runtime_error_with_output_tail(monkeypatch):
	long_out = ("x" * 3000).encode("utf-8")

	def fake_check_output(cmd, stderr=None):
		raise subprocess.CalledProcessError(returncode=7, cmd=cmd, output=long_out)

	monkeypatch.setattr(kh.subprocess, "check_output", fake_check_output)

	with pytest.raises(RuntimeError) as e:
		kh.get_json_ctx("ctx1", ["get", "pods", "-o", "json"])

	msg = str(e.value)
	assert "kubectl failed" in msg
	assert "rc=7" in msg
	assert "output_tail=" in msg
