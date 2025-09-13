# 01_system_setup.sh
#!/usr/bin/env bash
set -euo pipefail

DOCKER_CHANNEL="${DOCKER_CHANNEL:-stable}"
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.34.0}"
KWOK_VERSION="${KWOK_VERSION:-v0.7.0}"

export DEBIAN_FRONTEND=noninteractive

sudo apt-get update
sudo apt-get install -y --no-install-recommends \
  ca-certificates curl gnupg git python3 python3-venv jq

# Docker
echo "[info] Installing Docker ..."
sudo install -m 0755 -d /etc/apt/keyrings
[ -f /etc/apt/keyrings/docker.gpg ] || \
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$UBUNTU_CODENAME") ${DOCKER_CHANNEL}" \
| sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
sudo apt-get update
sudo apt-get install -y --no-install-recommends docker-ce docker-ce-cli containerd.io
sudo systemctl enable --now docker

# kubectl
echo "[info] Installing kubectl ${KUBECTL_VERSION}..."
cd /tmp
curl -fsSLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
sudo install -m 0755 kubectl /usr/local/bin/kubectl
kubectl version --client=true

# kwokctl
echo "[info] Installing kwokctl ${KWOK_VERSION}..."
curl -fsSLO "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwokctl-linux-amd64"
sudo install -m 0755 kwokctl-linux-amd64 /usr/local/bin/kwokctl
kwokctl --version

# Add current user to docker group (no-op if already)
if ! id -nG "$USER" | tr ' ' '\n' | grep -qx docker; then
  sudo usermod -aG docker "$USER" || true
  echo "[info] Added $USER to docker group (new shell needed)"
fi

echo "[ok] system setup done"
