# Copyright 2020 The Kubernetes Authors.
ARG GO_BASE_IMAGE=golang:1.24
ARG DISTROLESS_BASE_IMAGE=gcr.io/distroless/static:nonroot
FROM --platform=${BUILDPLATFORM} $GO_BASE_IMAGE AS builder

WORKDIR /workspace
# 首先只复制依赖相关文件，并进行同步
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod tidy

# 复制其余源码
COPY . .
RUN go mod tidy
ARG TARGETARCH
RUN make build-scheduler GO_BUILD_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH}'

FROM --platform=${BUILDPLATFORM} $DISTROLESS_BASE_IMAGE
WORKDIR /bin
COPY --from=builder /workspace/bin/kube-scheduler .
USER 65532:65532
ENTRYPOINT ["/bin/kube-scheduler"]
