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

COMMONENVVAR=GOOS=$(shell uname -s | tr A-Z a-z) GOARCH=$(subst x86_64,amd64,$(patsubst i%86,386,$(shell uname -m)))
BUILDENVVAR=CGO_ENABLED=0

LOCAL_REGISTRY=localhost:5000/scheduler-plugins
LOCAL_IMAGE=kube-scheduler:latest
LOCAL_CONTROLLER_IMAGE=controller:latest

# RELEASE_REGISTRY is the container registry to push
# into. The default is to push to the staging
# registry, not production(k8s.gcr.io).
RELEASE_REGISTRY?=gcr.io/k8s-staging-scheduler-plugins
RELEASE_VERSION?=$(shell git describe --tags --match "v*")
RELEASE_IMAGE:=kube-scheduler:$(RELEASE_VERSION)
RELEASE_CONTROLLER_IMAGE:=controller:$(RELEASE_VERSION)

.PHONY: all
all: build

.PHONY: build
build: autogen
	$(COMMONENVVAR) $(BUILDENVVAR) go build -ldflags '-w' -o bin/kube-scheduler cmd/scheduler/main.go
	$(COMMONENVVAR) $(BUILDENVVAR) go build -ldflags '-w' -o bin/controller cmd/controller/controller.go

.PHONY: local-image
local-image: clean
	docker build -f ./build/scheduler/Dockerfile -t $(LOCAL_REGISTRY)/$(LOCAL_IMAGE) .
	docker build -f ./build/controller/Dockerfile -t $(LOCAL_REGISTRY)/$(LOCAL_CONTROLLER_IMAGE) .

.PHONY: release-image
release-image: clean
	docker build -f ./build/scheduler/Dockerfile -t $(RELEASE_REGISTRY)/$(RELEASE_IMAGE) .
	docker build -f ./build/controller/Dockerfile -t $(RELEASE_REGISTRY)/$(RELEASE_CONTROLLER_IMAGE) .

.PHONY: push-release-image
push-release-image: release-image
	gcloud auth configure-docker
	docker push $(RELEASE_REGISTRY)/$(RELEASE_IMAGE)
	docker push $(RELEASE_REGISTRY)/$(RELEASE_CONTROLLER_IMAGE)

.PHONY: update-vendor
update-vendor:
	hack/update-vendor.sh

.PHONY: unit-test
unit-test: autogen
	hack/unit-test.sh

.PHONY: install-etcd
install-etcd:
	hack/install-etcd.sh

.PHONY: autogen
autogen: update-vendor
	hack/update-generated-openapi.sh

.PHONY: integration-test
integration-test: install-etcd autogen
	hack/integration-test.sh

.PHONY: verify-gofmt
verify-gofmt:
	hack/verify-gofmt.sh

.PHONY: clean
clean:
	rm -rf ./bin
