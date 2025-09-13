#!/usr/bin/env bash
set -euo pipefail

DOCKER_CHANNEL="${DOCKER_CHANNEL:-stable}"
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.34.0}"
KWOK_VERSION="${KWOK_VERSION:-v0.7.0}"

export DEBIAN_FRONTEND=noninteractive

sudo apt-get update
sudo apt-get install -y \
  ca-certificates curl gnupg lsb-release git make \
  python3 python3-venv python3-pip

# ---- Docker repo + engine ----
install -m 0755 -d /etc/apt/keyrings
if [ ! -f /etc/apt/keyrings/docker.gpg ]; then
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
    | sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg
fi
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$UBUNTU_CODENAME") ${DOCKER_CHANNEL}" \
| sudo tee /etc/apt/sources.list.d/docker.list >/dev/null

sudo apt-get update
sudo apt-get install -y \
  docker-ce docker-ce-cli containerd.io \
  docker-buildx-plugin docker-compose-plugin   # <-- add these

sudo systemctl enable --now docker

# Quick sanity check (ensures plugin is present)
docker --version
docker buildx version

# ---- kubectl + kwokctl ----
cd /tmp
curl -fsSLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
sudo install -m 0755 kubectl /usr/local/bin/kubectl
kubectl version --client=true

curl -fsSLO "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwokctl-linux-amd64"
sudo install -m 0755 kwokctl-linux-amd64 /usr/local/bin/kwokctl
kwokctl --version


# Add current user to docker group (no-op if already)
if ! id -nG "$USER" | tr ' ' '\n' | grep -qx docker; then
  sudo usermod -aG docker "$USER" || true
  echo "[info] Added $USER to docker group (new shell needed)"
fi

echo "[ok] system setup done"
