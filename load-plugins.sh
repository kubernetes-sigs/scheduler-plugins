#!/bin/bash

# Hot reload script for scheduler plugins with custom plugin selection
# This script builds and deploys a custom scheduler with specified plugins
# Usage: ./load-plugins.sh [cluster-name] [plugin1,plugin2,...]
# Example: ./load-plugins.sh mycluster "MyCrossNodePreemption, ..."

set -e

CLUSTER_NAME=${1:-mycluster}
PLUGINS=${2:-"MyCrossNodePreemption"}
SCHEDULER_NAME="kube-scheduler"
IMAGE_TAG="v$(date +%Y%m%d-%H%M%S)"
IMAGE_NAME="localhost:5000/scheduler-plugins/kube-scheduler:${IMAGE_TAG}"

# Get Go version from go.mod (same as Makefile)
GO_VERSION="1.24"
GO_BASE_IMAGE="golang:${GO_VERSION}"

echo "🔥 Load plugins: '${PLUGINS}' into scheduler"
echo "📋 Building image: ${IMAGE_NAME}"
echo "📋 Using Go base image: ${GO_BASE_IMAGE}"

# 1. Check if Kind cluster exists
echo "🔍 Checking Kind cluster..."
if ! kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
    echo "❌ Kind cluster '${CLUSTER_NAME}' not found!"
    echo "Create it with: ./setup-cluster.sh ${CLUSTER_NAME}"
    exit 1
fi

CONTROL_PLANE_CONTAINER=$(docker ps | grep ${CLUSTER_NAME}-control-plane | awk '{print $1}')

# 2. Create scheduler configuration with specified plugins and disable default preemption
echo "📝 Creating scheduler configuration with plugins: ${PLUGINS} (DefaultPreemption disabled)"
IFS=',' read -ra PLUGIN_ARRAY <<< "$PLUGINS"

# Build the plugins section for the scheduler config
PLUGIN_CONFIG=""
for plugin in "${PLUGIN_ARRAY[@]}"; do
    plugin=$(echo "$plugin" | xargs) # trim whitespace
    PLUGIN_CONFIG+="      - name: $plugin"$'\n'
done

# TODO: Only disable DefaultPreemption if MyCrossNodePreemption is enabled
docker exec ${CONTROL_PLANE_CONTAINER} bash -c "cat > /etc/kubernetes/sched-cc.yaml << 'EOF'
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: /etc/kubernetes/scheduler.conf
profiles:
- schedulerName: default-scheduler
  plugins:
    # Disable default preemption to avoid conflicts with custom cross-node preemption
    multiPoint:
      disabled:
      - name: DefaultPreemption
      enabled:
${PLUGIN_CONFIG}
EOF"

echo "✅ Scheduler configuration created with plugins: ${PLUGINS}"

# 3. Build new Docker image with plugins
echo "📦 Building Docker image with specified plugins..."
docker build -t ${IMAGE_NAME} -f build/scheduler/Dockerfile --build-arg GO_BASE_IMAGE=${GO_BASE_IMAGE} --build-arg TARGETARCH=amd64 .

# 4. Load image into Kind cluster
echo "📋 Loading image into Kind cluster..."
kind load docker-image ${IMAGE_NAME} --name ${CLUSTER_NAME}

# 5. Update scheduler manifest to use custom config and image
echo "🔄 Updating scheduler manifest..."
docker exec ${CONTROL_PLANE_CONTAINER} bash -c "cat > /etc/kubernetes/manifests/kube-scheduler.yaml << 'EOF'
apiVersion: v1
kind: Pod
metadata:
  creationTimestamp: null
  labels:
    component: kube-scheduler
    tier: control-plane
  name: kube-scheduler
  namespace: kube-system
spec:
  containers:
  - command:
    - kube-scheduler
    - --config=/etc/kubernetes/sched-cc.yaml
    - --authentication-kubeconfig=/etc/kubernetes/scheduler.conf
    - --authorization-kubeconfig=/etc/kubernetes/scheduler.conf
    - --bind-address=127.0.0.1
    - --leader-elect=true
    - --v=2
    image: ${IMAGE_NAME}
    imagePullPolicy: IfNotPresent
    livenessProbe:
      failureThreshold: 8
      httpGet:
        host: 127.0.0.1
        path: /livez
        port: 10259
        scheme: HTTPS
      initialDelaySeconds: 10
      periodSeconds: 10
      timeoutSeconds: 15
    name: kube-scheduler
    readinessProbe:
      failureThreshold: 3
      httpGet:
        host: 127.0.0.1
        path: /readyz
        port: 10259
        scheme: HTTPS
      periodSeconds: 1
      timeoutSeconds: 15
    resources:
      requests:
        cpu: 100m
    startupProbe:
      failureThreshold: 24
      httpGet:
        host: 127.0.0.1
        path: /livez
        port: 10259
        scheme: HTTPS
      initialDelaySeconds: 10
      periodSeconds: 10
      timeoutSeconds: 15
    volumeMounts:
    - mountPath: /etc/kubernetes/scheduler.conf
      name: kubeconfig
      readOnly: true
    - mountPath: /etc/kubernetes/sched-cc.yaml
      name: sched-cc
      readOnly: true
  hostNetwork: true
  priority: 2000001000
  priorityClassName: system-node-critical
  securityContext:
    seccompProfile:
      type: RuntimeDefault
  volumes:
  - hostPath:
      path: /etc/kubernetes/scheduler.conf
      type: FileOrCreate
    name: kubeconfig
  - hostPath:
      path: /etc/kubernetes/sched-cc.yaml
      type: FileOrCreate
    name: sched-cc
status: {}
EOF
"

# 6. Wait for scheduler pod to restart and be ready
echo "⏳ Waiting for scheduler pod to restart..."
sleep 10
kubectl wait --for=condition=Ready pod -l component=kube-scheduler -n kube-system --timeout=120s --context kind-${CLUSTER_NAME}

# 7. Get the scheduler pod name and show status
SCHEDULER_POD=$(kubectl get pods -n kube-system -l component=kube-scheduler --context kind-${CLUSTER_NAME} -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")

echo ""
echo "✅ Hot reload complete! Custom scheduler is now active with plugins: ${PLUGINS}"
echo "🎯 Scheduler pod: $SCHEDULER_POD"
echo "📋 Check logs with: kubectl logs -n kube-system $SCHEDULER_POD -f --context kind-${CLUSTER_NAME}"
echo "📋 Image used: ${IMAGE_NAME}"
echo "🧪 Test with your plugin-specific test scenarios"

# 8. Show plugin configuration
echo ""
echo "📋 Current scheduler configuration:"
docker exec ${CONTROL_PLANE_CONTAINER} cat /etc/kubernetes/sched-cc.yaml
