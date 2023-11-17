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

ARCHS = amd64 arm64
COMMONENVVAR=GOOS=$(shell uname -s | tr A-Z a-z)
BUILDENVVAR=CGO_ENABLED=0
INTEGTESTENVVAR=SCHED_PLUGINS_TEST_VERBOSE=1

# RELEASE_REGISTRY is the container registry to push
# into. The default is to push to the staging
# registry, not production(registry.k8s.io).
RELEASE_REGISTRY?=gcr.io/k8s-staging-scheduler-plugins
RELEASE_VERSION?=v$(shell date +%Y%m%d)-$(shell git describe --tags --match "v*")
RELEASE_IMAGE:=kube-scheduler:$(RELEASE_VERSION)
RELEASE_CONTROLLER_IMAGE:=controller:$(RELEASE_VERSION)

# VERSION is the scheduler's version
#
# The RELEASE_VERSION variable can have one of two formats:
# v20201009-v0.18.800-46-g939c1c0 - automated build for a commit(not a tag) and also a local build
# v20200521-v0.18.800             - automated build for a tag
VERSION=$(shell echo $(RELEASE_VERSION) | awk -F - '{print $$2}')
VERSION:=$(or $(VERSION),v0.0.$(shell date +%Y%m%d))

.PHONY: all
all: build

.PHONY: build
build: build-controller build-scheduler

.PHONY: build.amd64
build.amd64: build-controller.amd64 build-scheduler.amd64

.PHONY: build.arm64v8
build.arm64v8: build-controller.arm64v8 build-scheduler.arm64v8

.PHONY: build-controller
build-controller:
	$(COMMONENVVAR) $(BUILDENVVAR) go build -ldflags '-w' -o bin/controller cmd/controller/controller.go

.PHONY: build-controller.amd64
build-controller.amd64:
	$(COMMONENVVAR) $(BUILDENVVAR) GOARCH=amd64 go build -ldflags '-w' -o bin/controller cmd/controller/controller.go

.PHONY: build-controller.arm64v8
build-controller.arm64v8:
	$(COMMONENVVAR) $(BUILDENVVAR) GOARCH=arm64 go build -ldflags '-w' -o bin/controller cmd/controller/controller.go

.PHONY: build-scheduler
build-scheduler:
	$(COMMONENVVAR) $(BUILDENVVAR) go build -ldflags '-X k8s.io/component-base/version.gitVersion=$(VERSION) -w' -o bin/kube-scheduler cmd/scheduler/main.go

.PHONY: build-scheduler.amd64
build-scheduler.amd64:
	$(COMMONENVVAR) $(BUILDENVVAR) GOARCH=amd64 go build -ldflags '-X k8s.io/component-base/version.gitVersion=$(VERSION) -w' -o bin/kube-scheduler cmd/scheduler/main.go

.PHONY: build-scheduler.arm64v8
build-scheduler.arm64v8:
	$(COMMONENVVAR) $(BUILDENVVAR) GOARCH=arm64 go build -ldflags '-X k8s.io/component-base/version.gitVersion=$(VERSION) -w' -o bin/kube-scheduler cmd/scheduler/main.go

.PHONY: local-image
local-image: clean
	RELEASE_VERSION=$(RELEASE_VERSION) hack/build-images.sh

.PHONY: release-image.amd64
release-image.amd64: clean
	ARCH="amd64" \
	RELEASE_VERSION=$(RELEASE_VERSION) \
	REGISTRY=$(RELEASE_REGISTRY) \
	IMAGE=$(RELEASE_IMAGE)-amd64 \
	CONTROLLER_IMAGE=$(RELEASE_CONTROLLER_IMAGE)-amd64 \
	hack/build-images.sh

.PHONY: release-image.arm64v8
release-image.arm64v8: clean
	ARCH="arm64" \
	RELEASE_VERSION=$(RELEASE_VERSION) \
	REGISTRY=$(RELEASE_REGISTRY) \
	IMAGE=$(RELEASE_IMAGE)-arm64 \
	CONTROLLER_IMAGE=$(RELEASE_CONTROLLER_IMAGE)-arm64 \
	hack/build-images.sh

.PHONY: push-release-images
push-release-images: release-image.amd64 release-image.arm64v8
	gcloud auth configure-docker
	for arch in $(ARCHS); do \
		docker push $(RELEASE_REGISTRY)/$(RELEASE_IMAGE)-$${arch} ;\
		docker push $(RELEASE_REGISTRY)/$(RELEASE_CONTROLLER_IMAGE)-$${arch} ;\
	done
	DOCKER_CLI_EXPERIMENTAL=enabled docker manifest create $(RELEASE_REGISTRY)/$(RELEASE_IMAGE) $(addprefix --amend $(RELEASE_REGISTRY)/$(RELEASE_IMAGE)-, $(ARCHS))
	DOCKER_CLI_EXPERIMENTAL=enabled docker manifest create $(RELEASE_REGISTRY)/$(RELEASE_CONTROLLER_IMAGE) $(addprefix --amend $(RELEASE_REGISTRY)/$(RELEASE_CONTROLLER_IMAGE)-, $(ARCHS))
	for arch in $(ARCHS); do \
		DOCKER_CLI_EXPERIMENTAL=enabled docker manifest annotate --arch $${arch} $(RELEASE_REGISTRY)/$(RELEASE_IMAGE) $(RELEASE_REGISTRY)/$(RELEASE_IMAGE)-$${arch} ;\
		DOCKER_CLI_EXPERIMENTAL=enabled docker manifest annotate --arch $${arch} $(RELEASE_REGISTRY)/$(RELEASE_CONTROLLER_IMAGE) $(RELEASE_REGISTRY)/$(RELEASE_CONTROLLER_IMAGE)-$${arch} ;\
	done
	DOCKER_CLI_EXPERIMENTAL=enabled docker manifest push $(RELEASE_REGISTRY)/$(RELEASE_IMAGE) ;\
	DOCKER_CLI_EXPERIMENTAL=enabled docker manifest push $(RELEASE_REGISTRY)/$(RELEASE_CONTROLLER_IMAGE) ;\

.PHONY: update-vendor
update-vendor:
	hack/update-vendor.sh

.PHONY: unit-test
unit-test: install-envtest
	hack/unit-test.sh

.PHONY: install-envtest
install-envtest:
	hack/install-envtest.sh

.PHONY: integration-test
integration-test: install-envtest
	$(INTEGTESTENVVAR) hack/integration-test.sh $(ARGS)

.PHONY: verify
verify:
	hack/verify-gomod.sh
	hack/verify-gofmt.sh
	hack/verify-crdgen.sh
	hack/verify-structured-logging.sh
	hack/verify-toc.sh

.PHONY: clean
clean:
	rm -rf ./bin
