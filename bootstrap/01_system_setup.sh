#!/usr/bin/env bash
set -euo pipefail

# Read runtime if .kwokrc exists (optional)
KWOK_RUNTIME="${KWOK_RUNTIME:-${KWOK_RUNTIME_FROM_ENV:-}}"
if [ -z "${KWOK_RUNTIME}" ] && [ -f "${HOME}/scheduler-plugins/.kwokrc" ]; then
  # shellcheck disable=SC1090
  source "${HOME}/scheduler-plugins/.kwokrc"
fi
KWOK_RUNTIME="${KWOK_RUNTIME:-binary}"   # default

KUBECTL_VERSION="${KUBECTL_VERSION:-v1.32.7}"
KWOK_VERSION="${KWOK_VERSION:-v0.7.0}"
GO_VERSION="${GO_VERSION:-1.24.3}"      # <-- pick your Go version
GO_ARCH="amd64"

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y --no-install-recommends ca-certificates curl make

# --- kubectl ---
cd /tmp
curl -fsSLo kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
install -m 0755 kubectl /usr/local/bin/kubectl
rm -f kubectl
kubectl version --client=true

# --- kwokctl + kwok (binary runtime) ---
curl -fsSLO "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwokctl-linux-amd64"
curl -fsSLO "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwok-linux-amd64"
install -m 0755 kwokctl-linux-amd64 /usr/local/bin/kwokctl
install -m 0755 kwok-linux-amd64   /usr/local/bin/kwok
rm -f kwokctl-linux-amd64 kwok-linux-amd64
kwokctl --version
kwok --version

# --- Go toolchain ---
# only if KWOK_RUNTIME is binary
if [ "${KWOK_RUNTIME}" = "binary" ]; then
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
fi

# --- minimal Python for solver/generator if needed later ---
apt-get install -y --no-install-recommends python3 python3-pip python3-venv

# Docker only if we have KWOK_RUNTIME=docker
if [ "${KWOK_RUNTIME}" = "docker" ]; then
  install -m 0755 -d /etc/apt/keyrings
  if [ ! -f /etc/apt/keyrings/docker.gpg ]; then
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  fi
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$UBUNTU_CODENAME") stable" \
    > /etc/apt/sources.list.d/docker.list
  apt-get update
  apt-get install -y --no-install-recommends docker-ce docker-ce-cli containerd.io
  systemctl enable --now docker
  docker --version
fi

echo "[ok] system setup done (runtime=${KWOK_RUNTIME})"
