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
ARG GO_BASE_IMAGE
ARG DISTROLESS_BASE_IMAGE=gcr.io/distroless/static:nonroot
FROM --platform=${BUILDPLATFORM} $GO_BASE_IMAGE AS builder

WORKDIR /workspace
COPY . .
ARG TARGETARCH
RUN make build-scheduler GO_BUILD_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH}'

FROM --platform=${BUILDPLATFORM} $DISTROLESS_BASE_IMAGE

WORKDIR /bin
COPY --from=builder /workspace/bin/kube-scheduler .
USER 65532:65532

ENTRYPOINT ["/bin/kube-scheduler"]
