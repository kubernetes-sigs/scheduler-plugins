#!/usr/bin/env bash
set -euo pipefail

########################## Defaults ##########################
#############################################################
CONTENT_DIR="${CONTENT_DIR:-/workspace}"

CONTENT_DIR_WAIT_TIMEOUT_S="${CONTENT_DIR_WAIT_TIMEOUT:-30}" # seconds
CONTENT_DIR_WAIT_INTERVAL_S="${CONTENT_DIR_WAIT_INTERVAL:-2}" # seconds

BUILD_SCHEDULER="${BUILD_SCHEDULER:-false}" # if true, build the scheduler binary/image; otherwise assume pre-built

REPO_OWNER="${REPO_OWNER:-henrikdchristensen}"
REPO_NAME="${REPO_NAME:-scheduler-plugins}"
REPO_BRANCH="${REPO_BRANCH:-henrikdc-cross-preemp}"
REPO_URL="https://github.com/${REPO_OWNER}/${REPO_NAME}"
REPO_DIR="${REPO_DIR:-repo}"              # can be relative to CONTENT_DIR

CLUSTER_NAME="${CLUSTER_NAME:-kwok1}"
KWOK_RUNTIME="${KWOK_RUNTIME:-binary}"    # binary | docker

RESULTS_DIR="${RESULTS_DIR:-}"             # can be relative to CONTENT_DIR
CONFIG_FILE="${CONFIG_FILE:-}"   # can be relative to CONTENT_DIR
SEED="${SEED:-}"
COUNT="${COUNT:-}"
GEN_SEEDS_TO_FILE="${GEN_SEEDS_TO_FILE:-}"
TEST="${TEST:-}"
OVERWRITE="${OVERWRITE:-}"
LOG_LEVEL="${LOG_LEVEL:-}"
SEED_FILE="${SEED_FILE:-}"                 # can be relative to CONTENT_DIR
MATRIX_FILE="${MATRIX_FILE:-}"             # can be relative to CONTENT_DIR

TRIGGER_OPTIMIZER="${TRIGGER_OPTIMIZER:-}"
OPTIMIZER_URL="${OPTIMIZER_URL:-}"
ACTIVE_URL="${ACTIVE_URL:-}"
PAUSE="${PAUSE:-}"

SEEDS_NOT_ALL_RUNNING="${SEEDS_NOT_ALL_RUNNING:-0}" # int: how many seeds can be allowed to not reach all pods running (0=all must reach all running)

SAVE_SOLVER_STATS="${SAVE_SOLVER_STATS:-}"

SAVE_SCHEDULER_LOGS="${SAVE_SCHEDULER_LOGS:-}"

CLEAN_RESULTS="${CLEAN_RESULTS:-}" 

KUBECTL_VERSION="${KUBECTL_VERSION:-v1.32.7}"
KWOK_VERSION="${KWOK_VERSION:-v0.7.0}"

GO_VERSION="${GO_VERSION:-1.24.3}"
GO_ARCH="${GO_ARCH:-amd64}"

IMAGE_REMOTE_TAG="${IMAGE_REMOTE_TAG:-localhost:5000/scheduler-plugins/kube-scheduler:dev}"
IMAGE_TAG_RESOLVED="${IMAGE_TAG_RESOLVED:-${IMAGE_REMOTE_TAG}}"

VENV_DIR="/opt/venv"
SOLVER_DIR="/opt/solver"

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
  MATRIX_FILE="$(to_abs_under_folder "$MATRIX_FILE")"
  REPO_DIR="$(to_abs_under_folder "$REPO_DIR")"
}

print_cfg() {
  log cfg "BUILD_SCHEDULER=${BUILD_SCHEDULER}"
  log cfg "CONTENT_DIR=${CONTENT_DIR}"
  log cfg "KWOK_RUNTIME=${KWOK_RUNTIME}"
  if [ -n "${MATRIX_FILE}" ]; then
    log cfg "MATRIX_FILE=${MATRIX_FILE:-<unset>}"
  else
    log cfg "CLUSTER_NAME=${CLUSTER_NAME:-<unset>}"
    log cfg "CONFIG_FILE=${CONFIG_FILE:-<unset>}"
    log cfg "RESULTS_DIR=${RESULTS_DIR:-<unset>}"
    log cfg "SEED_FILE=${SEED_FILE:-<unset>}"
    log cfg "SEED=${SEED:-<unset>}"
    log cfg "COUNT=${COUNT:-<unset>}"
    log cfg "GEN_SEEDS_TO_FILE=${GEN_SEEDS_TO_FILE:-<unset>}"
    log cfg "OVERWRITE=${OVERWRITE:-<unset>}"
    log cfg "LOG_LEVEL=${LOG_LEVEL:-<unset>}"
    log cfg "PAUSE=${PAUSE:-<unset>}"
    log cfg "REPO_DIR=${REPO_DIR:-<unset>}"
    log cfg "CLEAN_RESULTS=${CLEAN_RESULTS:-<unset>}"
  fi
  if [ -n "${TRIGGER_OPTIMIZER:-}" ]; then
    log cfg "TRIGGER_OPTIMIZER=${TRIGGER_OPTIMIZER}"
    log cfg "OPTIMIZER_URL=${OPTIMIZER_URL:-<unset>}"
    log cfg "ACTIVE_URL=${ACTIVE_URL:-<unset>}"
  else
    log cfg "TRIGGER_OPTIMIZER=<unset>"
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
  if [ "${BUILD_SCHEDULER}" = "true" ] && [ "${KWOK_RUNTIME}" = "docker" ]; then
    log cfg "IMAGE_REMOTE_TAG=${IMAGE_REMOTE_TAG}"
  fi
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

  # Clone only if we need to build
  if [ "${BUILD_SCHEDULER}" = "true" ]; then
    if [ ! -d "${REPO_DIR}/.git" ]; then
      cd '/tmp'
      log init "cloning https://github.com/${REPO_OWNER}/${REPO_NAME}#${REPO_BRANCH} to ${REPO_DIR}"
      run_root "install -d -m 0755 '$(dirname "${REPO_DIR}")'"
      run_root "git clone --branch '${REPO_BRANCH}' --single-branch '${REPO_URL}' '${REPO_DIR}'"
      log ok "cloned repo to ${REPO_DIR}"
    else
      log init "updating existing repo in ${REPO_DIR}"
      run_root "cd '${REPO_DIR}' && git fetch && git checkout '${REPO_BRANCH}' && git pull --ff-only || true"
      log ok "updated repo in ${REPO_DIR}"
    fi
  fi

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

  # Docker (if needed)
  if [ "${KWOK_RUNTIME}" = "docker" ]; then
    log init "installing docker"
    run_root "
      install -m 0755 -d /etc/apt/keyrings
      if [ ! -f /etc/apt/keyrings/docker.gpg ]; then
        curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
      fi
      echo \"deb [arch=\$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/ubuntu \$(. /etc/os-release && echo \"\$UBUNTU_CODENAME\") stable\" > /etc/apt/sources.list.d/docker.list
      apt-get update
      apt-get install -y --no-install-recommends docker-ce docker-ce-cli containerd.io docker-buildx-plugin
      systemctl enable --now docker
    "
    docker --version >/dev/null 2>&1 || die "docker not installed"
    log ok "docker installed"
  fi

  # Go (if needed)
  if [ "${BUILD_SCHEDULER}" = "true" ] && [ "${KWOK_RUNTIME}" = "binary" ]; then
    log init "installing Go ${GO_VERSION}"
    run_root "
      curl -fsSLo /tmp/go.tgz https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz
      rm -rf /usr/local/go
      tar -C /usr/local -xzf /tmp/go.tgz
      tee /etc/profile.d/golang.sh >/dev/null <<'EOF_G'
export PATH=\"/usr/local/go/bin:\$PATH\"
export GOPATH=\"\$HOME/go\"
export GOCACHE=\"\$HOME/.cache/go-build\"
EOF_G
      chmod 0644 /etc/profile.d/golang.sh
    "
    export PATH="/usr/local/go/bin:${PATH}"
    go version >/dev/null 2>&1 || die "go not installed"
    log ok "Go installed"
  fi
  
  log ok "setup done"
}

######################## Stage Build ########################
#############################################################
stage_build() {
  log init "build starting"

  resolve_paths_relative_to_folder
  print_cfg

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

  # Build scheduler (if needed)
  if [ "${BUILD_SCHEDULER}" = "true" ]; then
    if [ "${KWOK_RUNTIME}" = "binary" ]; then # build binary
      log init "building kube-scheduler binary"
      run_root "
        set -euo pipefail
        export PATH=\"/usr/local/go/bin:\$PATH\"  # from setup stage
        cd '${REPO_DIR}'
        make build-scheduler GO_BUILD_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=amd64'
        install -d -m 0755 '${CONTENT_DIR}/bin'
        install -m 0755 '${REPO_DIR}/bin/kube-scheduler' '${CONTENT_DIR}/bin/kube-scheduler'
      "
      log ok "built binary: ${CONTENT_DIR}/bin/kube-scheduler"
    else # build docker image
      log init "building kube-scheduler docker image"
      run_root "
        set -euo pipefail
        cd '${REPO_DIR}'
        docker build -t '${IMAGE_TAG_RESOLVED}' -f build/scheduler/Dockerfile .
      "
      log ok "built image: ${IMAGE_TAG_RESOLVED}"
    fi
  else
    # Not building: ensure the binary is present for binary runtime
    if [ "${KWOK_RUNTIME}" = "binary" ]; then # binary
      run_root "chmod +x '${CONTENT_DIR}/bin/kube-scheduler'" \
        || die "KWOK_RUNTIME=binary but no prebuilt scheduler at '${CONTENT_DIR}/bin/kube-scheduler'. Set BUILD_SCHEDULER=true or place the binary there."
    else # docker
      if ! docker image inspect "${IMAGE_TAG_RESOLVED}" >/dev/null 2>&1; then
        log warn "image '${IMAGE_TAG_RESOLVED}' not found locally; attempting docker pull"
        if ! docker pull "${IMAGE_REMOTE_TAG}"; then
          die "KWOK_RUNTIME=docker but image '${IMAGE_REMOTE_TAG}' not present and pull failed. \
Place the image locally (docker load/tag) or set BUILD_SCHEDULER=true."
        fi
        docker tag "${IMAGE_REMOTE_TAG}" "${IMAGE_TAG_RESOLVED}"
      fi
      log ok "scheduler image present: ${IMAGE_TAG_RESOLVED}"
    fi
  fi

  log ok "build done"
}

######################## Stage Test #########################
#############################################################
stage_test() {
  log init "KWOK test starting"
  resolve_paths_relative_to_folder
  print_cfg
  run_root "'${VENV_DIR}/bin/pip' install --no-cache-dir -r '${CONTENT_DIR}/scripts/kwok/requirements.txt'"

  # Build flags to forward to test_generator.py
  local passthru; passthru="$(build_passthrough_flags)"

  # Build the argv list once, only adding args the user actually set
  local args=()
  [ -n "${KWOK_RUNTIME:-}"     ] && args+=( --kwok-runtime "${KWOK_RUNTIME}" )
  [ -n "${CLUSTER_NAME:-}"     ] && args+=( --cluster-name "${CLUSTER_NAME}" )
  [ -n "${CONFIG_FILE:-}" ] && args+=( --config-file "${CONFIG_FILE}" )
  [ -n "${RESULTS_DIR:-}"      ] && args+=( --results-dir "${RESULTS_DIR}" )
  [ -n "${SEED_FILE:-}"        ] && args+=( --seed-file "${SEED_FILE}" )
  [ -n "${SEED:-}"             ] && args+=( --seed "${SEED}" )
  [ -n "${COUNT:-}"            ] && args+=( --count "${COUNT}" )
  [ -n "${GEN_SEEDS_TO_FILE:-}" ] && args+=( --gen-seeds-to-file "${GEN_SEEDS_TO_FILE}" )
  [ -n "${LOG_LEVEL:-}"        ] && args+=( --log-level "${LOG_LEVEL}" )
  [ -n "${MATRIX_FILE:-}"      ] && args+=( --matrix-file "${MATRIX_FILE}" )
  [ -n "${OPTIMIZER_URL:-}"    ] && args+=( --optimizer-url "${OPTIMIZER_URL}" )
  [ -n "${ACTIVE_URL:-}"       ] && args+=( --active-url "${ACTIVE_URL}" )
  [ -n "${RETRIES:-}"          ] && args+=( --retries "${RETRIES}" )
  [ -n "${REPEATS:-}"          ] && args+=( --repeats "${REPEATS}" )
  [ -n "${SEEDS_NOT_ALL_RUNNING:-}" ] && args+=( --seeds-not-all-running "${SEEDS_NOT_ALL_RUNNING}" )

  # Append passthrough boolean flags (like --trigger-optimizer, --save-scheduler-logs, --overwrite, --clean-results, --pause)
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
    { [ '${KWOK_RUNTIME}' != 'binary' ] || chmod +x './bin/kube-scheduler'; } && \
    '${VENV_DIR}/bin/python' scripts/kwok/test_generator.py ${quoted_args}
  "

  log ok "test done"
}

##################### Args and Dispatch #####################
#############################################################
# ---------- Flag spec + parser ----------
# FORMAT per line: name|VAR|kind|pass
# kind: "flag" (boolean) or "value" (needs a value)
# pass: the flag to forward ("" to disable)
FLAGS_SPEC=(
  "build-scheduler|BUILD_SCHEDULER|value|"
  "content-dir|CONTENT_DIR|value|"
  "image-remote-tag|IMAGE_REMOTE_TAG|value|"
  "cluster-name|CLUSTER_NAME|value|"
  "kwok-runtime|KWOK_RUNTIME|value|"
  "config-file|CONFIG_FILE|value|"
  "results-dir|RESULTS_DIR|value|"
  "seed-file|SEED_FILE|value|"
  "seed|SEED|value|"
  "count|COUNT|value|"
  "gen-seeds-to-file|GEN_SEEDS_TO_FILE|value|"
  "matrix-file|MATRIX_FILE|value|"
  "trigger-optimizer|TRIGGER_OPTIMIZER|flag|--trigger-optimizer"
  "optimizer-url|OPTIMIZER_URL|value|"
  "active-url|ACTIVE_URL|value|"
  "save-solver-stats|SAVE_SOLVER_STATS|flag|--save-solver-stats"
  "save-scheduler-logs|SAVE_SCHEDULER_LOGS|flag|--save-scheduler-logs"
  "retries|RETRIES|value|"
  "repeats|REPEATS|value|"
  "seeds-not-all-running|SEEDS_NOT_ALL_RUNNING|value|"  # now a value (int), not a bare flag
  "overwrite|OVERWRITE|flag|--overwrite"
  "clean-results|CLEAN_RESULTS|flag|--clean-results"
  "pause|PAUSE|flag|--pause"
  "log-level|LOG_LEVEL|value|"
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
  # Also supports a leading subcommand (all|setup-build|test)
  case "${1-}" in all|setup-build|test) cmd="$1"; shift;; esac

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
  setup-build) stage_setup; stage_build ;;
  test)        stage_test ;;
  all)         stage_setup; stage_build; stage_test ;;
esac

exit 0
