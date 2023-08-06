# Copyright 2023 The Kubernetes Authors.
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

HUGO = $(shell which hugo)

.PHONY: serve
serve: themes/hugo-book
	$(HUGO) server --bind="0.0.0.0" \
	--ignoreCache \
	--buildFuture \
	--disableFastRender

.PHONY: build
build: themes/hugo-book
	$(HUGO) --enableGitInfo --buildFuture --minify

themes/hugo-book:
	git clone --depth 1 --branch v9 https://github.com/alex-shpak/hugo-book $@
