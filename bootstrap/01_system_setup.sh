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
GO_VERSION="${GO_VERSION:-1.24.3}"
GO_ARCH="amd64"

# --- Install kubectl + kwokctl + kwok ---
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

# --- Install Go; if binary runtime
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

# --- Install Python ---
apt-get install -y --no-install-recommends python3 python3-pip python3-venv

# --- Install Docker; if docker runtime
if [ "${KWOK_RUNTIME}" = "docker" ]; then
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
  # ensure the login user can talk to dockerd
  TARGET_USER="${TARGET_USER:-${SUDO_USER:-vagrant}}"
  if ! id -nG "${TARGET_USER}" | tr ' ' '\n' | grep -qx docker; then
    usermod -aG docker "${TARGET_USER}"
    echo "[info] Added ${TARGET_USER} to docker group"
  fi
  docker --version
fi

echo "[ok] system setup done (runtime=${KWOK_RUNTIME})"
