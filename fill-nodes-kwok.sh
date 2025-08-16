#!/usr/bin/env bash
set -euo pipefail

# Args: <CLUSTER_NAME> <NAMESPACE> <NUM_NODES> <NODE_CPU> <NODE_MEM> <PODS_PER_NODE> <TARGET_UTIL>
# Example:
#   ./kwok-fill.sh kwok crossnode-test 3 24 32Gi 6 0.9
#
# Notes:
# - NODE_CPU can be "24" (cores), NODE_MEM like "32Gi".
# - TARGET_UTIL is 0..1 (per node). We randomize requests but the per-node sum ≈ TARGET_UTIL * capacity.

CLUSTER_NAME=${1:-kwok}
CTX="kwok-${CLUSTER_NAME}"
NS=${2:-crossnode-test}
NUM_NODES=${3:-3}
NODE_CPU_IN=${4:-24}
NODE_MEM_IN=${5:-32Gi}
PODS_PER_NODE=${6:-4}
TARGET_UTIL=${7:-0.9}

# Image doesn’t run; KWOK will flip to Running. Keep it lightweight.
IMAGE="registry.k8s.io/pause:3.9"

# Tuning: 1..100 (higher => more spread among pods on the same node)
VARIANCE="${VARIANCE:-50}"

die(){ echo "[kwok-fill][ERROR] $*" >&2; exit 1; }
log(){ echo "[kwok-fill] $*"; }

# --- Quantity helpers ---
cpu_to_mc() {  # "250m" or "2" -> millicores
  local v="$1"
  if [[ "$v" =~ ^[0-9]+m$ ]]; then echo "${v%m}"; else awk -v c="$v" 'BEGIN{printf("%d", c*1000)}'; fi
}
mem_to_mi() {  # "512Mi" "1Gi" -> Mi
  local v="$1" num unit; num="${v//[!0-9]/}"; unit="${v//$num/}"
  case "$unit" in
    ""|Mi|mi) echo "$num" ;;
    Gi|gi) awk -v n="$num" 'BEGIN{printf("%d", n*1024)}' ;;
    Ki|ki) awk -v n="$num" 'BEGIN{printf("%d", n/1024)}' ;;
    Ti|ti) awk -v n="$num" 'BEGIN{printf("%d", n*1024*1024)}' ;;
    *) die "Unsupported mem unit: $v" ;;
  esac
}
mc() { echo "${1}m"; }
mi() { echo "${1}Mi"; }

# Random partition of TOTAL across K buckets with MIN each, exact sum == TOTAL
partition_int() {
  local TOTAL="$1" K="$2" MIN="$3"
  local -a w parts; local i sumw=0 rem
  rem=$(( TOTAL - K*MIN )); (( rem < 0 )) && rem=0
  for ((i=0;i<K;i++)); do w[i]=$(( (RANDOM % VARIANCE) + 1 )); sumw=$(( sumw + w[i] )); done
  local allocated=0 share
  for ((i=0;i<K-1;i++)); do
    share=$(awk -v r="$rem" -v wi="${w[i]}" -v sw="$sumw" 'BEGIN{printf("%d", (r*wi)/sw)}')
    parts[i]=$(( MIN + share )); allocated=$(( allocated + share ))
  done
  parts[K-1]=$(( MIN + rem - allocated ))
  echo "${parts[*]}"
}

ensure_ns() {
  kubectl --context "$CTX" get ns "$NS" >/dev/null 2>&1 || kubectl --context "$CTX" create ns "$NS" >/dev/null
}

ensure_pclasses() {
  for v in 1 2 3 4; do
    local pc="p${v}"
    kubectl --context "$CTX" get priorityclass "$pc" >/dev/null 2>&1 && continue
    cat <<EOF | kubectl --context "$CTX" apply -f -
apiVersion: scheduling.k8s.io/v1
kind: PriorityClass
metadata: {name: ${pc}}
value: ${v}
preemptionPolicy: PreemptLowerPriority
globalDefault: false
description: "pod priority ${v}"
EOF
  done
}

# Create KWOK-managed Nodes with capacity/allocatable (doc pattern). :contentReference[oaicite:1]{index=1}
create_nodes() {
  local pods_cap=$(( PODS_PER_NODE * 10 )) # plenty of headroom
  for i in $(seq 1 "$NUM_NODES"); do
    local name="kwok-node-${i}"
    cat <<EOF | kubectl --context "$CTX" apply -f -
apiVersion: v1
kind: Node
metadata:
  name: ${name}
  annotations:
    kwok.x-k8s.io/node: fake
    node.alpha.kubernetes.io/ttl: "0"
  labels:
    kubernetes.io/hostname: ${name}
    kubernetes.io/os: linux
    kubernetes.io/arch: amd64
    node-role.kubernetes.io/agent: ""
    type: kwok
spec:
  taints:
  - key: kwok.x-k8s.io/node
    value: fake
    effect: NoSchedule
status:
  capacity:
    cpu: ${NODE_CPU_IN}
    memory: ${NODE_MEM_IN}
    pods: ${pods_cap}
  allocatable:
    cpu: ${NODE_CPU_IN}
    memory: ${NODE_MEM_IN}
    pods: ${pods_cap}
  nodeInfo:
    architecture: amd64
    kubeletVersion: fake
    kubeProxyVersion: fake
    operatingSystem: linux
  phase: Running
EOF
  done
}

# Render a Pod pinned to a specific node (hostname label), tolerating the KWOK taint
render_pod() {
  local name="$1" node="$2" qcpu="$3" qmem="$4" pc="$5"
  cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${NS}
  labels: {node: "${node}"}
spec:
  priorityClassName: ${pc}
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: "${node}"
  tolerations:
  - key: "kwok.x-k8s.io/node"
    operator: "Exists"
    effect: "NoSchedule"
  containers:
  - name: filler
    image: ${IMAGE}
    resources:
      requests: {cpu: ${qcpu}, memory: ${qmem}}
      limits:   {cpu: ${qcpu}, memory: ${qmem}}
EOF
}

main() {
  # sanity
  ensure_ns
  ensure_pclasses
  create_nodes
  log "Created/updated ${NUM_NODES} KWOK nodes with cpu=${NODE_CPU_IN}, mem=${NODE_MEM_IN}"

  local NODE_MC NODE_MI TARGET_MC TARGET_MI
  NODE_MC="$(cpu_to_mc "$NODE_CPU_IN")"
  NODE_MI="$(mem_to_mi "$NODE_MEM_IN")"
  TARGET_MC=$(awk -v a="$NODE_MC" -v u="$TARGET_UTIL" 'BEGIN{printf("%d", a*u)}')
  TARGET_MI=$(awk -v a="$NODE_MI" -v u="$TARGET_UTIL" 'BEGIN{printf("%d", a*u)}')

  for i in $(seq 1 "$NUM_NODES"); do
    local node="kwok-node-${i}"
    IFS=' ' read -r -a cpu_parts <<< "$(partition_int "$TARGET_MC" "$PODS_PER_NODE" 1)"
    IFS=' ' read -r -a mem_parts <<< "$(partition_int "$TARGET_MI" "$PODS_PER_NODE" 1)"

    # Emit the pods
    for p in $(seq 1 "$PODS_PER_NODE"); do
      local idx=$((p-1))
      local qcpu qmem pc podname
      qcpu="$(mc "${cpu_parts[$idx]}")"
      qmem="$(mi "${mem_parts[$idx]}")"
      pc="p$(( (RANDOM % 4) + 1 ))"
      podname="${node//./-}-${p}-${pc}"
      render_pod "$podname" "$node" "$qcpu" "$qmem" "$pc" | kubectl --context "$CTX" apply -f -
    done
  done

  log "✅ Done. Check with:"
  echo "  kubectl --context ${CTX} get nodes -o wide"
  echo "  kubectl --context ${CTX} -n ${NS} get pods -o wide"
}

main
