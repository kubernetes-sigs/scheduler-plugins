# bootstrap/02_build_test.sh
#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="${REPO_DIR:-$HOME/scheduler-plugins}"
KWOK_DIR="${KWOK_DIR:-$REPO_DIR/scripts/kwok}"
KWOKRC="${REPO_DIR}/.kwokrc"

# Defaults / settings
KUBE_VERSION="${KUBE_VERSION:-v1.32.7}"   # Kubernetes version kwokctl should use with binary runtime
KWOK_RUNTIME="${KWOK_RUNTIME:-binary}"    # read below from .kwokrc if present

# Load settings from .kwokrc
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

# --- Python deps on host (no docker) ---
VENV_DIR="${VENV_DIR:-$REPO_DIR/.venv}"
if [[ ! -d "$VENV_DIR/bin" ]]; then
  echo "[venv] creating $VENV_DIR"
  python3 -m venv "$VENV_DIR"
fi
# shellcheck disable=SC1090
source "$VENV_DIR/bin/activate"
python -m pip install --upgrade pip >/dev/null
python -m pip install -r "$KWOK_DIR/requirements.txt"

# --- Create KWOK cluster ---
# If you have a kwok cluster config yaml that’s tailored for binary runtime, you can pass it with --config.
# Otherwise, create it via flags (simple default cluster).
echo "[kwok] (re)creating cluster ${KWOK_CLUSTER} with runtime=${KWOK_RUNTIME}"
kwokctl delete cluster --name "${KWOK_CLUSTER}" >/dev/null 2>&1 || true

if [ "${KWOK_RUNTIME}" = "binary" ]; then
  # Pure host processes (no Docker)
  kwokctl create cluster \
    --name "${KWOK_CLUSTER}" \
    --runtime=binary \
    --kube-version "${KUBE_VERSION}"
else
  # Fallback: docker runtime (kept for convenience)
  kwokctl create cluster \
    --name "${KWOK_CLUSTER}" \
    --runtime=docker
fi

# --- Run your generator on the host ---
cd "$KWOK_DIR"
python kwok_test_generator.py \
  --cluster-name "${KWOK_CLUSTER}" \
  --config-dir "${KWOK_CONFIG_DIR}" \
  --results-dir "${RESULTS_DIR}" \
  --seed-file "${SEED_FILE}"

echo "[ok] Results CSVs: $RESULTS_DIR"
