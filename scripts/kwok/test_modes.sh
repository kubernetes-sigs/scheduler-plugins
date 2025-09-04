#!/usr/bin/env bash
set -euo pipefail

# -------------------- Config (override via env) --------------------
CLUSTER_NAME="${CLUSTER_NAME:-kwok1}"                  # KWOK cluster short name
CTX="kwok-${CLUSTER_NAME}"                             # kubectl context used by your Python script
NS="${NS:-crossnode-test}"                             # workload namespace created by the test script

# Result tracking
declare -a CASES=()
declare -a STATUSES=()
declare -a NOTES=()
PASS=0
FAIL=0

wait_for_running_count() {
  local ns="$1" expected="$2" timeout="$3"
  local start end
  start=$(date +%s)

  while true; do
    running=$(kubectl --context "${CTX}" -n "${ns}" get pods \
      --no-headers | awk '$3 == "Running" {count++} END{print count+0}')
    echo "[wait] ${running}/${expected} Running in ns/${ns}"
    if [[ "$running" -ge "$expected" ]]; then
      echo "[wait] reached target: ${running} pods Running"
      return 0
    fi
    end=$(date +%s)
    if (( end - start > timeout )); then
      echo "[wait] timeout after ${timeout}s waiting for ${expected} pods Running"
      return 1
    fi
    sleep 2
  done
}

run_case() {
  local mode="$1" at="$2" expected="$3"
  local label="${mode}-${at}"
  local ok=true
  local note=""

  echo "===== Running case: ${label} ====="

  # Fresh KWOK cluster for this case
  kwokctl delete cluster --name "${CLUSTER_NAME}" || true
  if ! kwokctl create cluster --name "${CLUSTER_NAME}" --config "scripts/kwok/test-${mode}-${at}.yaml"; then
    ok=false
    note+="kwokctl create failed; "
  fi

  # Ensure the kubectl context matches CLUSTER_NAME (matrix calls used kwok1 literal before)
  CTX="kwok-${CLUSTER_NAME}"

  # Build python args
  pyargs=(
    "${CLUSTER_NAME}" 6 5 0
    --seed 112233
  )
  if [[ "${mode}" == "for_every" || "${at}" == "postfilter" ]]; then
      pyargs+=(--wait-each)
  fi

  # Run generator; do NOT abort whole script on failure
  if ! python3 scripts/kwok/kwok_test_generator.py "${pyargs[@]}"; then
    ok=false
    note+="generator failed; "
  fi

  # --- Only waiting timeouts affect pass/fail summary ---
  if ! wait_for_running_count "${NS}" 30 80; then
    ok=false
    note+="baseline wait timed out; "
  fi

  # Apply RS; do NOT abort whole script on failure
  if ! kubectl --context "${CTX}" apply -f scripts/kwok/test-high-prio-rs.yaml; then
    ok=false
    note+="apply RS failed; "
  fi

  if ! wait_for_running_count "${NS}" "$expected" 80; then
    ok=false
    note+="final wait timed out; "
  fi

  if $ok; then
    echo "===== Case ${label} PASS ====="
    CASES+=("${label}"); STATUSES+=("PASS"); NOTES+=("${note}")
    ((++PASS))
  else
    echo "===== Case ${label} FAIL ====="
    CASES+=("${label}"); STATUSES+=("FAIL"); NOTES+=("${note}")
    ((++FAIL))
  fi
}

# -------------------- Test Matrix --------------------
run_case "in_batches"   "preenqueue" 34
run_case "for_every"    "preenqueue" 34
run_case "for_every"    "postfilter" 34
run_case "in_batches"   "postfilter" 34
run_case "continuously" "postfilter" 34

# -------------------- Summary --------------------
echo
echo "==================== TEST SUMMARY ===================="
printf "%-24s  %-6s  %s\n" "CASE" "RESULT" "NOTE"
echo "------------------------------------------------------"
for i in "${!CASES[@]}"; do
  printf "%-24s  %-6s  %s\n" "${CASES[$i]}" "${STATUSES[$i]}" "${NOTES[$i]}"
done
echo "------------------------------------------------------"
echo "Passed: ${PASS}  Failed: ${FAIL}"
echo "======================================================"

# Exit non-zero if any case failed due to wait timeouts (or other noted issues)
if (( FAIL > 0 )); then
  exit 1
fi
