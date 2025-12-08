#!/usr/bin/env bash
set -euo pipefail

########################## Defaults ##########################
#############################################################
CONTENT_DIR="${CONTENT_DIR:-/workspace}"
CONTENT_DIR_WAIT_TIMEOUT_S="${CONTENT_DIR_WAIT_TIMEOUT:-30}" # seconds
CONTENT_DIR_WAIT_INTERVAL_S="${CONTENT_DIR_WAIT_INTERVAL:-2}" # seconds

KUBECTL_VERSION="${KUBECTL_VERSION:-v1.32.7}"
KWOK_VERSION="${KWOK_VERSION:-v0.7.0}"

GO_VERSION="${GO_VERSION:-1.24.3}"
GO_ARCH="${GO_ARCH:-amd64}"

VENV_DIR="/opt/venv"
SOLVER_DIR="/opt/solver"

# Runner selection: test_runner (default) or trace_replayer
RUNNER="${RUNNER:-test_runner}"

# Shared config
CLUSTER_NAME="${CLUSTER_NAME:-}"
KWOK_RUNTIME="binary"  # binary | docker
JOB_FILE="${JOB_FILE:-}"                # can be relative to CONTENT_DIR
LOG_LEVEL="${LOG_LEVEL:-}"
CLEAN_START="${CLEAN_START:-}"

# For test_runner
RESULTS_DIR="${RESULTS_DIR:-}"          # can be relative to CONTENT_DIR
CONFIG_FILE="${CONFIG_FILE:-}"          # can be relative to CONTENT_DIR
SEED_FILE="${SEED_FILE:-}"              # can be relative to CONTENT_DIR
SEED="${SEED:-}"
RE_RUN_SEEDS="${RE_RUN_SEEDS:-}"
DEFAULT_SCHEDULER="${DEFAULT_SCHEDULER:-}" # if true, use the default kube-scheduler instead of the custom one
SOLVER_TRIGGER="${SOLVER_TRIGGER:-}"
PAUSE="${PAUSE:-}"
SEEDS_NOT_ALL_RUNNING="${SEEDS_NOT_ALL_RUNNING:-}" # int: how many seeds can be allowed to not reach all pods running (0=all must reach all running)
SAVE_SOLVER_STATS="${SAVE_SOLVER_STATS:-}"
SAVE_SCHEDULER_LOGS="${SAVE_SCHEDULER_LOGS:-}"

# For trace_replayer
TRACE_DIR="${TRACE_DIR:-}"                       # can be relative to CONTENT_DIR
KWOKCTL_CONFIG_FILE="${KWOKCTL_CONFIG_FILE:-}"  # can be relative to CONTENT_DIR
NODE_CPU="${NODE_CPU:-}"
NODE_MEM="${NODE_MEM:-}"
MONITOR_INTERVAL="${MONITOR_INTERVAL:-}"

########################## Helpers ##########################
#############################################################

log(){ printf '[%s] %s\n' "$1" "$2"; }
die(){ log error "$1"; exit 1; }

run_root(){ if [ "$(id -u)" -eq 0 ]; then bash -lc "$*"; else sudo bash -lc "$*"; fi; }

wait_for_dir() {
  local dir="${1:?}"; local timeout="${2:-}"; local interval="${3:-1}"
  local start elapsed
  start="$(date +%s)"
  log wait "waiting for CONTENT_DIR='${dir}'"
  while [ ! -d "$dir" ]; do
    sleep "$interval"
    if [ -n "$timeout" ]; then
      elapsed="$(( $(date +%s) - start ))"
      if [ "$elapsed" -ge "$timeout" ]; then
        die "CONTENT_DIR not found after ${elapsed}s: ${dir}"
      fi
    fi
  done
  log ok "CONTENT_DIR available: ${dir}"
}

# Return absolute path: if input is empty => empty; if absolute => as-is; else => CONTENT_DIR/<input>
to_abs_under_folder() {
  local path="${1:-}"
  if [ -z "$path" ]; then
    echo ""
  elif [[ "$path" = /* ]]; then
    echo "$path"
  else
    echo "${CONTENT_DIR%/}/$path"
  fi
}

# Normalize all user-provided paths against CONTENT_DIR
resolve_paths_relative_to_folder() {
  CONFIG_FILE="$(to_abs_under_folder "$CONFIG_FILE")"
  RESULTS_DIR="$(to_abs_under_folder "$RESULTS_DIR")"
  SEED_FILE="$(to_abs_under_folder "$SEED_FILE")"
  JOB_FILE="$(to_abs_under_folder "$JOB_FILE")"
  TRACE_DIR="$(to_abs_under_folder "$TRACE_DIR")"
  KWOKCTL_CONFIG_FILE="$(to_abs_under_folder "$KWOKCTL_CONFIG_FILE")"
}

print_cfg() {
  log cfg "CONTENT_DIR=${CONTENT_DIR}"
  log cfg "KWOK_RUNTIME=${KWOK_RUNTIME}"
  log cfg "RUNNER=${RUNNER}"
  if [ -n "${JOB_FILE}" ]; then
    log cfg "JOB_FILE=${JOB_FILE:-<unset>}"
  else
    log cfg "CLUSTER_NAME=${CLUSTER_NAME:-<unset>}"
    log cfg "CONFIG_FILE=${CONFIG_FILE:-<unset>}"
    log cfg "RESULTS_DIR=${RESULTS_DIR:-<unset>}"
    log cfg "SEED_FILE=${SEED_FILE:-<unset>}"
    log cfg "SEED=${SEED:-<unset>}"
    log cfg "RE_RUN_SEEDS=${RE_RUN_SEEDS:-<unset>}"
    log cfg "CLEAN_START=${CLEAN_START:-<unset>}"
    log cfg "LOG_LEVEL=${LOG_LEVEL:-<unset>}"
    log cfg "PAUSE=${PAUSE:-<unset>}"
  fi
  log cfg "DEFAULT_SCHEDULER=${DEFAULT_SCHEDULER:-<unset>}"
  if [ -n "${SEEDS_NOT_ALL_RUNNING:-}" ]; then
    log cfg "SEEDS_NOT_ALL_RUNNING=${SEEDS_NOT_ALL_RUNNING}"
  else
    log cfg "SEEDS_NOT_ALL_RUNNING=<unset>"
  fi
  if [ -n "${SOLVER_TRIGGER:-}" ]; then
    log cfg "SOLVER_TRIGGER=${SOLVER_TRIGGER}"
  else
    log cfg "SOLVER_TRIGGER=<unset>"
  fi
  if [ -n "${SAVE_SOLVER_STATS:-}" ]; then
    log cfg "SAVE_SOLVER_STATS=${SAVE_SOLVER_STATS}"
  else
    log cfg "SAVE_SOLVER_STATS=<unset>"
  fi
  if [ -n "${SAVE_SCHEDULER_LOGS:-}" ]; then
    log cfg "SAVE_SCHEDULER_LOGS=${SAVE_SCHEDULER_LOGS}"
  else
    log cfg "SAVE_SCHEDULER_LOGS=<unset>"
  fi
  log cfg "TRACE_DIR=${TRACE_DIR:-<unset>}"
  log cfg "KWOKCTL_CONFIG_FILE=${KWOKCTL_CONFIG_FILE:-<unset>}"
  log cfg "NODE_CPU=${NODE_CPU:-<unset>}"
  log cfg "NODE_MEM=${NODE_MEM:-<unset>}"
  log cfg "MONITOR_INTERVAL=${MONITOR_INTERVAL:-<unset>}"
}

######################## Stage Setup ########################
#############################################################
stage_setup() {
  log init "setup starting"

  resolve_paths_relative_to_folder
  print_cfg

  run_root "export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get install -y --no-install-recommends git ca-certificates curl make python3 python3-pip python3-venv"

  # kubectl/kwokctl/kwok
  log init "installing kubectl ${KUBECTL_VERSION}, kwokctl ${KWOK_VERSION}, kwok ${KWOK_VERSION}"
  run_root "
    cd /tmp
    curl -fsSLo kubectl https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl
    curl -fsSLO https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwokctl-linux-amd64
    curl -fsSLO https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwok-linux-amd64
    install -m 0755 kubectl /usr/local/bin/kubectl
    install -m 0755 kwokctl-linux-amd64 /usr/local/bin/kwokctl
    install -m 0755 kwok-linux-amd64   /usr/local/bin/kwok
    rm -f kubectl kwokctl-linux-amd64 kwok-linux-amd64
  "
  kubectl version --client=true >/dev/null 2>&1 || die "kubectl not installed"
  kwokctl --version >/dev/null 2>&1 || die "kwokctl not installed"
  kwok --version    >/dev/null 2>&1 || die "kwok not installed"
  log ok "kubectl/kwokctl/kwok installed"

  # Normalize paths
  resolve_paths_relative_to_folder
  # Print configuration
  print_cfg

  # Ensure kube-scheduler binary is present for binary runtime
  log init "verifying kube-scheduler binary"
  run_root "chmod +x '${CONTENT_DIR}/bin/kube-scheduler'" \
    || die "KWOK_RUNTIME=binary but no prebuilt scheduler at '${CONTENT_DIR}/bin/kube-scheduler'. Set BUILD_SCHEDULER=true or place the binary there."
  log ok "kube-scheduler binary verified"

  # Stage the solver
  log init "staging solver to ${SOLVER_DIR} (venv @ ${VENV_DIR})"
  run_root "
    set -euo pipefail
    install -d -m 0755 '${SOLVER_DIR}'
    install -d -m 0755 '${VENV_DIR}'
    cp -a '${CONTENT_DIR}/scripts/python_solver/main.py' '${SOLVER_DIR}/main.py'
    python3 -m venv '${VENV_DIR}'
    '${VENV_DIR}/bin/python' -m pip install --upgrade pip
    '${VENV_DIR}/bin/pip' install --no-cache-dir -r '${CONTENT_DIR}/scripts/python_solver/requirements.txt'
  "
  log ok "staged solver (venv @ ${VENV_DIR})"

  log ok "setup done"
}

######################## Stage Test #########################
#############################################################
stage_test() {
  log init "KWOK test starting"
  resolve_paths_relative_to_folder
  print_cfg

  # Install per-runner requirements if present
  run_root "
    set -euo pipefail
    if [ -f '${CONTENT_DIR}/scripts/kwok_workload_once/requirements.txt' ]; then
      '${VENV_DIR}/bin/pip' install --no-cache-dir -r '${CONTENT_DIR}/scripts/kwok_workload_once/requirements.txt'
    fi
    if [ -f '${CONTENT_DIR}/scripts/kwok_trace_replayer/requirements.txt' ]; then
      '${VENV_DIR}/bin/pip' install --no-cache-dir -r '${CONTENT_DIR}/scripts/kwok_trace_replayer/requirements.txt'
    fi
  "

  case "${RUNNER}" in
    trace_replayer)
      log init "running trace_replayer"
      # Build args for trace_replayer
      local tr_args=()
      [ -n "${KWOK_RUNTIME:-}"         ] && tr_args+=( --kwok-runtime "${KWOK_RUNTIME}" )
      [ -n "${CLUSTER_NAME:-}"         ] && tr_args+=( --cluster-name "${CLUSTER_NAME}" )
      [ -n "${TRACE_DIR:-}"            ] && tr_args+=( --trace-dir "${TRACE_DIR}" )
      [ -n "${KWOKCTL_CONFIG_FILE:-}"  ] && tr_args+=( --kwokctl-config-file "${KWOKCTL_CONFIG_FILE}" )
      [ -n "${NODE_CPU:-}"             ] && tr_args+=( --node-cpu "${NODE_CPU}" )
      [ -n "${NODE_MEM:-}"             ] && tr_args+=( --node-mem "${NODE_MEM}" )
      [ -n "${MONITOR_INTERVAL:-}"     ] && tr_args+=( --monitor-interval "${MONITOR_INTERVAL}" )
      [ -n "${LOG_LEVEL:-}"            ] && tr_args+=( --log-level "${LOG_LEVEL}" )
      [ -n "${JOB_FILE:-}"             ] && tr_args+=( --job-file "${JOB_FILE}" )

      local tr_quoted=""
      if ((${#tr_args[@]})); then
        printf -v tr_quoted '%q ' "${tr_args[@]}"
      fi

      run_root "
        cd '${CONTENT_DIR}' && \
        { [ '${KWOK_RUNTIME}' != 'binary' ] || chmod +x './bin/kube-scheduler'; } && \
        '${VENV_DIR}/bin/python' -m scripts.kwok_trace_replayer.trace_replayer ${tr_quoted}
      "
      log ok "trace_replayer done"
      ;;

    test_runner|*)
      log init "running test_runner (default)"

      # Build flags to forward to test_runner.py
      local passthru; passthru="$(build_passthrough_flags)"

      # Build the argv list once, only adding args the user actually set
      local args=()
      [ -n "${KWOK_RUNTIME:-}"          ] && args+=( --kwok-runtime "${KWOK_RUNTIME}" )
      [ -n "${CLUSTER_NAME:-}"          ] && args+=( --cluster-name "${CLUSTER_NAME}" )
      [ -n "${CONFIG_FILE:-}"           ] && args+=( --config-file "${CONFIG_FILE}" )
      [ -n "${RESULTS_DIR:-}"           ] && args+=( --results-dir "${RESULTS_DIR}" )
      [ -n "${SEED_FILE:-}"             ] && args+=( --seed-file "${SEED_FILE}" )
      [ -n "${SEED:-}"                  ] && args+=( --seed "${SEED}" )
      [ -n "${REPEATS:-}"               ] && args+=( --repeats "${REPEATS}" )
      [ -n "${LOG_LEVEL:-}"             ] && args+=( --log-level "${LOG_LEVEL}" )
      [ -n "${JOB_FILE:-}"              ] && args+=( --job-file "${JOB_FILE}" )
      [ -n "${SEEDS_NOT_ALL_RUNNING:-}" ] && args+=( --seeds-not-all-running "${SEEDS_NOT_ALL_RUNNING}" )
      [ -n "${DEFAULT_SCHEDULER:-}"    ] && args+=( --default-scheduler "${DEFAULT_SCHEDULER}" )
      [ -n "${KWOKCTL_CONFIG_FILE:-}"  ] && args+=( --kwokctl-config-file "${KWOKCTL_CONFIG_FILE}" )

      # Append passthrough boolean flags (like --solver-trigger, --save-scheduler-logs, --clean-start, --pause)
      # shellcheck disable=SC2206
      local pass_flags=( ${passthru} )
      args+=( "${pass_flags[@]}" )

      # Quote the entire argv safely for a nested bash -lc
      local quoted_args=""
      if ((${#args[@]})); then
        printf -v quoted_args '%q ' "${args[@]}"
      fi

      # Run the tests
      run_root "
        cd '${CONTENT_DIR}' && \
        chmod +x './bin/kube-scheduler' && \
        '${VENV_DIR}/bin/python' -m scripts.kwok_workload_once.test_runner ${quoted_args}
      "

      log ok "test_runner done"
      ;;
  esac
}

##################### Args and Dispatch #####################
#############################################################
# ---------- Flag spec + parser ----------
# FORMAT per line: name|VAR|kind|pass
# kind: "flag" (boolean) or "value" (needs a value)
# pass: the flag to forward ("" to disable)
FLAGS_SPEC=(
  "content-dir|CONTENT_DIR|value|"
  "cluster-name|CLUSTER_NAME|value|"
  "kwok-runtime|KWOK_RUNTIME|value|"
  "config-file|CONFIG_FILE|value|"
  "results-dir|RESULTS_DIR|value|"
  "seed-file|SEED_FILE|value|"
  "seed|SEED|value|"
  "repeats|REPEATS|value|"
  "job-file|JOB_FILE|value|"
  "solver-trigger|SOLVER_TRIGGER|flag|--solver-trigger"
  "save-solver-stats|SAVE_SOLVER_STATS|flag|--save-solver-stats"
  "save-scheduler-logs|SAVE_SCHEDULER_LOGS|flag|--save-scheduler-logs"
  "seeds-not-all-running|SEEDS_NOT_ALL_RUNNING|value|"  # now a value (int), not a bare flag
  "re-run-seeds|RE_RUN_SEEDS|flag|--re-run-seeds"
  "clean-start|CLEAN_START|flag|--clean-start"
  "pause|PAUSE|flag|--pause"
  "log-level|LOG_LEVEL|value|"
  "default-scheduler|DEFAULT_SCHEDULER|flag|--default-scheduler"
  "runner|RUNNER|value|"
  "trace-dir|TRACE_DIR|value|"
  "kwokctl-config-file|KWOKCTL_CONFIG_FILE|value|"
  "node-cpu|NODE_CPU|value|"
  "node-mem|NODE_MEM|value|"
  "monitor-interval|MONITOR_INTERVAL|value|"
)

get_spec_field() { # usage: get_spec_field "<name>" <idx>
  local name="$1" idx="$2" row IFS='|'
  for row in "${FLAGS_SPEC[@]}"; do
    read -r n var kind pass <<<"$row"
    if [ "$n" = "$name" ]; then
      case "$idx" in
        0) echo "$n"   ;; 1) echo "$var" ;;
        2) echo "$kind";; 3) echo "$pass";;
      esac
      return 0
    fi
  done
  return 1
}

set_var() { # set shell var by name under set -u
  local var="$1" val="$2"
  printf -v "$var" '%s' "$val"
}

parse_cli_using_spec() {
  # Also supports a leading subcommand (all|setup|test)
  case "${1-}" in all|setup|test) cmd="$1"; shift;; esac

  while [ "$#" -gt 0 ]; do
    case "$1" in
      --*=*)
        opt="${1%%=*}"; val="${1#*=}"; opt="${opt#--}"
        if ! var="$(get_spec_field "$opt" 1)"; then die "unknown argument: --$opt"; fi
        set_var "$var" "${val}"
        ;;
      --*)
        opt="${1#--}"
        if ! var="$(get_spec_field "$opt" 1)"; then die "unknown argument: --$opt"; fi
        kind="$(get_spec_field "$opt" 2)"
        if [ "$kind" = "flag" ]; then
          if [ "$#" -ge 2 ] && [[ "$2" != --* ]] && [[ "$2" =~ ^(true|false)$ ]]; then
            set_var "$var" "$2"; shift
          else
            set_var "$var" "true"
          fi
        else
          [ "$#" -ge 2 ] || die "missing value for --$opt"
          set_var "$var" "$2"; shift
        fi
        ;;
      --) shift; break ;;   # explicit end-of-options
      *) die "unknown argument: $1" ;;
    esac
    shift
  done
}

build_passthrough_flags() {
  local out=() row
  local OLDIFS="$IFS"
  IFS='|'
  for row in "${FLAGS_SPEC[@]}"; do
    # shellcheck disable=SC2034  # some fields unused in this loop
    read -r name var kind pass <<<"$row"
    [ -n "$pass" ] || continue
    if [ "$kind" = "flag" ]; then
      if [ -n "${!var:-}" ] && [ "${!var}" != "false" ]; then
        out+=("$pass")
      fi
    fi
  done
  IFS="$OLDIFS"
  # print one per line so caller can split on whitespace safely
  printf '%s\n' "${out[@]}"
}

# -------- Parse first, THEN wait for CONTENT_DIR --------
cmd="all"
parse_cli_using_spec "$@"

# Ensure CONTENT_DIR exists after any --content-dir override
wait_for_dir "${CONTENT_DIR}" "${CONTENT_DIR_WAIT_TIMEOUT_S}" "${CONTENT_DIR_WAIT_INTERVAL_S}"

case "${cmd}" in
  setup)  stage_setup ;;
  test)   stage_test ;;
  all)    stage_setup; stage_test ;;
esac

exit 0
