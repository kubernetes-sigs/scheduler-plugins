#!/bin/bash
set -euo pipefail

# This script deletes a Kind cluster
# Usage: ./delete-cluster.sh [cluster-name]

CLUSTER_NAME=${1:-mycluster}

echo "🚀 Deleting cluster: '${CLUSTER_NAME}'..."
kind delete cluster --name "$CLUSTER_NAME"