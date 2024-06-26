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
ARG ARCH
ARG GO_BASE_IMAGE=golang
ARG DISTROLESS_BASE_IMAGE=gcr.io/distroless/static:nonroot-$ARCH
FROM $GO_BASE_IMAGE:1.21

WORKDIR /go/src/sigs.k8s.io/scheduler-plugins
COPY . .
ARG ARCH
RUN make build-controller.$ARCH

# Reference from https://github.com/kubernetes-sigs/cluster-api/blob/e52848c7dbb8bd688fdf87a9453735b6ffa0d366/cmd/clusterctl/Dockerfile
FROM $DISTROLESS_BASE_IMAGE

COPY --from=0 /go/src/sigs.k8s.io/scheduler-plugins/bin/controller /bin/controller

WORKDIR /bin
# Refer to https://github.com/kubernetes-sigs/cluster-api/blob/main/Dockerfile
# Use uid of nonroot user (65532) because kubernetes expects numeric user when applying pod security policies
USER 65532
CMD ["controller"]
