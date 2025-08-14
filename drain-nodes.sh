#!/usr/bin/env bash

set -e # exit immediately if a command exits with a non-zero status.

CLUSTER_CONTEXT=${1:-mycluster}
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