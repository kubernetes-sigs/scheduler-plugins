#!/usr/bin/env python3
# test_cluster_stats.py

import json
import time as _time

import pytest

from scripts.helpers import cluster_stats as cs
from scripts.test.test_utils import time_sequence


# ---------------------------------------------------------------------------
# sum_pod_requests
# ---------------------------------------------------------------------------


def test_sum_pod_requests_sums_containers_requests():
	pod = {
		"spec": {
			"containers": [
				{"resources": {"requests": {"cpu": "250m", "memory": "128Mi"}}},
				{"resources": {"requests": {"cpu": "0.5", "memory": "256Mi"}}},
			],
		}
	}
	cpu_m, mem_b = cs.sum_pod_requests(pod)
	assert cpu_m == 250 + 500
	assert mem_b == 128 * 1024**2 + 256 * 1024**2


def test_sum_pod_requests_adds_max_init_container_requests():
	pod = {
		"spec": {
			"containers": [
				{"resources": {"requests": {"cpu": "250m", "memory": "128Mi"}}},
			],
			"initContainers": [
				{"resources": {"requests": {"cpu": "1", "memory": "512Mi"}}},
				{"resources": {"requests": {"cpu": "500m", "memory": "1Gi"}}},
			],
		}
	}
	cpu_m, mem_b = cs.sum_pod_requests(pod)
	# initContainers contribute max(cpu), max(mem)
	assert cpu_m == 250 + 1000
	assert mem_b == (128 * 1024**2) + (1 * 1024**3)


# ---------------------------------------------------------------------------
# get_running_and_unscheduled
# ---------------------------------------------------------------------------


def test_get_running_and_unscheduled_all_running(monkeypatch):
	pods_obj = {
		"items": [
			{"metadata": {"name": "b"}, "spec": {"nodeName": "n2"}, "status": {"phase": "Running"}},
			{"metadata": {"name": "a"}, "spec": {"nodeName": "n1"}, "status": {"phase": "Running"}},
		]
	}

	monkeypatch.setattr(cs, "get_json_ctx", lambda ctx, cmd: pods_obj)
	status, running, unsched = cs.get_running_and_unscheduled("ctx", "ns", expected=2, timeout=1)
	assert status == "all_running"
	# Should be sorted by pod name
	assert running == [("a", "n1"), ("b", "n2")]
	assert unsched == []


def test_get_running_and_unscheduled_some_unschedulable(monkeypatch):
	pods_obj = {
		"items": [
			{"metadata": {"name": "a"}, "spec": {"nodeName": "n1"}, "status": {"phase": "Running"}},
			{"metadata": {"name": "b"}, "spec": {}, "status": {"phase": "Pending"}},
		]
	}

	monkeypatch.setattr(cs, "get_json_ctx", lambda ctx, cmd: pods_obj)
	status, running, unsched = cs.get_running_and_unscheduled("ctx", "ns", expected=2, timeout=1)
	assert status == "some_unschedulable"
	assert running == [("a", "n1")]
	assert unsched == ["b"]


def test_get_running_and_unscheduled_timeout(monkeypatch):
	pods_obj = {
		"items": [
			{"metadata": {"name": "a"}, "spec": {}, "status": {"phase": "Pending"}},
		]
	}

	# Make time advance deterministically: start=0, then immediate timeout.
	fake_time = time_sequence([0.0, 0.0, 999.0])

	monkeypatch.setattr(cs, "get_json_ctx", lambda ctx, cmd: pods_obj)
	monkeypatch.setattr(cs.time, "time", fake_time)
	monkeypatch.setattr(cs.time, "sleep", lambda _: None)

	status, running, unsched = cs.get_running_and_unscheduled("ctx", "ns", expected=2, timeout=0)
	assert status == "timeout"
	assert running == []
	assert unsched == ["a"]


# ---------------------------------------------------------------------------
# stat_snapshot
# ---------------------------------------------------------------------------


def test_stat_snapshot_computes_utilization_and_counts(monkeypatch):
	# Provide deterministic running/unscheduled via helper.
	monkeypatch.setattr(
		cs,
		"get_running_and_unscheduled",
		lambda ctx, ns, expected: ("all_running", [("p1", "n1"), ("p2", "n2")], ["p3"]),
	)

	nodes = {
		"items": [
			{"metadata": {"name": "n1"}, "status": {"allocatable": {"cpu": "2", "memory": "1Gi"}}},
			{"metadata": {"name": "n2"}, "status": {"allocatable": {"cpu": "1", "memory": "1Gi"}}},
		]
	}
	pods = {
		"items": [
			{
				"metadata": {"name": "p1"},
				"spec": {
					"nodeName": "n1",
					"priorityClassName": "p5",
					"containers": [
						{"resources": {"requests": {"cpu": "500m", "memory": "512Mi"}}},
					],
				},
				"status": {"phase": "Running"},
			},
			{
				"metadata": {"name": "p2"},
				"spec": {
					"nodeName": "n2",
					"priorityClassName": "",
					"containers": [
						{"resources": {"requests": {"cpu": "1", "memory": "1Gi"}}},
					],
				},
				"status": {"phase": "Running"},
			},
			{
				"metadata": {"name": "p3"},
				"spec": {"priorityClassName": "p10", "containers": []},
				"status": {"phase": "Pending"},
			},
		]
	}

	def fake_get_json_ctx(ctx, cmd):
		# cmd is a list like ["get","nodes","-o","json"] or ["-n", ns, "get", "pods", "-o", "json"]
		if cmd[:2] == ["get", "nodes"]:
			return nodes
		if "pods" in cmd:
			return pods
		raise AssertionError(f"unexpected cmd: {cmd}")

	monkeypatch.setattr(cs, "get_json_ctx", fake_get_json_ctx)

	snap = cs.stat_snapshot("ctx", "ns", expected=3)
	assert snap.cpu_alloc_by_node == {"n1": 2000, "n2": 1000}
	assert snap.mem_alloc_by_node == {"n1": 1024**3, "n2": 1024**3}

	assert snap.cpu_req_by_node == {"n1": 500, "n2": 1000}
	assert snap.mem_req_by_node == {"n1": 512 * 1024**2, "n2": 1024**3}

	assert snap.pods_run_by_node == {"n1": 1, "n2": 1}
	assert snap.running_placed_by_prio == {"p5": 1, "": 1}
	assert snap.unschedulable_by_prio == {"p10": 1}

	assert snap.pods_running == [("p1", "n1"), ("p2", "n2")]
	assert snap.pods_unscheduled == ["p3"]

	assert snap.cpu_run_util == pytest.approx((500 + 1000) / (2000 + 1000))
	assert snap.mem_run_util == pytest.approx(((512 * 1024**2) + (1024**3)) / (2 * 1024**3))
