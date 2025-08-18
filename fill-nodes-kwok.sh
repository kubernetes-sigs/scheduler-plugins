#!/usr/bin/env bash
set -euo pipefail

# Args: <CLUSTER_NAME> <NUM_NODES> <PODS_PER_NODE> <NUM_REPLICASET> <TARGET_UTIL> <NAMESPACE> <NODE_CPU> <NODE_MEM>
# Example:
#   ./kwok-fill.sh kwok 3 4 5 0.9 crossnode-test 24 32Gi
#
# Behavior:
# - NUM_REPLICASET == 0  -> create naked Pods pinned to each node (legacy mode).
# - NUM_REPLICASET > 0   -> create exactly that many ReplicaSets. Each RS gets a random number
#                            of replicas; the sum across all RS equals NUM_NODES * PODS_PER_NODE.
#                            Per-RS pods are identical and cluster totals hit TARGET_UTIL closely.
# - Priority rule (ROLL OVER): rs1->p1, rs2->p2, rs3->p3, rs4->p4, rs5->p1, rs6->p2, ...

CLUSTER_NAME=${1:-kwok}
CTX="kwok-${CLUSTER_NAME}"
NUM_NODES=${2:-3}
PODS_PER_NODE=${3:-4} # Pods per node
NUM_REPLICASET=${4:-0} # 0 for naked Pods, else for num of ReplicaSets
TARGET_UTIL=${5:-0.9}
NS=${6:-crossnode-test}
NODE_CPU_IN=${7:-24}       # cores or millicores (e.g. "24" or "24000m")
NODE_MEM_IN=${8:-32Gi}

IMAGE="registry.k8s.io/pause:3.9"

# Randomness controls
VARIANCE="${VARIANCE:-50}"   # spread for random partitions
SRAND_SEED="${SRAND_SEED:-}" # optional deterministic seed

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
  (( K > 0 )) || die "partition_int: K must be > 0"
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

# Create KWOK nodes
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

# --- Renderers ---

# Naked Pod pinned to a node (legacy)
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

# ReplicaSet with N identical replicas (per RS CPU/Mem exact); RS name encodes rs<id>-p<prio>
render_rs_nreplicas() {
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

# Compute per-RS *per-replica* CPU/Mem so that:
#   sum_i replicas[i]*cpuPerRep[i] == TARGET (same for mem)
compute_per_rep_requests_exact() {
  local TARGET="$1" K="$2"            # total target in integer units (mc or Mi), number of RS
  shift 2
  local -a replicas=( "$@" )          # length K

  # 1) random weights
  local -a w; local sumwr=0
  for ((i=0;i<K;i++)); do
    w[i]=$(( (RANDOM % VARIANCE) + 1 ))
    sumwr=$(( sumwr + w[i]*replicas[i] ))
  done
  (( sumwr == 0 )) && sumwr=1

  # 2) base per-rep
  local -a per_rep; local total=0
  for ((i=0;i<K;i++)); do
    per_rep[i]=$(( (TARGET * w[i]) / sumwr ))
    (( per_rep[i] < 1 )) && per_rep[i]=1
    total=$(( total + per_rep[i]*replicas[i] ))
  done

  # 3) remainder distribution
  local rem=$(( TARGET - total ))
  if (( rem > 0 )); then
    while (( rem > 0 )); do
      local bi=-1
      for ((i=0;i<K;i++)); do
        (( replicas[i] == 0 )) && continue
        if (( bi < 0 )) || (( w[i]*replicas[bi] > w[bi]*replicas[i] )); then
          bi=$i
        fi
      done
      (( bi < 0 )) && break
      per_rep[bi]=$(( per_rep[bi] + 1 ))
      rem=$(( rem - replicas[bi] ))
    done
  fi

  echo "${per_rep[*]}"
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
  log "Created/updated ${NUM_NODES} KWOK nodes with cpu=${NODE_CPU_IN}, mem=${NODE_MEM_IN}"

  # Capacity targets
  local NODE_MC NODE_MI TARGET_MC_NODE TARGET_MI_NODE
  NODE_MC="$(cpu_to_mc "$NODE_CPU_IN")"
  NODE_MI="$(mem_to_mi "$NODE_MEM_IN")"
  TARGET_MC_NODE=$(awk -v a="$NODE_MC" -v u="$TARGET_UTIL" 'BEGIN{printf("%d", a*u)}')
  TARGET_MI_NODE=$(awk -v a="$NODE_MI" -v u="$TARGET_UTIL" 'BEGIN{printf("%d", a*u)}')

  local TOTAL_PODS=$(( NUM_NODES * PODS_PER_NODE ))
  local TARGET_MC_CLUSTER=$(( TARGET_MC_NODE * NUM_NODES ))
  local TARGET_MI_CLUSTER=$(( TARGET_MI_NODE * NUM_NODES ))

  if (( NUM_REPLICASET <= 0 )); then
    # ---------- Naked Pod mode ----------
    for i in $(seq 1 "$NUM_NODES"); do
      local node="kwok-node-${i}"
      IFS=' ' read -r -a cpu_parts <<< "$(partition_int "$TARGET_MC_NODE" "$PODS_PER_NODE" 1)"
      IFS=' ' read -r -a mem_parts <<< "$(partition_int "$TARGET_MI_NODE" "$PODS_PER_NODE" 1)"
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
    log "✅ Done (naked pods)."
    echo "  kubectl --context ${CTX} -n ${NS} get pods -o wide"
    exit 0
  fi

  # ---------- ReplicaSet mode ----------
  # 1) Replica counts per RS: sum == TOTAL_PODS
  IFS=' ' read -r -a rs_replicas <<< "$(partition_int "$TOTAL_PODS" "$NUM_REPLICASET" 1)"

  # 2) Compute per-RS per-replica CPU & Mem so cluster totals hit targets
  IFS=' ' read -r -a cpu_per_rep <<< "$(compute_per_rep_requests_exact "$TARGET_MC_CLUSTER" "$NUM_REPLICASET" "${rs_replicas[@]}")"
  IFS=' ' read -r -a mem_per_rep <<< "$(compute_per_rep_requests_exact "$TARGET_MI_CLUSTER" "$NUM_REPLICASET" "${rs_replicas[@]}")"

  # 3) Create RS objects with **ROLLOVER** priorities: rs1->p1, rs2->p2, ..., rs5->p1, etc.
  for i in $(seq 1 "$NUM_REPLICASET"); do
    local idx=$((i-1))
    local replicas="${rs_replicas[$idx]}"
    local cpu_mc="${cpu_per_rep[$idx]}"
    local mem_mi="${mem_per_rep[$idx]}"

    (( cpu_mc < 1 )) && cpu_mc=1
    (( mem_mi < 1 )) && mem_mi=1

    local qcpu qmem pc rsname
    qcpu="$(mc "${cpu_mc}")"
    qmem="$(mi "${mem_mi}")"

    # ---- ROLLOVER (1..4 then back to 1) ----
    local prio_idx=$(( ((i-1) % 4) + 1 ))
    pc="p${prio_idx}"

    rsname="rs${i}-p${pc}"

    render_rs_nreplicas "$rsname" "$replicas" "$qcpu" "$qmem" "$pc" | kubectl --context "$CTX" apply -f -
  done

  log "✅ Done (replicasets=${NUM_REPLICASET}, total pods=${TOTAL_PODS})."
  echo "  kubectl --context ${CTX} -n ${NS} get rs"
  echo "  kubectl --context ${CTX} -n ${NS} get pods -o wide"
}

main
