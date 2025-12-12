#!/usr/bin/env python3
# func_audit.py

import argparse
import ast
import csv
import os
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable, Iterator, Optional


# -----------------------------
# Generic filesystem utilities
# -----------------------------

DEFAULT_SKIP_DIRS = {
    ".git",
    ".venv",
    "venv",
    "__pycache__",
    "node_modules",
    "vendor",
}


def iter_files(
    roots: Iterable[Path],
    *,
    suffix: str,
    skip_dirs: set[str],
) -> Iterator[Path]:
    for root in roots:
        root = root.resolve()
        if root.is_file():
            if root.suffix == suffix:
                yield root
            continue
        if not root.exists():
            continue
        for dirpath, dirnames, filenames in os.walk(root):
            # Mutate dirnames in-place to prune traversal.
            dirnames[:] = [d for d in dirnames if d not in skip_dirs]
            for fn in filenames:
                if fn.endswith(suffix):
                    yield Path(dirpath) / fn


# -----------------------------
# Go parsing (lightweight)
# -----------------------------

_GO_FUNC_RE = re.compile(
    r"\bfunc\s*(?:\((?P<recv>[^)]*)\)\s*)?(?P<name>[A-Za-z_]\w*)\s*\(",
    flags=re.MULTILINE,
)


def strip_go_comments(src: str) -> str:
    """Best-effort removal of // and /* */ comments.

    This is intentionally lightweight (not a full Go lexer), but it is good
    enough for most repositories.
    """
    out: list[str] = []
    i = 0
    n = len(src)
    in_line = False
    in_block = False
    in_str = False
    in_rune = False
    in_raw = False

    while i < n:
        ch = src[i]
        nxt = src[i + 1] if i + 1 < n else ""

        if in_line:
            if ch == "\n":
                in_line = False
                out.append(ch)
            i += 1
            continue

        if in_block:
            if ch == "*" and nxt == "/":
                in_block = False
                i += 2
            else:
                i += 1
            continue

        if in_str:
            out.append(ch)
            if ch == "\\":
                # escape
                if i + 1 < n:
                    out.append(src[i + 1])
                    i += 2
                else:
                    i += 1
                continue
            if ch == '"':
                in_str = False
            i += 1
            continue

        if in_rune:
            out.append(ch)
            if ch == "\\":
                if i + 1 < n:
                    out.append(src[i + 1])
                    i += 2
                else:
                    i += 1
                continue
            if ch == "'":
                in_rune = False
            i += 1
            continue

        if in_raw:
            out.append(ch)
            if ch == "`":
                in_raw = False
            i += 1
            continue

        # Not in any special mode
        if ch == "/" and nxt == "/":
            in_line = True
            i += 2
            continue
        if ch == "/" and nxt == "*":
            in_block = True
            i += 2
            continue

        if ch == '"':
            in_str = True
            out.append(ch)
            i += 1
            continue
        if ch == "'":
            in_rune = True
            out.append(ch)
            i += 1
            continue
        if ch == "`":
            in_raw = True
            out.append(ch)
            i += 1
            continue

        out.append(ch)
        i += 1

    return "".join(out)


def line_for_offset(src: str, offset: int) -> int:
    # 1-based line numbers
    return src.count("\n", 0, offset) + 1


@dataclass(frozen=True)
class GoFunc:
    name: str
    receiver: str
    is_method: bool
    file: Path
    line: int


def scan_go_functions(files: Iterable[Path]) -> list[GoFunc]:
    results: list[GoFunc] = []
    for p in files:
        try:
            raw = p.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            raw = p.read_text(encoding="utf-8", errors="replace")

        src = strip_go_comments(raw)
        for m in _GO_FUNC_RE.finditer(src):
            name = m.group("name") or ""
            recv = (m.group("recv") or "").strip()
            is_method = bool(recv)
            ln = line_for_offset(src, m.start())
            results.append(GoFunc(name=name, receiver=recv, is_method=is_method, file=p, line=ln))
    return results


def scan_go_test_names(files: Iterable[Path]) -> set[str]:
    # We only care about existence of TestXxx symbols.
    names: set[str] = set()
    for p in files:
        if not p.name.endswith("_test.go"):
            continue
        try:
            raw = p.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            raw = p.read_text(encoding="utf-8", errors="replace")
        src = strip_go_comments(raw)
        for m in _GO_FUNC_RE.finditer(src):
            name = m.group("name") or ""
            if name.startswith("Test"):
                names.add(name)
    return names


# -----------------------------
# Python parsing (ast)
# -----------------------------

@dataclass(frozen=True)
class PyFunc:
    qualname: str
    name: str
    is_method: bool
    file: Path
    line: int


def scan_python_functions(files: Iterable[Path]) -> list[PyFunc]:
    results: list[PyFunc] = []

    for p in files:
        try:
            src = p.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            src = p.read_text(encoding="utf-8", errors="replace")

        try:
            tree = ast.parse(src, filename=str(p))
        except SyntaxError:
            continue

        class_stack: list[str] = []

        class Visitor(ast.NodeVisitor):
            def visit_ClassDef(self, node: ast.ClassDef) -> None:
                class_stack.append(node.name)
                self.generic_visit(node)
                class_stack.pop()

            def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
                qual = ".".join(class_stack + [node.name]) if class_stack else node.name
                results.append(
                    PyFunc(
                        qualname=qual,
                        name=node.name,
                        is_method=bool(class_stack),
                        file=p,
                        line=int(getattr(node, "lineno", 1) or 1),
                    )
                )
                self.generic_visit(node)

            def visit_AsyncFunctionDef(self, node: ast.AsyncFunctionDef) -> None:
                qual = ".".join(class_stack + [node.name]) if class_stack else node.name
                results.append(
                    PyFunc(
                        qualname=qual,
                        name=node.name,
                        is_method=bool(class_stack),
                        file=p,
                        line=int(getattr(node, "lineno", 1) or 1),
                    )
                )
                self.generic_visit(node)

        Visitor().visit(tree)

    return results


def scan_python_test_names(files: Iterable[Path]) -> set[str]:
    # Track function names as they appear in test modules (top-level + methods).
    names: set[str] = set()
    for p in files:
        try:
            src = p.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            src = p.read_text(encoding="utf-8", errors="replace")
        try:
            tree = ast.parse(src, filename=str(p))
        except SyntaxError:
            continue

        class Visitor(ast.NodeVisitor):
            def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
                names.add(node.name)
                self.generic_visit(node)

            def visit_AsyncFunctionDef(self, node: ast.AsyncFunctionDef) -> None:
                names.add(node.name)
                self.generic_visit(node)

        Visitor().visit(tree)

    return names


# -----------------------------
# CSV writers
# -----------------------------


def write_go_csv(
    out_path: Path,
    funcs: list[GoFunc],
    test_names: set[str],
    repo_root: Path,
) -> None:
    out_path.parent.mkdir(parents=True, exist_ok=True)
    with open(out_path, "w", newline="", encoding="utf-8") as f:
        w = csv.writer(f)
        w.writerow([
            "file",
            "function",
            "receiver",
            "is_method",
            "line",
            "expected_test",
            "has_test",
        ])
        for fn in sorted(
            funcs,
            key=lambda x: (
                str(x.file.resolve().relative_to(repo_root).as_posix()).lower(),
                x.name,
                x.line,
            ),
        ):
            expected = f"Test{fn.name}"
            rel_file = str(fn.file.resolve().relative_to(repo_root).as_posix())
            w.writerow([
                rel_file,
                fn.name,
                fn.receiver,
                "1" if fn.is_method else "0",
                fn.line,
                expected,
                "1" if expected in test_names else "0",
            ])


def write_python_csv(
    out_path: Path,
    funcs: list[PyFunc],
    test_names: set[str],
    repo_root: Path,
) -> None:
    out_path.parent.mkdir(parents=True, exist_ok=True)
    with open(out_path, "w", newline="", encoding="utf-8") as f:
        w = csv.writer(f)
        w.writerow([
            "file",
            "qualname",
            "function",
            "is_method",
            "line",
            "expected_test_pytest",
            "expected_test_unittest",
            "has_test",
        ])
        for fn in sorted(
            funcs,
            key=lambda x: (
                str(x.file.resolve().relative_to(repo_root).as_posix()).lower(),
                x.name,
                x.qualname,
                x.line,
            ),
        ):
            expected_pytest = f"test_{fn.name}"
            expected_unittest = f"Test{fn.name}"
            has = (expected_pytest in test_names) or (expected_unittest in test_names)
            rel_file = str(fn.file.resolve().relative_to(repo_root).as_posix())
            w.writerow([
                rel_file,
                fn.qualname,
                fn.name,
                "1" if fn.is_method else "0",
                fn.line,
                expected_pytest,
                expected_unittest,
                "1" if has else "0",
            ])


# -----------------------------
# CLI
# -----------------------------


def parse_args(argv: Optional[list[str]] = None) -> argparse.Namespace:
    ap = argparse.ArgumentParser(description="Scan Go/Python functions and test coverage naming.")

    ap.add_argument("--go-dir", action="append", default=[], help="Root dir/file to scan for Go functions (repeatable)")
    ap.add_argument("--go-test-dir", action="append", default=[], help="Root dir/file to scan for Go test funcs (repeatable)")
    ap.add_argument("--py-dir", action="append", default=[], help="Root dir/file to scan for Python functions (repeatable)")
    ap.add_argument("--py-test-dir", action="append", default=[], help="Root dir/file to scan for Python test funcs (repeatable)")

    ap.add_argument("--out-go", default="audit_go_functions.csv", help="Output CSV path for Go results")
    ap.add_argument("--out-py", default="audit_py_functions.csv", help="Output CSV path for Python results")

    ap.add_argument(
        "--skip-dir",
        action="append",
        default=[],
        help="Directory name to skip during recursion (repeatable). Defaults include vendor, __pycache__, .git, .venv.",
    )

    return ap.parse_args(argv)


def main(argv: Optional[list[str]] = None) -> int:
    args = parse_args(argv)

    repo_root = Path.cwd().resolve()
    skip_dirs = set(DEFAULT_SKIP_DIRS) | set(args.skip_dir or [])

    go_roots = [Path(p) for p in (args.go_dir or [])]
    go_test_roots = [Path(p) for p in (args.go_test_dir or [])]
    py_roots = [Path(p) for p in (args.py_dir or [])]
    py_test_roots = [Path(p) for p in (args.py_test_dir or [])]

    # Go
    go_files = list(iter_files(go_roots, suffix=".go", skip_dirs=skip_dirs))
    go_funcs = scan_go_functions(go_files)

    go_test_files = list(iter_files(go_test_roots, suffix=".go", skip_dirs=skip_dirs))
    go_test_names = scan_go_test_names(go_test_files)

    # Python
    py_files = list(iter_files(py_roots, suffix=".py", skip_dirs=skip_dirs))
    py_funcs = scan_python_functions(py_files)

    py_test_files = list(iter_files(py_test_roots, suffix=".py", skip_dirs=skip_dirs))
    py_test_names = scan_python_test_names(py_test_files)

    write_go_csv(Path(args.out_go), go_funcs, go_test_names, repo_root)
    write_python_csv(Path(args.out_py), py_funcs, py_test_names, repo_root)

    print(f"Go files scanned: {len(go_files)}; functions found: {len(go_funcs)}; go test symbols: {len(go_test_names)}")
    print(f"Python files scanned: {len(py_files)}; functions found: {len(py_funcs)}; py test symbols: {len(py_test_names)}")
    print(f"Wrote: {args.out_go}")
    print(f"Wrote: {args.out_py}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
