#!/usr/bin/env bash
set -euo pipefail

# Header: fixed widths per column
printf "%-25s %-14s %-22s %-22s %-14s %-22s %-22s\n" \
  "NODE" "CPU_ALLOC(m)" "CPU_REQ(m)" "CPU_FREE(m)" \
  "MEM_ALLOC(Mi)" "MEM_REQ(Mi)" "MEM_FREE(Mi)"

while read -r node allocCpu allocMem; do
  # ---- allocatable -> m / Mi ----
  if [[ $allocCpu =~ m$ ]]; then allocMc=${allocCpu%m}; else allocMc=$(( ${allocCpu}*1000 )); fi
  if [[ $allocMem =~ Ki$ ]]; then ki=${allocMem%Ki}; allocMi=$(( ki/1024 ))
  elif [[ $allocMem =~ Gi$ ]]; then gi=${allocMem%Gi}; allocMi=$(( gi*1024 ))
  else allocMi=${allocMem%Mi}; fi

  # ---- sum requests (containers + init + ephemeral) ----
  cpuReq=$(kubectl get pods --all-namespaces --field-selector spec.nodeName="$node" \
            -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.resources.requests.cpu}{"\n"}{end}{range .spec.initContainers[*]}{.resources.requests.cpu}{"\n"}{end}{range .spec.ephemeralContainers[*]}{.resources.requests.cpu}{"\n"}{end}{end}' \
          | awk '{ if($1~/m$/){sub(/m$/,"");sum+=$1} else if($1~/^[0-9]+(\.[0-9]+)?$/){sum+=($1*1000)} } END{print sum+0}')
  memReq=$(kubectl get pods --all-namespaces --field-selector spec.nodeName="$node" \
            -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.resources.requests.memory}{"\n"}{end}{range .spec.initContainers[*]}{.resources.requests.memory}{"\n"}{end}{range .spec.ephemeralContainers[*]}{.resources.requests.memory}{"\n"}{end}{end}' \
          | awk '{ if($1~/Ki$/){sub(/Ki$/,"");sum+=($1/1024)} else if($1~/Mi$/){sub(/Mi$/,"");sum+=$1} else if($1~/Gi$/){sub(/Gi$/,"");sum+=($1*1024)} } END{print sum+0}')

  # ---- free + percentages ----
  cpuFree=$((allocMc - cpuReq))
  memFree=$((allocMi - memReq))
  cpuReqPct=$(awk -v r="$cpuReq" -v a="$allocMc" 'BEGIN{printf "%.1f", (a>0)?(r/a*100):0}')
  cpuFreePct=$(awk -v f="$cpuFree" -v a="$allocMc" 'BEGIN{printf "%.1f", (a>0)?(f/a*100):0}')
  memReqPct=$(awk -v r="$memReq" -v a="$allocMi" 'BEGIN{printf "%.1f", (a>0)?(r/a*100):0}')
  memFreePct=$(awk -v f="$memFree" -v a="$allocMi" 'BEGIN{printf "%.1f", (a>0)?(f/a*100):0}')

  # ---- build cell strings (no internal padding!) ----
  cpuAllocStr=$(printf "alloc=%dm"    "$allocMc")
  cpuReqStr=$(printf   "req=%dm (%s%%)"  "$cpuReq" "$cpuReqPct")
  cpuFreeStr=$(printf  "free=%dm (%s%%)" "$cpuFree" "$cpuFreePct")
  memAllocStr=$(printf "alloc=%dMi"   "$allocMi")
  memReqStr=$(printf   "req=%dMi (%s%%)"  "$memReq" "$memReqPct")
  memFreeStr=$(printf  "free=%dMi (%s%%)" "$memFree" "$memFreePct")

  # ---- row: pad the whole cells to fixed widths ----
  printf "%-25s %-14s %-22s %-22s %-14s %-22s %-22s\n" \
    "$node" "$cpuAllocStr" "$cpuReqStr" "$cpuFreeStr" "$memAllocStr" "$memReqStr" "$memFreeStr"

done < <(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.allocatable.cpu}{" "}{.status.allocatable.memory}{"\n"}{end}')
