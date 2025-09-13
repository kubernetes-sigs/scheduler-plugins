#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="${REPO_DIR:-$HOME/scheduler-plugins}"
KWOK_DIR="${KWOK_DIR:-$REPO_DIR/scripts/kwok}"
KWOKRC="${REPO_DIR}/.kwokrc"

# Load settings
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
mkdir -p "$RESULTS_DIR"

echo "[cfg] cluster=${KWOK_CLUSTER}"
echo "[cfg] configs=${KWOK_CONFIG_DIR}"
echo "[cfg] seeds=${SEED_FILE}"
echo "[cfg] results=${RESULTS_DIR}"

# Pull the prebuilt scheduler image (override via env if needed)
SCHED_IMAGE="${SCHED_IMAGE:-ghcr.io/henrikdchristensen/scheduler-plugins/kube-scheduler:dev}"
echo "[pull] ${SCHED_IMAGE}"
docker pull "${SCHED_IMAGE}"

# Run the Python generator in a container so we don't install python locally
# Mount the repo so results/configs appear on host
echo "[gen] running generator in python:3.11-slim"
docker run --rm \
  -v "${REPO_DIR}:${REPO_DIR}" \
  -w "${KWOK_DIR}" \
  python:3.11-slim \
  bash -lc "pip install -r requirements.txt && python kwok_test_generator.py \
    --cluster-name '${KWOK_CLUSTER}' \
    --config-dir '${KWOK_CONFIG_DIR}' \
    --results-dir '${RESULTS_DIR}' \
    --seed-file '${SEED_FILE}'"

echo "[ok] generator finished; results in ${RESULTS_DIR}"
