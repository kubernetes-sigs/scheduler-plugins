# ---------- build kube-scheduler ----------
ARG GO_BASE_IMAGE=golang:1.24
ARG UBUNTU_BASE_IMAGE=ubuntu:22.04
ARG VERSION=dev

FROM ${GO_BASE_IMAGE} AS builder
WORKDIR /src

# Sanity check: fail early if Go is missing
RUN go version

# 1) Cache deps
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# 2) Copy just what’s needed to compile
COPY cmd/scheduler/ ./cmd/scheduler/
COPY pkg/ ./pkg/
COPY apis/ ./apis/
# COPY internal/ ./internal/   # add this if you import from internal/

# 3) Build (no shell tricks, no GO_BUILD_ENV indirection)
ENV CGO_ENABLED=0 GO111MODULE=on
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags "-X k8s.io/component-base/version.gitVersion=${VERSION} -w" \
    -o /out/kube-scheduler ./cmd/scheduler

FROM ${UBUNTU_BASE_IMAGE}

# (optional) much faster final image:
# FROM python:3.12-slim AS runtime

# Python for the solver
RUN apt-get update && apt-get install -y --no-install-recommends python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*

ENV SOLVER_DIR=/opt/solver
RUN mkdir -p ${SOLVER_DIR}

# Keep this layer cached
COPY scripts/mycrossnodepreemption/requirements.txt ${SOLVER_DIR}/requirements.txt
RUN --mount=type=cache,target=/root/.cache/pip \
    pip3 install -r ${SOLVER_DIR}/requirements.txt

# Copy solver code
COPY scripts/mycrossnodepreemption/ ${SOLVER_DIR}/

# Scheduler binary
COPY --from=builder /out/kube-scheduler /bin/kube-scheduler

WORKDIR /bin
CMD ["kube-scheduler"]
