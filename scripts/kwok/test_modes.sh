#!/usr/bin/env bash
set -euo pipefail

# -------------------- Config (override via env) --------------------
CLUSTER_NAME="${CLUSTER_NAME:-kwok1}"                  # KWOK cluster short name
CTX="kwok-${CLUSTER_NAME}"                             # kubectl context used by your Python script
NS="${NS:-crossnode-test}"                             # workload namespace created by the test script

#docker build -t localhost:5000/scheduler-plugins/kube-scheduler:dev -f build/scheduler/Dockerfile .

wait_for_running_count() {
  local ns="$1" expected="$2" timeout="$3"
  local start=$(date +%s)

  while true; do
    running=$(kubectl --context "${CTX}" -n "${ns}" get pods \
      --no-headers | awk '$3 == "Running" {count++} END{print count+0}')
    echo "[wait] ${running}/${expected} Running in ns/${ns}"
    if [[ "$running" -ge "$expected" ]]; then
      echo "[wait] reached target: ${running} pods Running"
      return 0
    fi
    if (( $(date +%s) - start > timeout )); then
      echo "[wait] timeout after ${timeout}s waiting for ${expected} pods Running"
      return 1
    fi
    sleep 2
  done
}

run_case() {
  local mode="$1" at="$2" expected="$3" wait

  echo "===== Running case: ${mode}-${at} ====="

    kwokctl delete cluster --name kwok1 && kwokctl create cluster --name kwok1 --config scripts/kwok/test-${mode}-${at}.yaml

    pyargs=(
        "${CLUSTER_NAME}" 6 5 0
        --seed 112233
    )
    if [[ "${mode}" == "for_every" || "${at}" == "postfilter" ]]; then
        pyargs+=(--wait-each)
    fi

    python3 scripts/kwok/kwok_test_generator.py "${pyargs[@]}"

    wait_for_running_count "${NS}" 30 80

    kubectl apply -f scripts/kwok/test-high-prio-rs.yaml

    wait_for_running_count "${NS}" 35 80

  echo "===== Case ${mode}-${at} complete ====="
}

run_case "continuously" "postfilter" 34
run_case "in_batches" "postfilter" 34
run_case "for_every" "postfilter" 35
run_case "for_every" "preenqueue" 35
run_case "in_batches" "preenqueue" 35