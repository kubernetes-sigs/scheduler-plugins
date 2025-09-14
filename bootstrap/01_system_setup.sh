#!/usr/bin/env bash
set -euo pipefail

REPO_NAME="scheduler-plugins"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="${REPO_DIR:-$(dirname "$SCRIPT_DIR")}"

KUBECTL_VERSION="v1.32.7"
KWOK_VERSION="v0.7.0"

GO_VERSION="1.24.3"
GO_ARCH="amd64"

# Read .kwokrc
KWOKRC="${REPO_DIR}/.kwokrc"
if [ ! -f "${KWOKRC}" ]; then
  echo "[error] missing ${KWOKRC} (run 00_init.sh first)"; exit 1
fi
# shellcheck disable=SC1090
source "${REPO_DIR}/.kwokrc"
echo "[init] read ${KWOKRC}: cluster=${KWOK_CLUSTER_NAME} configs=${KWOK_CONFIGS} seeds=${KWOK_SEEDS} runtime=${KWOK_RUNTIME}"

# Install kubectl + kwokctl + kwok
echo "[instal] installing kubectl ${KUBECTL_VERSION}, kwokctl+kwok ${KWOK_VERSION}"
cd /tmp
curl -fsSLo kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
curl -fsSLO "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwokctl-linux-amd64"
curl -fsSLO "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwok-linux-amd64"
install -m 0755 kubectl /usr/local/bin/kubectl
install -m 0755 kwokctl-linux-amd64 /usr/local/bin/kwokctl
install -m 0755 kwok-linux-amd64   /usr/local/bin/kwok
rm -f kubectl kwokctl-linux-amd64 kwok-linux-amd64
kubectl version --client=true
kwokctl --version
kwok --version
echo "[ok] kubectl, kwokctl, kwok installed"

# Install Go; if binary runtime
if [ "${KWOK_RUNTIME}" = "binary" ]; then
echo "[instal] installing golang ${GO_VERSION}"
curl -fsSLo /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
rm -rf /usr/local/go
tar -C /usr/local -xzf /tmp/go.tgz
tee /etc/profile.d/golang.sh >/dev/null <<'EOF'
export PATH="/usr/local/go/bin:${PATH}"
export GOPATH="${HOME}/go"
export GOCACHE="${HOME}/.cache/go-build"
EOF
chmod 0644 /etc/profile.d/golang.sh
# make Go available in this non-login shell too
export PATH="/usr/local/go/bin:${PATH}"
go version
echo "[ok] go installed: $(go version)"
fi

# Install Python
echo "[instal] installing python3, pip, venv"
apt-get install -y --no-install-recommends python3 python3-pip python3-venv
echo "[ok] python installed: $(python3 --version)"

# Install Docker; if docker runtime
if [ "${KWOK_RUNTIME}" = "docker" ]; then
  echo "[instal] installing docker"
  install -m 0755 -d /etc/apt/keyrings
  if [ ! -f /etc/apt/keyrings/docker.gpg ]; then
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  fi
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$UBUNTU_CODENAME") stable" \
    > /etc/apt/sources.list.d/docker.list
  apt-get update
  apt-get install -y --no-install-recommends \
    docker-ce docker-ce-cli containerd.io docker-buildx-plugin
  systemctl enable --now docker
  docker --version
  echo "[ok] docker installed"
fi
