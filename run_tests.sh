#!/usr/bin/env bash
set -euo pipefail

MODE="${1:-all}"

RUN_PY=false
RUN_GO=false

case "$MODE" in
  all|"")
    RUN_PY=true
    RUN_GO=true
    ;;
  py|python)
    RUN_PY=true
    ;;
  go|golang)
    RUN_GO=true
    ;;
  *)
    echo "Usage: $0 [py|go|all]" >&2
    echo "  py   - run only Python tests (pytest)" >&2
    echo "  go   - run only Go tests (mypriorityoptimizer package)" >&2
    echo "  all  - run both Python and Go tests (default)" >&2
    exit 1
    ;;
esac

mkdir -p coverage/python
mkdir -p coverage/go

if $RUN_PY; then
  echo "=== Running Python tests (pytest) ==="
  python -m pytest
  echo "Python tests completed."
fi

if $RUN_GO; then
  echo "=== Running Go tests (pkg/mypriorityoptimizer) ==="
  go test ./pkg/mypriorityoptimizer -coverprofile=coverage/go/go_coverage.out
  go tool cover -func=coverage/go/go_coverage.out
  go tool cover -html=coverage/go/go_coverage.out -o coverage/go/coverage.html
  echo "Go coverage reports generated in coverage/go/"
fi

if $RUN_PY && $RUN_GO; then
  echo "Coverage (Python & Go) available under coverage/python and coverage/go."
elif $RUN_PY; then
  echo "Python tests finished. (Coverage location depends on pytest-cov config.)"
elif $RUN_GO; then
  echo "Go tests finished. Coverage available under coverage/go."
fi
