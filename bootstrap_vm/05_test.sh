#!/usr/bin/env bash
set -euo pipefail

# -------------------------------------------------------------
# Run kwok_test_generator.py for configs in ./kwok_configs
# -------------------------------------------------------------
# Defaults (override via flags or env):
#   CLUSTER_NAME    : kwok1
#   KWOK_CONFIG_DIR : ./kwok_configs
#   RESULTS_DIR     : ./results
#   VENV_DIR        : ./.venv
#
# Flags:
#   -n, --cluster-name <name>     : override cluster name (default: kwok1)
#   -k, --kwok-config-dir <dir>   : path to config dir (default: ./kwok_configs)
#   -o, --results-dir <dir>       : path to results dir (default: ./results)
#   -s, --seed <int>              : single seed (mutually exclusive with --count)
#   -f, --seed-file <path>        : file with seeds (csv or newline list)
#   -c, --count <int|-1>          : generate random seeds; -1=infinite
#   -t, --test                    : test mode (requires exactly one --seed)
#   -h, --help                    : show help
#
# Examples:
#   ./04_test.sh -s 123
#   ./04_test.sh -f ./seeds.csv
#   ./04_test.sh -c 50
#   ./04_test.sh --test -s 42
# -------------------------------------------------------------


# --- resolve script dir ---
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"

REQ="${SCRIPT_DIR}/requirements.txt"

# --- defaults (can be overridden by flags) ---
CLUSTER_NAME="${CLUSTER_NAME:-kwok1}"
KWOK_CONFIG_DIR="${KWOK_CONFIG_DIR:-${SCRIPT_DIR}/kwok_configs}"
RESULTS_DIR="${RESULTS_DIR:-${SCRIPT_DIR}/results}"
VENV_DIR="${VENV_DIR:-${SCRIPT_DIR}/.venv}"

SEED=""
SEED_FILE=""
COUNT="3"
TEST_MODE="false"

# --- sanity checks ---
command -v python3 >/dev/null || { echo "python3 not found"; exit 1; }
command -v kubectl  >/dev/null || { echo "kubectl not found"; exit 1; }
command -v kwokctl  >/dev/null || { echo "kwokctl not found"; exit 1; }

# Python venv for your helper scripts
python3 -m venv .venv
source .venv/bin/activate
echo "[info] Activated venv"

# Deps
pip install --upgrade pip >/dev/null
pip install -r "${REQ}"
echo "[ok] Installed requirements from ${REQ} ..."

# --- paths to Python scripts (we run from SCRIPT_DIR so imports work) ---
GEN="kwok_test_generator.py"

# --- build arg list for generator ---
args=(
  "--cluster-name" "${CLUSTER_NAME}"
  "--kwok-config-dir" "${KWOK_CONFIG_DIR}"
  "--results-dir" "${RESULTS_DIR}"
  "--count" "${COUNT}"
)

echo "[run] ${PY} ${GEN} ${args[*]}"

# --- execute generator ---
set -o pipefail
python3 "${SCRIPT_DIR}/${GEN}" "${args[@]}" 2>&1"

echo
echo "----- kwok test end: $(date +%Y%m%d_%H%M%S) -----"
echo "[ok] Results CSVs will be under: ${RESULTS_DIR}"
