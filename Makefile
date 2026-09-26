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
AX_IMAGE_REPO ?= gcr.io/ax-substrate/ate-images
TASK_RUNNER_REPO ?= $(AX_IMAGE_REPO)/ax-task-runner
CONTAINER_CLI ?= $(shell which podman 2>/dev/null || which docker 2>/dev/null)

.PHONY: all build build-binaries build-task-runner install push push-task-runner deploy deploy-server deploy-redis apply-example test clean

all: build

## --------------------------------------
## Build Targets
## --------------------------------------

# Build all local binaries (ax CLI, server)
build: build-binaries

build-binaries:
	@echo "==> Building local binaries (ax, ax-server)..."
	@mkdir -p bin
	go build -trimpath -ldflags="-s -w" -o bin/ax ./cmd/ax
	go build -trimpath -ldflags="-s -w" -o bin/ax-server ./cmd/ax-server

# Install the ax CLI into $(go env GOPATH)/bin
install:
	@echo "==> Installing ax CLI to $$(go env GOPATH)/bin..."
	go install -trimpath -ldflags="-s -w" ./cmd/ax

# Cross-compile ax-task-runner for Linux amd64 and build container image with Python, Antigravity, and git/curl
build-task-runner:
	@echo "==> Cross-compiling ax-task-runner for linux/amd64..."
	@mkdir -p bin/linux_amd64
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/linux_amd64/ax-task-runner ./cmd/ax-task-runner
	@echo "==> Building container image $(TASK_RUNNER_REPO):latest using $(CONTAINER_CLI)..."
	$(CONTAINER_CLI) build --platform linux/amd64 -t $(TASK_RUNNER_REPO):latest -f Dockerfile.task-runner .

# Push task-runner container image to registry
push-task-runner: build-task-runner
	@echo "==> Pushing task runner image to $(TASK_RUNNER_REPO):latest..."
	$(CONTAINER_CLI) push $(TASK_RUNNER_REPO):latest
	@echo "==> Current pushed digest:"
	@gcloud container images list-tags $(TASK_RUNNER_REPO) --filter="tags=latest" --format="get(digest)"

# Build and push all images
push: push-task-runner

## --------------------------------------
## Deployment Targets
## --------------------------------------

# Deploy all AX components to Kubernetes (Redis, ax-server)
deploy: deploy-redis deploy-server

deploy-redis:
	@echo "==> Deploying Redis to ax-system namespace..."
	kubectl apply -f deploy/redis.yaml

deploy-server:
	@echo "==> Building and deploying ax-server using ko..."
	KO_DOCKER_REPO=$(AX_IMAGE_REPO) ko apply -f deploy/ax-server.yaml

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

clean:
	@echo "==> Cleaning build artifacts..."
	rm -rf bin/
