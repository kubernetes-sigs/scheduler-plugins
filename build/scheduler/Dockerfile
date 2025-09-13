# global args (must appear before any FROM that uses them)
ARG GO_BASE_IMAGE=golang:1.24
ARG UBUNTU_BASE_IMAGE=ubuntu:22.04

# ---------- builder ----------
FROM ${GO_BASE_IMAGE} AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd/scheduler ./cmd/scheduler
COPY pkg           ./pkg
COPY apis          ./apis

ARG VERSION=dev
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build \
    -ldflags "-X k8s.io/component-base/version.gitVersion=${VERSION} -w" \
    -o /out/kube-scheduler ./cmd/scheduler

# ---------- runtime ----------
FROM ${UBUNTU_BASE_IMAGE} AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-pip \
    && rm -rf /var/lib/apt/lists/*

ENV SOLVER_DIR=/opt/solver
RUN mkdir -p ${SOLVER_DIR}
COPY scripts/mycrossnodepreemption/requirements.txt ${SOLVER_DIR}/requirements.txt
RUN --mount=type=cache,target=/root/.cache/pip \
    pip3 install -r ${SOLVER_DIR}/requirements.txt
COPY scripts/mycrossnodepreemption/ ${SOLVER_DIR}/

COPY --from=builder /out/kube-scheduler /bin/kube-scheduler
WORKDIR /bin
CMD ["kube-scheduler"]
