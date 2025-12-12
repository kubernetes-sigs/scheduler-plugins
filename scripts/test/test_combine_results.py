import csv
import math
from pathlib import Path

from scripts.kwok_workload_once import combine_results as cr


CSV_COLUMNS = [
    "seed",
    "util_run_cpu_now",
    "util_run_mem_now",
    "running_placed_by_prio_now",
    "unscheduled_count_before",
    "error",
    "best_solver_status",
    "best_solver_name",
    "best_solver_duration_ms",
    "best_solver_score",
]


def _write_results_csv(path: Path, rows: list[dict]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=CSV_COLUMNS)
        writer.writeheader()
        for row in rows:
            writer.writerow({k: row.get(k, "") for k in CSV_COLUMNS})


def test_helpers_rate_dirname_and_json_parsing():
    assert cr.parse_solver_dirname("not-a-match") is None
    meta = cr.parse_solver_dirname("nodes2_pods10_prio3_util050_timeout10")
    assert meta is not None
    assert meta["nodes"] == 2
    assert meta["pods"] == 10
    assert meta["priorities"] == 3
    assert meta["util"] == 50.0
    assert meta["timeout"] == 10
    assert meta["pods_per_node"] == 5
    assert meta["default_dirname"] == "nodes2_pods10_prio3_util050"

    assert math.isnan(cr.rate(1, 0))
    assert cr.rate(2, 4) == 0.5

    assert cr.strip_outer_quotes(None) is None
    assert cr.strip_outer_quotes('"x"') == "x"
    assert cr.strip_outer_quotes("'x'") == "x"
    assert cr.strip_outer_quotes("x") == "x"

    assert cr.parse_json_cell(None) is None
    assert cr.parse_json_cell("") is None
    assert cr.parse_json_cell("  ") is None
    assert cr.parse_json_cell('{"a": 1}') == {"a": 1}
    assert cr.parse_json_cell("not-json") is None


def test_load_csv_and_default_vs_solver_per_seed(tmp_path: Path):
    solver_csv = tmp_path / "solver" / "results.csv"
    default_csv = tmp_path / "default" / "results.csv"

    # Build a small set of seeds that cover all category branches.
    solver_rows = [
        # all running
        {
            "seed": "s_all",
            "util_run_cpu_now": "10",
            "util_run_mem_now": "100",
            "running_placed_by_prio_now": '{"p2": 1}',
            "unscheduled_count_before": "0",
            "best_solver_status": "OPTIMAL",
            "best_solver_name": "solver",
            "best_solver_duration_ms": "5",
        },
        # default optimal (equal placement)
        {
            "seed": "s_def_opt",
            "util_run_cpu_now": "11",
            "util_run_mem_now": "110",
            "running_placed_by_prio_now": '{"p2": 1}',
            "unscheduled_count_before": "1",
            "best_solver_status": "OPTIMAL",
            "best_solver_name": "solver",
            "best_solver_duration_ms": "10",
        },
        # solver optimal (better placement)
        {
            "seed": "s_sol_opt",
            "util_run_cpu_now": "12",
            "util_run_mem_now": "120",
            "running_placed_by_prio_now": '{"p2": 2}',
            "unscheduled_count_before": "1",
            "best_solver_status": "OPTIMAL",
            "best_solver_name": "solver",
            "best_solver_duration_ms": "11",
        },
        # solver feasible (better placement)
        {
            "seed": "s_sol_feas",
            "util_run_cpu_now": "13",
            "util_run_mem_now": "130",
            "running_placed_by_prio_now": '{"p2": 2}',
            "unscheduled_count_before": "1",
            "best_solver_status": "FEASIBLE",
            "best_solver_name": "solver",
            "best_solver_duration_ms": "12",
        },
        # solver failed
        {
            "seed": "s_fail",
            "util_run_cpu_now": "14",
            "util_run_mem_now": "140",
            "running_placed_by_prio_now": '{"p2": 0}',
            "unscheduled_count_before": "1",
            "best_solver_status": "FAILED",
            "best_solver_name": "",
            "best_solver_duration_ms": "",
        },
    ]

    default_rows = [
        {
            "seed": "s_all",
            "util_run_cpu_now": "9",
            "util_run_mem_now": "90",
            "running_placed_by_prio_now": '{"p2": 1}',
            "unscheduled_count_before": "0",
            "best_solver_status": "",
            "best_solver_name": "",
            "best_solver_duration_ms": "",
        },
        {
            "seed": "s_def_opt",
            "util_run_cpu_now": "10",
            "util_run_mem_now": "100",
            "running_placed_by_prio_now": '{"p2": 1}',
            "unscheduled_count_before": "1",
            "best_solver_status": "",
            "best_solver_name": "",
            "best_solver_duration_ms": "",
        },
        {
            "seed": "s_sol_opt",
            "util_run_cpu_now": "10",
            "util_run_mem_now": "100",
            "running_placed_by_prio_now": '{"p2": 1}',
            "unscheduled_count_before": "1",
            "best_solver_status": "",
            "best_solver_name": "",
            "best_solver_duration_ms": "",
        },
        {
            "seed": "s_sol_feas",
            "util_run_cpu_now": "10",
            "util_run_mem_now": "100",
            "running_placed_by_prio_now": '{"p2": 1}',
            "unscheduled_count_before": "1",
            "best_solver_status": "",
            "best_solver_name": "",
            "best_solver_duration_ms": "",
        },
        {
            "seed": "s_fail",
            "util_run_cpu_now": "10",
            "util_run_mem_now": "100",
            "running_placed_by_prio_now": '{"p2": 1}',
            "unscheduled_count_before": "1",
            "best_solver_status": "",
            "best_solver_name": "",
            "best_solver_duration_ms": "",
        },
    ]

    _write_results_csv(solver_csv, solver_rows)
    _write_results_csv(default_csv, default_rows)

    df_s = cr.load_csv(solver_csv)
    assert "placed_by_prio" in df_s.columns
    assert df_s["seed"].tolist() == ["s_all", "s_def_opt", "s_sol_opt", "s_sol_feas", "s_fail"]

    joined = cr.default_vs_solver_per_seed(solver_csv, default_csv, cfg_name="cfg")
    assert len(joined) == 5
    assert joined.loc[joined["seed"] == "s_all", "default_all_running"].item() is True
    assert joined.loc[joined["seed"] == "s_def_opt", "placed_cmp"].item() == 0
    assert joined.loc[joined["seed"] == "s_sol_opt", "placed_cmp"].item() == 1

    counts = cr.CombineResultsAnalyzer._compute_category_counts(joined)
    assert counts.n_seeds == 5
    assert counts.n_default_all_running == 1
    assert counts.n_default_optimal == 1
    assert counts.n_solver_optimal == 1
    assert counts.n_solver_feasible == 1
    assert counts.n_solver_failed == 1
    assert counts.n_solver_improve == 2
    assert counts.n_other == 0


def test_analyze_combo_skips_missing_dirs_and_files(tmp_path: Path, capsys):
    args = cr.CombineResultsArgs(
        results_root=tmp_path,
        solver_dir="solver",
        default_dir="default",
        results_csv="results.csv",
        out_dir=tmp_path / "analysis",
        decimals=2,
    )
    analyzer = cr.CombineResultsAnalyzer(args)

    meta = cr.parse_solver_dirname("nodes1_pods1_prio1_util050_timeout10")
    assert meta is not None

    solver_dir = tmp_path / "solver" / "nodes1_pods1_prio1_util050_timeout10"
    default_dir = tmp_path / "default" / meta["default_dirname"]

    # missing default dir
    assert analyzer.analyze_combo(solver_dir=solver_dir, default_dir=default_dir, meta=meta) is None
    out = capsys.readouterr().out
    assert "default folder missing" in out

    # create default dir but missing solver results.csv
    default_dir.mkdir(parents=True)
    assert analyzer.analyze_combo(solver_dir=solver_dir, default_dir=default_dir, meta=meta) is None
    out = capsys.readouterr().out
    assert "solver results.csv missing" in out

    # create solver csv but missing default results.csv
    solver_dir.mkdir(parents=True)
    _write_results_csv(solver_dir / "results.csv", [])
    assert analyzer.analyze_combo(solver_dir=solver_dir, default_dir=default_dir, meta=meta) is None
    out = capsys.readouterr().out
    assert "default results.csv missing" in out


def test_analyze_combo_emits_warn_branches_when_patched(tmp_path: Path, capsys, monkeypatch):
    # Minimal valid directory structure
    solver_dir = tmp_path / "solver" / "nodes1_pods1_prio1_util050_timeout10"
    default_dir = tmp_path / "default" / "nodes1_pods1_prio1_util050"
    solver_csv = solver_dir / "results.csv"
    default_csv = default_dir / "results.csv"

    _write_results_csv(
        solver_csv,
        [
            {
                "seed": "s1",
                "util_run_cpu_now": "10",
                "util_run_mem_now": "100",
                "running_placed_by_prio_now": '{"p1": 1}',
                "unscheduled_count_before": "1",
                "best_solver_status": "OPTIMAL",
                "best_solver_name": "solver",
                "best_solver_duration_ms": "1",
            }
        ],
    )
    _write_results_csv(
        default_csv,
        [
            {
                "seed": "s1",
                "util_run_cpu_now": "9",
                "util_run_mem_now": "90",
                "running_placed_by_prio_now": '{"p1": 1}',
                "unscheduled_count_before": "1",
                "best_solver_status": "",
                "best_solver_name": "",
                "best_solver_duration_ms": "",
            }
        ],
    )

    args = cr.CombineResultsArgs(
        results_root=tmp_path,
        solver_dir="solver",
        default_dir="default",
        results_csv="results.csv",
        out_dir=tmp_path / "analysis",
        decimals=2,
    )
    analyzer = cr.CombineResultsAnalyzer(args)
    meta = cr.parse_solver_dirname(solver_dir.name)
    assert meta is not None

    # Force the warning branches inside analyze_combo
    def fake_counts(_df):
        return cr.CategoryCounts(
            n_seeds=1,
            n_seeds_not_all_running=1,
            n_default_all_running=0,
            n_solver_called=0,
            n_default_optimal=0,
            n_solver_optimal=0,
            n_solver_feasible=0,
            n_solver_failed=0,
            n_solver_improve=0,
            n_other=0,
            default_all_running_rate=2.0,
            solver_called_rate=2.0,
            default_optimal_rate=2.0,
            solver_optimal_rate=2.0,
            solver_feasible_rate=2.0,
            solver_failed_rate=2.0,
            solver_improve_rate=2.0,
            other_rate=2.0,
        )

    monkeypatch.setattr(cr.CombineResultsAnalyzer, "_compute_category_counts", staticmethod(fake_counts))
    row = analyzer.analyze_combo(solver_dir=solver_dir, default_dir=default_dir, meta=meta)
    assert row is not None

    out = capsys.readouterr().out
    assert "rates sum > 1.00" in out
    assert "category counts sum" in out


def test_run_writes_per_combo_csv_and_skips_bad_folder(tmp_path: Path, capsys):
    results_root = tmp_path / "results"
    solver_root = results_root / "solver"
    default_root = results_root / "default"

    good_solver_dir = solver_root / "nodes1_pods1_prio1_util050_timeout10"
    bad_solver_dir = solver_root / "bad-name"

    good_default_dir = default_root / "nodes1_pods1_prio1_util050"

    _write_results_csv(
        good_solver_dir / "results.csv",
        [
            {
                "seed": "s1",
                "util_run_cpu_now": "10",
                "util_run_mem_now": "100",
                "running_placed_by_prio_now": '{"p1": 1}',
                "unscheduled_count_before": "0",
                "best_solver_status": "OPTIMAL",
                "best_solver_name": "solver",
                "best_solver_duration_ms": "1",
            }
        ],
    )
    _write_results_csv(
        good_default_dir / "results.csv",
        [
            {
                "seed": "s1",
                "util_run_cpu_now": "9",
                "util_run_mem_now": "90",
                "running_placed_by_prio_now": '{"p1": 1}',
                "unscheduled_count_before": "0",
                "best_solver_status": "",
                "best_solver_name": "",
                "best_solver_duration_ms": "",
            }
        ],
    )

    bad_solver_dir.mkdir(parents=True, exist_ok=True)

    args = cr.CombineResultsArgs(
        results_root=results_root,
        solver_dir="solver",
        default_dir="default",
        results_csv="results.csv",
        out_dir=tmp_path / "analysis",
        decimals=None,
    )
    cr.CombineResultsAnalyzer(args).run()

    out = capsys.readouterr().out
    assert "bad folder name" in out
    assert "wrote" in out

    out_csv = tmp_path / "analysis" / "per_combo_results.csv"
    assert out_csv.exists()
    text = out_csv.read_text(encoding="utf-8")
    assert "config_dir" in text
    assert "nodes1_pods1_prio1_util050_timeout10" in text


def test_main_parses_args_and_decimals_disable(tmp_path: Path, monkeypatch):
    seen = {}

    def fake_run(self):
        seen["args"] = self.args

    monkeypatch.setattr(cr.CombineResultsAnalyzer, "run", fake_run)

    cr.main(
        [
            "--results-root",
            str(tmp_path),
            "--solver-dir",
            "solver",
            "--default-dir",
            "default",
            "--out-dir",
            str(tmp_path / "analysis"),
            "--decimals",
            "-1",
        ]
    )

    assert "args" in seen
    assert seen["args"].results_root == tmp_path
    assert seen["args"].decimals is None
