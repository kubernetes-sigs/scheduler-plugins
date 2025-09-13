#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="${REPO_DIR:-$HOME/scheduler-plugins}"
KWOK_DIR="${KWOK_DIR:-$REPO_DIR/scripts/kwok}"
TEST_GENERATOR_SCRIPT="${TEST_GENERATOR_SCRIPT:-$KWOK_DIR/kwok_test_generator.py}"
REQ_FILE="$KWOK_DIR/requirements.txt"

# Load settings
KWOKRC="${REPO_DIR}/.kwokrc"
if [[ -f "$KWOKRC" ]]; then
  # shellcheck disable=SC1090
  source "$KWOKRC"
else
  echo "[warn] $KWOKRC not found; using defaults"
  KWOK_CLUSTER="${KWOK_CLUSTER:-kwok1}"
  KWOK_CONFIGS="${KWOK_CONFIGS:-baseline}"
  KWOK_SEEDS="${KWOK_SEEDS:-seeds001.txt}"
fi

KWOK_CONFIG_DIR="${KWOK_DIR}/configs/${KWOK_CONFIGS}"
SEED_FILE="${KWOK_DIR}/seeds/${KWOK_SEEDS}"
RESULTS_DIR="${RESULTS_DIR:-${REPO_DIR}/results}"

echo "[cfg] cluster=${KWOK_CLUSTER}"
echo "[cfg] configs=${KWOK_CONFIG_DIR}"
echo "[cfg] seeds=${SEED_FILE}"
echo "[cfg] results=${RESULTS_DIR}"

[[ -d "$KWOK_CONFIG_DIR" ]] || { echo "[error] config dir not found: $KWOK_CONFIG_DIR"; exit 1; }
[[ -f "$SEED_FILE" ]]      || { echo "[error] seed file not found: $SEED_FILE"; exit 1; }
mkdir -p "$RESULTS_DIR"

# Sanity checks
for bin in python3 pip3 kubectl kwokctl docker; do
  command -v "$bin" >/dev/null || { echo "[error] $bin not found in PATH"; exit 1; }
done
docker buildx version >/dev/null || { echo "[error] docker buildx plugin missing"; exit 1; }

# Build scheduler image (two tags; avoids invalid tag with double colon)
IMG_TAG_BASE="${IMG_TAG:-localhost:5000/scheduler-plugins/kube-scheduler}"
cd "${REPO_DIR}"
GIT_VERSION="$(git -C "${REPO_DIR}" describe --tags --always 2>/dev/null || echo dev)"

echo "[build] docker image ${IMG_TAG_BASE}:${GIT_VERSION} and ${IMG_TAG_BASE}:dev"
docker buildx build --load \
  --build-arg VERSION="${GIT_VERSION}" \
  ${GO_BASE_IMAGE:+--build-arg GO_BASE_IMAGE="${GO_BASE_IMAGE}"} \
  ${UBUNTU_BASE_IMAGE:+--build-arg UBUNTU_BASE_IMAGE="${UBUNTU_BASE_IMAGE}"} \
  -t "${IMG_TAG_BASE}:${GIT_VERSION}" \
  -t "${IMG_TAG_BASE}:dev" \
  -f build/scheduler/Dockerfile .
echo "[ok] image built locally: ${IMG_TAG_BASE}:${GIT_VERSION} (and :dev)"

# Python deps (no venv; fastest)
export PATH="$HOME/.local/bin:$PATH"
python3 -m pip install --user --upgrade pip >/dev/null
python3 -m pip install --user -r "$REQ_FILE"

# Run the generator
cd "$KWOK_DIR"
[[ -f "$TEST_GENERATOR_SCRIPT" ]] || { echo "[error] missing: $TEST_GENERATOR_SCRIPT"; exit 1; }

args=(
  "--cluster-name" "$KWOK_CLUSTER"
  "--config-dir" "$KWOK_CONFIG_DIR"
  "--results-dir" "$RESULTS_DIR"
  "--seed-file" "$SEED_FILE"
)

echo "----- kwok test start: $(date +%Y%m%d_%H%M%S) -----"
set -o pipefail
python3 "$TEST_GENERATOR_SCRIPT" "${args[@]}"
echo
echo "----- kwok test end:   $(date +%Y%m%d_%H%M%S) -----"
echo "[ok] Results CSVs: $RESULTS_DIR"
