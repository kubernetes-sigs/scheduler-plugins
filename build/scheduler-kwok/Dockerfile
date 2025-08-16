ARG GO_BASE_IMAGE=golang:1.24
ARG UBUNTU_BASE_IMAGE=ubuntu:22.04

FROM ${GO_BASE_IMAGE} AS builder
WORKDIR /src
COPY . .
RUN make build-scheduler

FROM ${UBUNTU_BASE_IMAGE}

# Python + pip
RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-pip && \
    rm -rf /var/lib/apt/lists/*

# Solver
ENV SOLVER_DIR=/opt/solver
RUN mkdir -p ${SOLVER_DIR}
COPY scripts/optimal-pod-assignments/ ${SOLVER_DIR}/
RUN pip3 install --no-cache-dir -r ${SOLVER_DIR}/requirements.txt

# 4) Copy the scheduler binary
COPY --from=builder /src/bin/kube-scheduler /bin/kube-scheduler

# 5) EntryPoint: handle KWOK's probe "kube-scheduler version"
RUN printf '%s\n' \
    '#!/bin/sh' \
    'set -eu' \
    '' \
    '# If first arg is the binary name (KWOK does: "kube-scheduler version"), drop it.' \
    '[ "${1:-}" = "kube-scheduler" ] && shift || true' \
    '' \
    'case "${1:-}" in' \
    '  version|-V|--version)' \
    '    exec /bin/kube-scheduler --version ;;' \
    '  "")' \
    '    exec /bin/kube-scheduler ;;' \
    '  *)' \
    '    exec /bin/kube-scheduler "$@" ;;' \
    'esac' \
    > /entrypoint.sh && chmod +x /entrypoint.sh

WORKDIR /bin
ENTRYPOINT ["/entrypoint.sh"]
