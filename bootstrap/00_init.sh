#!/usr/bin/env bash
set -euo pipefail

# ------------------ Config (override via env) ------------------
REPO_URL="${REPO_URL:-https://github.com/henrikdchristensen/scheduler-plugins}"
REPO_BRANCH="${REPO_BRANCH:-henrikdc-cross-preemp}"
TEST_ARGS="${TEST_ARGS:-}"  # e.g. "--seed 123" or "-c 50"
#TODO find a way to pass CLUSTER_NAME, SEED_FILE, RESULTS_DIR, ...

# ------------------ Detect target user/home --------------------
pick_target_user() {
  if [ "$(id -u)" -eq 0 ]; then
    if [ -n "${SUDO_USER:-}" ] && [ "${SUDO_USER}" != "root" ]; then
      echo "${SUDO_USER}"; return
    fi
    # first non-system user (uid >= 1000), fallback root
    local u
    u="$(getent passwd | awk -F: '$3>=1000 && $1!="nobody"{print $1; exit}')"
    echo "${u:-root}"
  else
    id -un
  fi
}

TARGET_USER="$(pick_target_user)"
TARGET_HOME="$(eval echo "~${TARGET_USER}")"
REPO_DIR="${REPO_DIR:-${TARGET_HOME}/scheduler-plugins}"
BOOT_DIR="${BOOT_DIR:-${REPO_DIR}/bootstrap}"

# ------------------ Helpers ------------------
run_as_target() {
  if [ "$(id -un)" = "${TARGET_USER}" ]; then
    bash -lc "$*"
  else
    sudo -iu "${TARGET_USER}" bash -lc "$*"
  fi
}

run_root() {
  if [ "$(id -u)" -eq 0 ]; then
    bash -lc "$*"
  else
    sudo bash -lc "$*"
  fi
}

# ------------------ Minimal OS prereqs ------------------
echo "[init] installing base packages (git, curl, ca-certificates, python3-venv, pip, jq)..."
run_root "export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y git curl ca-certificates python3 python3-venv python3-pip jq"

# ------------------ Clone or update repo (owned by TARGET_USER) ------------------
if [ ! -d "${REPO_DIR}/.git" ]; then
  echo "[init] cloning ${REPO_URL} (branch=${REPO_BRANCH}) into ${REPO_DIR}"
  run_as_target "git clone --branch '${REPO_BRANCH}' --single-branch '${REPO_URL}' '${REPO_DIR}'"
else
  echo "[init] repo exists; updating branch=${REPO_BRANCH}"
  run_as_target "git -C '${REPO_DIR}' fetch --all --prune &&
                 git -C '${REPO_DIR}' checkout '${REPO_BRANCH}' &&
                 git -C '${REPO_DIR}' pull --ff-only"
fi

# Ensure ownership (in case script was run with sudo)
run_root "chown -R '${TARGET_USER}:${TARGET_USER}' '${REPO_DIR}'"

# ------------------ Mark bootstrap scripts executable ------------------
run_root "chmod +x '${BOOT_DIR}'/01_system_setup.sh '${BOOT_DIR}'/02_build_test.sh"

# ------------------ System-level setup (as root) ------------------
echo "[init] 01_system_setup.sh (Docker, Go, kubectl, kwokctl)"
run_root "cd '${BOOT_DIR}' && ./01_system_setup.sh"

# ------------------ Docker group for TARGET_USER ------------------
if command -v docker >/dev/null 2>&1; then
  if ! id -nG "${TARGET_USER}" | tr ' ' '\n' | grep -qx docker; then
    echo "[init] adding ${TARGET_USER} to docker group"
    run_root "usermod -aG docker '${TARGET_USER}'"
  fi
else
  echo "[warn] docker not found in PATH after 01_system_setup.sh"
fi

# ------------------ User-level steps (build + tests) ------------------
echo "[init] 02_build_test.sh ${TEST_ARGS} (runs as ${TARGET_USER})"
run_as_target "cd '${BOOT_DIR}' && ./02_build_test.sh ${TEST_ARGS}"

echo "[done] Initialization completed. Repo: ${REPO_DIR}"
