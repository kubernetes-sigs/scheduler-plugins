#!/usr/bin/env bash
set -euo pipefail

# Args: <CLUSTER_NAME> <NUM_NODES> <PODS_PER_NODE> <NUM_REPLICASET> <TARGET_UTIL> <NAMESPACE> <NODE_CPU> <NODE_MEM>
# Example:
#   ./kwok-fill.sh kwok 3 4 5 0.9 crossnode-test 24 32Gi
#
# Behavior:
# - NUM_REPLICASET == 0:
#     Create naked Pods pinned per node. For each node, split the node-level target CPU/Mem
#     evenly across the node’s PODS_PER_NODE pods (with small rounding).
# - NUM_REPLICASET > 0:
#     Randomly partition TOTAL_REPLICAS = NUM_NODES*PODS_PER_NODE across NUM_REPLICASET RSs.
#     Distribute cluster-level target CPU/Mem evenly across TOTAL_REPLICAS (with rounding) and
#     assign contiguous slices of that distribution to each RS. No “fit” checks. No waiting.
#
# Priority rule (descending rollover): rs1->p4, rs2->p3, rs3->p2, rs4->p1, rs5->p4, ...

CLUSTER_NAME=${1:-kwok}
CTX="kwok-${CLUSTER_NAME}"
NUM_NODES=${2:-3}
PODS_PER_NODE=${3:-4}
NUM_REPLICASET=${4:-0}
TARGET_UTIL=${5:-0.90}
NS=${6:-crossnode-test}
NODE_CPU_IN=${7:-24}     # e.g. "24" or "24000m"
NODE_MEM_IN=${8:-32Gi}

IMAGE="registry.k8s.io/pause:3.9"

# Optional envs
SRAND_SEED="${SRAND_SEED:-}" # if set, use deterministic partitioning
VARIANCE="${VARIANCE:-50}"   # affects random partition spread

die(){ echo "[kwok-fill][ERROR] $*" >&2; exit 1; }
log(){ echo "[kwok-fill] $*"; }

# ---- Quantity helpers ----
cpu_to_mc() {  # "250m" or "2" -> millicores
  local v="$1"
  if [[ "$v" =~ ^[0-9]+m$ ]]; then echo "${v%m}"
  else awk -v c="$v" 'BEGIN{printf("%d", c*1000)}'
  fi
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
mc(){ echo "${1}m"; }
mi(){ echo "${1}Mi"; }

# Random partition of TOTAL across K buckets with MIN each, exact sum == TOTAL.
# Uses bash $RANDOM; spread influenced by VARIANCE.
partition_int() {
  local TOTAL="$1" K="$2" MIN="$3"
  (( K > 0 )) || die "partition_int: K must be > 0"
  local rem=$(( TOTAL - K*MIN )); (( rem < 0 )) && rem=0
  local -a w parts; local i sumw=0 allocated=0 share
  for ((i=0;i<K;i++)); do
    w[i]=$(( (RANDOM % VARIANCE) + 1 ))
    sumw=$(( sumw + w[i] ))
  done
  for ((i=0;i<K-1;i++)); do
    # proportional share (integer)
    share=$(( rem * w[i] / sumw ))
    parts[i]=$(( MIN + share ))
    allocated=$(( allocated + share ))
  done
  parts[K-1]=$(( MIN + rem - allocated ))
  echo "${parts[*]}"
}

# Even integer split of TOTAL across N slots -> array of length N summing to TOTAL.
# Distributes remainder (+1) to the first few slots.
split_even() {
  local TOTAL="$1" N="$2"
  (( N > 0 )) || die "split_even: N must be > 0"
  local base=$(( TOTAL / N ))
  local rem=$(( TOTAL % N ))
  local out=()
  local i
  for ((i=0;i<N;i++)); do
    if (( i < rem )); then out+=( $((base+1)) ); else out+=( $base ); fi
  done
  echo "${out[*]}"
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

# Create KWOK nodes with provided allocatable
create_nodes() {
  local pods_cap=$(( PODS_PER_NODE * 10 ))
  for i in $(seq 1 "$NUM_NODES"); do
    local name="kwok-node-${i}"
    cat <<EOF | kubectl --context "$CTX" apply -f -
apiVersion: v1
kind: Node
metadata:
  name: ${name}
  annotations:
    kwok.x-k8s.io/node: fake
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

# YAML renderers
render_pod() {
  local name="$1" node="$2" qcpu="$3" qmem="$4" pc="$5"
  cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${NS}
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

render_rs() {
  local rs_name="$1" replicas="$2" qcpu="$3" qmem="$4" pc="$5"
  cat <<EOF
apiVersion: apps/v1
kind: ReplicaSet
metadata:
  name: ${rs_name}
  namespace: ${NS}
spec:
  replicas: ${replicas}
  selector:
    matchLabels:
      app: ${rs_name}
  template:
    metadata:
      labels:
        app: ${rs_name}
    spec:
      priorityClassName: ${pc}
      restartPolicy: Always
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

# Priority mapping: step 1->p4, 2->p3, 3->p2, 4->p1, 5->p4, ...
prio_for_step_desc() {
  local step="$1"
  echo $(( 4 - ((step-1) % 4) ))
}

main() {
  # optional deterministic RNG
  if [[ -n "${SRAND_SEED}" ]]; then
    python3 - "$SRAND_SEED" >/dev/null <<'PY'
import random,sys
random.seed(sys.argv[1])
print(random.random())
PY
  fi

  ensure_ns
  ensure_pclasses
  create_nodes
  log "Nodes ready: ${NUM_NODES} (cpu=${NODE_CPU_IN}, mem=${NODE_MEM_IN})"

  # Numeric capacities
  local NODE_MC NODE_MI TARGET_MC_NODE TARGET_MI_NODE
  NODE_MC="$(cpu_to_mc "$NODE_CPU_IN")"
  NODE_MI="$(mem_to_mi "$NODE_MEM_IN")"
  TARGET_MC_NODE=$(awk -v a="$NODE_MC" -v u="$TARGET_UTIL" 'BEGIN{printf("%d", a*u)}')
  TARGET_MI_NODE=$(awk -v a="$NODE_MI" -v u="$TARGET_UTIL" 'BEGIN{printf("%d", a*u)}')

  local TOTAL_PODS=$(( NUM_NODES * PODS_PER_NODE ))
  local TARGET_MC_CLUSTER=$(( TARGET_MC_NODE * NUM_NODES ))
  local TARGET_MI_CLUSTER=$(( TARGET_MI_NODE * NUM_NODES ))

  if (( NUM_REPLICASET <= 0 )); then
    # -------- Naked Pod mode (even split per node) --------
    for i in $(seq 1 "$NUM_NODES"); do
      local node="kwok-node-${i}"
      # Even split of node targets across PODS_PER_NODE
      IFS=' ' read -r -a cpu_parts <<< "$(split_even "$TARGET_MC_NODE" "$PODS_PER_NODE")"
      IFS=' ' read -r -a mem_parts <<< "$(split_even "$TARGET_MI_NODE" "$PODS_PER_NODE")"
      for p in $(seq 1 "$PODS_PER_NODE"); do
        local idx=$((p-1))
        local qcpu qmem pc podname
        qcpu="$(mc "${cpu_parts[$idx]}")"
        qmem="$(mi "${mem_parts[$idx]}")"
        pc="p$(( (RANDOM % 4) + 1 ))"
        podname="kwok-node-${i}-${p}-${pc}"
        render_pod "$podname" "$node" "$qcpu" "$qmem" "$pc" | kubectl --context "$CTX" apply -f -
      done
    done
    log "✅ Created $((NUM_NODES*PODS_PER_NODE)) naked pods (even per-node target split)."
    echo "  kubectl --context ${CTX} -n ${NS} get pods -o wide"
    exit 0
  fi

  # -------- ReplicaSet mode (random replica partition; even resource per replica) --------
  # 1) Randomly partition TOTAL_PODS into NUM_REPLICASET RS replica counts (>=1 each).
  IFS=' ' read -r -a rs_replicas <<< "$(partition_int "$TOTAL_PODS" "$NUM_REPLICASET" 1)"

  # 2) Evenly split TOTAL target CPU/Mem across TOTAL_PODS replicas (integer, exact sum).
  IFS=' ' read -r -a per_rep_cpu_mc <<< "$(split_even "$TARGET_MC_CLUSTER" "$TOTAL_PODS")"
  IFS=' ' read -r -a per_rep_mem_mi <<< "$(split_even "$TARGET_MI_CLUSTER" "$TOTAL_PODS")"

  # 3) For RS i, take a contiguous slice of those per-rep arrays.
  local offset=0
  for i in $(seq 1 "$NUM_REPLICASET"); do
    local count="${rs_replicas[$((i-1))]}"
    (( count < 1 )) && count=1

    # Priority (descending rollover)
    local prio="$(prio_for_step_desc "$i")"
    local pc="p${prio}"
    local rsname="rs${i}-p${pc}"

    # Choose a representative per-rep request for this RS:
    #   Use the average of the slice (still simple & deterministic).
    local sum_mc=0 sum_mi=0 idx
    for ((idx=0; idx<count; idx++)); do
      sum_mc=$(( sum_mc + per_rep_cpu_mc[offset+idx] ))
      sum_mi=$(( sum_mi + per_rep_mem_mi[offset+idx] ))
    done
    local avg_mc=$(( sum_mc / count ))
    local avg_mi=$(( sum_mi / count ))
    (( avg_mc < 1 )) && avg_mc=1
    (( avg_mi < 1 )) && avg_mi=1

    render_rs "$rsname" "$count" "$(mc "$avg_mc")" "$(mi "$avg_mi")" "$pc" \
      | kubectl --context "$CTX" apply -f -

    log "RS ${i}/${NUM_REPLICASET} (${rsname}): replicas=${count}, per-rep=${avg_mc}m/${avg_mi}Mi, priority=${pc}"
    offset=$(( offset + count ))
  done

  log "✅ Done (replicasets=${NUM_REPLICASET}, total replicas=${TOTAL_PODS})."
  echo "  kubectl --context ${CTX} -n ${NS} get rs"
  echo "  kubectl --context ${CTX} -n ${NS} get pods -o wide"
}

main "$@"
