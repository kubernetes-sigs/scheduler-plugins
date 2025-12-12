import argparse
import builtins
import csv
import json
import random
from pathlib import Path

import pytest

from scripts.kwok_workload_once import test_runner as tr



# ---------------------------------------------------------------------------
# build_argparser
# ---------------------------------------------------------------------------


def test_build_argparser_parses_basic_args():
	ap = tr.build_argparser()
	args = ap.parse_args([
		"--workload-config-file", "wl.yaml",
		"--kwokctl-config-file", "kwokctl.yaml",
		"--seed", "1",
	])
	assert args.workload_config_file == "wl.yaml"
	assert args.kwokctl_config_file == "kwokctl.yaml"
	assert args.seed == 1


# ---------------------------------------------------------------------------
# TestRunner.ensure_default_args
# ---------------------------------------------------------------------------


def test_ensure_default_args_sets_defaults_and_validates_paths(tmp_path):
	wl = tmp_path / "wl.yaml"
	kw = tmp_path / "kwokctl.yaml"
	wl.write_text("namespace: ns\n", encoding="utf-8")
	kw.write_text("componentsPatches: []\n", encoding="utf-8")

	args = argparse.Namespace(
		workload_config_file=str(wl),
		kwokctl_config_file=str(kw),
		seed=1,
		seed_file=None,
		count=None,
		seeds_not_all_running=0,
		# leave the rest unset on purpose
	)

	out = tr.TestRunner.ensure_default_args(args)
	assert out.cluster_name == "kwok1"
	assert out.kwok_runtime == "binary"
	assert out.output_dir == "./output"
	assert out.clean_start is False
	assert out.re_run_seeds is False
	assert out.pause is False
	assert out.log_level == "INFO"
	assert out.default_scheduler is False
	assert out.repeats == 1
	assert out.solver_timeout_ms == 10000


def test_ensure_default_args_rejects_incompatible_seed_args(tmp_path):
	wl = tmp_path / "wl.yaml"
	kw = tmp_path / "kwokctl.yaml"
	wl.write_text("namespace: ns\n", encoding="utf-8")
	kw.write_text("componentsPatches: []\n", encoding="utf-8")

	args = argparse.Namespace(
		workload_config_file=str(wl),
		kwokctl_config_file=str(kw),
		seed=1,
		seed_file=None,
		count=5,
		seeds_not_all_running=0,
	)
	with pytest.raises(SystemExit):
		tr.TestRunner.ensure_default_args(args)


def test_ensure_default_args_defaults_count_from_seeds_not_all_running(tmp_path):
	wl = tmp_path / "wl.yaml"
	kw = tmp_path / "kwokctl.yaml"
	wl.write_text("namespace: ns\n", encoding="utf-8")
	kw.write_text("componentsPatches: []\n", encoding="utf-8")

	args = argparse.Namespace(
		workload_config_file=str(wl),
		kwokctl_config_file=str(kw),
		seed=None,
		seed_file=None,
		count=None,
		seeds_not_all_running=3,
	)
	out = tr.TestRunner.ensure_default_args(args)
	assert out.count == 3


# ---------------------------------------------------------------------------
# Seed helpers: _read_seeds_file
# ---------------------------------------------------------------------------


def test_read_seeds_file_parses_dedupes_and_skips_invalid(tmp_path):
	p = tmp_path / "seeds.txt"
	p.write_text(
		"""
		# comment
		1
		2
		2
		nope
		0
		-5
		3
		""",
		encoding="utf-8",
	)
	seeds = tr.TestRunner._read_seeds_file(p)
	assert seeds == [1, 2, 3]


def test_read_seeds_file_missing_raises_value_error(tmp_path):
	with pytest.raises(ValueError):
		tr.TestRunner._read_seeds_file(tmp_path / "missing.txt")


# ---------------------------------------------------------------------------
# Config helpers: _parse_config_doc, _validate_workload_config
# ---------------------------------------------------------------------------


def test_parse_config_doc_applies_override_first():
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	base = {
		"namespace": "ns1",
		"num_nodes": 2,
		"num_pods": 5,
		"util": 0.5,
		"wait_pod_mode": "none",
		"num_priorities": [1, 3],
		"cpu_per_pod": ["100m", "200m"],
		"mem_per_pod": ["128Mi", "256Mi"],
		"num_replicas_per_rs": [1, 3],
	}
	override = {"namespace": "ns2", "num_nodes": 3, "wait_pod_mode": "ready"}
	cfg = runner._parse_config_doc(base, override=override)
	assert cfg.namespace == "ns2"
	assert cfg.num_nodes == 3
	assert cfg.wait_pod_mode == "ready"


def test_validate_workload_config_reports_multiple_errors():
	bad = tr.TestConfigRaw(
		namespace="",
		num_nodes=0,
		num_pods=0,
		num_priorities=None,
		num_replicas_per_rs=None,
		cpu_per_pod=None,
		mem_per_pod=None,
		util=0.0,
	)
	ok, msg = tr.TestRunner._validate_workload_config(bad)
	assert ok is False
	assert "namespace" in msg
	assert "num_nodes" in msg
	assert "num_pods" in msg
	assert "util" in msg


def test_validate_workload_config_ok():
	good = tr.TestConfigRaw(
		namespace="ns",
		num_nodes=2,
		num_pods=5,
		num_priorities=(1, 2),
		num_replicas_per_rs=(1, 3),
		cpu_per_pod=("100m", "200m"),
		mem_per_pod=("128Mi", "256Mi"),
		util=0.5,
	)
	ok, msg = tr.TestRunner._validate_workload_config(good)
	assert ok is True
	assert msg == ""


# ---------------------------------------------------------------------------
# Logging helpers: _write_info_file, _combined_job_configs_seed_str
# ---------------------------------------------------------------------------


def test_write_info_file_calls_write_info_file(monkeypatch, tmp_path):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.output_dir_resolved = tmp_path
	runner.args = argparse.Namespace(
		job_file="job.yaml",
		workload_config_file="wl.yaml",
		kwokctl_config_file="kwokctl.yaml",
		seed_file=None,
	)
	runner.job_doc = {"a": 1}
	runner.workload_config_doc = {"b": 2}
	runner.kwokctl_config_doc = {"c": 3}

	seen = {}

	def fake_write_info_file(out_path, meta_extra, inputs, logger):
		seen["out_path"] = out_path
		seen["meta_extra"] = meta_extra
		seen["inputs"] = inputs
		seen["logger"] = logger

	monkeypatch.setattr(tr, "write_info_file", fake_write_info_file)
	monkeypatch.setattr(tr, "build_cli_cmd", lambda: "CMD")

	runner._write_info_file()
	assert Path(seen["out_path"]).name == "info.yaml"
	assert seen["meta_extra"]["job_file"] == "job.yaml"
	assert seen["inputs"]["cli-cmd"] == "CMD"
	assert seen["inputs"]["job"] == {"a": 1}


def test_combined_job_configs_seed_str_formats_optional_fields():
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(
		job_file=None,
		workload_config_file="wl.yaml",
		kwokctl_config_file="kwokctl.yaml",
		seed_file=None,
	)
	s = runner._combined_job_configs_seed_str()
	assert "workload_config_file=wl.yaml" in s
	assert "kwokctl_config_file=kwokctl.yaml" in s
	assert "job_file=" not in s

	runner.args.job_file = "job.yaml"
	runner.args.seed_file = "seeds.txt"
	s2 = runner._combined_job_configs_seed_str()
	assert "job_file=job.yaml" in s2
	assert "seed_file=seeds.txt" in s2


# ---------------------------------------------------------------------------
# Parsing helpers: _parse_waits, _get_wait_pod_mode_from_dict
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
	"raw,expected",
	[
		(None, None),
		("", None),
		("none", None),
		("exist", "exist"),
		("ready", "ready"),
		("running", "running"),
	],
)
def test_get_wait_pod_mode_from_dict(raw, expected):
	doc = {"wait_pod_mode": raw}
	if expected is None:
		assert tr.TestRunner._get_wait_pod_mode_from_dict(doc, "wait_pod_mode", None) is None
	else:
		assert tr.TestRunner._get_wait_pod_mode_from_dict(doc, "wait_pod_mode", None) == expected


def test_get_wait_pod_mode_from_dict_invalid_raises():
	with pytest.raises(ValueError):
		tr.TestRunner._get_wait_pod_mode_from_dict({"wait_pod_mode": "bogus"}, "wait_pod_mode", None)


def test_parse_waits_defaults_and_none_mode():
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	raw = tr.TestConfigRaw(
		wait_pod_mode="none",
		wait_pod_timeout=None,
		settle_timeout_min=None,
		settle_timeout_max=None,
	)
	mode, pod_to, settle_min, settle_max = runner._parse_waits(raw)
	assert mode is None
	assert pod_to == 3
	assert settle_min == 2
	assert settle_max == 0


# ---------------------------------------------------------------------------
# Logging helpers: _record_failure
# ---------------------------------------------------------------------------


def test_record_failure_suppressed_stores_failure_object(tmp_path):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.suppress_fail_log = True
	runner.failure = None
	runner.failed_f = tmp_path / "failed.tsv"

	runner._record_failure("cat", 7, "phase", "msg", details="d")
	assert runner.failure is not None
	assert runner.failure.category == "cat"
	assert runner.failure.seed == 7
	assert runner.failure.phase == "phase"
	assert runner.failure.message == "msg"
	assert runner.failure.details == "d"
	assert not runner.failed_f.exists()

# ---------------------------------------------------------------------------
# ETA helpers
# ---------------------------------------------------------------------------


def test_eta_record_seed_duration_appends_non_negative(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.seed_durations = []

	monkeypatch.setattr(tr.time, "time", lambda: 100.0)
	runner._eta_record_seed_duration(started_at=90.0)
	assert runner.seed_durations == [10.0]


def test_eta_estimation_returns_none_when_unknown_or_no_samples():
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.seed_durations = []
	assert runner._eta_estimation(1, 10) is None

	runner.seed_durations = [1.0]
	assert runner._eta_estimation(1, -1) is None
	assert runner._eta_estimation(1, 0) is None


def test_eta_estimation_returns_epoch(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.seed_durations = [2.0, 4.0]  # avg = 3s
	monkeypatch.setattr(tr.time, "time", lambda: 100.0)
	# seed_idx=3 => seeds_done=2 => left=8 (for total=10) => eta=100+8*3
	assert runner._eta_estimation(3, 10) == 124.0

# ---------------------------------------------------------------------------
# Job file helpers: _merge_doc, merge_job_fields_into_args, _get_kwokctl_envs
# ---------------------------------------------------------------------------


def test_merge_doc_shallow_overwrite():
	dst = {"a": 1, "b": 2}
	src = {"b": 3, "c": 4}
	out = tr.TestRunner._merge_doc(dst, src)
	assert out is dst
	assert dst == {"a": 1, "b": 3, "c": 4}


def test_merge_job_fields_into_args_only_fills_missing():
	args = argparse.Namespace(
		cluster_name="cli",
		kwok_runtime=None,
		workload_config_file=None,
		kwokctl_config_file=None,
		output_dir=None,
		clean_start=None,
		re_run_seeds=None,
		log_level=None,
		default_scheduler=None,
		seed=None,
		seed_file=None,
		count=None,
		repeats=None,
		seeds_not_all_running=None,
		save_solver_stats=None,
		save_scheduler_logs=None,
		solver_trigger=None,
	)
	job = {
		"cluster-name": "job",
		"kwok-runtime": "docker",
		"workload-config-file": "w.yaml",
		"kwokctl-config-file": "k.yaml",
		"output-dir": "./out",
		"clean-start": True,
		"re-run-seeds": True,
		"log-level": "DEBUG",
		"default-scheduler": False,
		"seed": 7,
		"count": 9,
		"repeats": 2,
		"seeds-not-all-running": 1,
		"save-solver-stats": True,
		"save-scheduler-logs": True,
		"solver-trigger": True,
		"override-workload-config": {"namespace": "ns"},
		"override-kwokctl-envs": [{"name": "X", "value": "1"}],
	}
	merged, overrides = tr.TestRunner.merge_job_fields_into_args(args, job)
	# CLI already set -> preserved
	assert merged.cluster_name == "cli"
	# Missing -> filled
	assert merged.kwok_runtime == "docker"
	assert merged.workload_config_file == "w.yaml"
	assert merged.kwokctl_config_file == "k.yaml"
	assert merged.output_dir == "./out"
	assert merged.seed == 7
	# count should not be filled because seed already filled (priority in ensure_default_args),
	# but merge_job_fields_into_args itself can set it when None.
	assert merged.count == 9
	assert overrides["workload_config"] == {"namespace": "ns"}
	assert overrides["kwokctl_envs"] == [{"name": "X", "value": "1"}]


def test_get_kwokctl_envs_extracts_and_sorts():
	doc = {
		"componentsPatches": [
			{
				"name": "kube-scheduler",
				"extraEnvs": [
					{"name": "B", "value": "2"},
					{"name": "A", "value": "1"},
					{"name": "EMPTY", "value": ""},
					{"name": "NOVAL"},
				],
			}
		]
	}
	envs = tr.TestRunner._get_kwokctl_envs(doc, component="kube-scheduler")
	assert envs == {"A": "1", "B": "2", "EMPTY": None, "NOVAL": None}


# ---------------------------------------------------------------------------
# Seed outcome tracking: _record_seed_outcome
# ---------------------------------------------------------------------------


def test_record_seed_outcome_appends_to_correct_file(tmp_path):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.seeds_all_running_f = tmp_path / "all.txt"
	runner.seeds_not_all_running_f = tmp_path / "notall.txt"

	runner._record_seed_outcome(seed=11, all_running=True)
	runner._record_seed_outcome(seed=12, all_running=False)
	assert runner.seeds_all_running_f.read_text(encoding="utf-8") == "11\n"
	assert runner.seeds_not_all_running_f.read_text(encoding="utf-8") == "12\n"


# ---------------------------------------------------------------------------
# CSV/helpers: _prepare_output_dir
# ---------------------------------------------------------------------------


def test_prepare_output_dir_clean_start_recreates(tmp_path):
	out_dir = tmp_path / "out"
	out_dir.mkdir()
	(out_dir / "old.txt").write_text("x", encoding="utf-8")

	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(output_dir=str(out_dir), clean_start=True)
	resolved = runner._prepare_output_dir()
	assert resolved.exists()
	assert resolved.is_dir()
	assert not (resolved / "old.txt").exists()


def test_prepare_output_dir_no_clean_start_keeps_existing(tmp_path):
	out_dir = tmp_path / "out"
	out_dir.mkdir()
	(out_dir / "keep.txt").write_text("x", encoding="utf-8")

	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(output_dir=str(out_dir), clean_start=False)
	resolved = runner._prepare_output_dir()
	assert (resolved / "keep.txt").exists()


# ---------------------------------------------------------------------------
# CSV/helpers: _append_result_csv, _purge_mismatched_results_csv, _load_seen_results_csv
# ---------------------------------------------------------------------------


def test_purge_mismatched_results_csv_deletes_when_clean_start(tmp_path):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(clean_start=True)
	p = tmp_path / "results.csv"
	p.write_text("a,b\n1,2\n", encoding="utf-8")

	deleted = runner._purge_mismatched_results_csv(p, expected_header=["x", "y"])
	assert deleted is True
	assert not p.exists()


def test_purge_mismatched_results_csv_keeps_when_not_clean_start(tmp_path):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(clean_start=False)
	p = tmp_path / "results.csv"
	p.write_text("a,b\n1,2\n", encoding="utf-8")

	deleted = runner._purge_mismatched_results_csv(p, expected_header=["x", "y"])
	assert deleted is False
	assert p.exists()


def test_append_result_csv_rerun_seeds_removes_old_rows(tmp_path):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(clean_start=False, re_run_seeds=True)
	runner.results_f = tmp_path / "results.csv"

	# Precreate with correct header and two rows, one matching seed.
	with open(runner.results_f, "w", encoding="utf-8", newline="") as fh:
		w = csv.DictWriter(fh, fieldnames=tr.RESULTS_HEADER)
		w.writeheader()
		w.writerow({"timestamp": "t1", "seed": "5"})
		w.writerow({"timestamp": "t2", "seed": "6"})

	runner._append_result_csv({"timestamp": "t3", "seed": "5"})
	with open(runner.results_f, "r", encoding="utf-8", newline="") as fh:
		rows = list(csv.DictReader(fh))
	seeds = [r.get("seed") for r in rows]
	assert seeds.count("5") == 1
	assert "6" in seeds


def test_load_seen_results_csv_parses_seed_set(tmp_path):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.results_f = tmp_path / "results.csv"
	with open(runner.results_f, "w", encoding="utf-8", newline="") as fh:
		w = csv.DictWriter(fh, fieldnames=tr.RESULTS_HEADER)
		w.writeheader()
		w.writerow({"timestamp": "t", "seed": "1"})
		w.writerow({"timestamp": "t", "seed": "2"})

	assert runner._load_seen_results_csv() == {1, 2}


# ---------------------------------------------------------------------------
# Solver attempt helpers: _extract_best_attempt_fields, _get_solver_attempts
# ---------------------------------------------------------------------------


def test_extract_best_attempt_fields_empty_inputs():
	score, dur, status = tr.TestRunner._extract_best_attempt_fields("", [])
	assert score is None and dur is None and status == ""


def test_extract_best_attempt_fields_extracts_values():
	attempts = [
		{"name": "a", "score": {"x": 1}, "duration_ms": "12", "status": "OK"},
		{"name": "b", "score": {"y": 2}, "duration_ms": 7, "status": "FAIL"},
	]
	score, dur, status = tr.TestRunner._extract_best_attempt_fields("b", attempts)
	assert score == json.dumps({"y": 2}, separators=(",", ":"), sort_keys=True)
	assert dur == 7
	assert status == "FAIL"


def test_get_solver_attempts_prefers_last_solver_result():
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.last_solver_result = {
		"error": "",
		"best_name": "b",
		"attempts": [{"name": "b", "score": 1}],
		"baseline": {"k": "v"},
	}
	baseline, best_name, attempts, error = runner._get_solver_attempts()
	assert baseline == {"k": "v"}
	assert best_name == "b"
	assert attempts == [{"name": "b", "score": 1}]
	assert error == ""


def test_get_solver_attempts_falls_back_to_configmap_runs_json(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.last_solver_result = None
	runner.ctx = "ctx"

	cm = {
		"data": {
			"runs.json": json.dumps([
				{"best_name": "a", "attempts": [{"name": "a"}], "baseline": {"x": 1}},
				{"error": "E", "best_name": "b", "attempts": [{"name": "b"}], "baseline": {"y": 2}},
			])
		}
	}
	monkeypatch.setattr(runner, "_get_latest_configmap", lambda *a, **k: cm)
	baseline, best_name, attempts, error = runner._get_solver_attempts()
	assert baseline == {"y": 2}
	assert best_name == "b"
	assert attempts == [{"name": "b"}]
	assert error == "E"


# ---------------------------------------------------------------------------
# ConfigMap helpers: _get_latest_configmap
# ---------------------------------------------------------------------------


def test_get_latest_configmap_retries_then_returns_latest(monkeypatch):
	# First call fails, second returns list with two matching items; should pick newest.
	runs = {"i": 0}

	class R:
		def __init__(self, rc, out=b"{}"):
			self.returncode = rc
			self.stdout = out
			self.stderr = b""

	items = {
		"items": [
			{"metadata": {"name": "solver-stats-aaa", "creationTimestamp": "2020-01-01T00:00:00Z", "resourceVersion": "1"}},
			{"metadata": {"name": "solver-stats-bbb", "creationTimestamp": "2021-01-01T00:00:00Z", "resourceVersion": "2"}},
			{"metadata": {"name": "other", "creationTimestamp": "2022-01-01T00:00:00Z", "resourceVersion": "9"}},
		]
	}

	def fake_run(args, stdout, stderr, check):
		runs["i"] += 1
		if runs["i"] == 1:
			return R(1)
		return R(0, json.dumps(items).encode("utf-8"))

	slept = {"n": 0}
	monkeypatch.setattr(tr.subprocess, "run", fake_run)
	monkeypatch.setattr(tr.time, "sleep", lambda _s: slept.__setitem__("n", slept["n"] + 1))

	cm = tr.TestRunner._get_latest_configmap("ctx", "ns", "solver-stats", retries=1, sleep_seconds=0.0)
	assert cm is not None
	assert cm["metadata"]["name"] == "solver-stats-bbb"
	assert slept["n"] == 1


# ---------------------------------------------------------------------------
# Solver stats/log dump helpers: _write_solver_stats_json, _save_scheduler_logs
# ---------------------------------------------------------------------------


def test_write_solver_stats_json_writes_runs_raw(tmp_path, monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.ctx = "ctx"
	runner.solver_stats_dir = tmp_path / "solver-stats"

	runs_raw = "[{\"best_name\":\"x\"}]"
	cm = {"data": {"runs.json": runs_raw}}
	monkeypatch.setattr(runner, "_get_latest_configmap", lambda *a, **k: cm)

	runner._write_solver_stats_json(seed=7, run_idx=2)
	out = runner.solver_stats_dir / "solver_stats_seed-7_run-2.json"
	assert out.exists()
	assert out.read_text(encoding="utf-8") == runs_raw


def test_write_solver_stats_json_skips_when_missing(monkeypatch, tmp_path):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.ctx = "ctx"
	runner.solver_stats_dir = tmp_path / "solver-stats"
	monkeypatch.setattr(runner, "_get_latest_configmap", lambda *a, **k: None)
	# should not raise
	runner._write_solver_stats_json(seed=1, run_idx=1)
	assert not runner.solver_stats_dir.exists()


def test_save_scheduler_logs_prunes_collision_and_writes_bytes(tmp_path, monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.scheduler_logs_dir = tmp_path / "scheduler-logs"
	runner.scheduler_logs_dir.mkdir(parents=True, exist_ok=True)
	runner.args = argparse.Namespace(cluster_name="kwok1")

	out = runner.scheduler_logs_dir / "sched_logs_seed-3_run-1.log"
	out.write_text("old", encoding="utf-8")

	class R:
		def __init__(self):
			self.stdout = b"LOGDATA"

	monkeypatch.setattr(tr.subprocess, "run", lambda *a, **k: R())
	runner._save_scheduler_logs(seed=3, run_idx=1)
	assert out.read_bytes() == b"LOGDATA"


# ---------------------------------------------------------------------------
# Small JSON helpers: _build_node_info
# ---------------------------------------------------------------------------


def test_build_node_info_json():
	s = tr.TestRunner._build_node_info(["n1", "n2"], cap_cpu_m=1000, cap_mem_b=2048)
	data = json.loads(s)
	assert data == [
		{"name": "n1", "cap_cpu_m": 1000, "cap_mem_bytes": 2048},
		{"name": "n2", "cap_cpu_m": 1000, "cap_mem_bytes": 2048},
	]


# ---------------------------------------------------------------------------
# Pod list helper: _build_pod_list
# ---------------------------------------------------------------------------


def test_build_pod_list_maps_rs_prefix_fields():
	running_by_name = {"rs-01-p2-abc": "n1"}
	unsched = ["rs-01-p2-def", "other"]
	rs_specs = [{"name": "rs-01-p2", "req_cpu_m": 100, "req_mem_bytes": 200, "priority": 2}]
	items = tr.TestRunner._build_pod_list(running_by_name, unsched, rs_specs)
	by_name = {p["name"]: p for p in items}
	assert by_name["rs-01-p2-abc"]["node"] == "n1"
	assert by_name["rs-01-p2-abc"]["cpu_m"] == 100
	assert by_name["rs-01-p2-def"]["cpu_m"] == 100
	assert by_name["other"]["cpu_m"] == 0



# ---------------------------------------------------------------------------
# Workload helpers: _gen_rs_sizes
# ---------------------------------------------------------------------------


def test_gen_rs_sizes_hits_total_pods_and_bounds():
	rng = random.Random(0)
	sizes = tr.TestRunner._gen_rs_sizes(rng, num_pods=20, replicas_per_set=(6, 8))
	assert sum(sizes) == 20
	assert all(s >= 1 for s in sizes)
	assert all(s <= 8 for s in sizes)
	# First sizes should typically be >= lo; last may drift smaller.
	assert sizes[0] >= 6


def test_make_replicaset_specs_only_builds_specs():
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)

	class TA:
		pass

	ta = TA()
	ta.rs_sets = [2, 3]
	ta.rs_parts_cpu_m = [100, 200]
	ta.rs_parts_mem_b = [1000, 2000]
	ta.num_priorities = 5

	rng = random.Random(0)
	specs = runner._make_replicaset_specs_only(rng, ta)
	assert len(specs) == 2
	assert specs[0]["replicas"] == 2
	assert specs[0]["req_cpu_m"] == 100
	assert specs[0]["req_mem_bytes"] == 1000
	assert specs[0]["name"].startswith("rs-01-p")


def test_make_replicaset_specs_only_rejects_empty_rs_sets():
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)

	class TA:
		pass

	ta = TA()
	ta.rs_sets = []
	ta.rs_parts_cpu_m = []
	ta.rs_parts_mem_b = []
	ta.num_priorities = 1
	with pytest.raises(ValueError):
		runner._make_replicaset_specs_only(random.Random(0), ta)


def test_apply_replicasets_calls_kubectl_and_optional_wait(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.ctx = "ctx"

	calls = {"apply": [], "wait": []}
	monkeypatch.setattr(tr, "yaml_kwok_rs", lambda *a, **k: "YAML")
	monkeypatch.setattr(tr, "kubectl_apply_yaml", lambda log, ctx, y: calls["apply"].append((ctx, y)))
	monkeypatch.setattr(tr, "wait_rs_pods", lambda log, ctx, name, ns, timeout_s, mode: calls["wait"].append((name, mode)))

	ta = tr.TestConfigApplied(
		namespace="ns",
		num_nodes=1,
		num_pods=2,
		num_priorities=2,
		num_replicaset=1,
		num_replicas_per_rs=(1, 2),
		node_cpu_m=1000,
		node_mem_b=1000,
		cpu_per_pod_m=(100, 100),
		mem_per_pod_b=(100, 100),
		util=0.5,
		wait_pod_mode="ready",
		wait_pod_timeout_s=3,
		settle_timeout_min_s=0,
		settle_timeout_max_s=0,
	)

	specs = [{"name": "rs-01-p1", "priority": 1, "req_cpu_m": 100, "req_mem_bytes": 200, "replicas": 2}]
	runner._apply_replicasets(ta, specs)
	assert calls["apply"] == [("ctx", "YAML")]
	assert calls["wait"] == [("rs-01-p1", "ready")]


# ---------------------------------------------------------------------------
# Solver helpers: _solver_directly
# ---------------------------------------------------------------------------


def test_solver_directly_exports_input_and_output_and_meta(tmp_path, monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(
		solver_timeout_ms=123,
		solver_input_export=tmp_path / "in.json",
		solver_output_export=tmp_path / "out.json",
		solver_directly_running_target_util=0.0,
	)

	class TA:
		namespace = "ns"
		num_nodes = 2
		node_cpu_m = 1000
		node_mem_b = 2000

	rs_specs = [{"name": "rs-01-p1", "priority": 1, "req_cpu_m": 100, "req_mem_bytes": 200, "replicas": 2}]

	monkeypatch.setattr(tr.importlib_metadata, "version", lambda _d: "9.14.6206")

	class R:
		stdout = json.dumps({"status": "OK", "placements": [1, 2]}).encode("utf-8")
		stderr = b""

	monkeypatch.setattr(tr.subprocess, "run", lambda *a, **k: R())
	monkeypatch.setattr(tr.time, "time", lambda: 1.0)

	resp, meta = runner._solver_directly(TA(), seed=7, rs_specs=rs_specs)
	assert resp["status"] == "OK"
	assert (tmp_path / "in.json").exists()
	assert (tmp_path / "out.json").exists()
	assert meta["total_pods"] == 2
	assert meta["total_node_cpu"] == 2000
	assert meta["total_node_mem"] == 4000


def test_solver_directly_stdout_parse_error(tmp_path, monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(
		solver_timeout_ms=1,
		solver_input_export=None,
		solver_output_export=tmp_path / "out.json",
		solver_directly_running_target_util=0.0,
	)

	class TA:
		namespace = "ns"
		num_nodes = 1
		node_cpu_m = 100
		node_mem_b = 100

	rs_specs = [{"name": "rs-01-p1", "priority": 1, "req_cpu_m": 1, "req_mem_bytes": 1, "replicas": 1}]

	monkeypatch.setattr(tr.importlib_metadata, "version", lambda _d: "9.14.6206")

	class R:
		stdout = b"not-json"
		stderr = b""

	monkeypatch.setattr(tr.subprocess, "run", lambda *a, **k: R())
	resp, _meta = runner._solver_directly(TA(), seed=1, rs_specs=rs_specs)
	assert resp["status"] == "PY_SOLVER_STDOUT_PARSE_ERROR"
	assert (tmp_path / "out.json").exists()


def test_solver_directly_preplace_sets_initial_running_uids(tmp_path, monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(
		solver_timeout_ms=1,
		solver_input_export=tmp_path / "in.json",
		solver_output_export=None,
		solver_directly_running_target_util=0.5,
	)

	class TA:
		namespace = "ns"
		num_nodes = 2
		node_cpu_m = 1000
		node_mem_b = 1000

	rs_specs = [{"name": "rs-01-p1", "priority": 1, "req_cpu_m": 100, "req_mem_bytes": 100, "replicas": 6}]

	monkeypatch.setattr(tr.importlib_metadata, "version", lambda _d: "9.14.6206")

	class R:
		stdout = json.dumps({"status": "OK"}).encode("utf-8")
		stderr = b""

	monkeypatch.setattr(tr.subprocess, "run", lambda *a, **k: R())
	resp, meta = runner._solver_directly(TA(), seed=9, rs_specs=rs_specs)
	assert resp["status"] == "OK"
	assert meta["initial_running_uids"]
	inst = json.loads((tmp_path / "in.json").read_text(encoding="utf-8"))
	assert any((p.get("node") or "") for p in inst.get("pods", []))


# ---------------------------------------------------------------------------
# Solver helpers: _wait_solver_inactive_http
# ---------------------------------------------------------------------------


def test_wait_solver_inactive_http_returns_true_when_inactive(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)

	# Deterministic time progression.
	t = {"now": 0.0}

	def fake_time():
		return t["now"]

	def fake_sleep(dt):
		t["now"] += float(dt)

	# First poll: active true, second poll: active false
	responses = [
		(200, '{"Active": true}'),
		(200, '{"Active": false}'),
	]

	def fake_get(url):
		return responses.pop(0)

	monkeypatch.setattr(tr.time, "time", fake_time)
	monkeypatch.setattr(tr.time, "sleep", fake_sleep)
	monkeypatch.setattr(tr, "get_solver_active_status_http", fake_get)

	# Keep test fast even if sleep monkeypatching ever regresses.
	ok = runner._wait_solver_inactive_http("http://x/active", timeout_s=10, poll_initial_s=0.0, poll_interval_s=0.0)
	assert ok is True


def test_wait_solver_inactive_http_times_out(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)

	t = {"now": 0.0}

	def fake_time():
		return t["now"]

	def fake_sleep(dt):
		t["now"] += float(dt)

	monkeypatch.setattr(tr.time, "time", fake_time)
	monkeypatch.setattr(tr.time, "sleep", fake_sleep)
	monkeypatch.setattr(tr, "get_solver_active_status_http", lambda url: (200, '{"Active": true}'))

	ok = runner._wait_solver_inactive_http("http://x/active", timeout_s=0, poll_initial_s=0.0, poll_interval_s=0.0)
	assert ok is False


# ---------------------------------------------------------------------------
# Seed run helpers: _pause
# ---------------------------------------------------------------------------


def test_pause_skips_when_disabled(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(pause=False)
	# Should do nothing even if stdin is tty
	monkeypatch.setattr(tr.sys.stdin, "isatty", lambda: True)
	monkeypatch.setattr(builtins, "input", lambda *_a, **_k: (_ for _ in ()).throw(AssertionError("should not prompt")))
	runner._pause(next_exists=True)


def test_pause_prompts_when_enabled_and_tty(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(pause=True)
	monkeypatch.setattr(tr.sys.stdin, "isatty", lambda: True)
	seen = {"called": 0}
	monkeypatch.setattr(builtins, "input", lambda *_a, **_k: seen.__setitem__("called", seen["called"] + 1))
	runner._pause(next_exists=True)
	assert seen["called"] == 1


def test_pause_keyboard_interrupt_raises_system_exit(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(pause=True)
	monkeypatch.setattr(tr.sys.stdin, "isatty", lambda: True)

	def boom(*_a, **_k):
		raise KeyboardInterrupt()

	monkeypatch.setattr(builtins, "input", boom)
	with pytest.raises(SystemExit):
		runner._pause(next_exists=True)


# ---------------------------------------------------------------------------
# Seed execution: _run_single_seed, _execute_seed_direct, _execute_seed_on_cluster
# ---------------------------------------------------------------------------


def test_run_single_seed_retries_then_succeeds(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(solver_directly=True)
	runner.quota_reached = False

	# Keep attempts small and deterministic.
	monkeypatch.setattr(tr, "RETRIES_ON_FAIL", 1)
	monkeypatch.setattr(tr.time, "time", lambda: 0.0)

	calls = {"n": 0}

	def fake_exec(seed, ta):
		calls["n"] += 1
		return calls["n"] == 2

	monkeypatch.setattr(runner, "_execute_seed_direct", fake_exec)

	ok = runner._run_single_seed(seed=123, ta=object(), run_idx=1)
	assert ok is True
	assert calls["n"] == 2
	assert runner.suppress_fail_log is False
	assert runner.failure is None


def test_execute_seed_direct_logs_summary_and_returns_true(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(solver_directly=True)

	# Make specs deterministic and small.
	monkeypatch.setattr(runner, "_make_replicaset_specs_only", lambda rng, ta: [{"name": "rs", "priority": 1, "req_cpu_m": 10, "req_mem_bytes": 20, "replicas": 2}])

	# Solver result: one newly placed pod, one moved-from-running, one eviction.
	resp = {
		"status": "OK",
		"placements": [
			{"pod": {"uid": "u1"}, "from_node": ""},
			{"pod": {"uid": "u2"}, "from_node": "node-0"},
		],
		"evictions": [
			{"pod": {"uid": "u2"}},
		],
	}
	meta = {
		"initial_running_uids": ["u2"],
		"uid_to_priority": {"u1": 5, "u2": 1},
		"uid_to_cpu": {"u1": 10, "u2": 10},
		"uid_to_mem": {"u1": 20, "u2": 20},
		"total_pods": 2,
		"total_node_cpu": 100,
		"total_node_mem": 200,
		"elapsed_ms": 7,
	}
	monkeypatch.setattr(runner, "_solver_directly", lambda ta, seed, rs_specs: (resp, meta))

	seen = {"seed": None, "note": None}
	monkeypatch.setattr(runner, "_log_seed_summary", lambda seed, note="": seen.update({"seed": seed, "note": note}))

	# Minimal applied config.
	ta = tr.TestConfigApplied(
		namespace="ns",
		num_nodes=1,
		num_pods=2,
		num_priorities=2,
		num_replicaset=1,
		num_replicas_per_rs=(1, 2),
		node_cpu_m=100,
		node_mem_b=200,
		cpu_per_pod_m=(10, 10),
		mem_per_pod_b=(20, 20),
		util=0.5,
		wait_pod_mode=None,
		wait_pod_timeout_s=0,
		settle_timeout_min_s=0,
		settle_timeout_max_s=0,
	)

	ok = runner._execute_seed_direct(seed=5, ta=ta)
	assert ok is True
	assert seen["seed"] == 5
	assert "direct-solving status=OK" in (seen["note"] or "")
	assert "running=" in (seen["note"] or "")
	assert "unscheduled=" in (seen["note"] or "")


def test_execute_seed_on_cluster_ensure_cluster_failure_records_failure(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(cluster_name="kwok1", kwok_runtime="binary", default_scheduler=True, solver_trigger=False)
	runner.kwokctl_config_doc = {}
	runner.ctx = "ctx"

	monkeypatch.setattr(tr, "ensure_kwok_cluster", lambda *a, **k: (_ for _ in ()).throw(RuntimeError("boom")))

	seen = {}
	monkeypatch.setattr(runner, "_record_failure", lambda cat, seed, phase, msg, details="": seen.update({"cat": cat, "seed": seed, "phase": phase, "msg": msg}))
	monkeypatch.setattr(runner, "_log_seed_summary", lambda *_a, **_k: None)

	ta = tr.TestConfigApplied(
		namespace="ns",
		num_nodes=1,
		num_pods=1,
		num_priorities=1,
		num_replicaset=1,
		num_replicas_per_rs=(1, 1),
		node_cpu_m=100,
		node_mem_b=100,
		cpu_per_pod_m=(10, 10),
		mem_per_pod_b=(10, 10),
		util=0.5,
		wait_pod_mode=None,
		wait_pod_timeout_s=0,
		settle_timeout_min_s=0,
		settle_timeout_max_s=0,
	)

	ok = runner._execute_seed_on_cluster(seed=1, ta=ta, run_idx=1)
	assert ok is False
	assert seen["cat"] == "seed"
	assert seen["phase"] == "ensure_cluster"


def test_execute_seed_on_cluster_snapshot_validation_mismatch_fails(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(cluster_name="kwok1", kwok_runtime="binary", default_scheduler=True, solver_trigger=False)
	runner.kwokctl_config_doc = {}
	runner.ctx = "ctx"

	# Make all cluster-mutating helpers no-ops.
	monkeypatch.setattr(tr, "ensure_kwok_cluster", lambda *a, **k: None)
	monkeypatch.setattr(tr, "create_kwok_nodes", lambda *a, **k: None)
	monkeypatch.setattr(tr, "ensure_namespace", lambda *a, **k: None)
	monkeypatch.setattr(tr, "ensure_service_account", lambda *a, **k: None)
	monkeypatch.setattr(tr, "ensure_priority_classes", lambda *a, **k: None)
	monkeypatch.setattr(runner, "_apply_replicasets", lambda *a, **k: None)
	monkeypatch.setattr(tr.time, "sleep", lambda *_a, **_k: None)
	monkeypatch.setattr(tr, "qty_to_mcpu_str", lambda *_a, **_k: "100m")
	monkeypatch.setattr(tr, "qty_to_bytes_str", lambda *_a, **_k: "1Mi")
	monkeypatch.setattr(tr, "kwok_pods_cap", lambda *_a, **_k: 10)
	monkeypatch.setattr(runner, "_make_replicaset_specs_only", lambda *_a, **_k: [{"name": "rs", "priority": 1, "req_cpu_m": 1, "req_mem_bytes": 1, "replicas": 1}])

	# Snapshot mismatch: running+unsched != expected.
	class Snap:
		pods_running = ["p1"]
		pods_unscheduled = []

	monkeypatch.setattr(tr, "stat_snapshot", lambda *_a, **_k: Snap())

	seen = {}
	monkeypatch.setattr(runner, "_record_failure", lambda cat, seed, phase, msg, details="": seen.update({"cat": cat, "seed": seed, "phase": phase, "msg": msg}))
	monkeypatch.setattr(runner, "_log_seed_summary", lambda *_a, **_k: None)

	ta = tr.TestConfigApplied(
		namespace="ns",
		num_nodes=1,
		num_pods=2,
		num_priorities=1,
		num_replicaset=1,
		num_replicas_per_rs=(1, 1),
		node_cpu_m=100,
		node_mem_b=100,
		cpu_per_pod_m=(10, 10),
		mem_per_pod_b=(10, 10),
		util=0.5,
		wait_pod_mode=None,
		wait_pod_timeout_s=0,
		settle_timeout_min_s=0,
		settle_timeout_max_s=0,
	)

	ok = runner._execute_seed_on_cluster(seed=2, ta=ta, run_idx=1)
	assert ok is False
	assert seen["phase"] == "snapshot_validation_before"
	assert "pod count mismatch" in seen["msg"]


def test_execute_seed_on_cluster_happy_path_writes_results_and_enforces_quota(monkeypatch, tmp_path):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.ctx = "ctx"
	runner.kwokctl_config_doc = {}
	runner.results_f = tmp_path / "results.csv"
	runner.quota_reached = False
	runner.saved_not_all_running = 0
	runner.args = argparse.Namespace(
		cluster_name="kwok1",
		kwok_runtime="binary",
		default_scheduler=False,
		solver_trigger=False,
		save_solver_stats=True,
		save_scheduler_logs=True,
		seeds_not_all_running=1,
	)

	# No-op all external/cluster side effects.
	monkeypatch.setattr(tr, "ensure_kwok_cluster", lambda *a, **k: None)
	monkeypatch.setattr(tr, "create_kwok_nodes", lambda *a, **k: None)
	monkeypatch.setattr(tr, "ensure_namespace", lambda *a, **k: None)
	monkeypatch.setattr(tr, "ensure_service_account", lambda *a, **k: None)
	monkeypatch.setattr(tr, "ensure_priority_classes", lambda *a, **k: None)
	monkeypatch.setattr(tr, "qty_to_mcpu_str", lambda *_a, **_k: "100m")
	monkeypatch.setattr(tr, "qty_to_bytes_str", lambda *_a, **_k: "1Mi")
	monkeypatch.setattr(tr, "kwok_pods_cap", lambda *_a, **_k: 10)
	monkeypatch.setattr(tr.time, "sleep", lambda *_a, **_k: None)
	monkeypatch.setattr(tr.time, "time", lambda: 0.0)
	monkeypatch.setattr(tr, "get_timestamp", lambda: "TS")

	# Deterministic specs.
	rs_specs = [{"name": "rs-01-p1", "priority": 1, "req_cpu_m": 10, "req_mem_bytes": 20, "replicas": 2}]
	monkeypatch.setattr(runner, "_make_replicaset_specs_only", lambda *_a, **_k: rs_specs)
	monkeypatch.setattr(runner, "_apply_replicasets", lambda *_a, **_k: None)

	# Fake snapshots (before and after) with the attributes used by result_row.
	class Snap:
		def __init__(self, running, unsched, cpu_util, mem_util):
			self.pods_running = running
			self.pods_unscheduled = unsched
			self.cpu_run_util = cpu_util
			self.mem_run_util = mem_util
			self.cpu_req_by_node = {"n1": 10}
			self.mem_req_by_node = {"n1": 20}
			self.pods_run_by_node = {"n1": [p[0] for p in running]}
			self.running_placed_by_prio = {"1": len(running)}
			self.unschedulable_by_prio = {"1": len(unsched)}

	snaps = [
		Snap(running=[("rs-01-p1-0", "n1")], unsched=["rs-01-p1-1"], cpu_util=0.1, mem_util=0.2),
		Snap(running=[("rs-01-p1-0", "n1")], unsched=["rs-01-p1-1"], cpu_util=0.1, mem_util=0.2),
	]
	monkeypatch.setattr(tr, "stat_snapshot", lambda *_a, **_k: snaps.pop(0))

	# Attempts parsing.
	monkeypatch.setattr(runner, "_get_solver_attempts", lambda: ({"x": 1}, "best", [{"name": "best", "score": {"s": 1}, "duration_ms": 7, "status": "OK"}], ""))

	# Capture outputs.
	seen = {"row": None, "solver_stats": 0, "sched_logs": 0, "outcome": None}
	monkeypatch.setattr(runner, "_append_result_csv", lambda row: seen.__setitem__("row", row))
	monkeypatch.setattr(runner, "_write_solver_stats_json", lambda *_a, **_k: seen.__setitem__("solver_stats", seen["solver_stats"] + 1))
	monkeypatch.setattr(runner, "_save_scheduler_logs", lambda *_a, **_k: seen.__setitem__("sched_logs", seen["sched_logs"] + 1))
	monkeypatch.setattr(runner, "_record_seed_outcome", lambda seed, all_running: seen.__setitem__("outcome", (seed, all_running)))
	monkeypatch.setattr(runner, "_log_seed_summary", lambda *_a, **_k: None)

	ta = tr.TestConfigApplied(
		namespace="ns",
		num_nodes=1,
		num_pods=2,
		num_priorities=1,
		num_replicaset=1,
		num_replicas_per_rs=(1, 2),
		node_cpu_m=100,
		node_mem_b=200,
		cpu_per_pod_m=(10, 10),
		mem_per_pod_b=(20, 20),
		util=0.5,
		wait_pod_mode=None,
		wait_pod_timeout_s=0,
		settle_timeout_min_s=0,
		settle_timeout_max_s=0,
	)

	ok = runner._execute_seed_on_cluster(seed=3, ta=ta, run_idx=1)
	assert ok is True
	assert seen["row"] is not None
	assert seen["solver_stats"] == 1
	assert seen["sched_logs"] == 1
	assert seen["outcome"] == (3, False)
	assert runner.quota_reached is True


def test_log_seed_run_and_summary_include_combined_job_configs(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	monkeypatch.setattr(runner, "_combined_job_configs_seed_str", lambda: "COMBINED")
	seen = []
	monkeypatch.setattr(tr.LOG, "info", lambda *a, **k: seen.append(a))
	runner._log_seed_run(seed=7, seed_idx=1, seeds_total=2)
	runner._log_seed_summary(seed=7, note="n")
	assert any("COMBINED" in str(args) for args in seen)


def test_log_kwokctl_envs_formats_values(monkeypatch):
	seen = []
	monkeypatch.setattr(tr.LOG, "info", lambda *a, **k: seen.append(a))
	tr.TestRunner.log_kwokctl_envs({
		"A": "1",
		"B": None,
		"C": {"k": "v"},
		"D": [1, 2],
	})
	assert seen


def test_log_args_emits_block(monkeypatch):
	args = argparse.Namespace(
		cluster_name="c",
		kwok_runtime="binary",
		workload_config_file="wl.yaml",
		kwokctl_config_file="kw.yaml",
		output_dir="./out",
		clean_start=False,
		re_run_seeds=False,
		pause=False,
		log_level="INFO",
		default_scheduler=False,
		solver_directly=False,
		solver_timeout_ms=1,
		solver_input_export=None,
		solver_output_export=None,
		solver_directly_running_target_util=0.0,
		gen_seeds_to_file=None,
		seed=None,
		seed_file=None,
		count=0,
		repeats=1,
		seeds_not_all_running=0,
		job_file=None,
		save_solver_stats=False,
		save_scheduler_logs=False,
		solver_trigger=False,
	)
	seen = []
	monkeypatch.setattr(tr.LOG, "info", lambda *a, **k: seen.append(a))
	tr.TestRunner.log_args(args)
	assert seen


def test_eta_write_file_creates_marker_and_prunes_old(tmp_path, monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.output_dir_resolved = tmp_path
	runner.args = argparse.Namespace(seeds_not_all_running=3)
	runner.saved_not_all_running = 1

	# Old marker should be pruned.
	(tmp_path / "eta_old_seeds-0-of-1").write_text("x", encoding="utf-8")

	# Stable timestamp formatting.
	orig_localtime = tr.time.localtime
	monkeypatch.setattr(tr.time, "strftime", lambda *_a, **_k: "20250101-000000" if "%Y%m%d-%H%M%S" in str(_a[0]) else "2025-01-01T00:00:00")
	monkeypatch.setattr(tr.time, "localtime", lambda *_a, **_k: orig_localtime(0))

	runner._eta_write_file(eta_epoch=123.0, seed_idx=2, seeds_total=10)
	markers = [p for p in tmp_path.iterdir() if p.name.startswith("eta_")]
	assert len(markers) == 1
	name = markers[0].name
	assert "seeds-2-of-10" in name
	assert "snar-2" in name  # 3 total - 1 saved
	payload = json.loads(markers[0].read_text(encoding="utf-8").strip())
	assert payload["seeds_at"] == 2
	assert payload["seeds_total"] == 10
	assert payload["snar_total"] == 3
	assert payload["snar_left"] == 2


def test_eta_update_marker_calls_estimation_and_write(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.seed_durations = [1.0]
	runner.output_dir_resolved = Path(".")
	runner.args = argparse.Namespace(seeds_not_all_running=0)

	seen = {}
	monkeypatch.setattr(runner, "_eta_estimation", lambda seed_idx, seeds_total: 42.0)
	monkeypatch.setattr(runner, "_eta_write_file", lambda eta_epoch, seed_idx, seeds_total: seen.update({"eta": eta_epoch, "idx": seed_idx, "total": seeds_total}))

	runner._eta_update_marker(3, 10)
	assert seen == {"eta": 42.0, "idx": 3, "total": 10}


def test_run_mode_single_seed_skips_when_already_seen(monkeypatch, tmp_path):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.results_f = tmp_path / "results.csv"
	runner.seen_results = {7}
	runner.args = argparse.Namespace(seed=7, re_run_seeds=False)

	seen = {"log": 0, "eta": 0, "summary": 0}
	monkeypatch.setattr(runner, "_log_seed_run", lambda *_a, **_k: seen.__setitem__("log", seen["log"] + 1))
	monkeypatch.setattr(runner, "_eta_update_marker", lambda *_a, **_k: seen.__setitem__("eta", seen["eta"] + 1))
	monkeypatch.setattr(runner, "_eta_summary", lambda *_a, **_k: seen.__setitem__("summary", seen["summary"] + 1))

	runner.run_mode_single_seed()
	assert seen["log"] == 1
	assert seen["eta"] == 1
	assert seen["summary"] == 1


def test_run_mode_count_runs_one_seed_without_cluster(monkeypatch, tmp_path):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.results_f = tmp_path / "results.csv"
	runner.seen_results = set()
	runner.quota_reached = False
	runner.seed_durations = []
	runner.args = argparse.Namespace(count=1, repeats=1, re_run_seeds=False, solver_directly=True)

	# Deterministic RNG and time.
	class Rng:
		def getrandbits(self, _n):
			return 42
	monkeypatch.setattr(tr.time, "time_ns", lambda: 1)
	monkeypatch.setattr(tr, "seeded_random", lambda *_a, **_k: Rng())
	monkeypatch.setattr(tr.time, "time", lambda: 0.0)

	monkeypatch.setattr(runner, "_resolve_config_for_seed", lambda _seed: object())
	monkeypatch.setattr(runner, "_run_single_seed", lambda *_a, **_k: True)
	monkeypatch.setattr(runner, "_pause", lambda *_a, **_k: None)
	monkeypatch.setattr(runner, "_log_seed_run", lambda *_a, **_k: None)
	monkeypatch.setattr(runner, "_eta_update_marker", lambda *_a, **_k: None)
	monkeypatch.setattr(runner, "_eta_summary", lambda *_a, **_k: None)

	runner.run_mode_count()
	assert 42 in runner.seen_results
	assert len(runner.seed_durations) == 1


def test_run_mode_seed_file_runs_one_seed(monkeypatch, tmp_path):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.results_f = tmp_path / "results.csv"
	runner.seen_results = set()
	runner.quota_reached = False
	runner.seed_durations = []
	runner.args = argparse.Namespace(
		seed_file="seeds.txt",
		repeats=1,
		re_run_seeds=False,
		solver_directly=True,
		# Needed for _combined_job_configs_seed_str via _log_seed_run
		job_file=None,
		workload_config_file="wl.yaml",
		kwokctl_config_file="kwokctl.yaml",
	)

	monkeypatch.setattr(runner, "_read_seeds_file", lambda *_a, **_k: [7])
	monkeypatch.setattr(runner, "_resolve_config_for_seed", lambda _seed: object())
	monkeypatch.setattr(runner, "_run_single_seed", lambda *_a, **_k: True)
	monkeypatch.setattr(runner, "_pause", lambda *_a, **_k: None)
	monkeypatch.setattr(runner, "_eta_update_marker", lambda *_a, **_k: None)
	monkeypatch.setattr(runner, "_eta_summary", lambda *_a, **_k: None)

	runner.run_mode_seed_file()
	assert 7 in runner.seen_results
	assert len(runner.seed_durations) == 1


def test_solver_directly_missing_ortools_raises_system_exit(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(
		solver_timeout_ms=1,
		solver_input_export=None,
		solver_output_export=None,
		solver_directly_running_target_util=0.0,
	)

	def boom(_dist):
		raise tr.importlib_metadata.PackageNotFoundError

	monkeypatch.setattr(tr.importlib_metadata, "version", boom)

	class TA:
		namespace = "ns"
		num_nodes = 1
		node_cpu_m = 100
		node_mem_b = 100

	with pytest.raises(SystemExit):
		runner._solver_directly(TA(), seed=1, rs_specs=[])


def test_solver_directly_wrong_ortools_version_raises_system_exit(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(
		solver_timeout_ms=1,
		solver_input_export=None,
		solver_output_export=None,
		solver_directly_running_target_util=0.0,
	)
	monkeypatch.setattr(tr.importlib_metadata, "version", lambda _d: "0.0.0")

	class TA:
		namespace = "ns"
		num_nodes = 1
		node_cpu_m = 100
		node_mem_b = 100

	with pytest.raises(SystemExit):
		runner._solver_directly(TA(), seed=1, rs_specs=[])


def test_run_single_seed_flushes_deferred_failure_on_last_attempt(monkeypatch, tmp_path):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.args = argparse.Namespace(solver_directly=True)
	runner.quota_reached = False
	runner.failed_f = tmp_path / "failed.tsv"

	# Two attempts total.
	monkeypatch.setattr(tr, "RETRIES_ON_FAIL", 1)
	monkeypatch.setattr(tr.time, "time", lambda: 0.0)
	monkeypatch.setattr(tr.time, "strftime", lambda *_a, **_k: "TS")
	orig_localtime = tr.time.localtime
	monkeypatch.setattr(tr.time, "localtime", lambda *_a, **_k: orig_localtime(0))

	calls = {"n": 0}

	def fake_exec(seed, ta):
		# _run_single_seed clears runner.failure at the start of *each* attempt,
		# so only a failure captured on the final attempt can be flushed.
		calls["n"] += 1
		if calls["n"] == 2:
			from types import SimpleNamespace
			runner.failure = SimpleNamespace(category="seed", seed=seed, phase="phase", message="msg", details="d")
		return False

	monkeypatch.setattr(runner, "_execute_seed_direct", fake_exec)

	ok = runner._run_single_seed(seed=9, ta=object(), run_idx=1)
	assert ok is False
	assert runner.failed_f.exists()
	assert "seed" in runner.failed_f.read_text(encoding="utf-8")


def test_resolve_config_for_seed_builds_applied_config(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)

	# Keep randomness deterministic across all seeded_random calls.
	monkeypatch.setattr(tr, "seeded_random", lambda *_a, **_k: random.Random(0))

	# Use simple numeric quantity parsing.
	monkeypatch.setattr(tr, "qty_to_mcpu_int", lambda v: int(str(v).rstrip("m")))
	monkeypatch.setattr(tr, "qty_to_bytes_int", lambda v: int(str(v).rstrip("B")))

	runner.workload_config = tr.TestConfigRaw(
		namespace="ns",
		num_nodes=2,
		num_pods=10,
		num_priorities=(1, 3),
		num_replicas_per_rs=(3, 4),
		cpu_per_pod=("10m", "20m"),
		mem_per_pod=("30B", "40B"),
		util=0.5,
		wait_pod_mode="none",
		wait_pod_timeout=None,
		settle_timeout_min=None,
		settle_timeout_max=None,
	)

	ta = runner._resolve_config_for_seed(seed_int=123)
	assert ta.namespace == "ns"
	assert ta.num_nodes == 2
	assert ta.num_pods == 10
	assert len(ta.pod_parts_cpu_m) == 10
	assert len(ta.pod_parts_mem_b) == 10
	assert sum(ta.rs_sets) == 10
	assert ta.node_cpu_m >= 1
	assert ta.node_mem_b >= 1
	assert ta.wait_pod_mode is None


def test_resolve_config_for_seed_requires_num_replicas_per_rs(monkeypatch):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	monkeypatch.setattr(tr, "seeded_random", lambda *_a, **_k: random.Random(0))
	monkeypatch.setattr(tr, "qty_to_mcpu_int", lambda v: 1)
	monkeypatch.setattr(tr, "qty_to_bytes_int", lambda v: 1)

	runner.workload_config = tr.TestConfigRaw(
		namespace="ns",
		num_nodes=1,
		num_pods=1,
		num_priorities=(1, 1),
		num_replicas_per_rs=None,
		cpu_per_pod=("1m", "1m"),
		mem_per_pod=("1B", "1B"),
		util=0.5,
		wait_pod_mode="none",
		wait_pod_timeout=None,
		settle_timeout_min=None,
		settle_timeout_max=None,
	)

	with pytest.raises(SystemExit):
		runner._resolve_config_for_seed(seed_int=1)


def test_run_mode_seed_file_skips_seen_seed(monkeypatch, tmp_path):
	runner = tr.TestRunner(argparse.Namespace(), initialize=False)
	runner.results_f = tmp_path / "results.csv"
	runner.seen_results = {7}
	runner.quota_reached = False
	runner.seed_durations = []
	runner.args = argparse.Namespace(
		seed_file="seeds.txt",
		repeats=1,
		re_run_seeds=False,
		solver_directly=True,
		job_file=None,
		workload_config_file="wl.yaml",
		kwokctl_config_file="kwokctl.yaml",
	)

	monkeypatch.setattr(runner, "_read_seeds_file", lambda *_a, **_k: [7])
	monkeypatch.setattr(runner, "_pause", lambda *_a, **_k: None)

	seen = {"eta": 0, "summary": 0}
	monkeypatch.setattr(runner, "_eta_update_marker", lambda *_a, **_k: seen.__setitem__("eta", seen["eta"] + 1))
	monkeypatch.setattr(runner, "_eta_summary", lambda *_a, **_k: seen.__setitem__("summary", seen["summary"] + 1))

	# Should skip without trying to run.
	monkeypatch.setattr(runner, "_run_single_seed", lambda *_a, **_k: (_ for _ in ()).throw(AssertionError("should skip")))

	runner.run_mode_seed_file()
	# One update in the skip branch, plus a final update after the loop.
	assert seen["eta"] == 2
	assert seen["summary"] == 1

