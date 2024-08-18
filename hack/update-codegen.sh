#!/usr/bin/env bash

# Copyright 2017 The Kubernetes Authors.
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

SCRIPT_ROOT=$(dirname "${BASH_SOURCE[@]}")/..

TOOLS_DIR=$(realpath ./hack/tools)
TOOLS_BIN_DIR="${TOOLS_DIR}/bin"
GO_INSTALL=$(realpath ./hack/go-install.sh)
CONTROLLER_GEN_VER=v0.14.0
CONTROLLER_GEN_BIN=controller-gen
CONTROLLER_GEN=${TOOLS_BIN_DIR}/${CONTROLLER_GEN_BIN}-${CONTROLLER_GEN_VER}
# Need v1 to support defaults in CRDs, unfortunately limiting us to k8s 1.16+
CRD_OPTIONS="crd:crdVersions=v1"

GOBIN=${TOOLS_BIN_DIR} ${GO_INSTALL} sigs.k8s.io/controller-tools/cmd/controller-gen ${CONTROLLER_GEN_BIN} ${CONTROLLER_GEN_VER}

CODEGEN_PKG=${CODEGEN_PKG:-$(cd "${SCRIPT_ROOT}"; ls -d -1 ./vendor/k8s.io/code-generator 2>/dev/null || echo ../code-generator)}

source "${CODEGEN_PKG}/kube_codegen.sh"

kube::codegen::gen_helpers \
  --boilerplate "${SCRIPT_ROOT}/hack/boilerplate/boilerplate.generatego.txt" \
  "${SCRIPT_ROOT}/apis"

kube::codegen::gen_client \
  --with-watch \
  --with-applyconfig \
  --output-dir "${SCRIPT_ROOT}/pkg/generated" \
  --output-pkg sigs.k8s.io/scheduler-plugins/pkg/generated \
  --boilerplate "${SCRIPT_ROOT}/hack/boilerplate/boilerplate.generatego.txt" \
  "${SCRIPT_ROOT}/apis"

${CONTROLLER_GEN} object:headerFile="hack/boilerplate/boilerplate.generatego.txt" \
  paths="./apis/scheduling/..."

${CONTROLLER_GEN} ${CRD_OPTIONS} rbac:roleName=work-manager webhook \
  paths="./apis/scheduling/..." \
  output:crd:artifacts:config=config/crd/bases
