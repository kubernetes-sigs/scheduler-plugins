#!/usr/bin/env bash
set -euo pipefail

# -------------------------------------------------------------
# Run kwok_test_generator.py for configs in the repo
# -------------------------------------------------------------

# Repo and script locations (override via env if you changed them)
REPO_DIR="${REPO_DIR:-$HOME/scheduler-plugins}"
KWOK_DIR="${KWOK_DIR:-$REPO_DIR/scripts/kwok}"
KWOK_CONFIG_DIR="${KWOK_CONFIG_DIR:-$KWOK_DIR/kwok_configs}"
RESULTS_DIR="${RESULTS_DIR:-$KWOK_DIR/results}"

# venv: keep it OUT of VirtualBox shared folders
VENV_DIR="${VENV_DIR:-$REPO_DIR/.venv}"

# Runtime knobs (override via env/flags if you like)
CLUSTER_NAME="${CLUSTER_NAME:-kwok1}"
COUNT="${COUNT:-3}"              # or set SEED/SEED_FILE/TEST_MODE=true
SEED="${SEED:-}"
SEED_FILE="${SEED_FILE:-}"
TEST_MODE="${TEST_MODE:-false}"

# --- sanity checks ---
command -v python3 >/dev/null || { echo "python3 not found"; exit 1; }
command -v kubectl  >/dev/null || { echo "kubectl not found"; exit 1; }
command -v kwokctl  >/dev/null || { echo "kwokctl not found"; exit 1; }

# --- ensure repo exists ---
if [[ ! -d "$REPO_DIR/.git" ]]; then
  echo "[error] Repo not found at $REPO_DIR. Run your clone/build script first."
  exit 1
fi

# --- venv (avoid vboxsf) ---
if [[ ! -d "$VENV_DIR/bin" ]]; then
  echo "[info] Creating venv at $VENV_DIR ..."
  mkdir -p "$(dirname "$VENV_DIR")"
  # --copies helps on odd filesystems; harmless elsewhere
  python3 -m venv --copies "$VENV_DIR" 2>/dev/null || python3 -m venv "$VENV_DIR"
fi
# shellcheck disable=SC1091
source "$VENV_DIR/bin/activate"
echo "[info] Activated venv at $VENV_DIR"

# Safety: ensure we only pip into a venv
export PIP_REQUIRE_VIRTUALENV=true

# Deps
REQ_FILE="$KWOK_DIR/requirements.txt"
python -m pip install --upgrade pip >/dev/null
if [[ -f "$REQ_FILE" ]]; then
  python -m pip install -r "$REQ_FILE"
else
  # minimal deps used by your scripts
  python -m pip install pyyaml tabulate
fi

# --- run generator from repo path ---
cd "$KWOK_DIR"
GEN="kwok_test_generator.py"
[[ -f "$GEN" ]] || { echo "[error] $GEN not found in $KWOK_DIR"; exit 1; }

args=(
  "--cluster-name" "$CLUSTER_NAME"
  "--config-dir" "$KWOK_CONFIG_DIR"
  "--results-dir" "$RESULTS_DIR"
)

# choose one of the seed modes
if [[ -n "$COUNT" ]]; then args+=( "--count" "$COUNT" ); fi
if [[ -n "$SEED" ]]; then args+=( "--seed" "$SEED" ); fi
if [[ -n "$SEED_FILE" ]]; then args+=( "--seed-file" "$SEED_FILE" ); fi
if [[ "$TEST_MODE" == "true" ]]; then args+=( "--test" ); fi

echo "[run] python3 $GEN ${args[*]}"
set -o pipefail
python3 "$GEN" "${args[@]}"

echo
echo "----- kwok test end: $(date +%Y%m%d_%H%M%S) -----"
echo "[ok] Results CSVs: $RESULTS_DIR"
