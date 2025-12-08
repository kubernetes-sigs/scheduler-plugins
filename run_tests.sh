#!/usr/bin/env bash
set -euo pipefail

# Default: run both Python + Go unit tests (no KWOK integration)
MODE="${1:-unit_all}"

RUN_UNIT_PY=false
RUN_UNIT_GO=false
RUN_INT_KWOK=false

case "$MODE" in
  all)
    # Run everything: Python unit, Go unit, and KWOK integration tests
    RUN_UNIT_PY=true
    RUN_UNIT_GO=true
    RUN_INT_KWOK=true
    ;;
  unit_all|"")
    # Only unit tests (default)
    RUN_UNIT_PY=true
    RUN_UNIT_GO=true
    ;;
  unit_py|unit_python)
    RUN_UNIT_PY=true
    ;;
  unit_go|unit_golang)
    RUN_UNIT_GO=true
    ;;
  int_kwok|integration)
    RUN_INT_KWOK=true
    ;;
  *)
    echo "Usage: $0 [unit_py|unit_go|unit_all|int_kwok|integration|all]" >&2
    echo "  all       - run unit tests + KWOK integration tests" >&2
    echo "  unit_all  - run Python and Go unit tests (default)" >&2
    echo "  unit_py   - run only Python unit tests (pytest)" >&2
    echo "  unit_go   - run only Go unit tests (mypriorityoptimizer package)" >&2
    echo "  int_kwok  - run only integration tests with KWOK" >&2
    exit 1
    ;;
esac

# Create coverage folders (HTML in subfolders for pytest)
mkdir -p coverage/unit/python/html
mkdir -p coverage/unit/go
mkdir -p coverage/integration/kwok/html

if "$RUN_UNIT_PY"; then
  echo "=== Running Python unit tests (pytest) ==="
  # Adjust --cov target as needed (e.g. scripts/, . , etc.)
  python -m pytest \
    --cov=. \
    --cov-report=term \
    --cov-report=html:coverage/unit/python/html
  echo "Python tests completed. Coverage HTML: coverage/unit/python/html/index.html"
fi

if "$RUN_UNIT_GO"; then
  echo "=== Running Go unit tests (pkg/mypriorityoptimizer) ==="
  go test ./pkg/mypriorityoptimizer -coverprofile=coverage/unit/go/go_coverage.out
  go tool cover -func=coverage/unit/go/go_coverage.out
  go tool cover -html=coverage/unit/go/go_coverage.out -o coverage/unit/go/coverage.html
  echo "Go coverage reports generated in coverage/unit/go/"
fi

if "$RUN_INT_KWOK"; then
  echo "=== Running Integration tests with KWOK ==="
  python -m pytest \
    tests/kwok_integration_tests/test_modes.py \
    --cov=. \
    --cov-report=term \
    --cov-report=html:coverage/integration/kwok/html
  echo "Integration tests with KWOK completed. Coverage HTML: coverage/integration/kwok/html/index.html"
fi

# Summary
if "$RUN_UNIT_PY" && "$RUN_UNIT_GO"; then
  echo "Unit coverage:"
  echo "  Python: coverage/unit/python/html/index.html"
  echo "  Go:     coverage/unit/go/coverage.html"
elif "$RUN_UNIT_PY"; then
  echo "Python unit coverage: coverage/unit/python/html/index.html"
elif "$RUN_UNIT_GO"; then
  echo "Go unit coverage: coverage/unit/go/coverage.html"
fi

if "$RUN_INT_KWOK"; then
  echo "Integration coverage (KWOK): coverage/integration/kwok/html/index.html"
fi
