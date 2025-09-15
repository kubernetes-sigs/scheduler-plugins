
# Copyright 2020 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

ARG GO_BASE_IMAGE=golang:1.24
ARG PY_BUILDER_IMAGE=python:3.11-slim
ARG DISTROLESS_PY=gcr.io/distroless/python3-debian12

# --- Go builder unchanged ---
FROM ${GO_BASE_IMAGE} AS gobuilder

WORKDIR /src

COPY go.mod go.sum ./

RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd/scheduler ./cmd/scheduler
COPY pkg ./pkg
COPY apis ./apis
COPY manifests ./manifests

ARG VERSION=v0.32.7

ENV CGO_ENABLED=0

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags "-X k8s.io/component-base/version.gitVersion=${VERSION} -w" \
    -o /out/kube-scheduler ./cmd/scheduler

# --- Python builder: make venv point to /usr/bin/python3 ---
FROM ${PY_BUILDER_IMAGE} AS pybuilder

# Ensure /usr/bin/python3 exists and points to the real interpreter in slim
RUN ln -sf /usr/local/bin/python3 /usr/bin/python3

# Create the venv using /usr/bin/python3 so the venv’s symlinks match distroless
RUN /usr/bin/python3 -m venv /opt/venv

COPY scripts/mycrossnodepreemption/requirements.txt /requirements.txt

RUN /opt/venv/bin/python -m pip install --upgrade pip \
    && /opt/venv/bin/pip install --no-cache-dir -r /requirements.txt

# --- Distroless runtime ---
FROM ${DISTROLESS_PY} AS runtime

WORKDIR /workspace

COPY --from=pybuilder /opt/venv /opt/venv
COPY scripts/mycrossnodepreemption/ /opt/solver/
COPY --from=gobuilder /out/kube-scheduler /bin/kube-scheduler

ENTRYPOINT []
CMD ["/bin/kube-scheduler"]