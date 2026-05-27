# Copyright 2020 The Kubernetes Authors.
ARG GO_BASE_IMAGE=golang:1.24
ARG DISTROLESS_BASE_IMAGE=gcr.io/distroless/static:nonroot
FROM --platform=${BUILDPLATFORM} $GO_BASE_IMAGE AS builder

WORKDIR /workspace
COPY go.mod go.mod
COPY go.sum go.sum
COPY pkg/ pkg/
COPY cmd/scheduler/ cmd/scheduler/
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o bin/kube-scheduler ./cmd/scheduler

FROM --platform=${BUILDPLATFORM} $DISTROLESS_BASE_IMAGE
WORKDIR /bin
COPY --from=builder /workspace/bin/kube-scheduler .
USER 65532:65532
ENTRYPOINT ["/bin/kube-scheduler"]
