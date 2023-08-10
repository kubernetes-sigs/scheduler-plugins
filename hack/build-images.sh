#!/usr/bin/env bash

# Copyright 2023 The Kubernetes Authors.
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

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT=$(realpath $(dirname "${BASH_SOURCE[@]}")/..)

SCHEDULER_DIR="${SCRIPT_ROOT}"/build/scheduler
CONTROLLER_DIR="${SCRIPT_ROOT}"/build/controller

REGISTRY=${REGISTRY:-"localhost:5000/scheduler-plugins"}
IMAGE=${IMAGE:-"kube-scheduler:latest"}
CONTROLLER_IMAGE=${CONTROLLER_IMAGE:-"controller:latest"}

RELEASE_VERSION=${RELEASE_VERSION:-"v0.0.0"}

BUILDER=${BUILDER:-"docker"}

if ! command -v ${BUILDER} && command -v nerdctl >/dev/null; then
  BUILDER=nerdctl
fi

ARCH=${ARCH:-$(go env GOARCH)}
if [[ "${ARCH}" == "arm64" ]]; then
  ARCH="arm64v8"
fi

cd "${SCRIPT_ROOT}"

${BUILDER} build \
           -f ${SCHEDULER_DIR}/Dockerfile \
           --build-arg ARCH=${ARCH} \
           --build-arg RELEASE_VERSION=${RELEASE_VERSION} \
           -t ${REGISTRY}/${IMAGE} .
${BUILDER} build \
           -f ${CONTROLLER_DIR}/Dockerfile \
           --build-arg ARCH=${ARCH} \
           --build-arg RELEASE_VERSION=${RELEASE_VERSION} \
           -t ${REGISTRY}/${CONTROLLER_IMAGE} .
