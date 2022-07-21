#!/usr/bin/env bash

# Copyright 2022 The Kubernetes Authors.
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

# This script is a helper script to install envtest
# (https://github.com/kubernetes-sigs/controller-runtime/tree/master/pkg/envtest)
# Mostly used by CI but can also be used for running integration tests locally.
# Usage: `hack/install-envtest.sh`.

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
source "${SCRIPT_ROOT}/hack/lib/init.sh"

# setup-envtest requires the version to be in the form of X.Y(.Z) (e.g. 1.23 or 1.23.3)
# thus we want to remove the 'v' from the version extracted out of the go.mod file
version=$(cat ${SCRIPT_ROOT}/go.mod | grep 'k8s.io/kubernetes' | grep -v '=>' | awk '{print $NF}' | awk 'BEGIN{FS=OFS="."}NF--' | sed 's/v//')

GOPATH=$(go env GOPATH)
TEMP_DIR=${TMPDIR-/tmp}
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
"${GOPATH}"/bin/setup-envtest use -p env "${version}" > "${TEMP_DIR}/setup-envtest"

