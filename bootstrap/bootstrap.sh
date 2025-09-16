#!/usr/bin/env bash
set -euo pipefail

# ---------- Defaults ----------
CONTENT_DIR="${CONTENT_DIR:-/workspace}"

CONTENT_DIR_WAIT_TIMEOUT_S="${CONTENT_DIR_WAIT_TIMEOUT:-30}" # seconds
CONTENT_DIR_WAIT_INTERVAL_S="${CONTENT_DIR_WAIT_INTERVAL:-2}" # seconds

BUILD_SCHEDULER="${BUILD_SCHEDULER:-false}" # if true, build the scheduler binary/image; otherwise assume pre-built

REPO_OWNER="${REPO_OWNER:-henrikdchristensen}"
REPO_NAME="${REPO_NAME:-scheduler-plugins}"
REPO_BRANCH="${REPO_BRANCH:-henrikdc-cross-preemp}"
REPO_URL="https://github.com/${REPO_OWNER}/${REPO_NAME}"
REPO_DIR="${REPO_DIR:-${CONTENT_DIR%/}/repo}"

KWOK_CLUSTER="${KWOK_CLUSTER:-kwok1}"
KWOK_RUNTIME="${KWOK_RUNTIME:-binary}"  # binary | docker

RESULTS_DIR="${RESULTS_DIR:-results}"      # can be relative to CONTENT_DIR
KWOK_CONFIG_DIR="${KWOK_CONFIG_DIR:-}"     # can be relative to CONTENT_DIR
SEED_FILE="${SEED_FILE:-}"                 # can be relative to CONTENT_DIR
MATRIX_FILE="${MATRIX_FILE:-}"             # can be relative to CONTENT_DIR
MATRIX_PARALLEL="${MATRIX_PARALLEL:-1}"    # number of parallel tests in matrix

KUBECTL_VERSION="${KUBECTL_VERSION:-v1.32.7}"
KWOK_VERSION="${KWOK_VERSION:-v0.7.0}"

GO_VERSION="${GO_VERSION:-1.24.3}"
GO_ARCH="${GO_ARCH:-amd64}"

SCHED_IMAGE_TAG="${SCHED_IMAGE_TAG:-localhost:5000/scheduler-plugins/kube-scheduler:dev}"

VENV_DIR="/opt/venv"
SOLVER_DIR="/opt/solver"

# ---------- Helpers ----------
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
  local p="${1:-}"
  if [ -z "$p" ]; then
    echo ""
  elif [[ "$p" = /* ]]; then
    echo "$p"
  else
    echo "${CONTENT_DIR%/}/$p"
  fi
}

# Normalize all user-provided paths against CONTENT_DIR
resolve_paths_relative_to_folder() {
  # Make sure CONTENT_DIR exists and is absolute
  [ -d "$CONTENT_DIR" ] || die "CONTENT_DIR not found: $CONTENT_DIR"
  CONTENT_DIR="$(cd "$CONTENT_DIR" && pwd -P)"

  KWOK_CONFIG_DIR="$(to_abs_under_folder "$KWOK_CONFIG_DIR")"
  RESULTS_DIR="$(to_abs_under_folder "$RESULTS_DIR")"
  SEED_FILE="$(to_abs_under_folder "$SEED_FILE")"
  MATRIX_FILE="$(to_abs_under_folder "$MATRIX_FILE")"
}

print_cfg() {
  log cfg "BUILD_SCHEDULER=${BUILD_SCHEDULER}"
  log cfg "CONTENT_DIR=${CONTENT_DIR}"
  log cfg "KWOK_RUNTIME=${KWOK_RUNTIME}"
  if [ -n "${MATRIX_FILE}" ]; then
    log cfg "MATRIX_FILE=${MATRIX_FILE:-<unset>}"
    log cfg "MATRIX_PARALLEL=${MATRIX_PARALLEL}"
  else
    log cfg "KWOK_CLUSTER=${KWOK_CLUSTER:-<unset>}"
    log cfg "KWOK_CONFIG_DIR=${KWOK_CONFIG_DIR:-<unset>}"
    log cfg "RESULTS_DIR=${RESULTS_DIR:-<unset>}"
    log cfg "SEED_FILE=${SEED_FILE:-<unset>}"
  fi
  if [ "${BUILD_SCHEDULER}" = "true" ] && [ "${KWOK_RUNTIME}" = "docker" ]; then
    log cfg "SCHED_IMAGE_TAG=${SCHED_IMAGE_TAG}"
  fi
  
}

# ---------- Stages ----------
stage_setup() {
  log init "setup starting"

  resolve_paths_relative_to_folder
  print_cfg

  run_root "export DEBIAN_FRONTEND=noninteractive
    apt-get update
    apt-get install -y --no-install-recommends git ca-certificates curl make python3 python3-pip python3-venv"

  # Clone only if we're going to build
  if [ "${BUILD_SCHEDULER}" = "true" ]; then
    if [ ! -d "${REPO_DIR}/.git" ]; then
      cd '/tmp'
      log init "cloning https://github.com/${REPO_OWNER}/${REPO_NAME}#${REPO_BRANCH}"
      run_root "git clone --branch '${REPO_BRANCH}' --single-branch '${REPO_URL}' '${REPO_DIR}'"
      run_root "chown -R '${TARGET_USER}:${TARGET_USER}' '${REPO_DIR}'"
    else
      run_root "cd '${REPO_DIR}' && git fetch && git checkout '${REPO_BRANCH}' && git pull --ff-only || true"
    fi
  fi

  # kubectl/kwokctl/kwok
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

  # --- Runtime prerequisites ---
  if [ "${KWOK_RUNTIME}" = "docker" ]; then
    # Always ensure Docker is available for docker runtime (build or not)
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
    if ! id -nG "${TARGET_USER}" | tr ' ' '\n' | grep -qx docker; then
      run_root "usermod -aG docker '${TARGET_USER}'"
    fi

    # If we're NOT building, ensure the scheduler image is present
    if [ "${BUILD_SCHEDULER}" != "true" ]; then
      if ! docker image inspect "${SCHED_IMAGE_TAG}" >/dev/null 2>&1; then
        log warn "image '${SCHED_IMAGE_TAG}' not found locally; attempting docker pull"
        if ! docker pull "${SCHED_IMAGE_TAG}"; then
          die "KWOK_RUNTIME=docker but image '${SCHED_IMAGE_TAG}' not present and pull failed. \
Place the image locally (docker load/tag) or set BUILD_SCHEDULER=true."
        fi
      fi
      log ok "scheduler image present: ${SCHED_IMAGE_TAG}"
    fi
  fi

  # Build-time prerequisites for binary build only
  if [ "${BUILD_SCHEDULER}" = "true" ] && [ "${KWOK_RUNTIME}" = "binary" ]; then
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
  fi

  log ok "setup complete"
}


# ---- stage_build: only build when BUILD_SCHEDULER=true; always set up venv ----
stage_build() {
  resolve_paths_relative_to_folder
  print_cfg

  run_root "
    set -euo pipefail
    install -d -m 0755 '${SOLVER_DIR}'
    install -d -m 0755 '${VENV_DIR}'
    cp -a '${CONTENT_DIR}/scripts/mycrossnodepreemption/main.py' '${SOLVER_DIR}/main.py'
    python3 -m venv '${VENV_DIR}'
    '${VENV_DIR}/bin/python' -m pip install --upgrade pip
    '${VENV_DIR}/bin/pip' install --no-cache-dir -r '${CONTENT_DIR}/scripts/mycrossnodepreemption/requirements.txt'
  "
  log ok "staged solver (venv @ ${VENV_DIR})"

  if [ "${BUILD_SCHEDULER}" = "true" ]; then
    if [ "${KWOK_RUNTIME}" = "binary" ]; then
      # Build kube-scheduler binary as TARGET_USER; copy under CONTENT_DIR/bin
      run_root "
        set -euo pipefail
        export PATH=\"/usr/local/go/bin:\$PATH\"  # from setup stage
        cd '${REPO_DIR}'
        make build-scheduler GO_BUILD_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=amd64'
        install -d -m 0755 '${CONTENT_DIR}/bin'
        install -m 0755 '${REPO_DIR}/bin/kube-scheduler' '${CONTENT_DIR}/bin/kube-scheduler'
      "
      log ok "built kube-scheduler binary -> ${CONTENT_DIR}/bin/kube-scheduler"
    else
      # Build container image as TARGET_USER (requires docker group membership from setup)
      run_root "
        set -euo pipefail
        cd '${REPO_DIR}'
        DOCKER_BUILDKIT=1 docker build -t '${SCHED_IMAGE_TAG}' -f build/scheduler/Dockerfile .
      "
      log ok "built image: ${SCHED_IMAGE_TAG}"
    fi
  else
    # Not building: ensure the binary is present for binary runtime
    if [ "${KWOK_RUNTIME}" = "binary" ]; then
      run_root "test -x '${CONTENT_DIR}/bin/kube-scheduler'" \
        || die "KWOK_RUNTIME=binary but no prebuilt scheduler at '${CONTENT_DIR}/bin/kube-scheduler'. Set BUILD_SCHEDULER=true or place the binary there."
    fi
  fi
}


stage_test() {
  resolve_paths_relative_to_folder
  print_cfg

  # KWOK helper requirements
  run_root "'${VENV_DIR}/bin/pip' install --no-cache-dir -r '${CONTENT_DIR}/scripts/kwok/requirements.txt'"

  # Run: matrix mode vs single-run mode
  if [ -n "${MATRIX_FILE}" ]; then
    run_root "cd '${CONTENT_DIR}' && \
      chmod +x './bin/kube-scheduler' && \
      '${VENV_DIR}/bin/python' scripts/kwok/kwok_test_generator.py \
        --kwok-runtime '${KWOK_RUNTIME}' \
        --matrix-file '${MATRIX_FILE}' \
        --matrix-parallel '${MATRIX_PARALLEL}'"
  else
    run_root "cd '${CONTENT_DIR}' && \
      chmod +x './bin/kube-scheduler' && \
      '${VENV_DIR}/bin/python' scripts/kwok/kwok_test_generator.py \
        --cluster-name '${KWOK_CLUSTER}' \
        --kwok-runtime '${KWOK_RUNTIME}' \
        --config-dir '${KWOK_CONFIG_DIR}' \
        --results-dir '${RESULTS_DIR}' \
        --seed-file '${SEED_FILE}'"
  fi

  log ok "KWOK test done"
}

# ---------- Args & Dispatch ----------
cmd="all"
case "${1-}" in all|setup-build|test) cmd="$1"; shift;; esac

while [ $# -gt 0 ]; do
  case "$1" in
    --build-scheduler=*)  BUILD_SCHEDULER="${1#*=}";;
    --build-scheduler)    BUILD_SCHEDULER="$2"; shift;;
    --content-dir=*)      CONTENT_DIR="${1#*=}";;
    --content-dir)        CONTENT_DIR="$2"; shift;;
    --kwok-cluster=*)     KWOK_CLUSTER="${1#*=}";;
    --kwok-cluster)       KWOK_CLUSTER="$2"; shift;;
    --kwok-runtime=*)     KWOK_RUNTIME="${1#*=}";;
    --kwok-runtime)       KWOK_RUNTIME="$2"; shift;;
    --kwok-config-dir=*)  KWOK_CONFIG_DIR="${1#*=}";;
    --kwok-config-dir)    KWOK_CONFIG_DIR="$2"; shift;;
    --results-dir=*)      RESULTS_DIR="${1#*=}";;
    --results-dir)        RESULTS_DIR="$2"; shift;;
    --seed-file=*)        SEED_FILE="${1#*=}";;
    --seed-file)          SEED_FILE="$2"; shift;;
    --matrix-file=*)      MATRIX_FILE="${1#*=}";;
    --matrix-file)        MATRIX_FILE="$2"; shift;;
    --matrix-parallel=*)  MATRIX_PARALLEL="${1#*=}";;
    --matrix-parallel)    MATRIX_PARALLEL="$2"; shift;;
    *) die "unknown argument: $1";;
  esac
  shift
done

# Ensure CONTENT_DIR exists before running any stage (respects --content-dir)
wait_for_dir "${CONTENT_DIR}" "${CONTENT_DIR_WAIT_TIMEOUT_S}" "${CONTENT_DIR_WAIT_INTERVAL_S}"

case "${cmd}" in
  setup-build) stage_setup; stage_build ;;
  test)        stage_test ;;
  all)         stage_setup; stage_build; stage_test ;;
esac
