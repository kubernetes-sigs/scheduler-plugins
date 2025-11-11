#!/bin/bash
set -euo pipefail

# This script loads the scheduler plugin into a running Kind cluster
# Usage: ./load-plugins.sh [cluster-name]

########################## Defaults #########################
#############################################################
CLUSTER_NAME=${1:-kind1}
IMAGE_NAME="localhost:5000/scheduler-plugins/kube-scheduler:dev"

########################## Helpers ##########################
#############################################################

log(){ printf '[%s] %s\n' "$1" "$2"; }
die(){ log error "$1"; exit 1; }

############################ Main script ####################
#############################################################

# Check if Kind cluster exists
log "info" "Checking Kind cluster..."
if ! kind get clusters | grep -qx "$CLUSTER_NAME"; then
    die "Kind cluster '${CLUSTER_NAME}' not found! Create it with: ./setup-cluster.sh ${CLUSTER_NAME}"
fi
log "ok" "Kind cluster '${CLUSTER_NAME}' found"

# Load scheduler config into control-plane container
log "info" "Loading plugin manifests into Kind cluster..."
CONTROL_PLANE_CONTAINER="$(kind get nodes --name "$CLUSTER_NAME" | grep control-plane | head -n1)"
[ -n "$CONTROL_PLANE_CONTAINER" ]
docker exec ${CONTROL_PLANE_CONTAINER} bash -c "cat > /etc/kubernetes/sched-cc.yaml << 'EOF'
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
clientConnection:
  kubeconfig: /etc/kubernetes/scheduler.conf
profiles:
- schedulerName: default-scheduler
  plugins:
    preEnqueue:
      enabled:
        - name: MyPriorityOptimizer
    preFilter:
      enabled:
        - name: MyPriorityOptimizer
    postFilter:
      enabled:
        - name: MyPriorityOptimizer
    reserve:
      enabled:
        - name: MyPriorityOptimizer
    postBind:
      enabled:
        - name: MyPriorityOptimizer
    multiPoint:
      disabled:
        - name: DefaultPreemption
EOF"

# Build new Docker image with plugins
log "info" "Building Docker image with the plugin"
docker build -t ${IMAGE_NAME} -f build/scheduler/Dockerfile .
log "ok" "Docker image built: ${IMAGE_NAME}"

# Load image into Kind cluster
log "info" "Loading image into Kind cluster..."
kind load docker-image ${IMAGE_NAME} --name ${CLUSTER_NAME}
log "ok" "Image loaded into Kind cluster"

# Update scheduler manifest to use custom config and image
log "info" "Updating scheduler manifest with specified environment variables..."
docker exec "${CONTROL_PLANE_CONTAINER}" bash -lc "cat > /etc/kubernetes/manifests/kube-scheduler.yaml << EOF
apiVersion: v1
kind: Pod
metadata:
  labels:
    component: kube-scheduler
    tier: control-plane
  name: kube-scheduler
  namespace: kube-system
spec:
  containers:
  - name: kube-scheduler
    image: ${IMAGE_NAME}
    imagePullPolicy: IfNotPresent
    command:
    - kube-scheduler
    - --config=/etc/kubernetes/sched-cc.yaml
    - --kubeconfig=/etc/kubernetes/scheduler.conf
    - --authentication-kubeconfig=/etc/kubernetes/scheduler.conf
    - --authorization-kubeconfig=/etc/kubernetes/scheduler.conf
    - --bind-address=127.0.0.1
    - --leader-elect=true
    - --v=0 # verbosity level
    env:
    - name: OPTIMIZE_MODE
      value: \"all_synch\"
    - name: OPTIMIZE_AT
      value: \"postfilter\"
    - name: OPTIMIZATION_INTERVAL
      value: \"30s\"
    - name: OPTIMIZATION_INITIAL_DELAY
      value: \"15s\"
    - name: PLAN_EXECUTION_TIMEOUT
      value: \"600h\"
    - name: SOLVER_PYTHON_ENABLED
      value: \"true\"
    - name: SOLVER_PYTHON_TIMEOUT
      value: \"10s\"
    volumeMounts:
    - name: kubeconfig
      mountPath: /etc/kubernetes/scheduler.conf
      readOnly: true
    - name: sched-cc
      mountPath: /etc/kubernetes/sched-cc.yaml
      readOnly: true
  hostNetwork: true
  priorityClassName: system-node-critical
  securityContext:
    seccompProfile:
      type: RuntimeDefault
  volumes:
  - name: kubeconfig
    hostPath:
      path: /etc/kubernetes/scheduler.conf
      type: FileOrCreate
  - name: sched-cc
    hostPath:
      path: /etc/kubernetes/sched-cc.yaml
      type: FileOrCreate
EOF"
log "ok" "Scheduler manifest updated"

# Wait for restart & ready
log "info" "Waiting for scheduler pod to restart..."
kubectl wait --for=condition=Ready pod -l component=kube-scheduler -n kube-system \
  --timeout=120s --context "kind-${CLUSTER_NAME}"
log "ok" "Scheduler pod is running"

# Get the scheduler pod name and show status
SCHEDULER_POD=$(kubectl get pods -n kube-system -l component=kube-scheduler --context kind-${CLUSTER_NAME} -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")

log "info" "Load complete! Scheduler is now running with the plugin"
log "info" "Scheduler pod: $SCHEDULER_POD"
log "info" "Check the status with: kubectl get pod -n kube-system $SCHEDULER_POD -o wide --context kind-${CLUSTER_NAME}"
log "info" "Check logs with: kubectl logs -n kube-system $SCHEDULER_POD -f --context kind-${CLUSTER_NAME}"
log "info" "Check all pods are running with: kubectl get pods -A --context kind-${CLUSTER_NAME}"

log "done" "Plugin loaded successfully"