#!/usr/bin/env python3
# test_python_solver.py

import io, json

import pytest

from ortools.sat.python import cp_model
from scripts.python_solver.main import (
    CPSATSolver,
    NO_NODES,
    NO_PODS,
    main as solver_main,
)

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def go_payload(*, solver_input: dict, solver_options: dict | None = None) -> dict:
    """Match the exact JSON shape Go sends to the Python solver."""
    return {
        "solver_input": solver_input or {},
        "solver_options": solver_options or {},
    }


def assert_solver_output_schema(out: dict, *, expect_full: bool) -> None:
    assert isinstance(out, dict)
    assert "status" in out
    assert isinstance(out["status"], str)
    if not expect_full:
        return
    assert "placements" in out
    assert "evictions" in out
    assert "phases" in out
    assert "duration_ms" in out
    assert isinstance(out["placements"], list)
    assert isinstance(out["evictions"], list)
    assert isinstance(out["phases"], list)
    assert isinstance(out["duration_ms"], int)


def assert_placement_entry_schema(pl: dict) -> None:
    assert isinstance(pl, dict)
    assert set(["uid", "name", "namespace", "old_node", "node"]).issubset(pl.keys())
    assert isinstance(pl["uid"], str)
    assert isinstance(pl["name"], str)
    assert isinstance(pl["namespace"], str)
    assert isinstance(pl["old_node"], str)
    assert isinstance(pl["node"], str)


def assert_eviction_entry_schema(ev: dict) -> None:
    assert isinstance(ev, dict)
    assert set(["uid", "name", "namespace", "node"]).issubset(ev.keys())
    assert isinstance(ev["uid"], str)
    assert isinstance(ev["name"], str)
    assert isinstance(ev["namespace"], str)
    assert isinstance(ev["node"], str)


def make_node(name="n1", cpu=1000, mem=1_000_000_000):
    return {
        "name": name,
        "cap_cpu_m": cpu,
        "cap_mem_bytes": mem,
    }


def make_pod(
    uid: str,
    *,
    cpu: int = 100,
    mem: int = 100_000_000,
    priority: int = 0,
    node: str = "",
    namespace: str = "default",
    protected: bool = False,
):
    return {
        "uid": uid,
        "namespace": namespace,
        "name": f"pod-{uid}",
        "req_cpu_m": cpu,
        "req_mem_bytes": mem,
        "priority": priority,
        "protected": protected,
        "node": node,  # "" = pending
    }


# A small timeout that is still enough to get FEASIBLE/OPTIMAL on tiny models
DEFAULT_TIMEOUT_MS = 2000

# ---------------------------------------------------------------------------
# Solver: _status_str
# ---------------------------------------------------------------------------

@pytest.mark.parametrize(
    "value,expected",
    [
        (cp_model.OPTIMAL, "OPTIMAL"),
        (cp_model.FEASIBLE, "FEASIBLE"),
        (123456, "UNKNOWN"),  # unknown int
        ("FOO", "FOO"),       # passthrough string
    ],
)
def test_status_str_handles_int_and_string(value, expected):
    assert CPSATSolver._status_str(value) == expected


# ---------------------------------------------------------------------------
# Solver: _dedupe_pods_by_uid and _apply_preemptor_to_pods
# ---------------------------------------------------------------------------

def test_dedupe_pods_by_uid_prefers_running_record():
    solver = CPSATSolver()
    pods = [
        make_pod("p1", cpu=100, node=""),     # pending
        make_pod("p1", cpu=100, node="n1"),   # running should win
    ]
    out = solver._dedupe_pods_by_uid(pods)
    assert list(out.keys()) == ["p1"]
    assert out["p1"]["node"] == "n1"


def test_apply_preemptor_to_pods_adds_pending_if_missing():
    solver = CPSATSolver()
    pod_by_uid = {}
    preemptor = {
        "uid": "pre",
        "namespace": "default",
        "name": "pre",
        "req_cpu_m": 300,
        "req_mem_bytes": 100_000_000,
        "priority": 10,
        "protected": False,
    }

    pod_by_uid, single_mode, pre_uid = solver._apply_preemptor_to_pods(pod_by_uid, preemptor)
    assert single_mode is True
    assert pre_uid == "pre"
    assert "pre" in pod_by_uid
    assert pod_by_uid["pre"]["node"] == ""  # added as pending


def test_freeze_problem_builds_indices_and_eligible_nodes():
    solver = CPSATSolver()
    nodes = [
        make_node("n1", cpu=500, mem=1_000_000_000),
        make_node("n2", cpu=1000, mem=1_000_000_000),
    ]
    pods = [
        make_pod("p1", cpu=600, mem=100_000_000, node=""),   # pending; only fits n2
        make_pod("p2", cpu=200, mem=100_000_000, node="n1"), # running; fits both
    ]

    frozen = solver._freeze_problem(
        nodes=nodes,
        pods=pods,
        single_preemptor_mode=False,
        preemptor_uid=None,
    )
    assert isinstance(frozen, dict) is False

    # p1 pending, p2 running
    assert set(frozen.pending_idxs) == {0}
    assert set(frozen.running_idxs) == {1}

    # Eligible nodes:
    # p1 (600m) fits only n2 (idx 1)
    assert frozen.eligible_nodes[0] == [1]
    # p2 (200m) fits both
    assert frozen.eligible_nodes[1] == [0, 1]


# ---------------------------------------------------------------------------
# Solver: quick exits NO_NODES / NO_PODS
# ---------------------------------------------------------------------------

@pytest.mark.parametrize(
    "nodes,pods,expected_status",
    [
        ([], [make_pod("p1")], NO_NODES),
        ([make_node("n1")], [], NO_PODS),
    ],
)
def test_quick_exits_no_nodes_or_no_pods(nodes, pods, expected_status):
    solver = CPSATSolver()
    out = solver.solve(
        go_payload(
            solver_input={
                "nodes": nodes,
                "pods": pods,
                "timeout_ms": DEFAULT_TIMEOUT_MS,
            }
        )
    )
    assert_solver_output_schema(out, expect_full=False)
    assert out["status"] == expected_status
    # These early exits don't build a model, so there should be no placements/evictions.
    assert out.get("placements", []) == []
    assert out.get("evictions", []) == []


# ---------------------------------------------------------------------------
# Solver: simple feasible placement
# ---------------------------------------------------------------------------

def test_single_pending_pod_is_placed_on_single_node():
    solver = CPSATSolver()
    nodes = [make_node("n1", cpu=1000, mem=1_000_000_000)]
    pods = [make_pod("p1", cpu=200, mem=100_000_000, priority=0, node="")]

    out = solver.solve(
        go_payload(
            solver_input={
                "nodes": nodes,
                "pods": pods,
                "timeout_ms": DEFAULT_TIMEOUT_MS,
            }
        )
    )

    assert_solver_output_schema(out, expect_full=True)
    assert out["status"] in ("FEASIBLE", "OPTIMAL")
    placements = out["placements"]
    evictions = out["evictions"]

    # One placement for p1: old_node "" -> node "n1"
    assert len(placements) == 1
    pl = placements[0]
    assert_placement_entry_schema(pl)
    assert pl["uid"] == "p1"
    assert pl["old_node"] == ""
    assert pl["node"] == "n1"

    # No evictions in this trivial case
    assert evictions == []


# ---------------------------------------------------------------------------
# Solver: infeasible batch mode
# ---------------------------------------------------------------------------

def test_pending_pod_too_big_makes_model_infeasible():
    solver = CPSATSolver()
    # Node too small to fit the pod
    nodes = [make_node("n1", cpu=50, mem=100_000_000)]
    pods = [make_pod("p1", cpu=200, mem=100_000_000, priority=0, node="")]

    out = solver.solve(
        go_payload(
            solver_input={
                "nodes": nodes,
                "pods": pods,
                "timeout_ms": DEFAULT_TIMEOUT_MS,
            }
        )
    )

    assert_solver_output_schema(out, expect_full=True)
    # Because of the constraint "at least one pending pod must be placed",
    # this becomes globally infeasible.
    assert out["status"] == "INFEASIBLE"
    assert out["placements"] == []
    assert out["evictions"] == []


def test_pending_pod_too_big_in_global_mode_is_infeasible_even_with_preemptor_none():
    solver = CPSATSolver()
    nodes = [make_node("n1", cpu=50, mem=100_000_000)]
    pods = [make_pod("p1", cpu=200, mem=100_000_000, priority=0, node="")]

    out = solver.solve(
        go_payload(
            solver_input={
                "nodes": nodes,
                "pods": pods,
                "preemptor": None,
                "timeout_ms": DEFAULT_TIMEOUT_MS,
            }
        )
    )

    assert_solver_output_schema(out, expect_full=True)
    assert out["status"] == "INFEASIBLE"
    assert out["placements"] == []
    assert out["evictions"] == []


# ---------------------------------------------------------------------------
# Solver: duplicate UID handling
# ---------------------------------------------------------------------------

def test_duplicate_pod_records_use_single_running_copy():
    """
    If the same UID appears twice (one running, one pending), the solver should
    keep only one record, preferring the running one. Otherwise this tiny
    instance would be infeasible (capacity < sum of both copies).
    """
    solver = CPSATSolver()

    # Node capacity just enough for ONE pod of size 300, but not for two.
    nodes = [make_node("n1", cpu=300, mem=100_000_000)]

    pods = [
        make_pod("p1", cpu=300, mem=100_000_000, priority=0, node="n1"),  # running
        make_pod("p1", cpu=300, mem=100_000_000, priority=0, node=""),    # duplicate, pending
    ]

    out = solver.solve(
        go_payload(
            solver_input={
                "nodes": nodes,
                "pods": pods,
                "timeout_ms": DEFAULT_TIMEOUT_MS,
            }
        )
    )

    assert_solver_output_schema(out, expect_full=True)
    # If dedup is working, we only see the running copy,
    # no pending pods => no placements/evictions, and model is feasible.
    assert out["status"] in ("FEASIBLE", "OPTIMAL")
    assert out["placements"] == []
    assert out["evictions"] == []


# ---------------------------------------------------------------------------
# Solver: single-preemptor mode
# ---------------------------------------------------------------------------

def test_single_preemptor_is_placed_if_feasible():
    solver = CPSATSolver()
    nodes = [make_node("n1", cpu=1000, mem=1_000_000_000)]

    preemptor = {
        "uid": "pre",
        "namespace": "default",
        "name": "pre",
        "req_cpu_m": 300,
        "req_mem_bytes": 100_000_000,
        "priority": 10,
        "protected": False,
    }

    out = solver.solve(
        go_payload(
            solver_input={
                "nodes": nodes,
                "pods": [],  # will be auto-augmented with preemptor as pending
                "preemptor": preemptor,
                "timeout_ms": DEFAULT_TIMEOUT_MS,
            }
        )
    )

    assert_solver_output_schema(out, expect_full=True)
    assert out["status"] in ("FEASIBLE", "OPTIMAL")
    placements = out["placements"]
    evictions = out["evictions"]

    # Preemptor should be placed on n1
    assert len(placements) == 1
    pl = placements[0]
    assert_placement_entry_schema(pl)
    assert pl["uid"] == "pre"
    assert pl["old_node"] == ""
    assert pl["node"] == "n1"

    # No evictions; we only had the preemptor
    assert evictions == []


def test_single_preemptor_infeasible_if_it_cannot_fit_any_node():
    solver = CPSATSolver()
    # Node too small for preemptor
    nodes = [make_node("n1", cpu=50, mem=100_000_000)]

    preemptor = {
        "uid": "pre",
        "namespace": "default",
        "name": "pre",
        "req_cpu_m": 300,
        "req_mem_bytes": 100_000_000,
        "priority": 10,
        "protected": False,
    }

    out = solver.solve(
        go_payload(
            solver_input={
                "nodes": nodes,
                "pods": [],
                "preemptor": preemptor,
                "timeout_ms": DEFAULT_TIMEOUT_MS,
            }
        )
    )

    assert_solver_output_schema(out, expect_full=True)
    # Single-preemptor mode enforces sum(assign[pre]) == 1, but there are
    # no eligible nodes -> model is infeasible.
    assert out["status"] == "INFEASIBLE"
    assert out["placements"] == []
    assert out["evictions"] == []


# ---------------------------------------------------------------------------
# Solver: global/background mode (preemptor nil/absent)
# ---------------------------------------------------------------------------

def test_global_mode_preemptor_none_does_not_force_any_new_placement_when_all_pods_running():
    """
    When preemptor is None (global/background mode), the solver must NOT enforce
    placing a preemptor. If there are no pending pods, it also must not enforce
    any new placement.
    """
    solver = CPSATSolver()
    nodes = [make_node("n1", cpu=1000, mem=1_000_000_000)]
    pods = [make_pod("p1", cpu=200, mem=100_000_000, priority=0, node="n1")]

    out = solver.solve(
        go_payload(
            solver_input={
                "nodes": nodes,
                "pods": pods,
                "preemptor": None,
                "timeout_ms": DEFAULT_TIMEOUT_MS,
            }
        )
    )

    assert_solver_output_schema(out, expect_full=True)
    assert out["status"] in ("FEASIBLE", "OPTIMAL")
    assert out["placements"] == []
    assert out["evictions"] == []


def test_protected_running_pod_that_cannot_stay_is_model_invalid():
    solver = CPSATSolver()

    # Node too small for the pod's request.
    nodes = [make_node("n1", cpu=50, mem=100_000_000)]
    pods = [
        make_pod(
            "p1",
            cpu=200,
            mem=100_000_000,
            priority=0,
            node="n1",  # running
            protected=True,
        )
    ]

    out = solver.solve(
        go_payload(
            solver_input={
                "nodes": nodes,
                "pods": pods,
                "timeout_ms": DEFAULT_TIMEOUT_MS,
            }
        )
    )

    assert_solver_output_schema(out, expect_full=False)
    assert out["status"] == "MODEL_INVALID"


def test_unprotected_running_pod_is_evicted_when_capacity_cannot_fit_all_running_pods():
    solver = CPSATSolver()

    # Single node can fit only one 600m pod.
    nodes = [make_node("n1", cpu=600, mem=1_000_000_000)]
    pods = [
        make_pod("p1", cpu=600, mem=100_000_000, node="n1"),
        make_pod("p2", cpu=600, mem=100_000_000, node="n1"),
    ]

    out = solver.solve(
        go_payload(
            solver_input={
                "nodes": nodes,
                "pods": pods,
                "timeout_ms": DEFAULT_TIMEOUT_MS,
            }
        )
    )

    assert_solver_output_schema(out, expect_full=True)
    assert out["status"] in ("FEASIBLE", "OPTIMAL")
    assert out["placements"] == []  # kept pod stays; evicted pod shows in evictions
    assert len(out["evictions"]) == 1
    assert_eviction_entry_schema(out["evictions"][0])


def test_unprotected_running_pod_is_moved_when_second_node_allows_keeping_all_running_pods():
    solver = CPSATSolver()

    # n1 can fit only one pod; n2 can fit one pod. This forces a move (not an eviction)
    # because the placement objective maximizes placed pods.
    nodes = [
        make_node("n1", cpu=600, mem=1_000_000_000),
        make_node("n2", cpu=600, mem=1_000_000_000),
    ]
    pods = [
        make_pod("p1", cpu=600, mem=100_000_000, node="n1"),
        make_pod("p2", cpu=600, mem=100_000_000, node="n1"),
    ]

    out = solver.solve(
        go_payload(
            solver_input={
                "nodes": nodes,
                "pods": pods,
                "timeout_ms": DEFAULT_TIMEOUT_MS,
            }
        )
    )

    assert_solver_output_schema(out, expect_full=True)
    assert out["status"] in ("FEASIBLE", "OPTIMAL")
    assert out["evictions"] == []
    assert len(out["placements"]) == 1
    pl = out["placements"][0]
    assert_placement_entry_schema(pl)
    assert pl["old_node"] == "n1"
    assert pl["node"] == "n2"


# ---------------------------------------------------------------------------
# CLI / main(): Go↔Python contract and robustness
# ---------------------------------------------------------------------------

def run_main_with_stdin(monkeypatch, capsys, payload_dict):
    """
    Helper: run main() with given JSON-serializable payload as stdin,
    return decoded JSON stdout.
    """
    stdin_data = json.dumps(payload_dict)
    monkeypatch.setattr("sys.stdin", io.StringIO(stdin_data))
    solver_main()
    captured = capsys.readouterr()
    # main() should always print exactly one JSON object to stdout
    out_str = captured.out.strip()
    assert out_str, "Expected main() to print something on stdout"
    return json.loads(out_str)


def test_main_valid_instance_roundtrip(monkeypatch, capsys):
    """
    End-to-end: JSON -> stdin -> main() -> stdout -> JSON.
    Ensure the structure is correct and matches expectations.
    """
    payload = go_payload(
        solver_input={
            "nodes": [make_node("n1")],
            "pods": [make_pod("p1")],
            "timeout_ms": DEFAULT_TIMEOUT_MS,
        },
        solver_options={},
    )

    out = run_main_with_stdin(monkeypatch, capsys, payload)

    assert_solver_output_schema(out, expect_full=True)

    # For this simple case we expect at least a feasible solution and
    # that p1 appears in placements.
    assert out["status"] in ("FEASIBLE", "OPTIMAL")
    placed_uids = {pl["uid"] for pl in out["placements"]}
    assert "p1" in placed_uids


def test_main_go_interface_contract_roundtrip(monkeypatch, capsys):
    """Validate the Go↔Python stdin/stdout contract (payload wrapper + output keys)."""
    payload = go_payload(
        solver_input={
            "nodes": [make_node("n1")],
            "pods": [make_pod("p1", node="")],
            "baseline_score": {"placed_by_priority": {}, "evicted": 0, "moved": 0},
            "timeout_ms": DEFAULT_TIMEOUT_MS,
            "ignore_affinity": True,
        },
        solver_options={
            "log_progress": False,
            "gap_limit": 0.00,
            "guaranteed_tier_fraction": 0.40,
            "move_fraction_of_tier": 0.30,
        },
    )

    out = run_main_with_stdin(monkeypatch, capsys, payload)

    assert_solver_output_schema(out, expect_full=True)

    # Placements/evictions entries must follow Go json tags: uid/name/namespace/old_node/node.
    assert out["status"] in ("FEASIBLE", "OPTIMAL")
    assert any(
        pl.get("uid") == "p1" and pl.get("old_node") == "" and isinstance(pl.get("node"), str)
        for pl in out["placements"]
    )
    for pl in out["placements"]:
        assert_placement_entry_schema(pl)
    for ev in out["evictions"]:
        assert_eviction_entry_schema(ev)


@pytest.mark.parametrize(
    "payload,expected_status",
    [
        (go_payload(solver_input={"nodes": [], "pods": [make_pod("p1")], "timeout_ms": DEFAULT_TIMEOUT_MS}), NO_NODES),
        (go_payload(solver_input={"nodes": [make_node("n1")], "pods": [], "timeout_ms": DEFAULT_TIMEOUT_MS}), NO_PODS),
    ],
)
def test_main_quick_exits_only_require_status(monkeypatch, capsys, payload, expected_status):
    out = run_main_with_stdin(monkeypatch, capsys, payload)
    assert_solver_output_schema(out, expect_full=False)
    assert out["status"] == expected_status


def test_main_ignores_unknown_fields_in_payload(monkeypatch, capsys):
    """Go may add fields over time; Python solver should ignore unknown keys."""
    payload = {
        **go_payload(
            solver_input={
                "nodes": [make_node("n1")],
                "pods": [make_pod("p1", node="")],
                "timeout_ms": DEFAULT_TIMEOUT_MS,
                "some_future_field": {"x": 1},
            },
            solver_options={
                "gap_limit": 0.00,
                "unknown_option": 123,
            },
        ),
        "totally_unknown_top_level": "ok",
    }

    out = run_main_with_stdin(monkeypatch, capsys, payload)
    assert_solver_output_schema(out, expect_full=True)
    assert out["status"] in ("FEASIBLE", "OPTIMAL")


def test_main_invalid_json_returns_python_exception(monkeypatch, capsys):
    """
    main() should robustly handle invalid JSON on stdin and still
    emit a JSON object with status="PYTHON_EXCEPTION".
    """
    monkeypatch.setattr("sys.stdin", io.StringIO("{not-json}"))
    solver_main()
    captured = capsys.readouterr()
    out_str = captured.out.strip()
    data = json.loads(out_str)  # should still be valid JSON
    assert_solver_output_schema(data, expect_full=False)
    assert data["status"] == "PYTHON_EXCEPTION"
    # The exact error message is implementation-dependent, so we don't assert on it.
