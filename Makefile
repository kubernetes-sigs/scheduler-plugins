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

GO_VERSION := $(shell awk '/^go /{print $$2}' go.mod|head -n1)
INTEGTESTENVVAR=SCHED_PLUGINS_TEST_VERBOSE=1

# Manage platform and builders
PLATFORMS ?= linux/amd64,linux/arm64,linux/s390x,linux/ppc64le
BUILDER ?= docker
ifeq ($(BUILDER),podman)
	ALL_FLAG=--all
else
	ALL_FLAG=
endif

# REGISTRY is the container registry to push
# into. The default is to push to the staging
# registry, not production(registry.k8s.io).
REGISTRY?=gcr.io/k8s-staging-scheduler-plugins
RELEASE_VERSION?=v$(shell date +%Y%m%d)-$(shell git describe --tags --match "v*")
RELEASE_IMAGE?=kube-scheduler:$(RELEASE_VERSION)
RELEASE_CONTROLLER_IMAGE:=controller:$(RELEASE_VERSION)
GO_BASE_IMAGE?=golang:$(GO_VERSION)
DISTROLESS_BASE_IMAGE?=gcr.io/distroless/static:nonroot
EXTRA_ARGS=""

# VERSION is the scheduler's version
#
# The RELEASE_VERSION variable can have one of two formats:
# v20201009-v0.18.800-46-g939c1c0 - automated build for a commit(not a tag) and also a local build
# v20200521-v0.18.800             - automated build for a tag
VERSION?=$(shell echo $(RELEASE_VERSION) | awk -F - '{print $$2}')
VERSION:=$(or $(VERSION),v0.0.$(shell date +%Y%m%d))

.PHONY: all
all: build

.PHONY: build
build: build-controller build-scheduler

.PHONY: build-controller
build-controller:
	$(GO_BUILD_ENV) go build -ldflags '-X k8s.io/component-base/version.gitVersion=$(VERSION) -w' -o bin/controller cmd/controller/controller.go

.PHONY: build-scheduler
build-scheduler:
	$(GO_BUILD_ENV) go build -ldflags '-X k8s.io/component-base/version.gitVersion=$(VERSION) -w' -o bin/kube-scheduler cmd/scheduler/main.go

.PHONY: build-images
build-images:
	BUILDER=$(BUILDER) \
	PLATFORMS=$(PLATFORMS) \
	RELEASE_VERSION=$(RELEASE_VERSION) \
	REGISTRY=$(REGISTRY) \
	IMAGE=$(RELEASE_IMAGE) \
	CONTROLLER_IMAGE=$(RELEASE_CONTROLLER_IMAGE) \
	GO_BASE_IMAGE=$(GO_BASE_IMAGE) \
	DISTROLESS_BASE_IMAGE=$(DISTROLESS_BASE_IMAGE) \
	DOCKER_BUILDX_CMD=$(DOCKER_BUILDX_CMD) \
	EXTRA_ARGS=$(EXTRA_ARGS) hack/build-images.sh

.PHONY: local-image
local-image: PLATFORMS="linux/$$(uname -m)"
local-image: RELEASE_VERSION="v0.0.0"
local-image: REGISTRY="localhost:5000/scheduler-plugins"
local-image: EXTRA_ARGS="--load"
local-image: clean build-images

.PHONY: release-images
push-images: EXTRA_ARGS="--push"
push-images: build-images

.PHONY: update-gomod
update-gomod:
	hack/update-gomod.sh

.PHONY: unit-test
unit-test: install-envtest install-crd
	hack/unit-test.sh $(ARGS)

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

# ----------------------------------- #
# targets for spyre-scheduler-plugins #
# ----------------------------------- #

DOCKERFILE_FIPS140_SCHEDULER	?= "build/scheduler/Dockerfile.fips140"
CONTROLLER_GEN					?= $(LOCALBIN)/controller-gen
CONTROLLER_TOOLS_VERSION		?= v0.17.3
FIPS_IMAGE_NAME					:= spyre-scheduler
FIPS_IMAGE						:= $(REGISTRY)/$(FIPS_IMAGE_NAME):$(VERSION)

# Shamesly copied from: https://github.com/opendatahub-io/opendatahub-operator/blob/a08c94a226585e43387ad263e2653c0fd43130f1/Makefile#L132C1-L139C1
define go-mod-version
$(shell go mod graph | grep $(1) | head -n 1 | cut -d'@' -f 2)
endef

define fetch-external-crds
GOFLAGS="-mod=readonly" $(CONTROLLER_GEN) crd \
paths=$(shell go env GOPATH)/pkg/mod/$(1)@$(call go-mod-version,$(1))/$(2)/... \
output:crd:artifacts:config=config/crd/external
endef

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: controller-gen
controller-gen: $(LOCALBIN) $(CONTROLLER_GEN) ## Download controller-gen if necessary
$(CONTROLLER_GEN): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: install-crd
install-crd: $(CONTROLLER_GEN)
	$(call fetch-external-crds,github.com/ibm-aiu/spyre-operator,api/v1alpha1)

.PHONY: build-fips140-scheduler-image
build-fips140-scheduler-image: clean
	$(BUILDER) build --pull \
		--tag $(FIPS_IMAGE) \
		--build-arg VERSION="$(VERSION)" \
		--file $(DOCKERFILE_FIPS140_SCHEDULER) .

.PHONY: docker-build
docker-build: build-fips140-scheduler-image

.PHONY: docker-push
docker-push: ## Push spyre webhook validator image image for the build host architecture
	$(BUILDER) push $(FIPS_IMAGE)

.PHONY: docker-build-push
docker-build-push: docker-build docker-push
