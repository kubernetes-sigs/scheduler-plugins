#!/usr/bin/env bash
set -euo pipefail

# This script creates a Kind cluster with the default scheduler.
# It names worker nodes as worker1..workerN via kubeadmConfigPatches.
# Usage: ./setup-cluster.sh [cluster-name] [num-workers]
# Example: ./setup-cluster.sh mycluster 3

CLUSTER_NAME=${1:-mycluster}
NUM_WORKERS=${2:-3}

if ! [[ "$NUM_WORKERS" =~ ^[0-9]+$ ]] || (( NUM_WORKERS < 1 )); then
  echo "❌ NUM_WORKERS must be a positive integer (got: $NUM_WORKERS)" >&2
  exit 1
fi

echo "🚀 Setting up cluster: '${CLUSTER_NAME}' with ${NUM_WORKERS} worker nodes (named worker1..worker${NUM_WORKERS})"
echo "📋 This script sets up the cluster with the default scheduler"

CONTEXT="kind-${CLUSTER_NAME}"

# --- Build kind config with explicit worker node names ---
gen_kind_config() {
  cat <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: ${CLUSTER_NAME}
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: InitConfiguration
    nodeRegistration:
      name: control-plane
EOF

  for ((i=1; i<=NUM_WORKERS; i++)); do
    cat <<EOF
- role: worker
  kubeadmConfigPatches:
  - |
    kind: JoinConfiguration
    nodeRegistration:
      name: worker${i}
EOF
  done
}

echo "🔍 Checking Kind cluster..."
if ! kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  echo "📋 Creating new Kind cluster..."
  cfg="$(mktemp)"
  gen_kind_config > "$cfg"
  kind create cluster --name "$CLUSTER_NAME" --config "$cfg"
  rm -f "$cfg"
  echo "✅ Kind cluster created successfully"
else
  echo "✅ Kind cluster '${CLUSTER_NAME}' already exists"
fi

# Correctly find the control-plane container for this cluster
CONTROL_PLANE_CONTAINER="$(
  docker ps --format '{{.ID}} {{.Names}}' \
  | awk -v n="${CLUSTER_NAME}-control-plane" '$2==n{print $1}'
)"
echo "🔍 Control plane container ID: ${CONTROL_PLANE_CONTAINER:-<not found>}"

# Best-effort: relax scheduler.conf perms
if [[ -n "${CONTROL_PLANE_CONTAINER:-}" ]]; then
  echo "🔒 Setting scheduler.conf permissions..."
  docker exec "${CONTROL_PLANE_CONTAINER}" bash -c "chmod 0644 /etc/kubernetes/scheduler.conf" || true
fi

# Wait for kube-scheduler to be ready
echo "⏳ Waiting for scheduler to be ready..."
SCHED_POD="$(kubectl --context "$CONTEXT" -n kube-system get pods -o name | grep -m1 '^pod/kube-scheduler-')"
kubectl --context "$CONTEXT" wait --for=condition=Ready -n kube-system "$SCHED_POD" --timeout=90s

# RBAC for scheduler (broad, fine for dev)
echo "🔧 Creating cluster role binding for scheduler (cluster-admin to kube-scheduler)..."
kubectl --context "$CONTEXT" create clusterrolebinding scheduler-admin \
  --clusterrole=cluster-admin --user=system:kube-scheduler 2>/dev/null || true

echo "✅ Cluster '${CLUSTER_NAME}' is ready with default scheduler"
echo "📋 To deploy scheduler plugins later, use: ./load-plugins.sh [cluster-name] [plugin1,plugin2,...]"
