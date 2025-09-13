# ---------- global args usable in any FROM ----------
ARG GO_BASE_IMAGE=golang:1.24
ARG UBUNTU_BASE_IMAGE=ubuntu:22.04

# ---------- builder ----------
FROM ${GO_BASE_IMAGE} AS builder
WORKDIR /src

# 1) deps first (cacheable)
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# 2) source
COPY cmd/scheduler ./cmd/scheduler
COPY pkg           ./pkg
COPY apis          ./apis

# 3) build (no login shell)
ARG VERSION=dev
ARG GO_BUILD_ENV="CGO_ENABLED=0"
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    $GO_BUILD_ENV go build \
    -ldflags "-X k8s.io/component-base/version.gitVersion=${VERSION} -w" \
    -o /out/kube-scheduler ./cmd/scheduler

# ---------- runtime ----------
FROM ${UBUNTU_BASE_IMAGE} AS runtime

# Python only (no recommends) for the solver
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*

ENV SOLVER_DIR=/opt/solver
RUN mkdir -p ${SOLVER_DIR}

# requirements first, then install (keeps this layer cached)
COPY scripts/mycrossnodepreemption/requirements.txt ${SOLVER_DIR}/requirements.txt
RUN --mount=type=cache,target=/root/.cache/pip \
    pip3 install -r ${SOLVER_DIR}/requirements.txt

# now the rest of the solver
COPY scripts/mycrossnodepreemption/ ${SOLVER_DIR}/

# scheduler binary from builder
COPY --from=builder /out/kube-scheduler /bin/kube-scheduler

WORKDIR /bin
CMD ["kube-scheduler"]
