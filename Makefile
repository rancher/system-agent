# Copyright © 2025 SUSE LLC
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

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

#
# Go.
#
GO_VERSION ?= $(shell grep "^go " go.mod | head -1 | awk '{print $$2}')
GO_CONTAINER_IMAGE ?= docker.io/library/golang:$(GO_VERSION)

# Use GOPROXY environment variable if set
GOPROXY := $(shell go env GOPROXY)
ifeq ($(GOPROXY),)
GOPROXY := https://proxy.golang.org
endif
export GOPROXY

# Active module mode, as we use go modules to manage dependencies
export GO111MODULE=on

# This option is for running docker manifest command
export DOCKER_CLI_EXPERIMENTAL := enabled

CURL_RETRIES=3

# Directories
ROOT_DIR:=$(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))
BIN_DIR := bin
DIST_DIR := dist
TOOLS_DIR := hack/tools
TOOLS_BIN_DIR := $(abspath $(TOOLS_DIR)/$(BIN_DIR))

$(BIN_DIR):
	mkdir -p $@

$(DIST_DIR):
	mkdir -p $@

$(TOOLS_BIN_DIR):
	mkdir -p $@

export PATH := $(abspath $(TOOLS_BIN_DIR)):$(PATH)

# Binaries and tools
GOLANGCI_LINT_VER := v1.64.5
GOLANGCI_LINT_BIN := golangci-lint
GOLANGCI_LINT := $(abspath $(TOOLS_BIN_DIR)/$(GOLANGCI_LINT_BIN)-$(GOLANGCI_LINT_VER))
GOLANGCI_LINT_PKG := github.com/golangci/golangci-lint/cmd/golangci-lint

GINKGO_VER := v2.28.1
GINKGO_BIN := $(abspath $(TOOLS_BIN_DIR)/ginkgo-$(GINKGO_VER))
GINKGO_PKG := github.com/onsi/ginkgo/v2/ginkgo

GO_INSTALL := ./scripts/go-install.sh

# Version information
COMMIT ?= $(shell git rev-parse --short HEAD)
GIT_TAG ?= $(shell git tag -l --contains HEAD 2>/dev/null | head -n 1)
DIRTY ?= $(shell git status --porcelain --untracked-files=no 2>/dev/null)
VERSION ?= $(if $(and $(GIT_TAG),$(if $(DIRTY),,true)),$(GIT_TAG),$(COMMIT)$(if $(DIRTY),-dirty,))

# Build configuration
CGO_ENABLED ?= 0

# E2E configuration
E2E_IMAGE_TAG ?= e2e-test
E2E_IMAGE_NAME ?= $(ORG)/$(IMAGE_NAME)
E2E_KIND_CLUSTER_NAME ?= system-agent-e2e
SKIP_RESOURCE_CLEANUP ?= false

# Ginkgo E2E configuration
GINKGO_LABEL_FILTER ?= short
GINKGO_NODES ?= 1
GINKGO_TIMEOUT ?= 30m
GINKGO_POLL_PROGRESS_AFTER ?= 10m
GINKGO_POLL_PROGRESS_INTERVAL ?= 1m
E2E_ARTIFACTS ?= $(ROOT_DIR)/_artifacts

# Registry / images
TAG ?= $(if $(shell echo $(VERSION) | grep -q dirty && echo dirty),dev,$(VERSION))
ARCH ?= $(shell go env GOARCH)
TARGET_OS ?= $(if $(GOOS),$(GOOS),$(shell go env GOOS))
REGISTRY ?= docker.io
ORG ?= rancher
IMAGE_NAME ?= system-agent
IMAGE ?= $(REGISTRY)/$(ORG)/$(IMAGE_NAME)
TAG_SUFFIX ?= $(TARGET_OS)-$(ARCH)

# Build flags
LDFLAGS := -X github.com/rancher/system-agent/pkg/version.Version=$(VERSION)
LDFLAGS += -X github.com/rancher/system-agent/pkg/version.GitCommit=$(COMMIT)
ifeq ($(TARGET_OS),linux)
	LDFLAGS += -extldflags "-static" -s
endif

# Builder image (used for containerized builds)
BUILDER_IMAGE ?= registry.suse.com/bci/golang:$(GO_VERSION)

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
	@echo ""
	@echo "Build modes:"
	@echo "  Local build:       make build (uses host Go toolchain)"
	@echo "  Container build:   make docker-build (uses multi-stage Dockerfile)"

##@ Development

.PHONY: generate
generate: ## Run all generators
	go generate ./...

.PHONY: mod
mod: ## Run go mod tidy to ensure modules are up to date
	go mod tidy
	go mod verify

.PHONY: vendor
vendor: ## Vendor dependencies
	go mod tidy
	go mod vendor
	go mod verify

.PHONY: vendor-clean
vendor-clean: ## Remove vendor directory
	rm -rf vendor

##@ Lint / Verify

.PHONY: fmt
fmt: ## Run go fmt against code
	@echo "Running go fmt..."
	@go fmt ./...

.PHONY: fmt-check
fmt-check: ## Check if code is formatted (fails if not)
	@echo "Checking go fmt..."
	@for package in $$(go list ./...); do \
		echo "Checking $$package"; \
		test -z "$$(gofmt -l $$(go list -f '{{.Dir}}' $$package) 2>/dev/null | tee /dev/stderr)"; \
	done
	@echo "All files are properly formatted"

.PHONY: vet
vet: ## Run go vet against code
	go vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Lint the codebase
	$(GOLANGCI_LINT) run -v --timeout 5m $(GOLANGCI_LINT_EXTRA_ARGS)

.PHONY: lint-fix
lint-fix: ## Lint the codebase and run auto-fixers if supported by the linter
	GOLANGCI_LINT_EXTRA_ARGS=--fix $(MAKE) lint

ALL_VERIFY_CHECKS = modules gen

.PHONY: verify
verify: $(addprefix verify-,$(ALL_VERIFY_CHECKS)) ## Run all verify-* targets

.PHONY: verify-modules
verify-modules: mod ## Verify go modules are up to date
	@if !(git diff --quiet HEAD -- go.sum go.mod); then \
		git diff; \
		echo "go module files are out of date, run make mod"; exit 1; \
	fi

.PHONY: verify-gen
verify-gen: generate ## Verify go generated files are up to date
	@if !(git diff --quiet HEAD); then \
		git diff; \
		echo "generated files are out of date, run make generate"; exit 1; \
	fi

##@ Testing

.PHONY: test
test: ## Run tests
	go test -cover -tags=test ./...

.PHONY: e2e-image
e2e-image: ## Build system-agent Docker image for e2e tests
	docker build --platform=linux/$(ARCH) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--target system-agent \
		-t $(E2E_IMAGE_NAME):$(E2E_IMAGE_TAG) .

.PHONY: test-e2e
test-e2e: $(GINKGO_BIN) e2e-image ## Run e2e tests (builds image and creates Kind cluster)
	@mkdir -p $(E2E_ARTIFACTS)
	cd test && \
	E2E_IMAGE_TAG=$(E2E_IMAGE_TAG) \
	E2E_IMAGE_NAME=$(E2E_IMAGE_NAME) \
	E2E_KIND_CLUSTER_NAME=$(E2E_KIND_CLUSTER_NAME) \
	SKIP_RESOURCE_CLEANUP=$(SKIP_RESOURCE_CLEANUP) \
	$(GINKGO_BIN) -v --trace \
		--tags=e2e \
		--label-filter="$(GINKGO_LABEL_FILTER)" \
		--nodes=$(GINKGO_NODES) \
		--timeout=$(GINKGO_TIMEOUT) \
		--poll-progress-after=$(GINKGO_POLL_PROGRESS_AFTER) \
		--poll-progress-interval=$(GINKGO_POLL_PROGRESS_INTERVAL) \
		--output-dir="$(E2E_ARTIFACTS)" \
		--junit-report="junit.e2e_suite.xml" \
		./e2e/suites/...

##@ Build

.PHONY: build
build: $(BIN_DIR) ## Build the system-agent binary
	@echo "Building rancher-system-agent for $(TARGET_OS)/$(ARCH)..."
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(TARGET_OS) GOARCH=$(ARCH) go build -ldflags "$(LDFLAGS)" -o bin/rancher-system-agent
	@echo "Build complete!"

##@ Docker (Local Development)

.PHONY: docker-build
docker-build: ## Build Docker image locally (no push)
	docker build --platform=linux/$(ARCH) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--target system-agent \
		-t $(IMAGE):$(TAG) .
	@echo "Built $(IMAGE):$(TAG)"

.PHONY: docker-build-suc
docker-build-suc: ## Build SUC Docker image locally (no push)
	docker build --platform=linux/$(ARCH) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--target system-agent-suc \
		-t $(IMAGE):$(TAG)-suc .
	@echo "Built $(IMAGE):$(TAG)-suc)"

##@ Docker (Release - used by CI/CD workflows)

.PHONY: docker-buildx-push
docker-buildx-push: ## Build and push Docker image with buildx (used by release workflow)
	docker buildx build --platform=linux/$(ARCH) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--target system-agent \
		-t $(IMAGE):$(TAG)-$(TAG_SUFFIX) \
		--push .
	@echo "Built and pushed $(IMAGE):$(TAG)-$(TAG_SUFFIX)"

.PHONY: docker-buildx-push-suc
docker-buildx-push-suc: ## Build and push SUC Docker image with buildx (used by release workflow)
	docker buildx build --platform=linux/$(ARCH) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--target system-agent-suc \
		-t $(IMAGE):$(TAG)-$(TAG_SUFFIX)-suc \
		--push .
	@echo "Built and pushed $(IMAGE):$(TAG)-$(TAG_SUFFIX)-suc"

.PHONY: docker-manifest
docker-manifest: ## Create and push multi-platform manifests (used by release workflow)
	@echo "Creating multi-platform manifest for $(IMAGE):$(TAG)"
	docker buildx imagetools create -t "$(IMAGE):$(TAG)" \
		"$(IMAGE):$(TAG)-linux-amd64" \
		"$(IMAGE):$(TAG)-linux-arm64"
	@echo "Creating multi-platform manifest for $(IMAGE):$(TAG)-suc"
	docker buildx imagetools create -t "$(IMAGE):$(TAG)-suc" \
		"$(IMAGE):$(TAG)-linux-amd64-suc" \
		"$(IMAGE):$(TAG)-linux-arm64-suc"
	@echo "Multi-platform manifests pushed successfully"

##@ CI / CD

.PHONY: validate
validate: lint fmt-check vet ## Run validation checks (lint + format check)

##@ Release

.PHONY: release-binaries
release-binaries: $(DIST_DIR) ## Build release binaries for all architectures
	@echo "Building release binaries..."
	GOOS=linux GOARCH=amd64 $(MAKE) build
	cp bin/rancher-system-agent $(DIST_DIR)/rancher-system-agent-amd64
	GOOS=linux GOARCH=arm64 $(MAKE) build
	cp bin/rancher-system-agent $(DIST_DIR)/rancher-system-agent-arm64
	cp install.sh system-agent-uninstall.sh $(DIST_DIR)/
	cd $(DIST_DIR) && sha256sum * > sha256sum.txt
	@echo "Release binaries created in $(DIST_DIR)/"
	@ls -lh $(DIST_DIR)/

##@ Cleanup

.PHONY: clean
clean: ## Clean build artifacts
	rm -rf $(BIN_DIR) $(DIST_DIR)

.PHONY: clean-all
clean-all: clean vendor-clean ## Clean everything including vendor

##@ Hack / Tools

.PHONY: $(GOLANGCI_LINT_BIN)
$(GOLANGCI_LINT_BIN): $(GOLANGCI_LINT) ## Build a local copy of golangci-lint

$(GOLANGCI_LINT): $(TOOLS_BIN_DIR) ## Build golangci-lint from tools folder
	GOOS= GOARCH= GOBIN=$(TOOLS_BIN_DIR) $(GO_INSTALL) $(GOLANGCI_LINT_PKG) $(GOLANGCI_LINT_BIN) $(GOLANGCI_LINT_VER)

$(GINKGO_BIN): $(TOOLS_BIN_DIR) ## Build ginkgo for e2e tests
	GOBIN=$(TOOLS_BIN_DIR) $(GO_INSTALL) $(GINKGO_PKG) ginkgo $(GINKGO_VER)

.PHONY: version
version: ## Display version information
	@echo "VERSION=$(VERSION)"
	@echo "TAG=$(TAG)"
	@echo "COMMIT=$(COMMIT)"
	@echo "ARCH=$(ARCH)"

.DEFAULT_GOAL := all
