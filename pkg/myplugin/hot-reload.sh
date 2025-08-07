#!/bin/bash

# Hot reload script for MyPlugin development (Single Scheduler Mode)
# This script updates the default Kubernetes scheduler with MyPlugin changes
# Prerequisites: Run './setup-scheduler-plugins.sh [cluster-name]' first
# Usage: ./hot-reload.sh [cluster-name]

set -e

CLUSTER_NAME=${1:-mycluster}
SCHEDULER_NAME="kube-scheduler"
IMAGE_TAG="v$(date +%Y%m%d-%H%M%S)"
IMAGE_NAME="localhost:5000/scheduler-plugins/kube-scheduler:${IMAGE_TAG}"

# Get Go version from go.mod (same as Makefile)
GO_VERSION=$(awk '/^go /{print $2}' go.mod | head -n1)
GO_BASE_IMAGE="golang:${GO_VERSION}"

echo "🔥 Hot reload for MyPlugin scheduler..."
echo "📋 Building image: ${IMAGE_NAME}"
echo "📋 Using Go base image: ${GO_BASE_IMAGE}"

# 1. Check if Kind cluster exists
echo "🔍 Checking Kind cluster..."
if ! kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
    echo "❌ Kind cluster '${CLUSTER_NAME}' not found!"
    echo "Create it with: ./setup-scheduler-plugins.sh ${CLUSTER_NAME}"
    exit 1
fi

# 2. Verify scheduler-plugins setup
echo "🔍 Checking scheduler-plugins installation..."
CONTROL_PLANE_CONTAINER=$(docker ps | grep ${CLUSTER_NAME}-control-plane | awk '{print $1}')

# Check if scheduler config file exists
if ! docker exec ${CONTROL_PLANE_CONTAINER} test -f /etc/kubernetes/sched-cc.yaml; then
    echo "❌ Scheduler config file not found."
    echo "🔧 Please run the setup script first: ./setup-scheduler-plugins.sh ${CLUSTER_NAME}"
    exit 1
fi

# Check if scheduler is using scheduler-plugins image
CURRENT_IMAGE=$(kubectl get pods -l component=kube-scheduler -n kube-system --context kind-${CLUSTER_NAME} -o=jsonpath="{.items[0].spec.containers[0].image}" 2>/dev/null || echo "")
if [[ ! "$CURRENT_IMAGE" =~ "scheduler-plugins" ]]; then
    echo "❌ Scheduler is not using scheduler-plugins image (current: $CURRENT_IMAGE)."
    echo "🔧 Please run the setup script first: ./setup-scheduler-plugins.sh ${CLUSTER_NAME}"
    exit 1
fi

echo "✅ Scheduler-plugins installation verified. Proceeding with hot-reload..."

# 3. Build new Docker image (much faster than full make)
echo "📦 Building Docker image with latest changes..."
docker build -t ${IMAGE_NAME} -f build/scheduler/Dockerfile --build-arg TARGETARCH=amd64 --build-arg GO_BASE_IMAGE=${GO_BASE_IMAGE} .

# 4. Load image into Kind cluster
echo "📋 Loading image into Kind cluster..."
kind load docker-image ${IMAGE_NAME} --name ${CLUSTER_NAME}

# 5. Update the default scheduler static pod with new image
echo "🔄 Updating default scheduler with new image..."
docker exec ${CONTROL_PLANE_CONTAINER} bash -c "
sed -i 's|image: registry.k8s.io/scheduler-plugins/kube-scheduler:.*|image: ${IMAGE_NAME}|g' /etc/kubernetes/manifests/kube-scheduler.yaml
sed -i 's|image: localhost:5000/scheduler-plugins/kube-scheduler:.*|image: ${IMAGE_NAME}|g' /etc/kubernetes/manifests/kube-scheduler.yaml
"

# 6. Wait for scheduler pod to restart and be ready
echo "⏳ Waiting for scheduler pod to restart..."
sleep 5
kubectl wait --for=condition=Ready pod -l component=kube-scheduler -n kube-system --timeout=60s --context kind-${CLUSTER_NAME}

# 7. Get the scheduler pod name and show status
SCHEDULER_POD=$(kubectl get pods -n kube-system -l component=kube-scheduler --context kind-${CLUSTER_NAME} -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")

echo ""
echo "✅ Hot reload complete! Your changes are now active."
echo "🎯 Default scheduler pod: $SCHEDULER_POD"
echo "📋 Check logs with: kubectl logs -n kube-system $SCHEDULER_POD -f --context kind-${CLUSTER_NAME}"
echo "📋 Image used: ${IMAGE_NAME}"
echo "🧪 Test with: kubectl apply -f test-myplugin-pod.yaml --context kind-${CLUSTER_NAME}"
