#!/usr/bin/env bash
set -euo pipefail

# -------------------------------------------------------------
# Run kwok_test_generator.py for configs in the repo
# -------------------------------------------------------------

REPO_URL="https://github.com/henrikdchristensen/scheduler-plugins"
REPO_DIR="${HOME}/scheduler-plugins"
IMG_TAG="${IMG_TAG:-localhost:5000/scheduler-plugins/kube-scheduler:dev}"

REPO_DIR="${REPO_DIR:-$HOME/scheduler-plugins}"
KWOK_DIR="${KWOK_DIR:-$REPO_DIR/scripts/kwok}"

CLUSTER_NAME="${CLUSTER_NAME:-kwok1}"
KWOK_CONFIG_DIR="${KWOK_CONFIG_DIR:-$KWOK_DIR/kwok_configs}"
RESULTS_DIR="${RESULTS_DIR:-$REPO_DIR/results}"
SEED_FILE="${SEED_FILE:-$KWOK_DIR/seeds/seeds1.txt}"

TEST_GENERATOR_SCRIPT="kwok_test_generator.py"

# venv: keep it OUT of VirtualBox shared folders
VENV_DIR="${VENV_DIR:-$REPO_DIR/.venv}"

# --- sanity checks ---
command -v python3 >/dev/null || { echo "python3 not found"; exit 1; }
command -v kubectl  >/dev/null || { echo "kubectl not found"; exit 1; }
command -v kwokctl  >/dev/null || { echo "kwokctl not found"; exit 1; }
#TODO: add also check for docker and GO

# --- ensure repo exists ---
if [[ ! -d "$REPO_DIR/.git" ]]; then
  echo "[error] Repo not found at $REPO_DIR. Run your clone/build script first."
  exit 1
fi

# ---- Build Docker image ----
cd "${REPO_DIR}"
echo "[info] Building image with tag: ${IMG_TAG} ..."
docker build -t "${IMG_TAG}" -f build/scheduler/Dockerfile .
echo "[ok] Image ready at: ${REPO_DIR}"


# --- venv (avoid vboxsf) ---
if [[ ! -d "$VENV_DIR/bin" ]]; then
  echo "[info] Creating venv at $VENV_DIR ..."
  mkdir -p "$(dirname "$VENV_DIR")"
  # --copies helps on odd filesystems; harmless elsewhere
  python3 -m venv --copies "$VENV_DIR" 2>/dev/null || python3 -m venv "$VENV_DIR"
fi
source "$VENV_DIR/bin/activate"
echo "[info] Activated venv at $VENV_DIR"
# ensure we only pip into a venv
export PIP_REQUIRE_VIRTUALENV=true

# Deps
REQ_FILE="$KWOK_DIR/requirements.txt"
python -m pip install --upgrade pip >/dev/null
python -m pip install -r "$REQ_FILE"

# --- run generator from repo path ---
cd "$KWOK_DIR"

[[ -f "$TEST_GENERATOR_SCRIPT" ]] || { echo "[error] $TEST_GENERATOR_SCRIPT not found in $KWOK_DIR"; exit 1; }

args=(
  "--cluster-name" "$CLUSTER_NAME"
  "--config-dir" "$KWOK_CONFIG_DIR"
  "--results-dir" "$RESULTS_DIR"
  "--seed-file" "$SEED_FILE"
)

echo "----- kwok test start: $(date +%Y%m%d_%H%M%S) -----"
set -o pipefail
python3 "$TEST_GENERATOR_SCRIPT" "${args[@]}"

echo
echo "----- kwok test end: $(date +%Y%m%d_%H%M%S) -----"
echo "[ok] Results CSVs: $RESULTS_DIR"