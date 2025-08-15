#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME=${1:-mycluster}
CLUSTER_CONTEXT="kind-${CLUSTER_NAME}"
NAMESPACE=${2:-crossnode-test}

# Check if namespace exists, then delete
if kubectl --context "$CLUSTER_CONTEXT" get ns "$NAMESPACE" >/dev/null 2>&1; then
  if kubectl --context "$CLUSTER_CONTEXT" delete ns "$NAMESPACE"; then
    echo "✅ Namespace '$NAMESPACE' deleted successfully."
  else
    echo "❌ Failed to delete namespace '$NAMESPACE'."
    exit 1
  fi
else
  echo "📋 Namespace '$NAMESPACE' does not exist."
  exit 1
fi