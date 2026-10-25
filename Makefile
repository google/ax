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

.PHONY: all build build-binaries build-task-runner install push push-task-runner deploy deploy-server deploy-postgres apply-example test test-e2e test-e2e-head e2e-setup e2e-run e2e-cleanup clean

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

# Deploy all AX components to Kubernetes (Postgres, ax-server)
deploy: deploy-postgres deploy-server

deploy-postgres:
	@echo "==> Deploying Postgres to ax-system namespace..."
	kubectl create namespace ax-system --dry-run=client -o yaml | kubectl apply -f -
	@# Create the password once. Postgres only reads it when initializing an
	@# empty volume, so it must not change on later deploys.
	@kubectl -n ax-system get secret ax-postgres > /dev/null 2>&1 || \
		kubectl -n ax-system create secret generic ax-postgres \
			--from-literal=password="$$(openssl rand -hex 24)"
	kubectl apply -f deploy/postgres.yaml
	kubectl -n ax-system rollout status statefulset/ax-postgres --timeout=180s

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
