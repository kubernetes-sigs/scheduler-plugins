
# test_general_helpers.py.

import io
import logging
import re
import sys
from pathlib import Path

import pytest

from scripts.helpers import general_helpers as gh

# ---------------------------------------------------------------------------
# Time helpers
# ---------------------------------------------------------------------------

def test_get_timestamp_format():
    ts = gh.get_timestamp()
    # Format: YYYY/MM/DD/HH:MM:SS
    assert isinstance(ts, str)
    assert re.fullmatch(r"\d{4}/\d{2}/\d{2}/\d{2}:\d{2}:\d{2}", ts)

@pytest.mark.parametrize(
    "seconds,expected",
    [
        (0, "0s"),
        (5, "5s"),
        (59, "59s"),
        (60, "1m"),
        (61, "1m1s"),
        (3599, "59m59s"),
        (3600, "1h"),
        (3661, "1h1m1s"),
        (-5, "0s"),
    ],
)
def test_format_hms(seconds, expected):
    assert gh.format_hms(seconds) == expected

# ---------------------------------------------------------------------------
# Logging helpers
# ---------------------------------------------------------------------------

def test_prefix_filter_injects_prefix():
    f = gh.PrefixFilter("[pfx] ")
    rec = logging.LogRecord(
        name="x",
        level=logging.INFO,
        pathname=__file__,
        lineno=1,
        msg="hello",
        args=(),
        exc_info=None,
    )
    assert f.filter(rec) is True
    assert getattr(rec, "prefix") == "[pfx] "

def test_setup_logging_formats_with_prefix_and_level(capsys):
    logger = gh.setup_logging("test-gh-logger", prefix="[w1] ", level="DEBUG")
    logger.debug("hello")
    out = capsys.readouterr().out
    assert "[w1] hello" in out

def test_make_header_footer_basic():
    header, footer = gh.make_header_footer("Running tests", width=20, border="=")
    assert isinstance(header, str) and isinstance(footer, str)
    # Same width
    assert len(header) == len(footer)
    # Message should be present with spaces around it
    assert " Running tests " in header
    # Footer is all border characters
    assert set(footer) == {"="}

# ---------------------------------------------------------------------------
# CLI / info bundle helpers
# ---------------------------------------------------------------------------

def test_build_cli_cmd(monkeypatch):
    # Simulate a simple argv
    monkeypatch.setattr(sys, "argv", ["myscript.py", "arg1", "arg two"])
    cmd = gh.build_cli_cmd()
    # Should start with "python3 " and contain proper quoting
    assert cmd.startswith("python3 ")
    # Ensure script name and args appear
    assert "myscript.py" in cmd
    assert "arg1" in cmd
    # "arg two" should appear as a single quoted argument
    assert "'arg two'" in cmd or '"arg two"' in cmd

@pytest.mark.parametrize(
    "value,expected",
    [
        (None, "<unset>"),
        ("", "<unset>"),
        ("foo", "foo"),
        (123, "123"),
    ],
)

def test_log_field_fmt(value, expected):
    assert gh.log_field_fmt(value) == expected

# ---------------------------------------------------------------------------
# Parser helpers
# ---------------------------------------------------------------------------

@pytest.mark.parametrize(
    "doc,key_combo,expected",
    [
        ({"single": 5}, ("single", "lo", "hi"), "5"),
        ({"single": " 10 "}, ("single", "lo", "hi"), "10"),
        ({"single": [1, 2]}, ("single", "lo", "hi"), "1,2"),
        ({"single": {"lo": "3", "hi": "7"}}, ("single", "lo", "hi"), "3,7"),
        ({"lo": "4", "hi": "8"}, ("single", "lo", "hi"), "4,8"),
        ({}, ("single", "lo", "hi"), None),
    ],
)

def test_normalize_interval(doc, key_combo, expected):
    assert gh.normalize_interval(doc, key_combo) == expected


def test_normalize_interval_force_empty_string():
    # allow_none=False should return "" when nothing found
    doc = {}
    key_combo = ("single", "lo", "hi")
    assert gh.normalize_interval(doc, key_combo, allow_none=False) == ""

@pytest.mark.parametrize(
    "s,expected",
    [
        ("10", 10.0),
        ("10s", 10.0),
        ("2m", 120.0),
        ("1h", 3600.0),
        ("1d", 86400.0),
        ("  3m  ", 180.0),
    ],
)

def test_parse_duration_to_seconds_valid(s, expected):
    assert gh.parse_duration_to_seconds(s) == pytest.approx(expected)

@pytest.mark.parametrize("s", ["", "s", "5x", "  "])
def test_parse_duration_to_seconds_invalid(s):
    with pytest.raises(ValueError):
        gh.parse_duration_to_seconds(s)

@pytest.mark.parametrize(
    "s,min_lo,expected",
    [
        ("5", 1, (5, 5)),
        ("0", 1, (1, 1)),
        ("1,10", 1, (1, 10)),
        ("0,0", 1, (1, 1)),
        ("10,5", 1, (10, 10)),  # hi >= lo
    ],
)

def test_parse_int_interval_valid(s, min_lo, expected):
    assert gh.parse_int_interval(s, min_lo=min_lo) == expected

def test_parse_int_interval_none():
    assert gh.parse_int_interval(None) is None
    assert gh.parse_int_interval("") is None

@pytest.mark.parametrize(
    "s,expected",
    [
        ("10m", ("10m", "10m")),
        ("10m,20m", ("10m", "20m")),
        (" a , b ", ("a", "b")),
    ],
)

def test_parse_qty_interval_valid(s, expected):
    assert gh.parse_qty_interval(s) == expected

def test_parse_qty_interval_none():
    assert gh.parse_qty_interval(None) is None
    assert gh.parse_qty_interval("") is None

@pytest.mark.parametrize(
    "t,default,expected",
    [
        (None, 60, 60),
        ("", 60, 60),
        ("10ms", 60, 1),  # 10ms -> 0.01s -> min 1
        ("1500ms", 60, 1),  # 1.5s -> cast -> 1
        ("10s", 60, 10),
        ("2m", 60, 120),
        ("1h", 60, 3600),
        ("42", 60, 42),
    ],
)

def test_parse_timeout_s_valid(t, default, expected):
    assert gh.parse_timeout_s(t, default=default) == expected

def test_parse_timeout_s_invalid_uses_default():
    assert gh.parse_timeout_s("notanumber", default=99) == 99

@pytest.mark.parametrize(
    "value,default,expected",
    [
        (None, False, False),
        (None, True, True),
        (True, False, True),
        (False, True, False),
        ("1", False, True),
        ("true", False, True),
        ("yes", False, True),
        ("y", False, True),
        ("on", False, True),
        ("0", True, False),
        ("false", True, False),
        ("no", True, False),
        ("n", True, False),
        ("off", True, False),
        ("maybe", False, False),  # falls back to default
    ],
)

def test_coerce_bool(value, default, expected):
    assert gh.coerce_bool(value, default=default) == expected

@pytest.mark.parametrize(
    "value,expected",
    [
        (None, None),
        ("", None),
        ("   ", None),
        ("abc", "abc"),
        ("  abc  ", "abc"),
    ],
)

def test_get_str(value, expected):
    assert gh.get_str(value) == expected

def test_get_int_from_dict():
    doc = {"a": "10", "b": 5.5, "c": "notint"}
    assert gh.get_int_from_dict(doc, "a", 0) == 10
    assert gh.get_int_from_dict(doc, "b", 0) == 5
    assert gh.get_int_from_dict(doc, "c", 42) == 42
    assert gh.get_int_from_dict(doc, "missing", 7) == 7

def test_get_float_from_dict():
    doc = {"a": "10.5", "b": 5, "c": "notfloat"}
    assert gh.get_float_from_dict(doc, "a", 0.0) == 10.5
    assert gh.get_float_from_dict(doc, "b", 0.0) == 5.0
    assert gh.get_float_from_dict(doc, "c", 3.14) == 3.14
    assert gh.get_float_from_dict(doc, "missing", 2.71) == 2.71

def test_get_str_from_dict():
    doc = {"a": "  hello  ", "b": "   ", "c": None}
    assert gh.get_str_from_dict(doc, "a", "default") == "hello"
    # empty/whitespace -> default
    assert gh.get_str_from_dict(doc, "b", "default") == "default"
    # None -> default
    assert gh.get_str_from_dict(doc, "c", "default") == "default"
    # missing -> default
    assert gh.get_str_from_dict(doc, "missing", "default") == "default"

# ---------------------------------------------------------------------------
# Quantity helpers
# ---------------------------------------------------------------------------

@pytest.mark.parametrize(
    "token,expected",
    [
        ("250m", 250),
        ("0.25", 250),
        ("1.5", 1500),
        ("2cpu", 2000),
        ("2 cores", 2000),
        (" 1000m ", 1000),
        ("0m", 1),  # clamped to >=1
    ],
)

def test_qty_to_mcpu_int_valid(token, expected):
    assert gh.qty_to_mcpu_int(token) == expected

@pytest.mark.parametrize("token", ["abc", "1x", "milli", "cpu"])
def test_qty_to_mcpu_int_invalid(token):
    with pytest.raises(ValueError):
        gh.qty_to_mcpu_int(token)

@pytest.mark.parametrize(
    "token,expected",
    [
        ("1024", 1024),
        ("1Ki", 1024),
        ("1Mi", 1024**2),
        ("1Gi", 1024**3),
        ("1.5Gi", int(1.5 * 1024**3)),
        ("500MB", 500 * 10**6),
        ("4G", 4 * 10**9),
        ("0", 1),  # clamped to >=1
    ],
)

def test_qty_to_bytes_int_valid(token, expected):
    assert gh.qty_to_bytes_int(token) == expected

@pytest.mark.parametrize("token", ["abc", "1xb", "GiB", " "])
def test_qty_to_bytes_int_invalid(token):
    with pytest.raises(ValueError):
        gh.qty_to_bytes_int(token)

@pytest.mark.parametrize(
    "m,expected",
    [
        (100, "100m"),
        (1000, "1"),
        (1500, "1500m"),
        (0, "0"),  # 0m → 0 cores, consistent with function
    ],
)

def test_qty_to_mcpu_str(m, expected):
    assert gh.qty_to_mcpu_str(m) == expected

@pytest.mark.parametrize(
    "b,expected",
    [
        (1024, "1024"),
        (0, "1"),  # clamped to >=1
        (-5, "1"),  # clamped to >=1
    ],
)

def test_qty_to_bytes_str(b, expected):
    assert gh.qty_to_bytes_str(b) == expected

# ---------------------------------------------------------------------------
# CSV helpers
# ---------------------------------------------------------------------------

def test_csv_read_header_and_append_row(tmp_path: Path):
    csv_path = tmp_path / "test.csv"

    # Initially file doesn't exist -> csv_read_header returns None
    assert gh.csv_read_header(csv_path) is None

    header = ["a", "b"]
    row = {"a": "1", "b": "2"}
    gh.csv_append_row(csv_path, header, row)

    # Header should now be present
    assert gh.csv_read_header(csv_path) == header

    # Append row with missing field 'b' -> should be empty string for 'b'
    row2 = {"a": "3"}
    gh.csv_append_row(csv_path, header, row2)

    content = csv_path.read_text(encoding="utf-8").strip().splitlines()
    # content[0] is header, content[1], content[2] are rows
    assert content[0] == "a,b"
    assert content[1] == "1,2"
    assert content[2] == "3,"  # missing 'b' -> empty string

def test_csv_append_row_rejects_extra_fields(tmp_path: Path):
    csv_path = tmp_path / "test_extra.csv"
    header = ["a", "b"]
    row = {"a": "1", "b": "2", "c": "3"}  # extra field 'c'

    with pytest.raises(ValueError):
        gh.csv_append_row(csv_path, header, row)

def test_csv_read_header_empty_file(tmp_path: Path):
    csv_path = tmp_path / "empty.csv"
    csv_path.write_text("", encoding="utf-8")
    assert gh.csv_read_header(csv_path) is None

# ---------------------------------------------------------------------------
# Seed helpers
# ---------------------------------------------------------------------------

def test_derive_seed_deterministic_and_dependent_on_labels():
    s1 = gh.derive_seed(123, "a", "b")
    s2 = gh.derive_seed(123, "a", "b")
    s3 = gh.derive_seed(123, "a", "c")
    s4 = gh.derive_seed(123, "a", "b", nbytes=8)

    # Deterministic for same inputs
    assert s1 == s2
    # Changing a label changes the seed
    assert s1 != s3
    # Different nbytes gives different value range
    assert s1 != s4

    # Seed value should be non-negative integer
    assert isinstance(s1, int)
    assert s1 >= 0

def test_seeded_random_reproducible():
    r1 = gh.seeded_random(42, "label")
    r2 = gh.seeded_random(42, "label")
    r3 = gh.seeded_random(42, "other")

    seq1 = [r1.random() for _ in range(5)]
    seq2 = [r2.random() for _ in range(5)]
    seq3 = [r3.random() for _ in range(5)]

    # Same base_seed and labels -> identical sequence
    assert seq1 == seq2
    # Different labels -> different sequence (very high probability)
    assert seq1 != seq3

@pytest.mark.parametrize(
    "args",
    [
        None,
        [],
        ["only-path"],
        ["p", "1", "2", "3"],
    ],
)

def test_generate_seeds_invalid_arity(args):
    with pytest.raises(SystemExit):
        gh.generate_seeds(args)

@pytest.mark.parametrize(
    "args",
    [
        ["p", "notint"],
        ["p", "0"],
        ["p", "2", "bad"],
        ["p", "2", "0"],
        ["p", "2", "3"],  # parts > total
    ],
)

def test_generate_seeds_invalid_numbers(args):
    with pytest.raises(SystemExit):
        gh.generate_seeds(args)

def test_generate_seeds_single_file(tmp_path: Path, monkeypatch, capsys):
    monkeypatch.setattr(gh.time, "time_ns", lambda: 123456789)
    out_file = tmp_path / "seeds.txt"
    gh.generate_seeds([str(out_file), "3"])

    lines = out_file.read_text(encoding="utf-8").strip().splitlines()
    assert len(lines) == 3
    assert all(int(x) > 0 for x in lines)
    out = capsys.readouterr().out
    assert "wrote 3 seeds" in out

def test_generate_seeds_template_parts(tmp_path: Path, monkeypatch):
    monkeypatch.setattr(gh.time, "time_ns", lambda: 999)
    tmpl = tmp_path / "seed-{i}.txt"
    gh.generate_seeds([str(tmpl), "2", "2"])
    p1 = tmp_path / "seed-1.txt"
    p2 = tmp_path / "seed-2.txt"
    assert p1.exists() and p2.exists()
    assert len(p1.read_text(encoding="utf-8").strip().splitlines()) == 1
    assert len(p2.read_text(encoding="utf-8").strip().splitlines()) == 1

def test_generate_seeds_non_template_parts_add_suffix(tmp_path: Path, monkeypatch):
    monkeypatch.setattr(gh.time, "time_ns", lambda: 1000)
    outp = tmp_path / "seeds.out"
    gh.generate_seeds([str(outp), "3", "2"])
    # Should produce seeds-1.out and seeds-2.out
    p1 = tmp_path / "seeds-1.out"
    p2 = tmp_path / "seeds-2.out"
    assert p1.exists() and p2.exists()

# ---------------------------------------------------------------------------
# HTTP helper functions (no real network)
# ---------------------------------------------------------------------------

def test_solver_trigger_http_success(monkeypatch):
    class _Resp:
        status = 204

        def __enter__(self):
            return self

        def __exit__(self, exc_type, exc, tb):
            return False

        def read(self):
            return b"{}"

    monkeypatch.setattr(gh._urlreq, "urlopen", lambda req, timeout=None: _Resp())
    logger = gh.setup_logging("test-gh-http", prefix="[t] ")
    code, body = gh.solver_trigger_http(logger, "http://example/solve", timeout=0.1)
    assert code == 204
    assert body == "{}"

def test_solver_trigger_http_http_error(monkeypatch):
    def raise_http_error(req, timeout=None):
        raise gh._urlerr.HTTPError(
            url=req.full_url,
            code=500,
            msg="boom",
            hdrs=None,
            fp=io.BytesIO(b"bad"),
        )

    monkeypatch.setattr(gh._urlreq, "urlopen", raise_http_error)
    logger = gh.setup_logging("test-gh-http-err", prefix="[t] ")
    code, body = gh.solver_trigger_http(logger, "http://example/solve", timeout=0.1)
    assert code == 500
    assert body == "bad"

def test_solver_trigger_http_connect_failure(monkeypatch):
    monkeypatch.setattr(gh._urlreq, "urlopen", lambda req, timeout=None: (_ for _ in ()).throw(OSError("down")))
    logger = gh.setup_logging("test-gh-http-fail", prefix="[t] ")
    code, body = gh.solver_trigger_http(logger, "http://example/solve", timeout=0.1)
    assert code == 0
    assert "connect-failed" in body

def test_get_solver_active_status_http_success(monkeypatch):
    class _Resp:
        status = 200

        def __enter__(self):
            return self

        def __exit__(self, exc_type, exc, tb):
            return False

        def read(self):
            return b"{\"active\":true}"

    monkeypatch.setattr(gh._urlreq, "urlopen", lambda req, timeout=None: _Resp())
    code, body = gh.get_solver_active_status_http("http://example/active", timeout=0.1)
    assert code == 200
    assert "active" in body

def test_get_solver_active_status_http_http_error(monkeypatch):
    def raise_http_error(req, timeout=None):
        raise gh._urlerr.HTTPError(
            url=req.full_url,
            code=404,
            msg="nope",
            hdrs=None,
            fp=io.BytesIO(b"missing"),
        )

    monkeypatch.setattr(gh._urlreq, "urlopen", raise_http_error)
    code, body = gh.get_solver_active_status_http("http://example/active", timeout=0.1)
    assert code == 404
    assert body == "missing"

def test_get_solver_active_status_http_connect_failure(monkeypatch):
    monkeypatch.setattr(gh._urlreq, "urlopen", lambda req, timeout=None: (_ for _ in ()).throw(OSError("down")))
    code, body = gh.get_solver_active_status_http("http://example/active", timeout=0.1)
    assert code == 0
    assert "connect-failed" in body
