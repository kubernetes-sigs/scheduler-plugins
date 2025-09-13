# build/scheduler/Dockerfile

ARG GO_BASE_IMAGE=golang:1.24
ARG RUNTIME_IMAGE=python:3.12-slim
ARG GO_BUILD_ENV="CGO_ENABLED=0"
ARG VERSION=dev

# ---------- builder ----------
FROM ${GO_BASE_IMAGE} AS builder
WORKDIR /src

# 1) Go deps first for better cache
COPY go.mod go.sum ./
# enable BuildKit cache for modules/build
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# 2) Copy only what the scheduler build needs
COPY cmd/scheduler ./cmd/scheduler
COPY pkg          ./pkg
COPY apis         ./apis
# copy anything else the build imports (add as needed)
# COPY config       ./config

# 3) Build
# Use /bin/sh so $VERSION expands here (not a Makefile var)
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    /bin/sh -lc "$GO_BUILD_ENV go build \
    -ldflags '-X k8s.io/component-base/version.gitVersion=$VERSION -w' \
    -o /out/kube-scheduler ./cmd/scheduler"

# ---------- runtime ----------
FROM ${RUNTIME_IMAGE}

# 1) Solver deps (pip already in python:slim)
ENV SOLVER_DIR=/opt/solver
WORKDIR /opt/solver
COPY scripts/mycrossnodepreemption/requirements.txt ./requirements.txt
RUN pip install --no-cache-dir -r requirements.txt

# 2) Solver code
COPY scripts/mycrossnodepreemption/ /opt/solver/

# 3) Scheduler binary
COPY --from=builder /out/kube-scheduler /bin/kube-scheduler

WORKDIR /bin
CMD ["kube-scheduler"]
