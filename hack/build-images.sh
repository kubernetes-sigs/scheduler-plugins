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

GO_BASE_IMAGE=${GO_BASE_IMAGE:-"golang"}
DISTROLESS_BASE_IMAGE=${DISTROLESS_BASE_IMAGE:-"gcr.io/distroless/static:nonroot"}

# -t is the Docker engine default
TAG_FLAG="-t"

# If docker is not present, fall back to nerdctl
# TODO: nerdctl doesn't seem to have buildx.
if ! command -v ${BUILDER} && command -v nerdctl >/dev/null; then
  BUILDER=nerdctl
fi

# podman needs the manifest flag in order to create a single image.
if [[ "${BUILDER}" == "podman" ]]; then
  TAG_FLAG="--manifest"
fi

cd "${SCRIPT_ROOT}"

# DOCKER_BUILDX_CMD is an env variable set in CI (valued as "/buildx-entrypoint")
# If it's set, use it; otherwise use "$BUILDER buildx"
${DOCKER_BUILDX_CMD:-${BUILDER} buildx} build \
  --platform=${PLATFORMS} \
  -f ${SCHEDULER_DIR}/Dockerfile \
  --build-arg RELEASE_VERSION=${RELEASE_VERSION} \
  --build-arg GO_BASE_IMAGE=${GO_BASE_IMAGE} \
  --build-arg DISTROLESS_BASE_IMAGE=${DISTROLESS_BASE_IMAGE} \
  ${TAG_FLAG} ${REGISTRY}/${IMAGE} .

${DOCKER_BUILDX_CMD:-${BUILDER} buildx} build \
  --platform=${PLATFORMS} \
  -f ${CONTROLLER_DIR}/Dockerfile \
  --build-arg RELEASE_VERSION=${RELEASE_VERSION} \
  --build-arg GO_BASE_IMAGE=${GO_BASE_IMAGE} \
  --build-arg DISTROLESS_BASE_IMAGE=${DISTROLESS_BASE_IMAGE} \
  {TAG_FLAG} ${REGISTRY}/${CONTROLLER_IMAGE} .
