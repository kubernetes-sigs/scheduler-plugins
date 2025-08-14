#!/usr/bin/env bash
set -euo pipefail

# Args: <CLUSTER_CONTEXT> <NAMESPACE> <NUM_NODES> <NODE_CPU> <NODE_MEM> <PODS_PER_NODE> <TARGET_UTIL>
# Example:
#   ./fill-nodes.sh kind-mycluster crossnode-test 3 4 16Gi 6 0.9

CLUSTER_CONTEXT=${1:-kind-mycluster}
NAMESPACE=${2:-crossnode-test}
NUM_NODES=${3:-3}
NODE_CPU_IN=${4:-24}
NODE_MEM_IN=${5:-32Gi}
PODS_PER_NODE=${6:-4}
TARGET_UTIL=${7:-0.9}

IMAGE="${IMAGE:-registry.k8s.io/pause:3.9}"

# Tuning: how uneven sizes should be (1..100). Higher = more variance.
VARIANCE="${VARIANCE:-50}"

# ---------- Helpers ----------
die(){ echo "[fill-nodes][ERROR] $*" >&2; exit 1; }
log(){ echo "[fill-nodes] $*"; }

flt_lt(){ awk -v a="$1" -v b="$2" 'BEGIN{exit !(a<b)}'; }
flt_gt(){ awk -v a="$1" -v b="$2" 'BEGIN{exit !(a>b)}'; }

# CPU "250m" or "2" -> millicores
cpu_to_mc() {
  local v="$1"
  if [[ "$v" =~ ^[0-9]+m$ ]]; then
    echo "${v%m}"
  else
    awk -v c="$v" 'BEGIN{printf("%d", c*1000)}'
  fi
}

# Memory "512Mi" "1Gi" "16384Mi" -> Mi
mem_to_mi() {
  local v="$1" num unit
  num="${v//[!0-9]/}"
  unit="${v//$num/}"
  case "$unit" in
    Ki|ki) awk -v n="$num" 'BEGIN{printf("%d", (n/1024))}' ;;
    ""|Mi|mi) echo "$num" ;;
    Gi|gi) awk -v n="$num" 'BEGIN{printf("%d", (n*1024))}' ;;
    Ti|ti) awk -v n="$num" 'BEGIN{printf("%d", (n*1024*1024))}' ;;
    *) die "Unsupported memory unit: $v" ;;
  esac
}

mc_to_qty(){ echo "${1}m"; }
mi_to_qty(){ echo "${1}Mi"; }

render_pod() {
  local name="$1" node="$2" qcpu="$3" qmem="$4"
  cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${NAMESPACE}
  labels:
    node: "${node}"
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: "${node}"
  containers:
  - name: filler
    image: ${IMAGE}
    resources:
      requests:
        cpu: ${qcpu}
        memory: ${qmem}
      limits:
        cpu: ${qcpu}
        memory: ${qmem}
EOF
}

ensure_namespace() {
  kubectl --context "$CLUSTER_CONTEXT" get ns "$NAMESPACE" >/dev/null 2>&1 || \
    kubectl --context "$CLUSTER_CONTEXT" create ns "$NAMESPACE" >/dev/null
}

check_context() {
  if ! kubectl config get-contexts "$CLUSTER_CONTEXT" >/dev/null 2>&1; then
    die "Kube context '$CLUSTER_CONTEXT' not found. See: kubectl config get-contexts"
  fi
}

# --- ONLY WORKERS: exclude control-plane/master, unschedulable, and NoSchedule tainted nodes
pick_nodes() {
  mapfile -t WORKERS < <(
    kubectl --context "$CLUSTER_CONTEXT" get nodes \
      -l '!node-role.kubernetes.io/control-plane,!node-role.kubernetes.io/master' \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.unschedulable}{"\t"}{range .spec.taints[*]}{.effect}{";"}{end}{"\n"}{end}' \
    | awk -F'\t' '
      {
        name=$1; unsched=$2; taints=$3;
        nosched=0;
        n=split(taints, t, ";");
        for (i=1;i<=n;i++){ if (t[i]=="NoSchedule") nosched=1 }
        if (unsched!="true" && !nosched) print name
      }'
  )

  (( ${#WORKERS[@]} > 0 )) || die "No eligible worker nodes found (all excluded or tainted with NoSchedule)."

  if (( NUM_NODES > ${#WORKERS[@]} )); then
    die "Requested NUM_NODES=$NUM_NODES but only ${#WORKERS[@]} worker nodes are eligible: ${WORKERS[*]}"
  fi

  NODES=("${WORKERS[@]:0:$NUM_NODES}")
}

# ---- Random partitioner that guarantees exact totals ----
# Partitions TOTAL (int) into K positive integers >= MIN, returns space-separated list.
partition_int() {
  local TOTAL="$1" K="$2" MIN="$3"
  local -a w parts
  local i sumw=0 rem base
  # Reserve MIN per part first
  rem=$(( TOTAL - K*MIN ))
  if (( rem < 0 )); then
    # not enough budget: fall back to MIN=1 and clamp
    MIN=1
    rem=$(( TOTAL - K*MIN ))
    (( rem < 0 )) && rem=0
  fi

  # Generate random weights 1..(VARIANCE)
  for ((i=0;i<K;i++)); do
    w[i]=$(( (RANDOM % VARIANCE) + 1 ))
    sumw=$(( sumw + w[i] ))
  done

  # Distribute remainder proportionally by weights
  local allocated=0 share
  for ((i=0;i<K-1;i++)); do
    # floor(rem * w[i] / sumw)
    share=$(awk -v r="$rem" -v wi="${w[i]}" -v sw="$sumw" 'BEGIN{printf("%d", (r*wi)/sw)}')
    parts[i]=$(( MIN + share ))
    allocated=$(( allocated + share ))
  done
  parts[K-1]=$(( MIN + rem - allocated ))

  echo "${parts[*]}"
}

apply_fill() {
  (( PODS_PER_NODE > 0 )) || die "PODS_PER_NODE must be > 0"
  flt_lt "$TARGET_UTIL" "0.10" && TARGET_UTIL="0.10"
  flt_gt "$TARGET_UTIL" "1.0" && die "TARGET_UTIL must be <= 1.0"

  NODE_MC="$(cpu_to_mc "$NODE_CPU_IN")"   # per-node CPU in millicores
  NODE_MI="$(mem_to_mi "$NODE_MEM_IN")"   # per-node Mem in Mi

  TARGET_MC=$(awk -v a="$NODE_MC" -v u="$TARGET_UTIL" 'BEGIN{printf("%d", a*u)}')
  TARGET_MI=$(awk -v a="$NODE_MI" -v u="$TARGET_UTIL" 'BEGIN{printf("%d", a*u)}')

  echo "Context=${CLUSTER_CONTEXT} Namespace=${NAMESPACE}"
  echo "Per-node allocatable (assumed): CPU=${NODE_CPU_IN} (${NODE_MC}m), MEM=${NODE_MEM_IN} (${NODE_MI}Mi)"
  echo "Target per node: CPU=${TARGET_MC}m, MEM=${TARGET_MI}Mi (~${TARGET_UTIL}) with ${PODS_PER_NODE} pods"

  check_context
  ensure_namespace
  pick_nodes
  echo "Selected worker nodes: ${NODES[*]}"

  # For uniqueness across reruns
  suffix="$(date +%s | tail -c 5)"

  for node in "${NODES[@]}"; do
    # Randomly partition CPU and MEM across the K pods (>=1m / >=1Mi each)
    IFS=' ' read -r -a cpu_parts <<< "$(partition_int "$TARGET_MC" "$PODS_PER_NODE" 1)"
    IFS=' ' read -r -a mem_parts <<< "$(partition_int "$TARGET_MI" "$PODS_PER_NODE" 1)"

    # (Sanity) recompute sums to show effective util
    sum_mc=$(IFS=+; echo "$(( ${cpu_parts[*]} ))")
    sum_mi=$(IFS=+; echo "$(( ${mem_parts[*]} ))")
    eff_cpu_util=$(awk -v s="$sum_mc" -v cap="$NODE_MC" 'BEGIN{printf("%.3f", s/cap)}')
    eff_mem_util=$(awk -v s="$sum_mi" -v cap="$NODE_MI" 'BEGIN{printf("%.3f", s/cap)}')
    echo "Node ${node}: effective CPU util=${eff_cpu_util}, MEM util=${eff_mem_util}"

    for i in $(seq 1 "$PODS_PER_NODE"); do
      idx=$(( i-1 ))
      qcpu="$(mc_to_qty "${cpu_parts[$idx]}")"
      qmem="$(mi_to_qty "${mem_parts[$idx]}")"
      safe_node="${node//./-}"
      name="${safe_node}-${i}-${suffix}"
      echo "  - Pod $i: ${qcpu}/${qmem}"
      render_pod "$name" "$node" "$qcpu" "$qmem" | kubectl --context "$CLUSTER_CONTEXT" apply -f -
    done
  done

  echo "✅ Done. Check: kubectl --context ${CLUSTER_CONTEXT} -n ${NAMESPACE} get pods -o wide"
}

apply_fill
