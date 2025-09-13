# builder stage
ARG GO_BASE_IMAGE=golang:1.24
FROM ${GO_BASE_IMAGE} AS builder
WORKDIR /src

# 1) deps first (keeps cache effective)
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# 2) source
COPY cmd/scheduler ./cmd/scheduler
COPY pkg           ./pkg
COPY apis          ./apis

# 3) build (NO login shell)
ARG VERSION=dev
ARG GO_BUILD_ENV="CGO_ENABLED=0"
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    $GO_BUILD_ENV go build \
    -ldflags "-X k8s.io/component-base/version.gitVersion=${VERSION} -w" \
    -o /out/kube-scheduler ./cmd/scheduler


FROM ${UBUNTU_BASE_IMAGE}
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-pip && \
    rm -rf /var/lib/apt/lists/*

ENV SOLVER_DIR=/opt/solver
RUN mkdir -p ${SOLVER_DIR}
COPY scripts/mycrossnodepreemption/requirements.txt ${SOLVER_DIR}/requirements.txt
RUN --mount=type=cache,target=/root/.cache/pip \
    pip3 install -r ${SOLVER_DIR}/requirements.txt
COPY scripts/mycrossnodepreemption/ ${SOLVER_DIR}/

COPY --from=builder /out/kube-scheduler /bin/kube-scheduler
WORKDIR /bin
CMD ["kube-scheduler"]
