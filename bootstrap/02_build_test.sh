#!/usr/bin/env bash
set -euo pipefail

REPO_NAME="scheduler-plugins"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="${REPO_DIR:-$(dirname "$SCRIPT_DIR")}"
KWOK_DIR="${KWOK_DIR:-$REPO_DIR/scripts/kwok}"
TEST_GENERATOR_SCRIPT="${KWOK_DIR}/kwok_test_generator.py"

# Read .kwokrc
KWOKRC="${REPO_DIR}/.kwokrc"
if [ ! -f "${KWOKRC}" ]; then
  echo "[error] missing ${KWOKRC} (run 00_init.sh first)"; exit 1
fi
# shellcheck disable=SC1090
source "${REPO_DIR}/.kwokrc"
echo "[init] read ${KWOKRC}: cluster=${KWOK_CLUSTER_NAME} configs=${KWOK_CONFIGS} seeds=${KWOK_SEEDS} runtime=${KWOK_RUNTIME}"

KWOK_CONFIG_DIR="${KWOK_DIR}/configs/${KWOK_CONFIGS}"
SEED_FILE="${KWOK_DIR}/seeds/${KWOK_SEEDS}"
RESULTS_DIR="${RESULTS_DIR:-${REPO_DIR}/results}"

echo "[cfg] cluster=${KWOK_CLUSTER_NAME}"
echo "[cfg] runtime=${KWOK_RUNTIME}"
echo "[cfg] configs=${KWOK_CONFIG_DIR}"
echo "[cfg] seeds=${SEED_FILE}"
echo "[cfg] results=${RESULTS_DIR}"

# Build kube-scheduler binary if runtime=binary; otherwise we build from Dockerfile
if [ "${KWOK_RUNTIME}" = "binary" ]; then
  echo "[build] build kube-scheduler (runtime=binary)"
  cd "${REPO_DIR}"
  echo "[build] make build-scheduler (CGO_DISABLED, linux/amd64)"
  make build-scheduler GO_BUILD_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=amd64'
  echo "[ok] kube-scheduler built: ${REPO_DIR}/build/kube-scheduler"
else
  echo "[build] docker image (runtime=docker)"
  cd "${REPO_DIR}"
  IMG_TAG="localhost:5000/scheduler-plugins/kube-scheduler:dev"
  echo "[build] docker image ${IMG_TAG}"
  DOCKER_BUILDKIT=1 docker build \
    -t "${IMG_TAG}" \
    -f build/scheduler/Dockerfile .
  echo "[ok] image built locally: ${IMG_TAG}"
fi

# Install Python deps for solver if runtime=binary
if [ "${KWOK_RUNTIME}" = "binary" ]; then
  echo "[instal] installing solver requirements"
  python3 -m pip install --no-cache-dir -r "${REPO_DIR}/scripts/mycrossnodepreemption/requirements.txt"
  echo "[ok] solver requirements installed"
fi

# Test Generator script arguments
args=(
  "--cluster-name" "$KWOK_CLUSTER_NAME"
  "--kwok-runtime" "$KWOK_RUNTIME"
  "--config-dir" "$KWOK_CONFIG_DIR"
  "--results-dir" "$RESULTS_DIR"
  "--seed-file" "$SEED_FILE"
)

# Run tests
echo "[tests] starting kwok tests"
set -o pipefail
python3 "$TEST_GENERATOR_SCRIPT" "${args[@]}"
echo
echo "[ok] kwok test done, results in $RESULTS_DIR"
