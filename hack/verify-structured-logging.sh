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

# This script is used to avoid regressions after a package is migrated
# to structured logging. once a package is completely migrated add
# it .structured_logging file to avoid any future regressions.

set -o errexit
set -o nounset
set -o pipefail

KUBE_ROOT=$(dirname "${BASH_SOURCE[0]}")/..
source "${KUBE_ROOT}/hack/lib/init.sh"
source "${KUBE_ROOT}/hack/lib/util.sh"

kube::golang::verify_go_version

GO111MODULE=on go install sigs.k8s.io/logtools/logcheck@latest

all_packages=()
# shellcheck disable=SC2010
for i in $(ls -d ./*/ | grep -v 'staging' | grep -v 'vendor' | grep -v 'hack')
do
  all_packages+=("$(go list ./"$i"/... 2> /dev/null | sed 's/sigs.k8s.io\/scheduler-plugins\///g')")
  all_packages+=(" ")
done

packages=()
while IFS='' read -r line; do
  if [ -z "$line" ]; then continue; fi
  packages+=("./$KUBE_ROOT/$line/...")
done < <(echo "${all_packages[@]}" | tr " " "\n")

ret=0
echo -e "\nRunning structured logging static check on all packages"

# Function to run logcheck and capture output
run_logcheck() {
  local os="$1"
  shift
  local output
  output=$(GOOS="$os" logcheck "$@" 2>&1)
  # TODO: Add more checks as needed
  # Currently, we only check for unstructured logging
  # more detail: https://github.com/kubernetes-sigs/scheduler-plugins/pull/831#discussion_r1835553079
  errors=$(echo "$output" | grep 'unstructured logging function')
  if [[ $? -eq 0 ]]; then
      echo "Error: Unstructured logging detected. Please fix the issues."
      echo "$errors"
      return 1
  fi
}

# Run logcheck for Linux
echo "Running logcheck for Linux..."
if ! run_logcheck linux "${packages[@]}"; then
  ret=1
fi

if [ $ret -eq 0 ]; then
  echo "Structured logging static check passed on all packages :)"
else
  echo "Structured logging static check failed. Please fix the issues."
  exit 1
fi
