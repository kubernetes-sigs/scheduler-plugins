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

# 1) Python + pip
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 python3-pip && \
    rm -rf /var/lib/apt/lists/*

# 2) Copy your solver code (adjust the path if needed)
#    You said the script + requirements live under scripts/optimal-pod-assignments/
ENV SOLVER_DIR=/opt/solver
RUN mkdir -p ${SOLVER_DIR}
COPY scripts/optimal-pod-assignments/ ${SOLVER_DIR}/

# 3) Install Python deps **globally** for python3
#    (so your Go plugin's "python3 /opt/solver/main.py" finds them)
RUN pip3 install --no-cache-dir -r ${SOLVER_DIR}/requirements.txt

# 4) Copy the scheduler binary
COPY --from=builder /src/bin/kube-scheduler /bin/kube-scheduler

WORKDIR /bin
CMD ["kube-scheduler"]
