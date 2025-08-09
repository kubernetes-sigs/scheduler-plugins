#!/bin/bash

# Setup script for scheduler-plugins cluster (Default Scheduler Only)
# This script creates a Kind cluster with default scheduler ready for plugin deployment
# Usage: ./setup-cluster.sh [cluster-name]

set -e

CLUSTER_NAME=${1:-mycluster}
NUM_WORKERS=${2:-3}

echo "🚀 Setting up cluster: '${CLUSTER_NAME}' with ${NUM_WORKERS} worker nodes..."
echo "📋 This script only sets up the cluster with default scheduler"

# 1. Check if Kind cluster exists
echo "🔍 Checking Kind cluster..."
if ! kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
    echo "📋 Creating new Kind cluster..."
    kind create cluster --name "$CLUSTER_NAME" --config=<(cat <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
$(for ((i=1; i<=NUM_WORKERS; i++)); do echo "  - role: worker"; done)
EOF
)
    echo "✅ Kind cluster created successfully"
else
    echo "✅ Kind cluster '${CLUSTER_NAME}' already exists"
fi

CONTROL_PLANE_CONTAINER=$(docker ps | grep ${CLUSTER_NAME}-control-plane | awk '{print $1}')
CONTEXT="kind-${CLUSTER_NAME}"

echo "🔍 Control plane container ID: ${CONTROL_PLANE_CONTAINER}"

# Set scheduler.conf permissions for future use
echo "🔒 Setting scheduler.conf permissions..."
docker exec ${CONTROL_PLANE_CONTAINER} bash -c "chmod 0644 /etc/kubernetes/scheduler.conf"

# Wait for scheduler to be ready
echo "⏳ Waiting for scheduler to be ready..."
SCHED_POD="$(kubectl --context "$CONTEXT" -n kube-system get pods -o name | grep -m1 '^pod/kube-scheduler-')"
kubectl --context "$CONTEXT" wait --for=condition=Ready -n kube-system "$SCHED_POD" --timeout=60s

# Create cluster role binding for scheduler (TODO: only add roles needed)
echo "🔧 Creating cluster role binding for scheduler..."
kubectl --context "$CONTEXT" create clusterrolebinding scheduler-admin --clusterrole=cluster-admin --user=system:kube-scheduler

# Default scheduler running (no plugin deployment)
echo "✅ Cluster setup complete! Default scheduler is running."
echo "📋 To deploy custom scheduler plugins, use: ./load-plugins.sh [cluster-name] [plugin1,plugin2,...]"
echo "📋 Check cluster with: kubectl get nodes --context kind-${CLUSTER_NAME}"