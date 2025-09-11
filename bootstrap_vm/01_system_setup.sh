#!/usr/bin/env bash
set -euo pipefail

# ---- Config (override with env if desired) ----
DOCKER_CHANNEL="${DOCKER_CHANNEL:-stable}"

export DEBIAN_FRONTEND=noninteractive

sudo apt-get update
sudo apt-get install -y \
  ca-certificates curl gnupg lsb-release git make \
  python3 python3-venv python3-pip

# ---- Docker Engine ----
install -m 0755 -d /etc/apt/keyrings
if [ ! -f /etc/apt/keyrings/docker.gpg ]; then
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
    | sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg
fi
echo \
  "deb [arch=$(dpkg --print-architecture) \
signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/ubuntu \
$(. /etc/os-release && echo "$UBUNTU_CODENAME") ${DOCKER_CHANNEL}" \
  | sudo tee /etc/apt/sources.list.d/docker.list >/dev/null

sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io

# Allow current user to use docker without sudo (effective next login)
if ! groups "$USER" | grep -q docker; then
  sudo usermod -aG docker "$USER" || true
  echo "[info] Added $USER to docker group (re-login or new shell for effect)."
fi

sudo systemctl enable --now docker
echo "[ok] Docker installed and running."
