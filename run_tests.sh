#!/usr/bin/env bash
set -euo pipefail

# Default: run both Python + Go unit tests (no KWOK integration)
MODE="${1:-all}"

RUN_UNIT_PY=false
RUN_UNIT_GO=false
RUN_INT_KWOK=false

case "$MODE" in
  all|"")
    # Run everything: Python unit, Go unit, and KWOK integration tests
    RUN_UNIT_PY=true
    RUN_UNIT_GO=true
    RUN_INT_KWOK=true
    ;;
  unit_all|unit)
    # Only unit tests (default)
    RUN_UNIT_PY=true
    RUN_UNIT_GO=true
    ;;
  unit_py)
    RUN_UNIT_PY=true
    ;;
  unit_go)
    RUN_UNIT_GO=true
    ;;
  int_all|int_kwok|integration)
    RUN_INT_KWOK=true
    ;;
  *)
    echo "Usage: $0 [unit_py|unit_go|unit_all|int_kwok|integration|all]" >&2
    echo "  all         - run unit tests + KWOK integration tests" >&2
    echo "  unit_all    - run Python and Go unit tests (default)" >&2
    echo "  unit_py     - run only Python unit tests (pytest)" >&2
    echo "  unit_go     - run only Go unit tests (mypriorityoptimizer package)" >&2
    echo "  int_kwok    - run only integration tests with KWOK" >&2
    echo "  integration - alias for int_kwok" >&2
    exit 1
    ;;
esac

# -------------------------------------------------------------------
# Shared settings for solver env (only used for integration tests)
# -------------------------------------------------------------------
VENV_DIR="${VENV_DIR:-/opt/venv}"
SOLVER_DIR="${SOLVER_DIR:-/opt/solver}"

ensure_solver_env() {
  echo "=== Ensuring solver Python environment ==="
  echo "VENV_DIR=${VENV_DIR}"
  echo "SOLVER_DIR=${SOLVER_DIR}"

  # Directories
  mkdir -p "${SOLVER_DIR}" "${VENV_DIR}"

  # Always copy latest solver code
  cp scripts/python_solver/main.py "${SOLVER_DIR}/main.py"

  # Create venv if missing
  if [[ ! -x "${VENV_DIR}/bin/python" ]]; then
    python -m venv "${VENV_DIR}"
  fi

  # Install solver requirements into that venv
  "${VENV_DIR}/bin/python" -m pip install --upgrade pip
  "${VENV_DIR}/bin/pip" install --no-cache-dir -r scripts/python_solver/requirements.txt
}

# -------------------------------------------------------------------
# Coverage folders
# -------------------------------------------------------------------
mkdir -p coverage/unit/python/html
mkdir -p coverage/unit/go
mkdir -p coverage/integration/kwok/html

if "$RUN_UNIT_PY"; then
  echo "=== Running Python unit tests (pytest) ==="
  # Adjust --cov target as needed (e.g. scripts/, . , etc.)
  python -m pytest \
    --cov=. \
    --cov-report=term \
    --cov-report=html:coverage/unit/python
  echo "Python tests completed. Coverage HTML: coverage/unit/python/index.html"
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

  # Create/update kube-scheduler binary
  echo "Building kube-scheduler with mypriorityoptimizer plugin..."
  make build-scheduler GO_BUILD_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=amd64'

  # Make sure solver env exists (mirrors CI workflow setup)
  export VENV_DIR SOLVER_DIR
  ensure_solver_env

  # Install integration test dependencies in the current Python env
  python -m pip install --upgrade pip
  if [ -f scripts/kwok_integration_tests/requirements.txt ]; then
    python -m pip install -r scripts/kwok_integration_tests/requirements.txt
  fi

  python -m pytest \
    scripts/kwok_integration_tests/test_modes.py \
    --cov=. \
    --cov-report=term \
    --cov-report=html:coverage/integration/kwok

  echo "Integration tests with KWOK completed. Coverage HTML: coverage/integration/kwok/index.html"
fi

# Summary
if "$RUN_UNIT_PY" && "$RUN_UNIT_GO"; then
  echo "Unit coverage:"
  echo "  Python: coverage/unit/python/index.html"
  echo "  Go:     coverage/unit/go/coverage.html"
elif "$RUN_UNIT_PY"; then
  echo "Python unit coverage: coverage/unit/python/index.html"
elif "$RUN_UNIT_GO"; then
  echo "Go unit coverage: coverage/unit/go/coverage.html"
fi

if "$RUN_INT_KWOK"; then
  echo "Integration coverage (KWOK): coverage/integration/kwok/index.html"
fi
