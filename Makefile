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
# Image the server runs for tasks that do not set spec.image. Substrate only
# accepts images pinned by digest, so after `make images` this defaults to the
# digest it recorded for the pushed ax-task-runner image.
AX_TASK_RUNNER_DIGEST := $(strip $(shell cat bin/ax-task-runner.digest 2>/dev/null))
AX_DEFAULT_TASK_IMAGE ?= $(if $(AX_TASK_RUNNER_DIGEST),$(AX_IMAGE_REPO)/ax-task-runner@$(AX_TASK_RUNNER_DIGEST),$(AX_TASK_RUNNER_IMAGE))

# Task image with Claude Code preinstalled; see Dockerfile.claude-code.
CLAUDE_CODE_REPO ?= gcr.io/ax-images/ax-images
CLAUDE_CODE_VERSION ?= latest
CLAUDE_CODE_BASE_IMAGE ?=

# Prefer podman, fall back to docker.
CONTAINER_CLI ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null)

.PHONY: build install images check-image-repo build-claude-code-image push-claude-code-image deploy deploy-server deploy-postgres apply-example test test-e2e test-e2e-head e2e-setup e2e-run e2e-cleanup clean

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
	$(call push_image,$(AX_SERVER_IMAGE),ax-server)
	$(call push_image,$(AX_TASK_RUNNER_IMAGE),ax-task-runner)
	@echo ""
	@echo "==> Pushed images. To deploy exactly these builds, run:"
	@echo ""
	@echo "    export AX_SERVER_IMAGE=$(AX_SERVER_IMAGE)@$$(cat bin/ax-server.digest)"
	@echo "    export AX_DEFAULT_TASK_IMAGE=$(AX_TASK_RUNNER_IMAGE)@$$(cat bin/ax-task-runner.digest)"
	@echo ""

# push_image pushes image $(1) and writes its manifest digest to bin/$(2).digest.
# podman reports the exact pushed digest via --digestfile; docker records it in
# the image's RepoDigests after a push.
define push_image
	@echo "==> Pushing $(1)..."
	@if $(CONTAINER_CLI) --version 2>/dev/null | grep -qi podman; then \
		$(CONTAINER_CLI) push --digestfile bin/$(2).digest $(1); \
	else \
		$(CONTAINER_CLI) push $(1) && \
		img="$(1)" && \
		$(CONTAINER_CLI) image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' $(1) | \
			grep "^$${img%:*}@" | head -n1 | sed 's/.*@//' > bin/$(2).digest; \
	fi
endef

# Build a task image with Claude Code preinstalled and claude-code-runner as
# its runner, on top of the default runner image
build-claude-code-image:
	@echo "==> Cross-compiling claude-code-runner for linux/amd64..."
	@mkdir -p bin/linux_amd64
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/linux_amd64/claude-code-runner ./cmd/runners/claude-code-runner
	@echo "==> Building container image $(CLAUDE_CODE_REPO):latest (Claude Code $(CLAUDE_CODE_VERSION)) using $(CONTAINER_CLI)..."
	$(CONTAINER_CLI) build --platform linux/amd64 --build-arg CLAUDE_CODE_VERSION=$(CLAUDE_CODE_VERSION) $(if $(CLAUDE_CODE_BASE_IMAGE),--build-arg BASE_IMAGE=$(CLAUDE_CODE_BASE_IMAGE),) -t $(CLAUDE_CODE_REPO):latest -f Dockerfile.claude-code .

# Push the Claude Code task image to the registry and print the digest to pin in spec.image
push-claude-code-image: build-claude-code-image
	@mkdir -p bin
	$(call push_image,$(CLAUDE_CODE_REPO):latest,claude-code)
	@echo "==> Pushed digest: $(CLAUDE_CODE_REPO)@$$(cat bin/claude-code.digest)"

## --------------------------------------
## Deployment Targets
## --------------------------------------

# Deploy all AX components to Kubernetes (Postgres, ax-server). Run `make images` first.
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

deploy-server: check-image-repo
	@echo "==> Deploying ax-server ($(AX_SERVER_IMAGE))..."
	sed -e 's|$${AX_SERVER_IMAGE}|$(AX_SERVER_IMAGE)|g' \
		-e 's|$${AX_DEFAULT_TASK_IMAGE}|$(AX_DEFAULT_TASK_IMAGE)|g' \
		deploy/ax-server.yaml | kubectl apply -f -

# Apply example task and resources
apply-example:
	@echo "==> Applying example task and resources using bin/ax..."
	./bin/ax apply -f examples/simple.yaml

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
