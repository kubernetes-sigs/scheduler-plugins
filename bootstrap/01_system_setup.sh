#!/usr/bin/env bash
set -euo pipefail

DOCKER_CHANNEL="${DOCKER_CHANNEL:-stable}"
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.34.0}"
KWOK_VERSION="${KWOK_VERSION:-v0.7.0}"
export DEBIAN_FRONTEND=noninteractive

# Only what's needed for Docker repo + downloads
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl gnupg

install -m 0755 -d /etc/apt/keyrings
if [ ! -f /etc/apt/keyrings/docker.gpg ]; then
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
fi

echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$UBUNTU_CODENAME") ${DOCKER_CHANNEL}" \
> /etc/apt/sources.list.d/docker.list

apt-get update
apt-get install -y --no-install-recommends \
  docker-ce docker-ce-cli containerd.io docker-buildx-plugin \
  python3 python3-pip   # minimal Python for the generator

systemctl enable --now docker

# Sanity
docker --version
docker buildx version

# kubectl
cd /tmp
curl -fsSLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
install -m 0755 kubectl /usr/local/bin/kubectl
kubectl version --client=true

# kwokctl
curl -fsSLO "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwokctl-linux-amd64"
install -m 0755 kwokctl-linux-amd64 /usr/local/bin/kwokctl
kwokctl --version

echo "[ok] system setup done"
