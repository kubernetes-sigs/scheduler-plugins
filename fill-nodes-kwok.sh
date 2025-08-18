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
#                            RSs are applied sequentially with feedback; per-RS pod size adapts
#                            to hit TARGET_UTIL while respecting the tightest per-node headroom.
# - Priority rule (**DESCENDING** ROLL OVER): rs1->p4, rs2->p3, rs3->p2, rs4->p1, rs5->p4, ...

CLUSTER_NAME=${1:-kwok}
CTX="kwok-${CLUSTER_NAME}"
NUM_NODES=${2:-3}
PODS_PER_NODE=${3:-4} # Pods per node
NUM_REPLICASET=${4:-0} # 0 for naked Pods, else for num of ReplicaSets
TARGET_UTIL=${5:-0.90}
NS=${6:-crossnode-test}
NODE_CPU_IN=${7:-24}       # cores or millicores (e.g. "24" or "24000m")
NODE_MEM_IN=${8:-32Gi}

IMAGE="registry.k8s.io/pause:3.9"

# Randomness controls
VARIANCE="${VARIANCE:-50}"   # spread for random partitions
SRAND_SEED="${SRAND_SEED:-}" # optional deterministic seed

# Headroom safety factor for per-replica capping (0.90 = 90%)
HEADROOM_PCT="${HEADROOM_PCT:-90}"

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

# --- Feedback helpers ---

# Sum requests for pods that have been assigned to a node (scheduled)
sum_cluster_requests() {
  kubectl --context "$CTX" -n "$NS" get pods -o json | jq -r '
    [ .items[]
      | select(.spec.nodeName != null)
      | [ .spec.containers[].resources.requests ]
      | map({
          cpu:(.cpu // "0"),
          mem:(.memory // "0")
        })
      | reduce .[] as $c (
          {"cpu":0,"mem":0};
          {
            cpu: (.cpu +
                  (if ($c.cpu|tostring|test("m$"))
                     then ($c.cpu|sub("m$";"")|tonumber)
                     else (($c.cpu|tonumber)*1000)
                   end)),
            mem: (.mem +
                  (if ($c.mem|tostring|test("Mi$"))
                     then ($c.mem|sub("Mi$";"")|tonumber)
                     elif ($c.mem|tostring|test("Gi$"))
                     then ($c.mem|sub("Gi$";"")|tonumber*1024)
                     elif ($c.mem|tostring|test("Ki$"))
                     then ($c.mem|sub("Ki$";"")|tonumber/1024)
                     else 0 end))
          }
        )
    ] | add // {"cpu":0,"mem":0}
    | "\(.cpu) \(.mem)"
  '
}

# Compute the minimum free CPU/Mem across all nodes (allocatable - scheduled requests)
min_headroom_mc_mi() {
  # per-node allocatable -> TSV: "<name>\t<cpu_mc>\t<mem_mi>"
  kubectl --context "$CTX" get nodes -o json | jq -r '
    .items[]
    | [
        .metadata.name,
        ( .status.allocatable.cpu as $c
          | (if ($c|tostring|test("m$")) then ($c|sub("m$";"")|tonumber)
             else (($c|tonumber)*1000) end)
        ),
        ( .status.allocatable.memory as $m
          | (if ($m|tostring|test("Mi$")) then ($m|sub("Mi$";"")|tonumber)
             elif ($m|tostring|test("Gi$")) then ($m|sub("Gi$";"")|tonumber*1024)
             elif ($m|tostring|test("Ki$")) then ($m|sub("Ki$";"")|tonumber/1024)
             else ($m|tonumber) end)
        )
      ]
    | @tsv
  ' > /tmp/_nodes_alloc.tsv

  # per-node used (scheduled) -> aggregate to TSV: "<name>\t<cpu_mc>\t<mem_mi>"
  kubectl --context "$CTX" -n "$NS" get pods -o json | jq -r '
    .items[] | select(.spec.nodeName != null)
    | [
        .spec.nodeName,
        ( [ .spec.containers[].resources.requests.cpu ]
          | map(select(.!=null))
          | map(if test("m$") then sub("m$";"")|tonumber else (tonumber*1000) end)
          | add // 0 ),
        ( [ .spec.containers[].resources.requests.memory ]
          | map(select(.!=null))
          | map(if test("Mi$") then sub("Mi$";"")|tonumber
                elif test("Gi$") then sub("Gi$";"")|tonumber*1024
                elif test("Ki$") then sub("Ki$";"")|tonumber/1024
                else 0 end)
          | add // 0 )
      ]
    | @tsv
  ' | awk -F'\t' '{cpu[$1]+=$2; mem[$1]+=$3} END{for(n in cpu) printf "%s\t%d\t%d\n", n, cpu[n], mem[n]}' > /tmp/_nodes_used.tsv

  # join & find min free across nodes
  awk -F'\t' '
    FNR==NR {allocC[$1]=$2; allocM[$1]=$3; next}
    {usedC[$1]=$2; usedM[$1]=$3; next}
    END{
      minC=1e18; minM=1e18;
      for (n in allocC) {
        uc = (n in usedC)?usedC[n]:0;
        um = (n in usedM)?usedM[n]:0;
        fc = allocC[n]-uc; fm = allocM[n]-um;
        if (fc<minC) minC=fc;
        if (fm<minM) minM=fm;
      }
      if (minC<0) minC=0; if (minM<0) minM=0;
      printf "%d %d\n", minC, minM
    }
  ' /tmp/_nodes_alloc.tsv /tmp/_nodes_used.tsv
}

# Wait until N pods of RS <name> are scheduled onto nodes
wait_rs_scheduled() {
  local rs="$1" expect="$2" timeout="${3:-180}"
  local sel="app=${rs}"
  local t=0
  while (( t < timeout )); do
    local ok
    ok=$(kubectl --context "$CTX" -n "$NS" get pods -l "$sel" -o json \
      | jq '[.items[] | select(.spec.nodeName != null)] | length')
    if (( ok >= expect )); then return 0; fi
    sleep 1; ((t++))
  done
  echo "[kwok-fill][WARN] Timeout waiting for RS ${rs} to be scheduled (${ok:-0}/${expect})."
  return 1
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

  # ---------- ReplicaSet mode (sequential, feedback after each apply) ----------
  IFS=' ' read -r -a rs_replicas <<< "$(partition_int "$TOTAL_PODS" "$NUM_REPLICASET" 1)"

  remaining_pods=$TOTAL_PODS
  for i in $(seq 1 "$NUM_REPLICASET"); do
    idx=$((i-1))
    replicas="${rs_replicas[$idx]}"
    (( replicas < 1 )) && replicas=1

    # Determine priority (descending)
    prio="$(prio_for_step_desc "$i")"
    pc="p${prio}"
    rsname="rs${i}-p${pc}"

    # Measure current scheduled requests (mc/Mi)
    read -r used_mc used_mi < <(sum_cluster_requests)

    # Residual budget to hit target totals
    residual_mc=$(( TARGET_MC_CLUSTER - used_mc ))
    residual_mi=$(( TARGET_MI_CLUSTER - used_mi ))
    (( residual_mc < 0 )) && residual_mc=0
    (( residual_mi < 0 )) && residual_mi=0

    # Even split across remaining pods (incl. this RS)
    per_rep_cpu_mc=$(( residual_mc > 0 && remaining_pods > 0 ? residual_mc / remaining_pods : 0 ))
    per_rep_mem_mi=$(( residual_mi > 0 && remaining_pods > 0 ? residual_mi / remaining_pods : 0 ))
    (( per_rep_cpu_mc < 1 )) && per_rep_cpu_mc=1
    (( per_rep_mem_mi < 1 )) && per_rep_mem_mi=1

    # Cap per-rep by tightest per-node headroom (with safety factor)
    read -r min_free_cpu_mc min_free_mem_mi < <(min_headroom_mc_mi)
    cap_cpu=$(( (min_free_cpu_mc * HEADROOM_PCT) / 100 ))
    cap_mem=$(( (min_free_mem_mi * HEADROOM_PCT) / 100 ))
    (( cap_cpu < 1 )) && cap_cpu=1
    (( cap_mem < 1 )) && cap_mem=1
    if (( per_rep_cpu_mc > cap_cpu )); then per_rep_cpu_mc=$cap_cpu; fi
    if (( per_rep_mem_mi > cap_mem )); then per_rep_mem_mi=$cap_mem; fi

    # Ensure this RS fits within residual budget; if not, shrink replicas
    max_by_cpu=$(( per_rep_cpu_mc > 0 ? residual_mc / per_rep_cpu_mc : 0 ))
    max_by_mem=$(( per_rep_mem_mi > 0 ? residual_mi / per_rep_mem_mi : 0 ))
    max_rep_for_budget=$(( max_by_cpu < max_by_mem ? max_by_cpu : max_by_mem ))
    if (( replicas > max_rep_for_budget && residual_mc > 0 && residual_mi > 0 )); then
      replicas=$max_rep_for_budget
      (( replicas < 0 )) && replicas=0
    fi
    if (( replicas == 0 )); then
      # no budget left — emit minimal RS so counts remain consistent
      replicas=1
      per_rep_cpu_mc=1
      per_rep_mem_mi=1
    fi

    # Render & apply this RS
    render_rs_nreplicas "$rsname" "$replicas" "$(mc "$per_rep_cpu_mc")" "$(mi "$per_rep_mem_mi")" "$pc" \
      | kubectl --context "$CTX" apply -f -

    # Wait for pods to be SCHEDULED (nodeName set)
    wait_rs_scheduled "$rsname" "$replicas" 240 || true

    # Update remaining pods and loop
    remaining_pods=$(( remaining_pods - replicas ))
    (( remaining_pods < 0 )) && remaining_pods=0

    # Log progress
    read -r used_mc used_mi < <(sum_cluster_requests)
    echo "[kwok-fill] RS ${i}/${NUM_REPLICASET} (${rsname}) applied: replicas=${replicas}, per-rep=${per_rep_cpu_mc}m/${per_rep_mem_mi}Mi, priority=${pc}"
    echo "[kwok-fill] -> scheduled now: CPU=${used_mc}m / MEM=${used_mi}Mi ; residual: CPU=$((TARGET_MC_CLUSTER-used_mc))m / MEM=$((TARGET_MI_CLUSTER-used_mi))Mi"
  done

  log "✅ Done (replicasets=${NUM_REPLICASET}, total pods=${TOTAL_PODS})."
  echo "  kubectl --context ${CTX} -n ${NS} get rs"
  echo "  kubectl --context ${CTX} -n ${NS} get pods -o wide"
}

main
