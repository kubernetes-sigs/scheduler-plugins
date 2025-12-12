import argparse
import csv
import json
from pathlib import Path

import pytest


pytest.importorskip("yaml")

from scripts.kwok_trace_replayer import trace_replayer as tr


class _OkFuture:
    def result(self):
        return None


class _ShutdownNoopMixin:
    def shutdown(self, wait: bool = True):
        return None


def test_rs_name_for_record_is_stable():
    assert tr.TraceReplayer._rs_name_for_record(1) == "rs-000001"
    assert tr.TraceReplayer._rs_name_for_record(123456) == "rs-123456"


def test_merge_job_fields_into_args_cli_wins_and_parses_monitor_interval():
    args = argparse.Namespace(
        trace_dir="/cli/trace",
        cluster_name=None,
        kwok_runtime=None,
        kwokctl_config_file=None,
        namespace=None,
        node_cpu=None,
        node_mem=None,
        monitor_interval=None,
        log_level=None,
        result_dir=None,
        save_scheduler_logs=None,
        job_file=None,
    )

    job = {
        "trace-dir": "/job/trace",
        "cluster-name": "kwok2",
        "kwok-runtime": "docker",
        "kwokctl-config-file": "/job/cfg.yaml",
        "namespace": "ns",
        "node-cpu": "500m",
        "node-mem": "512Mi",
        "monitor-interval": "2.5",
        "log-level": "DEBUG",
        "result-dir": "/job/results",
        "save-scheduler-logs": True,
        "override-kwokctl-envs": [{"name": "X", "value": "Y"}],
    }

    merged, override_envs = tr.merge_job_fields_into_args(args, job)

    # CLI trace_dir should not be overridden
    assert merged.trace_dir == "/cli/trace"
    assert merged.cluster_name == "kwok2"
    assert merged.kwok_runtime == "docker"
    assert merged.monitor_interval == 2.5
    assert merged.save_scheduler_logs is True
    assert override_envs == [{"name": "X", "value": "Y"}]


def test_merge_job_fields_into_args_fills_missing_and_ignores_invalid_types():
    args = argparse.Namespace(
        trace_dir=None,
        cluster_name=None,
        kwok_runtime=None,
        kwokctl_config_file=None,
        namespace=None,
        node_cpu=None,
        node_mem=None,
        monitor_interval=None,
        log_level=None,
        result_dir=None,
        save_scheduler_logs=None,
        job_file=None,
    )

    job = {
        "trace-dir": "/job/trace",
        "monitor-interval": "not-a-float",
        "save-scheduler-logs": "yes",
        "override-kwokctl-envs": [],
    }

    merged, override_envs = tr.merge_job_fields_into_args(args, job)
    assert merged.trace_dir == "/job/trace"
    assert merged.monitor_interval is None
    assert merged.save_scheduler_logs is None
    assert override_envs == []


def test_ensure_default_args_validates_required_fields(tmp_path: Path):
    args = argparse.Namespace(
        trace_dir=None,
        kwokctl_config_file=None,
        result_dir=None,
        cluster_name=None,
        kwok_runtime=None,
        namespace=None,
        node_cpu=None,
        node_mem=None,
        monitor_interval=None,
        log_level=None,
        job_file=None,
        save_scheduler_logs=None,
    )

    with pytest.raises(SystemExit):
        tr.ensure_default_args(args)

    trace_dir = tmp_path / "trace"
    trace_dir.mkdir()
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("kind: KwokctlConfiguration\n", encoding="utf-8")
    results = tmp_path / "results"

    args = argparse.Namespace(
        trace_dir=str(trace_dir),
        kwokctl_config_file=str(cfg),
        result_dir=str(results),
        cluster_name=None,
        kwok_runtime=None,
        namespace=None,
        node_cpu=None,
        node_mem=None,
        monitor_interval=None,
        log_level=None,
        job_file=None,
        save_scheduler_logs=None,
    )

    out = tr.ensure_default_args(args)
    assert out.cluster_name == "kwok1"
    assert out.kwok_runtime == "binary"
    assert out.namespace == "trace"
    assert out.node_cpu == "1000m"
    assert out.node_mem == "1Gi"
    assert out.monitor_interval == 1.0
    assert out.log_level == "INFO"
    assert out.save_scheduler_logs is False
    assert Path(out.result_dir).is_absolute()


def test_load_trace_and_build_events_and_save_logs(tmp_path: Path, monkeypatch):
    trace_dir = tmp_path / "trace"
    trace_dir.mkdir()
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("kind: KwokctlConfiguration\n", encoding="utf-8")
    result_dir = tmp_path / "results"

    raw = {
        "meta": {"num_nodes": 2, "trace_time_s": 123.0},
        "pods": [
            {"id": 2, "start_time": 5.0, "end_time": 6.0, "cpu": 0.2, "mem": 0.3, "priority": 2, "replicas": 1},
            {"id": 1, "start_time": 1.0, "end_time": 4.0, "cpu": 0.1, "mem": 0.2, "priority": 3, "replicas": 2},
        ],
    }
    (trace_dir / "trace.json").write_text(json.dumps(raw), encoding="utf-8")

    args = argparse.Namespace(
        trace_dir=str(trace_dir),
        kwokctl_config_file=str(cfg),
        result_dir=str(result_dir),
        cluster_name="kwok1",
        kwok_runtime="binary",
        namespace="trace",
        node_cpu="1000m",
        node_mem="1Gi",
        monitor_interval=1.0,
        log_level="INFO",
        job_file=None,
        save_scheduler_logs=False,
    )

    # Avoid writing info file / noisy logging during tests.
    monkeypatch.setattr(tr.TraceReplayer, "_write_info_file", lambda self: None)
    monkeypatch.setattr(tr.TraceReplayer, "log_args", lambda self: None)

    replayer = tr.TraceReplayer(args)
    replayer.load_trace()

    assert replayer.num_nodes == 2
    assert replayer.trace_time == 123.0
    assert [p.id for p in replayer.pods] == [1, 2]  # sorted by start_time
    assert replayer.max_prio == 3

    # Build events with concrete resource strings
    replayer.node_cpu_m = 1000
    replayer.node_mem_b = 1024 * 1024 * 1024
    replayer._build_events()

    assert len(replayer.events) == 4
    assert replayer.events[0].kind == "create"
    assert replayer.events[-1].kind == "delete"

    # _save_scheduler_logs should write a file even when kwokctl isn't available.
    class DummyRes:
        stdout = b"hello\n"

    monkeypatch.setattr(tr.subprocess, "run", lambda *a, **k: DummyRes())
    replayer._save_scheduler_logs()

    out_path = Path(args.result_dir) / "scheduler-logs" / "sched_logs.log"
    assert out_path.exists()
    assert out_path.read_bytes() == b"hello\n"


def test_save_scheduler_logs_prune_failure_is_non_fatal(tmp_path: Path, monkeypatch):
    trace_dir = tmp_path / "trace"
    trace_dir.mkdir()
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("kind: KwokctlConfiguration\n", encoding="utf-8")
    result_dir = tmp_path / "results"

    args = argparse.Namespace(
        trace_dir=str(trace_dir),
        kwokctl_config_file=str(cfg),
        result_dir=str(result_dir),
        cluster_name="kwok1",
        kwok_runtime="binary",
        namespace="trace",
        node_cpu="1000m",
        node_mem="1Gi",
        monitor_interval=1.0,
        log_level="INFO",
        job_file=None,
        save_scheduler_logs=False,
    )

    monkeypatch.setattr(tr.TraceReplayer, "_write_info_file", lambda self: None)
    monkeypatch.setattr(tr.TraceReplayer, "log_args", lambda self: None)
    replayer = tr.TraceReplayer(args)

    out_path = Path(args.result_dir) / "scheduler-logs" / "sched_logs.log"
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_bytes(b"old")

    real_unlink = tr.Path.unlink

    def unlink_raises(self: Path, *a, **k):
        if self == out_path:
            raise OSError("boom")
        return real_unlink(self, *a, **k)

    monkeypatch.setattr(tr.Path, "unlink", unlink_raises)

    class DummyRes:
        stdout = b"new\n"

    monkeypatch.setattr(tr.subprocess, "run", lambda *a, **k: DummyRes())
    replayer._save_scheduler_logs()
    assert out_path.exists()
    assert out_path.read_bytes() == b"new\n"


def test_snapshot_from_pods_computes_utilization_and_pending(tmp_path: Path, monkeypatch):
    trace_dir = tmp_path / "trace"
    trace_dir.mkdir()
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("kind: KwokctlConfiguration\n", encoding="utf-8")
    result_dir = tmp_path / "results"

    args = argparse.Namespace(
        trace_dir=str(trace_dir),
        kwokctl_config_file=str(cfg),
        result_dir=str(result_dir),
        cluster_name="kwok1",
        kwok_runtime="binary",
        namespace="trace",
        node_cpu="1000m",
        node_mem="1Gi",
        monitor_interval=1.0,
        log_level="INFO",
        job_file=None,
        save_scheduler_logs=False,
    )

    monkeypatch.setattr(tr.TraceReplayer, "_write_info_file", lambda self: None)
    monkeypatch.setattr(tr.TraceReplayer, "log_args", lambda self: None)
    replayer = tr.TraceReplayer(args)

    replayer.num_nodes = 2
    replayer.node_cpu_m = 1000
    replayer.node_mem_b = 1000

    # Make quantity parsing deterministic for test.
    monkeypatch.setattr(tr, "qty_to_mcpu_int", lambda q: 0 if q is None else int(str(q).rstrip("m")))
    monkeypatch.setattr(tr, "qty_to_bytes_int", lambda q: 0 if q is None else int(str(q)))

    pods_json = {
        "items": [
            {
                "metadata": {"name": "a"},
                "spec": {
                    "nodeName": "n1",
                    "containers": [
                        {"resources": {"requests": {"cpu": "100m", "memory": "10"}}},
                        {"resources": {"requests": {"cpu": "200m", "memory": "20"}}},
                    ],
                },
                "status": {"phase": "Running"},
            },
            {
                "metadata": {"name": "b"},
                "spec": {"containers": [{"resources": {"requests": {"cpu": "50m", "memory": "5"}}}]},
                "status": {"phase": "Pending"},
            },
            {
                "metadata": {"name": "c"},
                "spec": {"containers": [{"resources": {"requests": {}}}]},
                "status": {"phase": "Succeeded"},
            },
        ]
    }
    monkeypatch.setattr(tr, "get_json_ctx", lambda *_a, **_k: pods_json)

    cpu_u, mem_u, running, pending = replayer._snapshot_from_pods("trace")
    assert running == [("a", "n1")]
    assert pending == ["b"]

    # total cpu_m=300; capacity=2*1000 -> 0.15
    assert cpu_u == pytest.approx(0.15)
    # total mem=30; capacity=2*1000 -> 0.015
    assert mem_u == pytest.approx(0.015)


def test_monitor_loop_writes_csv_and_counts_by_priority(tmp_path: Path, monkeypatch):
    trace_dir = tmp_path / "trace"
    trace_dir.mkdir()
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("kind: KwokctlConfiguration\n", encoding="utf-8")
    result_dir = tmp_path / "results"

    args = argparse.Namespace(
        trace_dir=str(trace_dir),
        kwokctl_config_file=str(cfg),
        result_dir=str(result_dir),
        cluster_name="kwok1",
        kwok_runtime="binary",
        namespace="trace",
        node_cpu="1000m",
        node_mem="1Gi",
        monitor_interval=1.0,
        log_level="INFO",
        job_file=None,
        save_scheduler_logs=False,
    )

    monkeypatch.setattr(tr.TraceReplayer, "_write_info_file", lambda self: None)
    monkeypatch.setattr(tr.TraceReplayer, "log_args", lambda self: None)
    replayer = tr.TraceReplayer(args)

    # One running pod and two pending pods.
    monkeypatch.setattr(
        replayer,
        "_snapshot_from_pods",
        lambda _ns: (
            0.25,
            0.50,
            [("rs-000001-0", "n1")],
            ["rs-000001-1", "unknown-zzz"],
        ),
    )

    monkeypatch.setattr(tr, "get_timestamp", lambda: "T")

    # Keep time stable so the loop sleeps (and we can stop after 1 tick).
    monkeypatch.setattr(tr.time, "time", lambda: 0.0)

    stop_event = tr.threading.Event()

    def sleep_and_stop(_s: float):
        stop_event.set()

    monkeypatch.setattr(tr.time, "sleep", sleep_and_stop)

    out_csv = tmp_path / "m.csv"
    replayer._monitor_loop(
        namespace="trace",
        interval_s=10.0,
        max_prio=2,
        prio_by_identity={"rs-000001": 2},
        out_csv=out_csv,
        stop_event=stop_event,
        start_wall_time=0.0,
        sim_t0=0.0,
    )

    txt = out_csv.read_text(encoding="utf-8").splitlines()
    assert txt[0].startswith("timestamp,wall_time_s,sim_time_s,cpu_run_util,mem_run_util")
    row = next(csv.reader([txt[1]]))
    assert row[0] == "T"
    assert row[5] == "1"  # running_count
    assert row[6] == "2"  # unsched_count

    running_by_prio = json.loads(row[7])
    unsched_by_prio = json.loads(row[8])
    assert running_by_prio == {"p1": 0, "p2": 1}
    assert unsched_by_prio == {"p1": 0, "p2": 1}


def test_replay_events_degenerate_end_before_start_does_not_sleep(tmp_path: Path, monkeypatch):
    trace_dir = tmp_path / "trace"
    trace_dir.mkdir()
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("kind: KwokctlConfiguration\n", encoding="utf-8")
    result_dir = tmp_path / "results"

    args = argparse.Namespace(
        trace_dir=str(trace_dir),
        kwokctl_config_file=str(cfg),
        result_dir=str(result_dir),
        cluster_name="kwok1",
        kwok_runtime="binary",
        namespace="trace",
        node_cpu="1000m",
        node_mem="1Gi",
        monitor_interval=1.0,
        log_level="INFO",
        job_file=None,
        save_scheduler_logs=False,
    )

    monkeypatch.setattr(tr.TraceReplayer, "_write_info_file", lambda self: None)
    monkeypatch.setattr(tr.TraceReplayer, "log_args", lambda self: None)
    replayer = tr.TraceReplayer(args)
    replayer.events = []
    replayer.trace_time = 0.0

    sleeps = []
    monkeypatch.setattr(tr.time, "time", lambda: 0.0)
    monkeypatch.setattr(tr.time, "sleep", lambda s: sleeps.append(s))

    replayer._replay_events(namespace="trace", start_wall_time=0.0, sim_t0=1.0)
    assert sleeps == []


def test_replay_events_next_batch_beyond_end_aligns_and_exits(tmp_path: Path, monkeypatch):
    trace_dir = tmp_path / "trace"
    trace_dir.mkdir()
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("kind: KwokctlConfiguration\n", encoding="utf-8")
    result_dir = tmp_path / "results"

    args = argparse.Namespace(
        trace_dir=str(trace_dir),
        kwokctl_config_file=str(cfg),
        result_dir=str(result_dir),
        cluster_name="kwok1",
        kwok_runtime="binary",
        namespace="trace",
        node_cpu="1000m",
        node_mem="1Gi",
        monitor_interval=1.0,
        log_level="INFO",
        job_file=None,
        save_scheduler_logs=False,
    )

    monkeypatch.setattr(tr.TraceReplayer, "_write_info_file", lambda self: None)
    monkeypatch.setattr(tr.TraceReplayer, "log_args", lambda self: None)
    replayer = tr.TraceReplayer(args)
    replayer.trace_time = 2.0
    replayer.events = [tr.Event(sim_time_s=5.0, kind="create", record_id=1, cpu_str="1m", mem_str="1", pc_name="p1")]

    class DummyExecutor:
        def __init__(self, *a, **k):
            self.shutdown_called = False

        def submit(self, *a, **k):
            return _OkFuture()

        def shutdown(self, wait: bool = True):
            self.shutdown_called = True

    monkeypatch.setattr(tr, "ThreadPoolExecutor", DummyExecutor)

    sleeps = []
    monkeypatch.setattr(tr.time, "time", lambda: 0.0)
    monkeypatch.setattr(tr.time, "sleep", lambda s: sleeps.append(s))

    replayer._replay_events(namespace="trace", start_wall_time=0.0, sim_t0=0.0)
    assert sleeps and sleeps[0] == pytest.approx(2.0)


def test_replay_events_processes_batch_and_aligns_after_last_batch(tmp_path: Path, monkeypatch):
    trace_dir = tmp_path / "trace"
    trace_dir.mkdir()
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("kind: KwokctlConfiguration\n", encoding="utf-8")
    result_dir = tmp_path / "results"

    args = argparse.Namespace(
        trace_dir=str(trace_dir),
        kwokctl_config_file=str(cfg),
        result_dir=str(result_dir),
        cluster_name="kwok1",
        kwok_runtime="binary",
        namespace="trace",
        node_cpu="1000m",
        node_mem="1Gi",
        monitor_interval=1.0,
        log_level="INFO",
        job_file=None,
        save_scheduler_logs=False,
    )

    monkeypatch.setattr(tr.TraceReplayer, "_write_info_file", lambda self: None)
    monkeypatch.setattr(tr.TraceReplayer, "log_args", lambda self: None)
    replayer = tr.TraceReplayer(args)
    replayer.trace_time = 2.0
    replayer.ctx = "ctx"

    # Build one create and one delete in same batch.
    replayer.events = [
        tr.Event(sim_time_s=0.0, kind="create", record_id=1, cpu_str="1m", mem_str="1", pc_name="p1"),
        tr.Event(sim_time_s=0.0, kind="delete", record_id=1),
    ]

    # Stub YAML + kubectl helpers.
    monkeypatch.setattr(tr, "yaml_kwok_rs", lambda **_k: "yaml")
    monkeypatch.setattr(tr, "kubectl_apply_yaml", lambda *_a, **_k: None)
    monkeypatch.setattr(tr, "delete_rs", lambda *_a, **_k: None)

    class DummyExecutor(_ShutdownNoopMixin):
        def __init__(self, *a, **k):
            self.submits = []

        def submit(self, fn, *a, **k):
            self.submits.append((fn, a, k))
            return _OkFuture()

    monkeypatch.setattr(tr, "ThreadPoolExecutor", DummyExecutor)

    sleeps = []
    monkeypatch.setattr(tr.time, "time", lambda: 0.0)
    monkeypatch.setattr(tr.time, "sleep", lambda s: sleeps.append(s))

    replayer._replay_events(namespace="trace", start_wall_time=0.0, sim_t0=0.0)
    # Align to trace end at the end.
    assert sleeps and sleeps[-1] == pytest.approx(2.0)


def test_replay_events_future_exception_is_caught(tmp_path: Path, monkeypatch):
    trace_dir = tmp_path / "trace"
    trace_dir.mkdir()
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("kind: KwokctlConfiguration\n", encoding="utf-8")
    result_dir = tmp_path / "results"

    args = argparse.Namespace(
        trace_dir=str(trace_dir),
        kwokctl_config_file=str(cfg),
        result_dir=str(result_dir),
        cluster_name="kwok1",
        kwok_runtime="binary",
        namespace="trace",
        node_cpu="1000m",
        node_mem="1Gi",
        monitor_interval=1.0,
        log_level="INFO",
        job_file=None,
        save_scheduler_logs=False,
    )

    monkeypatch.setattr(tr.TraceReplayer, "_write_info_file", lambda self: None)
    monkeypatch.setattr(tr.TraceReplayer, "log_args", lambda self: None)
    replayer = tr.TraceReplayer(args)
    replayer.trace_time = 0.0
    replayer.events = [tr.Event(sim_time_s=0.0, kind="create", record_id=1, cpu_str="1m", mem_str="1", pc_name="p1")]

    monkeypatch.setattr(tr, "yaml_kwok_rs", lambda **_k: "yaml")

    class BadFuture:
        def result(self):
            raise RuntimeError("bad")

    class DummyExecutor(_ShutdownNoopMixin):
        def __init__(self, *a, **k):
            return None

        def submit(self, *_a, **_k):
            return BadFuture()

    monkeypatch.setattr(tr, "ThreadPoolExecutor", DummyExecutor)
    monkeypatch.setattr(tr.time, "time", lambda: 0.0)
    monkeypatch.setattr(tr.time, "sleep", lambda _s: None)

    # Should not raise even though future.result() raises.
    replayer._replay_events(namespace="trace", start_wall_time=0.0, sim_t0=0.0)


def test_run_orchestrates_and_saves_logs(tmp_path: Path, monkeypatch):
    trace_dir = tmp_path / "trace"
    trace_dir.mkdir()
    # minimal trace.json so load_trace isn't reading real files in this test
    (trace_dir / "trace.json").write_text(json.dumps({"meta": {"num_nodes": 1, "trace_time_s": 0.0}, "pods": []}), encoding="utf-8")

    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("kind: KwokctlConfiguration\n", encoding="utf-8")
    result_dir = tmp_path / "results"

    args = argparse.Namespace(
        trace_dir=str(trace_dir),
        kwokctl_config_file=str(cfg),
        result_dir=str(result_dir),
        cluster_name="kwok1",
        kwok_runtime="binary",
        namespace="trace",
        node_cpu="1000m",
        node_mem="1Gi",
        monitor_interval=1.0,
        log_level="INFO",
        job_file=None,
        save_scheduler_logs=True,
    )

    monkeypatch.setattr(tr.TraceReplayer, "_write_info_file", lambda self: None)
    monkeypatch.setattr(tr.TraceReplayer, "log_args", lambda self: None)

    called = {
        "merge_envs": 0,
        "ensure_cluster": 0,
        "create_nodes": 0,
        "ensure_ns": 0,
        "ensure_pcs": 0,
        "replay": 0,
        "save_logs": 0,
    }

    def fake_load_trace(self):
        self.pods = [tr.TraceRecord(id=1, start_time=0.0, end_time=0.0, cpu=0.1, mem=0.1, priority=1, replicas=1)]
        self.num_nodes = 1
        self.trace_time = 0.0
        self.max_prio = 1
        self.t_min = 0.0

    monkeypatch.setattr(tr.TraceReplayer, "load_trace", fake_load_trace)
    monkeypatch.setattr(tr.TraceReplayer, "_build_events", lambda self: setattr(self, "events", []))
    monkeypatch.setattr(tr.TraceReplayer, "_replay_events", lambda *a, **k: called.__setitem__("replay", called["replay"] + 1))
    monkeypatch.setattr(tr.TraceReplayer, "_save_scheduler_logs", lambda self: called.__setitem__("save_logs", called["save_logs"] + 1))

    monkeypatch.setattr(tr, "merge_kwokctl_envs", lambda doc, envs: (called.__setitem__("merge_envs", called["merge_envs"] + 1) or doc))
    monkeypatch.setattr(tr, "ensure_kwok_cluster", lambda *a, **k: called.__setitem__("ensure_cluster", called["ensure_cluster"] + 1))
    monkeypatch.setattr(tr, "create_kwok_nodes", lambda *a, **k: called.__setitem__("create_nodes", called["create_nodes"] + 1))
    monkeypatch.setattr(tr, "ensure_namespace", lambda *a, **k: called.__setitem__("ensure_ns", called["ensure_ns"] + 1))
    monkeypatch.setattr(tr, "ensure_priority_classes", lambda *a, **k: called.__setitem__("ensure_pcs", called["ensure_pcs"] + 1))
    monkeypatch.setattr(tr, "kwok_pods_cap", lambda: 10)

    # Avoid real thread.
    class DummyThread:
        def __init__(self, target, args, daemon):
            self.target = target
            self.args = args
            self.daemon = daemon
            self.started = False
            self.joined = False

        def start(self):
            self.started = True

        def join(self, timeout: float | None = None):
            self.joined = True

    monkeypatch.setattr(tr.threading, "Thread", DummyThread)
    monkeypatch.setattr(tr.time, "time", lambda: 0.0)

    replayer = tr.TraceReplayer(args, job_doc={"x": 1}, override_kwokctl_envs=[{"name": "A", "value": "B"}])
    replayer.run()

    assert called["merge_envs"] == 1
    assert called["ensure_cluster"] == 1
    assert called["create_nodes"] == 1
    assert called["ensure_ns"] == 1
    assert called["ensure_pcs"] == 1
    assert called["replay"] == 1
    assert called["save_logs"] == 1


def test_replay_events_no_events_sleeps_until_end(tmp_path: Path, monkeypatch):
    trace_dir = tmp_path / "trace"
    trace_dir.mkdir()
    cfg = tmp_path / "cfg.yaml"
    cfg.write_text("kind: KwokctlConfiguration\n", encoding="utf-8")
    result_dir = tmp_path / "results"

    args = argparse.Namespace(
        trace_dir=str(trace_dir),
        kwokctl_config_file=str(cfg),
        result_dir=str(result_dir),
        cluster_name="kwok1",
        kwok_runtime="binary",
        namespace="trace",
        node_cpu="1000m",
        node_mem="1Gi",
        monitor_interval=1.0,
        log_level="INFO",
        job_file=None,
        save_scheduler_logs=False,
    )

    monkeypatch.setattr(tr.TraceReplayer, "_write_info_file", lambda self: None)
    monkeypatch.setattr(tr.TraceReplayer, "log_args", lambda self: None)

    replayer = tr.TraceReplayer(args)
    replayer.events = []
    replayer.trace_time = 10.0

    sleeps = []
    monkeypatch.setattr(tr.time, "time", lambda: 0.0)
    monkeypatch.setattr(tr.time, "sleep", lambda s: sleeps.append(s))

    replayer._replay_events(namespace="trace", start_wall_time=0.0, sim_t0=0.0)
    assert sleeps and sleeps[0] == pytest.approx(10.0)
