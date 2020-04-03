#!/usr/bin/env bash

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

set -o errexit
set -o nounset
set -o pipefail

# TODO: make this script run faster.

SCRIPT_ROOT=$(dirname ${BASH_SOURCE})/..
export GOPATH="$(cd ${SCRIPT_ROOT} && pwd)/_output/local/go"
mkdir -p $GOPATH

go install k8s.io/kube-openapi/cmd/openapi-gen

KUBE_INPUT_DIRS=(
  $(
    grep --color=never -rl '+k8s:openapi-gen=' vendor/k8s.io | \
    xargs -n1 dirname | \
    sed "s,^vendor/,," | \
    sort -u | \
    sed '/^k8s\.io\/kubernetes\/build\/root$/d' | \
    sed '/^k8s\.io\/kubernetes$/d' | \
    sed '/^k8s\.io\/kubernetes\/staging$/d' | \
    sed 's,k8s\.io/kubernetes/staging/src/,,' | \
    grep -v 'k8s.io/code-generator' | \
    grep -v 'k8s.io/sample-apiserver'
  )
)

KUBE_INPUT_DIRS=$(IFS=,; echo "${KUBE_INPUT_DIRS[*]}")

function join { local IFS="$1"; shift; echo "$*"; }

echo "Generating Kubernetes openapi"

$GOPATH/bin/openapi-gen \
  --output-file-base zz_generated.openapi \
  --output-base="${GOPATH}/src" \
  --go-header-file ${SCRIPT_ROOT}/hack/boilerplate/boilerplate.generatego.txt \
  --output-base="./" \
  --input-dirs $(join , "${KUBE_INPUT_DIRS[@]}") \
  --output-package "vendor/k8s.io/kubernetes/pkg/generated/openapi" \
  --report-filename "${SCRIPT_ROOT}/hack/openapi-violation.list" \
  "$@"

# TODO: verify hack/openapi-violation.list