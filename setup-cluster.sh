#!/bin/bash

# Setup script for scheduler-plugins (Single Scheduler Mode)
# This script performs the one-time setup following the official install guide
# Usage: ./setup-scheduler-plugins.sh [cluster-name]

set -e

CLUSTER_NAME=${1:-mycluster}

echo "🚀 Setting up scheduler-plugins for cluster: ${CLUSTER_NAME}"
echo "📋 Following official guide: https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/doc/install.md"

# 1. Check if Kind cluster exists
echo "🔍 Checking Kind cluster..."
if ! kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
    echo "📋 Creating new Kind cluster..."
    kind create cluster --name ${CLUSTER_NAME} --config kind-3node-config.yaml
    echo "✅ Kind cluster created successfully"
else
    echo "✅ Kind cluster '${CLUSTER_NAME}' found"
fi

CONTROL_PLANE_CONTAINER=$(docker ps | grep ${CLUSTER_NAME}-control-plane | awk '{print $1}')
CONTEXT="kind-${CLUSTER_NAME}"

echo "🔍 Control plane container ID: ${CONTROL_PLANE_CONTAINER}"

# 2. Create scheduler configuration file
echo "📝 Creating scheduler configuration file..."
docker exec ${CONTROL_PLANE_CONTAINER} bash -c "cat > /etc/kubernetes/sched-cc.yaml << 'EOF'
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
leaderElection:
  # (Optional) Change true to false if you are not running a HA control-plane.
  leaderElect: true
clientConnection:
  kubeconfig: /etc/kubernetes/scheduler.conf
profiles:
- schedulerName: default-scheduler
  plugins:
    multiPoint:
      enabled:
      - name: MyPlugin
EOF"

# 3. Set scheduler.conf permissions
echo "🔒 Setting scheduler.conf permissions..."
docker exec ${CONTROL_PLANE_CONTAINER} bash -c "chmod 0644 /etc/kubernetes/scheduler.conf"

# 4. Update kube-scheduler.yaml to use scheduler-plugins image and config
# Original file can be copied by logging into control-plane and using 'docker cp'
    # docker exec -it mycluster-control-plane bash
    # docker cp mycluster-control-plane:/etc/kubernetes/scheduler.conf sched-cc.yaml
echo "🔄 Updating kube-scheduler.yaml..."
docker exec ${CONTROL_PLANE_CONTAINER} bash -c "
# Create new kube-scheduler.yaml with scheduler-plugins configuration
cat > /etc/kubernetes/manifests/kube-scheduler.yaml << 'EOF'
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
    - --authentication-kubeconfig=/etc/kubernetes/scheduler.conf
    - --authorization-kubeconfig=/etc/kubernetes/scheduler.conf
    - --bind-address=127.0.0.1
    - --config=/etc/kubernetes/sched-cc.yaml
    image: registry.k8s.io/scheduler-plugins/kube-scheduler:v0.32.7
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

# 9. Wait for scheduler to restart and be ready
echo "⏳ Waiting for scheduler to restart with new configuration..."
sleep 10
kubectl wait --for=condition=Ready pod -l component=kube-scheduler -n kube-system --timeout=120s --context ${CONTEXT}

echo ""
echo "Automatically running hot-reload to deploy MyPlugin..."

# 11. Automatically run hot-reload script to deploy MyPlugin
if [ -f "./hot-reload.sh" ]; then
    echo "�🚀 Running hot-reload script..."
    ./hot-reload.sh ${CLUSTER_NAME}
    echo ""
    echo "🎉 Complete setup finished! MyPlugin scheduler is now active."
    echo "📋 Test with: kubectl apply -f test-myplugin-pod.yaml --context ${CONTEXT}"
else
    echo "⚠️  Hot-reload script not found. You can manually run: './hot-reload.sh ${CLUSTER_NAME}'"
fi
