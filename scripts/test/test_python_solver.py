# test_python_solver.py
import pytest

import io, json
from ortools.sat.python import cp_model

from scripts.python_solver.main import CPSATSolver, NO_NODES, NO_PODS, main as solver_main

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

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
# _status_str via parametrize
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
# Quick exits: NO_NODES / NO_PODS (parametrized)
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
        {
            "nodes": nodes,
            "pods": pods,
            "timeout_ms": DEFAULT_TIMEOUT_MS,
        }
    )
    assert out["status"] == expected_status
    # These early exits don't build a model, so there should be no placements/evictions.
    assert out.get("placements", []) == []
    assert out.get("evictions", []) == []


# ---------------------------------------------------------------------------
# Simple feasible placement
# ---------------------------------------------------------------------------

def test_single_pending_pod_is_placed_on_single_node():
    solver = CPSATSolver()
    nodes = [make_node("n1", cpu=1000, mem=1_000_000_000)]
    pods = [make_pod("p1", cpu=200, mem=100_000_000, priority=0, node="")]

    out = solver.solve(
        {
            "nodes": nodes,
            "pods": pods,
            "timeout_ms": DEFAULT_TIMEOUT_MS,
        }
    )

    assert out["status"] in ("FEASIBLE", "OPTIMAL")
    placements = out["placements"]
    evictions = out["evictions"]

    # One placement for p1, from "" to n1
    assert len(placements) == 1
    pl = placements[0]
    assert pl["pod"]["uid"] == "p1"
    assert pl["from_node"] == ""
    assert pl["to_node"] == "n1"

    # No evictions in this trivial case
    assert evictions == []


# ---------------------------------------------------------------------------
# Infeasible batch mode: pending pod cannot fit anywhere
# ---------------------------------------------------------------------------

def test_pending_pod_too_big_makes_model_infeasible():
    solver = CPSATSolver()
    # Node too small to fit the pod
    nodes = [make_node("n1", cpu=50, mem=100_000_000)]
    pods = [make_pod("p1", cpu=200, mem=100_000_000, priority=0, node="")]

    out = solver.solve(
        {
            "nodes": nodes,
            "pods": pods,
            "timeout_ms": DEFAULT_TIMEOUT_MS,
        }
    )

    # Because of the constraint "at least one pending pod must be placed",
    # this becomes globally infeasible.
    assert out["status"] == "INFEASIBLE"
    assert out["placements"] == []
    assert out["evictions"] == []


# ---------------------------------------------------------------------------
# Duplicate pod UID handling (input correctness)
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
        {
            "nodes": nodes,
            "pods": pods,
            "timeout_ms": DEFAULT_TIMEOUT_MS,
        }
    )

    # If dedup is working, we only see the running copy,
    # no pending pods => no placements/evictions, and model is feasible.
    assert out["status"] in ("FEASIBLE", "OPTIMAL")
    assert out["placements"] == []
    assert out["evictions"] == []


# ---------------------------------------------------------------------------
# Single-preemptor mode
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
        {
            "nodes": nodes,
            "pods": [],  # will be auto-augmented with preemptor as pending
            "preemptor": preemptor,
            "timeout_ms": DEFAULT_TIMEOUT_MS,
        }
    )

    assert out["status"] in ("FEASIBLE", "OPTIMAL")
    placements = out["placements"]
    evictions = out["evictions"]

    # Preemptor should be placed on n1
    assert len(placements) == 1
    pl = placements[0]
    assert pl["pod"]["uid"] == "pre"
    assert pl["from_node"] == ""
    assert pl["to_node"] == "n1"

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
        {
            "nodes": nodes,
            "pods": [],
            "preemptor": preemptor,
            "timeout_ms": DEFAULT_TIMEOUT_MS,
        }
    )

    # Single-preemptor mode enforces sum(assign[pre]) == 1, but there are
    # no eligible nodes -> model is infeasible.
    assert out["status"] == "INFEASIBLE"
    assert out["placements"] == []
    assert out["evictions"] == []

# ---------------------------------------------------------------------------
# CLI / main() tests – input/output handling
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
    payload = {
        "nodes": [make_node("n1")],
        "pods": [make_pod("p1")],
        "timeout_ms": DEFAULT_TIMEOUT_MS,
    }

    out = run_main_with_stdin(monkeypatch, capsys, payload)

    # Check basic shape
    assert "status" in out
    assert "placements" in out
    assert "evictions" in out
    assert "stages" in out
    assert isinstance(out["placements"], list)
    assert isinstance(out["evictions"], list)
    assert isinstance(out["stages"], list)

    # For this simple case we expect at least a feasible solution and
    # that p1 appears in placements.
    assert out["status"] in ("FEASIBLE", "OPTIMAL")
    placed_uids = {pl["pod"]["uid"] for pl in out["placements"]}
    assert "p1" in placed_uids


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
    assert data["status"] == "PYTHON_EXCEPTION"
    # The exact error message is implementation-dependent, so we don't assert on it.
