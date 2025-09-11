#!/usr/bin/env bash
set -euo pipefail

# ------------------ Config (override via env) ------------------
REPO_URL="${REPO_URL:-https://github.com/henrikdchristensen/scheduler-plugins}"
REPO_BRANCH="${REPO_BRANCH:-henrikdc-cross-preemp}"   # e.g. main
GO_VERSION="${GO_VERSION:-1.24.3}"
TEST_ARGS="${TEST_ARGS:- -c 5}"                       # e.g. "--seed 123" or "-c 50"

# ------------------ Detect target user/home --------------------
pick_target_user() {
  if [ "$(id -u)" -eq 0 ]; then
    # Prefer the invoking user if present
    if [ -n "${SUDO_USER:-}" ] && [ "${SUDO_USER}" != "root" ]; then
      echo "${SUDO_USER}"
      return
    fi
    # Otherwise pick the first "human" user (uid >= 1000), fallback root
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
BOOT_DIR="${BOOT_DIR:-${REPO_DIR}/scripts/bootstrap}"

# ------------------ Helpers ------------------
run_root() {
  if [ "$(id -u)" -eq 0 ]; then
    bash -lc "$*"
  else
    sudo bash -lc "$*"
  fi
}

run_as_target() {
  # Use a login shell so new group membership (docker) takes effect
  if [ "$(id -un)" = "${TARGET_USER}" ]; then
    bash -lc "$*"
  else
    run_root "su -l ${TARGET_USER} -c $(printf '%q ' 'bash -lc' "$*")"
  fi
}

need_pkg() { dpkg -s "$1" >/dev/null 2>&1 || return 0 && return 1; }

# ------------------ Minimal OS prereqs ------------------
echo "[init] installing base packages (git, curl, ca-certificates, python3-venv, pip, jq)…"
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
run_root "chmod +x '${BOOT_DIR}'/01_system_setup.sh \
                '${BOOT_DIR}'/02_go.sh \
                '${BOOT_DIR}'/03_tools.sh \
                '${BOOT_DIR}'/04_clone_and_build.sh \
                '${BOOT_DIR}'/05_test.sh"

# ------------------ System-level setup (as root) ------------------
echo "[init] 01_system_setup.sh (Docker, etc.)"
run_root "cd '${BOOT_DIR}' && ./01_system_setup.sh"

echo "[init] 02_go.sh (Go ${GO_VERSION})"
run_root "cd '${BOOT_DIR}' && GO_VERSION='${GO_VERSION}' ./02_go.sh"

echo "[init] 03_tools.sh (kubectl + kwokctl)"
run_root "cd '${BOOT_DIR}' && ./03_tools.sh"

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
echo "[init] 04_clone_and_build.sh (runs as ${TARGET_USER})"
run_as_target "cd '${BOOT_DIR}' && ./04_clone_and_build.sh"

echo "[init] 05_test.sh ${TEST_ARGS} (runs as ${TARGET_USER})"
run_as_target "cd '${BOOT_DIR}' && ./05_test.sh ${TEST_ARGS}"

echo "[done] Initialization completed. Repo: ${REPO_DIR}"
