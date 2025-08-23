#!/usr/bin/env bash
set -euo pipefail

# Header
printf "%-25s %-14s %-22s %-22s %-14s %-22s %-22s %-16s\n" \
  "NODE" "CPU_ALLOC(m)" "CPU_REQ(m)" "CPU_FREE(m)" \
  "MEM_ALLOC(Mi)" "MEM_REQ(Mi)" "MEM_FREE(Mi)" "PODS_RUN"

# Capture nodes first so errors are visible (and don't break the loop silently)
nodes_out="$(
  kubectl get nodes \
    -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.allocatable.cpu}{" "}{.status.allocatable.memory}{"\n"}{end}'
)"

[[ -z "$nodes_out" ]] && { echo "No nodes found (check your current kubectl context)"; exit 1; }

while read -r node allocCpu allocMem; do
  # ---- allocatable -> m / Mi ----
  if [[ $allocCpu =~ m$ ]]; then allocMc=${allocCpu%m}; else allocMc=$(( ${allocCpu}*1000 )); fi
  if   [[ $allocMem =~ Ki$ ]]; then ki=${allocMem%Ki}; allocMi=$(( ki/1024 ))
  elif [[ $allocMem =~ Gi$ ]]; then gi=${allocMem%Gi}; allocMi=$(( gi*1024 ))
  else allocMi=${allocMem%Mi}; fi

  # ---- sum requests (containers + init + ephemeral) ----
  cpuReq="$(
    kubectl get pods --all-namespaces --field-selector spec.nodeName="$node" \
      -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.resources.requests.cpu}{"\n"}{end}{range .spec.initContainers[*]}{.resources.requests.cpu}{"\n"}{end}{range .spec.ephemeralContainers[*]}{.resources.requests.cpu}{"\n"}{end}{end}' \
    | awk '{ if($1~/m$/){sub(/m$/,"");sum+=$1} else if($1~/^[0-9]+(\.[0-9]+)?$/){sum+=($1*1000)} } END{print sum+0}'
  )"
  memReq="$(
    kubectl get pods --all-namespaces --field-selector spec.nodeName="$node" \
      -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.resources.requests.memory}{"\n"}{end}{range .spec.initContainers[*]}{.resources.requests.memory}{"\n"}{end}{range .spec.ephemeralContainers[*]}{.resources.requests.memory}{"\n"}{end}{end}' \
    | awk '{ if($1~/Ki$/){sub(/Ki$/,"");sum+=($1/1024)} else if($1~/Mi$/){sub(/Mi$/,"");sum+=$1} else if($1~/Gi$/){sub(/Gi$/,"");sum+=($1*1024)} } END{print sum+0}'
  )"

  # ---- per-node Running count (unscheduled pods aren't in this query by design) ----
  podsRun="$(
    kubectl get pods --all-namespaces --field-selector spec.nodeName="$node" \
      -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' 2>/dev/null \
    | awk '$1=="Running"{c++} END{print c+0}'
  )"

  # ---- free + percentages ----
  cpuFree=$((allocMc - cpuReq))
  memFree=$((allocMi - memReq))
  cpuReqPct=$(awk -v r="$cpuReq" -v a="$allocMc" 'BEGIN{printf "%.1f", (a>0)?(r/a*100):0}')
  cpuFreePct=$(awk -v f="$cpuFree" -v a="$allocMc" 'BEGIN{printf "%.1f", (a>0)?(f/a*100):0}')
  memReqPct=$(awk -v r="$memReq" -v a="$allocMi" 'BEGIN{printf "%.1f", (a>0)?(r/a*100):0}')
  memFreePct=$(awk -v f="$memFree" -v a="$allocMi" 'BEGIN{printf "%.1f", (a>0)?(f/a*100):0}')

  # ---- row (omit per-node NOTRUN) ----
  printf "%-25s %-14s %-22s %-22s %-14s %-22s %-22s %-16s\n" \
    "$node" \
    "$(printf 'alloc=%dm' "$allocMc")" \
    "$(printf 'req=%dm (%s%%)' "$cpuReq" "$cpuReqPct")" \
    "$(printf 'free=%dm (%s%%)' "$cpuFree" "$cpuFreePct")" \
    "$(printf 'alloc=%dMi' "$allocMi")" \
    "$(printf 'req=%dMi (%s%%)' "$memReq" "$memReqPct")" \
    "$(printf 'free=%dMi (%s%%)' "$memFree" "$memFreePct")" \
    "$(printf 'run=%d' "$podsRun")"
done <<< "$nodes_out"

# ---- cluster-wide totals (includes pods with NO nodeName, e.g. Pending) ----
all_phases="$(
  kubectl get pods --all-namespaces \
    -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' 2>/dev/null
)"
read -r total_run total_notrun <<<"$(
  printf '%s\n' "$all_phases" \
  | awk 'BEGIN{r=0;nr=0} { if ($1=="Running") r++; else if (length($1)) nr++; } END{printf "%d %d", r, nr}'
)"

echo
printf "TOTAL_PODS_RUN=%d  TOTAL_PODS_NOTRUN=%d\n" "$total_run" "$total_notrun"
