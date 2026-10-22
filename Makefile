# Copyright 2026 Google LLC
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

SHELL := /bin/bash

# Configuration
#
# AX_IMAGE_REPO is required by `make images` and `make deploy`: it is the
# registry the images are pushed to and pulled from, e.g.
# AX_IMAGE_REPO=gcr.io/my-project.
AX_IMAGE_REPO ?=
IMAGE_TAG ?= latest
IMAGE_ARCH ?= amd64
AX_SERVER_IMAGE ?= $(AX_IMAGE_REPO)/ax-server:$(IMAGE_TAG)
AX_TASK_RUNNER_IMAGE ?= $(AX_IMAGE_REPO)/ax-task-runner:$(IMAGE_TAG)

# Prefer podman, fall back to docker.
CONTAINER_CLI ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null)

.PHONY: build install images check-image-repo deploy deploy-server deploy-redis apply-example test test-e2e test-e2e-head e2e-setup e2e-run e2e-cleanup clean

## --------------------------------------
## Build Targets
## --------------------------------------

# Build all local binaries (ax CLI, server)
build:
	@echo "==> Building local binaries (ax, ax-server)..."
	@mkdir -p bin
	go build -trimpath -ldflags="-s -w" -o bin/ax ./cmd/ax
	go build -trimpath -ldflags="-s -w" -o bin/ax-server ./cmd/ax-server

# Install the ax CLI into $(go env GOPATH)/bin
install:
	@echo "==> Installing ax CLI to $$(go env GOPATH)/bin..."
	go install -trimpath -ldflags="-s -w" ./cmd/ax

check-image-repo:
	@if [ -z "$(AX_IMAGE_REPO)" ]; then \
		echo "error: AX_IMAGE_REPO is not set (e.g. AX_IMAGE_REPO=gcr.io/my-project)"; exit 1; \
	fi

# Cross-compile ax-server and ax-task-runner for linux/$(IMAGE_ARCH), then build
# and push their container images.
images: check-image-repo
	@if [ -z "$(CONTAINER_CLI)" ]; then \
		echo "error: neither podman nor docker found in PATH (or set CONTAINER_CLI)"; exit 1; \
	fi
	@echo "==> Cross-compiling ax-server and ax-task-runner for linux/$(IMAGE_ARCH)..."
	@mkdir -p bin/linux_$(IMAGE_ARCH)
	GOOS=linux GOARCH=$(IMAGE_ARCH) CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/linux_$(IMAGE_ARCH)/ax-server ./cmd/ax-server
	GOOS=linux GOARCH=$(IMAGE_ARCH) CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/linux_$(IMAGE_ARCH)/ax-task-runner ./cmd/ax-task-runner
	@echo "==> Building $(AX_SERVER_IMAGE) using $(CONTAINER_CLI)..."
	$(CONTAINER_CLI) build --platform linux/$(IMAGE_ARCH) --build-arg TARGETARCH=$(IMAGE_ARCH) -t $(AX_SERVER_IMAGE) -f Dockerfile.server .
	@echo "==> Building $(AX_TASK_RUNNER_IMAGE) using $(CONTAINER_CLI)..."
	$(CONTAINER_CLI) build --platform linux/$(IMAGE_ARCH) --build-arg TARGETARCH=$(IMAGE_ARCH) -t $(AX_TASK_RUNNER_IMAGE) -f Dockerfile.task-runner .
	@echo "==> Pushing $(AX_SERVER_IMAGE)..."
	$(CONTAINER_CLI) push $(AX_SERVER_IMAGE)
	@echo "==> Pushing $(AX_TASK_RUNNER_IMAGE)..."
	$(CONTAINER_CLI) push $(AX_TASK_RUNNER_IMAGE)

## --------------------------------------
## Deployment Targets
## --------------------------------------

# Deploy all AX components to Kubernetes (Redis, ax-server). Run `make images` first.
deploy: deploy-redis deploy-server

deploy-redis:
	@echo "==> Deploying Redis to ax-system namespace..."
	kubectl apply -f deploy/redis.yaml

deploy-server: check-image-repo
	@echo "==> Deploying ax-server ($(AX_SERVER_IMAGE))..."
	sed -e 's|$${AX_SERVER_IMAGE}|$(AX_SERVER_IMAGE)|g' \
		-e 's|$${AX_DEFAULT_TASK_IMAGE}|$(AX_TASK_RUNNER_IMAGE)|g' \
		deploy/ax-server.yaml | kubectl apply -f -

# Apply example task and resources
apply-example:
	@echo "==> Applying example task and resources using bin/ax..."
	./bin/ax apply -f examples/task.yaml

## --------------------------------------
## Test & Verification Targets
## --------------------------------------

test:
	@echo "==> Running tests..."
	go test -v ./...

# Provision Kind + Substrate + AX, then run the E2E suite. Substrate defaults to
# the version in go.mod; override with SUBSTRATE_REF=<branch|tag|commit>.
# Pass extra `go test` flags via E2E_ARGS, e.g. E2E_ARGS='-test.run TestE2E_ModelCRUD'.
test-e2e:
	./hack/run-e2e-kind.sh $(E2E_ARGS)

# Run the E2E suite against Substrate main.
test-e2e-head:
	SUBSTRATE_REF=main ./hack/run-e2e-kind.sh $(E2E_ARGS)

# Provision Kind + Substrate + AX without running tests (accepts SUBSTRATE_REF).
e2e-setup:
	./hack/setup-e2e-kind.sh

# Run the E2E suite against a cluster provisioned by e2e-setup.
e2e-run:
	./hack/run-e2e.sh $(E2E_ARGS)

# Delete the Kind E2E cluster.
e2e-cleanup:
	./hack/cleanup-e2e-kind.sh

clean:
	@echo "==> Cleaning build artifacts..."
	rm -rf bin/ .tmp/
