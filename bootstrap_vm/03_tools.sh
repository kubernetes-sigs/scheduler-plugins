#!/usr/bin/env bash
set -euo pipefail

# You can override versions via env
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.34.0}"
KWOK_VERSION="${KWOK_VERSION:-v0.7.0}"

# --- kubectl ---
cd /tmp
curl -fsSLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
sudo install -m 0755 kubectl /usr/local/bin/kubectl
kubectl version --client=true

# --- kwokctl ---
curl -fsSLO "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwokctl-linux-amd64"
sudo install -m 0755 kwokctl-linux-amd64 /usr/local/bin/kwokctl
kwokctl --version

echo "[ok] kubectl ${KUBECTL_VERSION} and kwokctl ${KWOK_VERSION} installed."
