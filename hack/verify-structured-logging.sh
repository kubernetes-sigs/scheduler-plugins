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

GO111MODULE=on go install k8s.io/klog/hack/tools/logcheck@latest

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
GOOS=linux    logcheck "${packages[@]}" || ret=$?
GOOS=windows  logcheck "${packages[@]}" || ret=$?

if [ $ret -eq 0 ]; then
  echo "Structured logging static check passed on all packages :)"
fi
