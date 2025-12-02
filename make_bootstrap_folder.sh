#!/usr/bin/env bash
set -euo pipefail

# 1) Create a new folder called "bootstrap" (remove existing one)
echo "Cleaning and creating bootstrap directory..."
rm -rf bootstrap
mkdir -p bootstrap/content

# 2) Copy "data" subfolder into "bootstrap/content"
echo "Copying data subfolder into bootstrap/content..."
rsync -a --exclude='__pycache__' data/    bootstrap/content/data

# 3) Copy scripts subfolder into "bootstrap/content/scripts" (excluding __pycache__)
echo "Copying scripts subfolder into bootstrap/content/scripts..."
# Use rsync to skip __pycache__ directories
rsync -a --exclude='__pycache__' scripts/    bootstrap/content/scripts/

# 4) Move bootstrap file scripts to "bootstrap/" folder and make them executable
echo "Move bootstrap files to 'bootstrap' folder and set permissions..."
mv bootstrap/content/scripts/bootstrap/* bootstrap/
rmdir bootstrap/content/scripts/bootstrap
sed -i 's/\r$//' bootstrap/*.sh     # ensure line endings are LF (not CRLF)
chmod +x bootstrap/*.sh             # make copied scripts executable

# 5) Copy manifests into "bootstrap/content/manifests" folder
echo "Copying manifests files into 'manifests' folder..."
mkdir -p bootstrap/content/manifests/mypriorityoptimizer
mkdir -p bootstrap/content/manifests/mydeterministicscore
cp -r manifests/mypriorityoptimizer/* bootstrap/content/manifests/mypriorityoptimizer/
cp -r manifests/mydeterministicscore/* bootstrap/content/manifests/mydeterministicscore/

# 6) Call make to build the scheduler binary
echo "Building scheduler binary with make..."
make build-scheduler GO_BUILD_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=amd64'

# 7) Copy built binary from "bin/kube-scheduler" to "bootstrap/content/bin" folder
echo "Copying built kube-scheduler binary into 'bin' folder and set permissions..."
mkdir -p bootstrap/content/bin
cp bin/kube-scheduler bootstrap/content/bin/kube-scheduler
chmod +x bootstrap/content/bin/kube-scheduler  # make the copied binary executable

echo "Done."
