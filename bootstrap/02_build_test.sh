#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="${REPO_DIR:-$HOME/scheduler-plugins}"
KWOK_DIR="${KWOK_DIR:-$REPO_DIR/scripts/kwok}"
KWOKRC="${REPO_DIR}/.kwokrc"

# Defaults
KUBE_VERSION="${KUBE_VERSION:-v1.32.7}"
KWOK_RUNTIME="${KWOK_RUNTIME:-binary}"

# Load .kwokrc
if [[ -f "$KWOKRC" ]]; then
  # shellcheck disable=SC1090
  source "$KWOKRC"
fi
KWOK_CLUSTER="${KWOK_CLUSTER:-kwok1}"
KWOK_CONFIGS="${KWOK_CONFIGS:-baseline}"
KWOK_SEEDS="${KWOK_SEEDS:-seeds001.txt}"
KWOK_RUNTIME="${KWOK_RUNTIME:-binary}"

KWOK_CONFIG_DIR="${KWOK_DIR}/configs/${KWOK_CONFIGS}"
SEED_FILE="${KWOK_DIR}/seeds/${KWOK_SEEDS}"
RESULTS_DIR="${RESULTS_DIR:-${REPO_DIR}/results}"
mkdir -p "$RESULTS_DIR"

echo "[cfg] cluster=${KWOK_CLUSTER}"
echo "[cfg] runtime=${KWOK_RUNTIME}"
echo "[cfg] configs=${KWOK_CONFIG_DIR}"
echo "[cfg] seeds=${SEED_FILE}"
echo "[cfg] results=${RESULTS_DIR}"

[[ -d "$KWOK_CONFIG_DIR" ]] || { echo "[error] config dir not found: $KWOK_CONFIG_DIR"; exit 1; }
[[ -f "$SEED_FILE" ]]      || { echo "[error] seed file not found: $SEED_FILE"; exit 1; }

# Ensure Go is available in this shell
export PATH="/usr/local/go/bin:${PATH}"
go version >/dev/null

# --- build kube-scheduler (from repo root) ---
# only build if KWOK_RUNTIME is binary, otherwise we build in the Dockerfile
if [ "${KWOK_RUNTIME}" = "binary" ]; then
  echo "[build] build kube-scheduler (binary runtime)"
  cd "${REPO_DIR}"
  echo "[build] make build-scheduler (CGO_DISABLED, linux/amd64)"
  make build-scheduler GO_BUILD_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=amd64'
  [[ -x "${REPO_DIR}/bin/kube-scheduler" ]] || { echo "[error] built binary not found: ${REPO_DIR}/bin/kube-scheduler"; exit 1; }
fi

# --- install Python deps for solver system-wide (so scheduler can import) ---
echo "[py] installing solver requirements system-wide"
python3 -m pip install --no-cache-dir -r "${REPO_DIR}/scripts/mycrossnodepreemption/requirements.txt"

# Your kwok_cluster.yaml references absolute paths under /home/vagrant/.
# If this VM user is not 'vagrant', adjust the file or generate a temp copy.
kwokctl create cluster \
  --name "${KWOK_CLUSTER}" \
  --runtime=binary \
  --config "${KWOK_DIR}/kwok_cluster.yaml"

# args=(
#   "--cluster-name" "$KWOK_CLUSTER"
#   "--config-dir" "$KWOK_CONFIG_DIR"
#   "--results-dir" "$RESULTS_DIR"
#   "--seed-file" "$SEED_FILE"
# )

# echo "----- kwok test start: $(date +%Y%m%d_%H%M%S) -----"
# set -o pipefail
# python3 "$TEST_GENERATOR_SCRIPT" "${args[@]}"
# echo
# echo "----- kwok test end:   $(date +%Y%m%d_%H%M%S) -----"
# echo "[ok] Results CSVs: $RESULTS_DIR"


echo "[ok] cluster up. results dir: ${RESULTS_DIR}"
