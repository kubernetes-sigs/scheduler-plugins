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
REG_PORT=5000

.PHONY: all
all: build

.PHONY: build
build: autogen
	$(COMMONENVVAR) $(BUILDENVVAR) go build -ldflags '-w' -o bin/kube-scheduler cmd/main.go

.PHONY: local-image
local-image: autogen
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '-w' -o ./build/kube-scheduler cmd/main.go
	chmod +x ./build/kube-scheduler
	docker build -t localhost:$(REG_PORT)/scheduler-plugins:latest ./build
	rm ./build/kube-scheduler

.PHONY: update-vendor
update-vendor:
	hack/update-vendor.sh

.PHONY: unit-test
unit-test: update-vendor
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
