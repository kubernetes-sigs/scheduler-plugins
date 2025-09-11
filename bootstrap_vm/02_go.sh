#!/usr/bin/env bash
set -euo pipefail

# Pin or override at runtime: GO_VERSION=1.24.3 sudo ./02_go.sh
GO_VERSION="${GO_VERSION:-1.24.3}"
GO_ARCH="amd64"

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
