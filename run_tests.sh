#!/usr/bin/env bash
set -euo pipefail

# Load environment variables
ENV_FILE="opt-prio.env"
echo "===Load environment variables from ${ENV_FILE} ==="
# shellcheck source=/dev/null
set -a
source "${ENV_FILE}"
set +a
echo "Environment variables loaded."

# Default: run all tests
MODE="${1:-all}"

RUN_UNIT_PY=false
RUN_UNIT_GO=false
RUN_INT_KWOK=false

case "$MODE" in
  all|"")
    RUN_UNIT_PY=true
    RUN_UNIT_GO=true
    RUN_INT_KWOK=true
    ;;
  unit_all|unit)
    RUN_UNIT_PY=true
    RUN_UNIT_GO=true
    ;;
  unit_py|unit_python)
    RUN_UNIT_PY=true
    ;;
  unit_go)
    RUN_UNIT_GO=true
    ;;
  int_all|int|int_kwok|integration)
    RUN_INT_KWOK=true
    ;;
  *)
    echo "Usage: $0 [all|unit_py|unit_go|unit_all|int_kwok|integration]" >&2
    echo "  all         - run unit tests + integration tests" >&2
    echo "  unit_all    - run Python and Go unit tests (default)" >&2
    echo "  unit        - alias for unit_all" >&2
    echo "  unit_py     - run only Python unit tests (pytest)" >&2
    echo "  unit_python - alias for unit_py" >&2
    echo "  unit_go     - run only Go unit tests (mypriorityoptimizer package)" >&2
    echo "  int_all     - run only integration tests with KWOK" >&2
    echo "  int         - alias for int_all" >&2
    echo "  int_kwok    - run only integration tests with KWOK" >&2
    echo "  integration - alias for int_kwok" >&2
    exit 1
    ;;
esac

# -------------------------------------------------------------------
# Shared settings for solver env (only used for integration tests)
# -------------------------------------------------------------------
ensure_python_solver_env() {
  echo "=== Ensuring Python solver environment ==="

  # Strip possible Windows \r from env file values
  PYTHON_SOLVER_OUT_SCRIPT_DIR="${PYTHON_SOLVER_OUT_SCRIPT_DIR%$'\r'}"
  PYTHON_SOLVER_OUT_VENV_DIR="${PYTHON_SOLVER_OUT_VENV_DIR%$'\r'}"
  PYTHON_SOLVER_SCRIPT_PATH="${PYTHON_SOLVER_SCRIPT_PATH%$'\r'}"

  # Provide sane defaults if unset/empty
  if [[ -z "${PYTHON_SOLVER_OUT_SCRIPT_DIR:-}" ]]; then
    PYTHON_SOLVER_OUT_SCRIPT_DIR="${PWD}/.solver"
  fi
  if [[ -z "${PYTHON_SOLVER_OUT_VENV_DIR:-}" ]]; then
    PYTHON_SOLVER_OUT_VENV_DIR="${PWD}/.venv_solver}"
  fi
  if [[ -z "${PYTHON_SOLVER_SCRIPT_PATH:-}" ]]; then
    PYTHON_SOLVER_SCRIPT_PATH="scripts/python_solver/main.py"
  fi

  # Try to create directories
  mkdir -p "${PYTHON_SOLVER_OUT_SCRIPT_DIR}" "${PYTHON_SOLVER_OUT_VENV_DIR}"

  # Always copy latest solver code
  cp "${PYTHON_SOLVER_SCRIPT_PATH}" "${PYTHON_SOLVER_OUT_SCRIPT_DIR}/main.py"

  # Create venv if missing
  if [[ ! -x "${PYTHON_SOLVER_OUT_VENV_DIR}/bin/python" ]]; then
    python -m venv "${PYTHON_SOLVER_OUT_VENV_DIR}"
  fi

  # Install solver requirements into that venv
  "${PYTHON_SOLVER_OUT_VENV_DIR}/bin/pip" install -r scripts/python_solver/requirements.txt
}

# -------------------------------------------------------------------
# Coverage folders
# -------------------------------------------------------------------
mkdir -p coverage/python/html
mkdir -p coverage/go

if "$RUN_UNIT_PY"; then
  echo "=== Running Python unit tests (pytest) ==="
  python -m pytest \
    scripts/test \
    --cov=scripts \
    --cov-config=pytest-cfg.coveragerc \
    --cov-report=term \
    --cov-report=html:coverage/python \
    --cov-report=term-missing
  echo "Python tests completed. Coverage HTML: coverage/python/index.html"
fi

if "$RUN_UNIT_GO"; then
  echo "=== Running Go unit tests (pkg/mypriorityoptimizer) ==="
  go test ./pkg/mypriorityoptimizer -coverprofile=coverage/go/go_coverage.out
  go tool cover -func=coverage/go/go_coverage.out
  go tool cover -html=coverage/go/go_coverage.out -o coverage/go/coverage.html
  echo "Go coverage reports generated in coverage/go/"
fi

if "$RUN_INT_KWOK"; then
  echo "=== Running Integration tests with KWOK ==="

  echo "Building kube-scheduler with mypriorityoptimizer plugin..."
  make build-scheduler GO_BUILD_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=amd64' VERSION=${SCHEDULER_VERSION}

  ensure_python_solver_env

  python -m pip install --upgrade pip
  if [ -f scripts/kwok_integration_tests/requirements.txt ]; then
    python -m pip install -r scripts/kwok_integration_tests/requirements.txt
  fi

  python -m pytest -s scripts/kwok_integration_tests/test_modes.py

  echo "Integration tests with KWOK completed."
fi

# Summary
if "$RUN_UNIT_PY" && "$RUN_UNIT_GO"; then
  echo "Unit coverage:"
  echo "  Python: coverage/python/index.html"
  echo "  Go:     coverage/go/coverage.html"
elif "$RUN_UNIT_PY"; then
  echo "Python unit coverage: coverage/python/index.html"
elif "$RUN_UNIT_GO"; then
  echo "Go unit coverage: coverage/go/coverage.html"
fi
