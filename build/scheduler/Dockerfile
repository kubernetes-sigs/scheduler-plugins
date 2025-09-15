
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
ARG PY_BUILDER_IMAGE=python:3.13-slim
ARG DISTROLESS_PY=gcr.io/distroless/python3-debian12

# --- Go builder ---
FROM ${GO_BASE_IMAGE} AS gobuilder
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/scheduler ./cmd/scheduler
COPY pkg           ./pkg
COPY apis          ./apis
COPY manifests     ./manifests
ARG VERSION=v0.32.7
ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags "-X k8s.io/component-base/version.gitVersion=${VERSION} -w" \
    -o /out/kube-scheduler ./cmd/scheduler

# --- Python builder: build deps into a relocatable prefix ---
FROM ${PY_BUILDER_IMAGE} AS pybuilder
WORKDIR /wheels
COPY scripts/mycrossnodepreemption/requirements.txt .
RUN pip install --upgrade pip \
    && pip install --prefix /opt/pydeps -r requirements.txt

# --- Runtime (distroless with python3) ---
FROM ${DISTROLESS_PY} AS runtime
# Make Python find the installed packages
ENV PYTHONPATH="/opt/pydeps/lib/python3.13/site-packages"
WORKDIR /workspace
COPY --from=pybuilder /opt/pydeps /opt/pydeps
COPY scripts/mycrossnodepreemption/ /opt/solver/
COPY --from=gobuilder /out/kube-scheduler /bin/kube-scheduler
ENTRYPOINT []
CMD ["/bin/kube-scheduler"]
