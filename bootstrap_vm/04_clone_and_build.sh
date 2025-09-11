#!/usr/bin/env bash
set -euo pipefail

REPO_URL="https://github.com/henrikdchristensen/scheduler-plugins"
REPO_DIR="${HOME}/scheduler-plugins"
IMG_TAG="${IMG_TAG:-localhost:5000/scheduler-plugins/kube-scheduler:dev}"

if [ ! -d "${REPO_DIR}/.git" ]; then
  git clone "${REPO_URL}" "${REPO_DIR}"
else
  echo "[info] Repo already present; pulling latest..."
  git -C "${REPO_DIR}" pull --ff-only
fi

cd "${REPO_DIR}"

# Build image
docker build -t "${IMG_TAG}" -f build/scheduler/Dockerfile .

echo
echo "[ok] Image ready at: ${REPO_DIR}"

