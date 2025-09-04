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

# ---------- build kube-scheduler ----------
ARG GO_BASE_IMAGE=golang:1.24
ARG UBUNTU_BASE_IMAGE=ubuntu:22.04

FROM ${GO_BASE_IMAGE} AS builder
WORKDIR /src
COPY . .
RUN make build-scheduler

FROM ${UBUNTU_BASE_IMAGE}

#TODO_HC: USE NEWEST PYTHON VERSION

# 1) Python + pip
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-pip && \
    rm -rf /var/lib/apt/lists/*

# 2) Copy solver code
ENV SOLVER_DIR=/opt/solver
RUN mkdir -p ${SOLVER_DIR}

# Copy just requirements first (keeps this layer cached)
COPY scripts/mycrossnodepreemption/requirements.txt ${SOLVER_DIR}/requirements.txt

# Install deps using cache (for faster rebuilds)
RUN --mount=type=cache,target=/root/.cache/pip \
    pip3 install -r ${SOLVER_DIR}/requirements.txt

# Now copy the rest of the solver code
COPY scripts/mycrossnodepreemption/ ${SOLVER_DIR}/

# 3) Copy scheduler binary
COPY --from=builder /src/bin/kube-scheduler /bin/kube-scheduler

WORKDIR /bin
CMD ["kube-scheduler"]