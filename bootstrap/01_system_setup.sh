#!/usr/bin/env bash
set -euo pipefail

DOCKER_CHANNEL="${DOCKER_CHANNEL:-stable}"
GO_VERSION="${GO_VERSION:-1.24.3}"
GO_ARCH="amd64"
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.34.0}"
KWOK_VERSION="${KWOK_VERSION:-v0.7.0}"

export DEBIAN_FRONTEND=noninteractive

sudo apt-get update
sudo apt-get install -y \
  ca-certificates curl gnupg lsb-release git make \
  python3 python3-venv python3-pip

########## DOCKER INSTALL ##########
echo "[info] Installing Docker ..."
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

########## GO INSTALL##########
echo "[info] Installing Go ${GO_VERSION} ..."
cd /tmp
curl -fsSLo go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"

# Clean old Go and install fresh
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go.tar.gz

# Persist PATH + defaults for all users/shells
sudo tee /etc/profile.d/golang.sh >/dev/null <<'EOF'
export PATH="/usr/local/go/bin:${PATH}"
export GOPATH="${HOME}/go"
export GOCACHE="${HOME}/.cache/go-build"
EOF
sudo chmod 0644 /etc/profile.d/golang.sh

# Ensure current non-login shell sees Go now
export PATH="/usr/local/go/bin:${PATH}"
export GOPATH="${HOME}/go"
export GOCACHE="${HOME}/.cache/go-build"

go version
echo "[ok] Go installed."

########### KUBECTL + KWOKCTL INSTALL ##########
echo "[info] Installing kubectl ${KUBECTL_VERSION}..."
cd /tmp
curl -fsSLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
sudo install -m 0755 kubectl /usr/local/bin/kubectl
kubectl version --client=true
echo "[ok] kubectl ${KUBECTL_VERSION} installed."
echo "[info] Installing kwokctl ${KWOK_VERSION}..."
curl -fsSLO "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwokctl-linux-amd64"
sudo install -m 0755 kwokctl-linux-amd64 /usr/local/bin/kwokctl
kwokctl --version
echo "[ok] kwokctl ${KWOK_VERSION} installed."