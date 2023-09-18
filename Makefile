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

export DOCKER_BUILDKIT=1

# RELEASE_REGISTRY is the container registry to push
# into. The default is to push to the staging
# registry, not production(k8s.gcr.io).
RELEASE_REGISTRY?=artifactory-kfs.habana-labs.com/k8s-infra-docker-dev/habana/scheduler-plugins
# RELEASE_VERSION?=v$(shell date +%Y%m%d)-v0.23.10
RELEASE_VERSION?=v0.23.28
RELEASE_IMAGE:=kube-scheduler:$(RELEASE_VERSION)
RELEASE_CONTROLLER_IMAGE:=controller:$(RELEASE_VERSION)

LOCAL_REGISTRY=artifactory-kfs.habana-labs.com/k8s-infra-docker-dev/habana/scheduler-plugins
LOCAL_IMAGE=kube-scheduler:$(RELEASE_VERSION)
LOCAL_CONTROLLER_IMAGE=controller:$(RELEASE_VERSION)

# VERSION is the scheduler's version
#
# The RELEASE_VERSION variable can have one of two formats:
# v20201009-v0.18.800-46-g939c1c0 - automated build for a commit(not a tag) and also a local build
# v20200521-v0.18.800             - automated build for a tag
VERSION=$(shell echo $(RELEASE_VERSION) | awk -F - '{print $$2}')

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
	GOOS=linux $(BUILDENVVAR) GOARCH=arm64 go build -ldflags '-w' -o bin/controller cmd/controller/controller.go

.PHONY: build-scheduler
build-scheduler:
	$(COMMONENVVAR) $(BUILDENVVAR) go build -ldflags '-X k8s.io/component-base/version.gitVersion=$(VERSION) -w' -o bin/kube-scheduler cmd/scheduler/main.go

.PHONY: build-scheduler.amd64
build-scheduler.amd64:
	$(COMMONENVVAR) $(BUILDENVVAR) GOARCH=amd64 go build -ldflags '-X k8s.io/component-base/version.gitVersion=$(VERSION) -w' -o bin/kube-scheduler cmd/scheduler/main.go

.PHONY: build-scheduler.arm64v8
build-scheduler.arm64v8:
	GOOS=linux $(BUILDENVVAR) GOARCH=arm64 go build -ldflags '-X k8s.io/component-base/version.gitVersion=$(VERSION) -w' -o bin/kube-scheduler cmd/scheduler/main.go

.PHONY: local-image
local-image: clean
	docker build -f ./build/scheduler/Dockerfile --build-arg ARCH="amd64" --build-arg RELEASE_VERSION="v20201009-v0.18.800-46-g939c1c0" -t $(LOCAL_REGISTRY)/$(LOCAL_IMAGE) .
	docker build -f ./build/controller/Dockerfile --build-arg ARCH="amd64" -t $(LOCAL_REGISTRY)/$(LOCAL_CONTROLLER_IMAGE) .
	docker push $(LOCAL_REGISTRY)/$(LOCAL_CONTROLLER_IMAGE)
	docker push $(LOCAL_REGISTRY)/$(LOCAL_IMAGE)

.PHONY: release-image.amd64
release-image.amd64: clean
	docker build -f ./build/scheduler/Dockerfile --build-arg ARCH="amd64" --build-arg RELEASE_VERSION="$(RELEASE_VERSION)" -t $(RELEASE_REGISTRY)/$(RELEASE_IMAGE)-amd64 .
	docker build -f ./build/controller/Dockerfile --build-arg ARCH="amd64" -t $(RELEASE_REGISTRY)/$(RELEASE_CONTROLLER_IMAGE)-amd64 .

.PHONY: release-image.arm64v8
release-image.arm64v8: clean
	docker build -f ./build/scheduler/Dockerfile --build-arg ARCH="arm64v8" --build-arg RELEASE_VERSION="$(RELEASE_VERSION)" -t $(RELEASE_REGISTRY)/$(RELEASE_IMAGE)-arm64 .
	docker build -f ./build/controller/Dockerfile --build-arg ARCH="arm64v8" -t $(RELEASE_REGISTRY)/$(RELEASE_CONTROLLER_IMAGE)-arm64 .

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
unit-test:
	hack/unit-test.sh

.PHONY: install-envtest
install-envtest:
	hack/install-envtest.sh

.PHONY: integration-test
integration-test: install-envtest
	$(INTEGTESTENVVAR) hack/integration-test.sh

.PHONY: verify
verify:
	hack/verify-gofmt.sh
	hack/verify-crdgen.sh
	hack/verify-structured-logging.sh

.PHONY: clean
clean:
	rm -rf ./bin

.PHONY: kind-load
kind-load: local-image
	kind load docker-image $(LOCAL_CONTROLLER_IMAGE) --nodes multi-control-plane,multi-worker,multi-worker2,multi-worker3 --name multi
	kind load docker-image $(LOCAL_IMAGE) --nodes multi-control-plane,multi-worker,multi-worker2,multi-worker3 --name multi

.PHONY: kind-down
kind-down:
	kind delete cluster --name multi

.PHONY: kind-up
kind-up:
	kind create cluster --name multi --config ~/kind-config.yaml


.PHONY: helm-install
helm-install:
	@cd manifests/install/charts/ && helm install scheduler-plugins as-a-second-scheduler/


.PHONY: refresh-image
refresh-image:
	ansible-playbook hack/ansible/refresh-image.yaml \
	-i $(INVENTORY) \
	-e image="$(LOCAL_REGISTRY)/$(LOCAL_IMAGE)"

.PHONY: refresh-image-dc01
refresh-image-dc01:
	@ansible-playbook hack/ansible/refresh-image.yaml \
	-i $(INVENTORY) \
	-e "image=$(LOCAL_REGISTRY)/$(LOCAL_IMAGE)"

.PHONY: refresh-image-dc02
refresh-image-dc02:
	@ansible-playbook hack/ansible/refresh-image.yaml \
	-i $(INVENTORY) \
	-e "image=$(LOCAL_REGISTRY)/$(LOCAL_IMAGE)"

## deploy-scheduler: deployes the scheduler to staging
.PHONY: deploy-scheduler
deploy-scheduler:
	@ansible-playbook hack/ansible/install-as-single.yaml \
	-i $(INVENTORY) \
	-e '{"image": "$(LOCAL_REGISTRY)/$(LOCAL_IMAGE)", "priority_zones": ["a", "b", "c", "d", "e", "f", "g", "h"]}'

.PHONY: deploy-scheduler-test
deploy-scheduler-test:
	@ansible-playbook hack/ansible/install-as-single.yaml \
	-i $(INVENTORY) \
	-e '{"image": "$(LOCAL_REGISTRY)/$(LOCAL_IMAGE)", "priority_zones": ["a", "b", "c", "d", "e", "f", "g", "h"]}'

.PHONY: deploy-scheduler-dc01
deploy-scheduler-dc01:
	@ansible-playbook hack/ansible/install-as-single.yaml \
	-i $(INVENTORY) \
	-e '{"image": "$(LOCAL_REGISTRY)/$(LOCAL_IMAGE)", "priority_zones": ["d", "a", "b", "c", "e", "f", "g", "h"]}'

.PHONY: deploy-scheduler-dc02
deploy-scheduler-dc02:
	@ansible-playbook hack/ansible/install-as-single.yaml \
	-i $(INVENTORY) \
	-e '{"image": "$(LOCAL_REGISTRY)/$(LOCAL_IMAGE)", "priority_zones": ["a", "b", "c", "d", "e", "f", "g", "h"]}'
