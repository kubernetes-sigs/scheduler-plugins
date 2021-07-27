#!/usr/bin/env bash

# Copyright 2021 The Kubernetes Authors.
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

SCHED_PLUGIN_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
source "${SCHED_PLUGIN_ROOT}/hack/lib/init.sh"

kube::golang::verify_go_version

cd "${SCHED_PLUGIN_ROOT}"

CRD_OPTIONS="crd:trivialVersions=true,preserveUnknownFields=false"

# Download controller-gen locally
CONTROLLER_GEN="$(shell pwd)/bin/controller-gen"
$(call go-get-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen)

# Create a temp directory
_tmpdir="$(mktemp -d "${SCHED_PLUGIN_ROOT}/_tmp")"
# Generate CRD
pushd "${_tmpdir}" &> /dev/null
$(CONTROLLER_GEN) ${CRD_OPTIONS} path="./..." output:crd:artifacts:config=${_tmpdir}/crd/bases

popd &> /dev/null

pushd "${SCHED_PLUGIN_ROOT}" > /dev/null 2>&1
if ! _out="$(diff -Naupr manifests/coscheduling/ "${_crdtmp}/crd/bases/}")"; then
    echo "Generated output differs:" >&2
    echo "${_out}" >&2
    echo "Verification failed."
    exit 1
fi
popd &> /dev/null

rm -r ./_tmpdir
echo "Controllers Gen for CRD verified."
