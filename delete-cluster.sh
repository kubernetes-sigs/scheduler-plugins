#!/bin/bash

# This script deletes a Kind cluster
# Usage: ./delete-cluster.sh [cluster-name]

set -e

CLUSTER_NAME=${1:-mycluster}

echo "🚀 Deleting cluster: '${CLUSTER_NAME}'..."
kind delete cluster --name "$CLUSTER_NAME"