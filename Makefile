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
AX_RUNNER_IMAGE ?= $(AX_IMAGE_REPO)/ax-runner:$(IMAGE_TAG)
# Image holding only the runner binary, mounted read-only into task containers;
# see Dockerfile.runner-volume.
AX_RUNNER_VOLUME_BUILD_IMAGE ?= $(AX_IMAGE_REPO)/ax-runner-volume:$(IMAGE_TAG)
# Digest-pinned runner volume image that `make deploy` configures ax-server to
# mount into task containers. Empty (the default) keeps running the runner baked
# into each task image.
AX_RUNNER_VOLUME_IMAGE ?=

# Prefer podman, fall back to docker.
CONTAINER_CLI ?= $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null)

BUF := go tool -modfile=hack/tools/buf/go.mod buf
GOLANGCI_LINT := go tool -modfile=hack/tools/golangci-lint/go.mod golangci-lint
VALIDATION_GEN := go tool -modfile=hack/tools/code-generator/go.mod validation-gen

.PHONY: build install images push-runner-volume check-image-repo generate deploy deploy-server deploy-postgres apply-example verify test test-e2e test-local-install build-local-cluster e2e-setup e2e-run e2e-cleanup clean

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

# Cross-compile ax-server and ax-runner for linux/$(IMAGE_ARCH), then build
# and push their container images.
images: check-image-repo
	@if [ -z "$(CONTAINER_CLI)" ]; then \
		echo "error: neither podman nor docker found in PATH (or set CONTAINER_CLI)"; exit 1; \
	fi
	@echo "==> Cross-compiling ax-server and ax-runner for linux/$(IMAGE_ARCH)..."
	@mkdir -p bin/linux_$(IMAGE_ARCH)
	GOOS=linux GOARCH=$(IMAGE_ARCH) CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/linux_$(IMAGE_ARCH)/ax-server ./cmd/ax-server
	GOOS=linux GOARCH=$(IMAGE_ARCH) CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/linux_$(IMAGE_ARCH)/ax-runner ./cmd/ax-runner
	@echo "==> Building $(AX_SERVER_IMAGE) using $(CONTAINER_CLI)..."
	$(CONTAINER_CLI) build --platform linux/$(IMAGE_ARCH) --build-arg TARGETARCH=$(IMAGE_ARCH) -t $(AX_SERVER_IMAGE) -f Dockerfile.server .
	@echo "==> Building $(AX_RUNNER_IMAGE) using $(CONTAINER_CLI)..."
	$(CONTAINER_CLI) build --platform linux/$(IMAGE_ARCH) --build-arg TARGETARCH=$(IMAGE_ARCH) -t $(AX_RUNNER_IMAGE) -f Dockerfile.runner .
	@echo "==> Building $(AX_RUNNER_VOLUME_BUILD_IMAGE) using $(CONTAINER_CLI)..."
	$(CONTAINER_CLI) build --platform linux/$(IMAGE_ARCH) --build-arg TARGETARCH=$(IMAGE_ARCH) -t $(AX_RUNNER_VOLUME_BUILD_IMAGE) -f Dockerfile.runner-volume .
	$(call push_image,$(AX_SERVER_IMAGE),ax-server)
	$(call push_image,$(AX_RUNNER_IMAGE),ax-runner)
	$(call push_image,$(AX_RUNNER_VOLUME_BUILD_IMAGE),ax-runner-volume)
	@echo ""
	@echo "==> Pushed images. To deploy exactly these builds, run:"
	@echo ""
	@echo "    export AX_SERVER_IMAGE=$(AX_SERVER_IMAGE)@$$(cat bin/ax-server.digest)"
	@echo "    export AX_RUNNER_VOLUME_IMAGE=$(AX_RUNNER_VOLUME_BUILD_IMAGE)@$$(cat bin/ax-runner-volume.digest)"
	@echo ""
	@echo "==> Tasks run ax-runner by setting spec.profile.image to:"
	@echo ""
	@echo "    $(AX_RUNNER_IMAGE)@$$(cat bin/ax-runner.digest)"
	@echo ""

# Build and push only the runner volume image (see Dockerfile.runner-volume)
# and print the digest to pin.
push-runner-volume: check-image-repo
	@if [ -z "$(CONTAINER_CLI)" ]; then \
		echo "error: neither podman nor docker found in PATH (or set CONTAINER_CLI)"; exit 1; \
	fi
	@mkdir -p bin/linux_$(IMAGE_ARCH)
	GOOS=linux GOARCH=$(IMAGE_ARCH) CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/linux_$(IMAGE_ARCH)/ax-runner ./cmd/ax-runner
	$(CONTAINER_CLI) build --platform linux/$(IMAGE_ARCH) --build-arg TARGETARCH=$(IMAGE_ARCH) -t $(AX_RUNNER_VOLUME_BUILD_IMAGE) -f Dockerfile.runner-volume .
	$(call push_image,$(AX_RUNNER_VOLUME_BUILD_IMAGE),ax-runner-volume)
	@echo "==> Pushed digest: $(AX_RUNNER_VOLUME_BUILD_IMAGE)@$$(cat bin/ax-runner-volume.digest)"

# push_image pushes image $(1) and writes its manifest digest to bin/$(2).digest.
# podman reports the exact pushed digest via --digestfile; docker records it in
# the image's RepoDigests after a push. A registry on localhost or 127.0.0.1
# (see `make build-local-cluster`) serves plain HTTP: docker allows that by
# default, podman only with --tls-verify=false.
define push_image
	@echo "==> Pushing $(1)..."
	@if $(CONTAINER_CLI) --version 2>/dev/null | grep -qi podman; then \
		case "$(1)" in localhost[:/]*|127.0.0.1[:/]*) tls="--tls-verify=false" ;; *) tls="" ;; esac; \
		$(CONTAINER_CLI) push $$tls --digestfile bin/$(2).digest $(1); \
	else \
		$(CONTAINER_CLI) push $(1) && \
		img="$(1)" && \
		$(CONTAINER_CLI) image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' $(1) | \
			grep "^$${img%:*}@" | head -n1 | sed 's/.*@//' > bin/$(2).digest; \
	fi
endef

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
		-e 's|$${AX_RUNNER_VOLUME_IMAGE}|$(AX_RUNNER_VOLUME_IMAGE)|g' \
		deploy/ax-server.yaml | kubectl apply -f -

# Apply example task and resources
apply-example:
	@echo "==> Applying example task and resources using bin/ax..."
	./bin/ax apply -f examples/simple.yaml

## --------------------------------------
## API Targets
## --------------------------------------

# Format and lint .proto files, and regenerate the Go code: the types, then the
# validation that their +k8s: tags declare. buf reads imported Agent Substrate
# protos from the Go module cache.
generate:
	go mod download go.ate.dev/substrate
	mkdir -p .tmp/buf
	ln -sfn "$$(go list -m -f '{{.Dir}}' go.ate.dev/substrate)" .tmp/buf/substrate
	$(BUF) format -w --path pkg/apis
	$(BUF) lint --path pkg/apis
	$(BUF) generate --path pkg/apis
	$(VALIDATION_GEN) \
		--go-header-file=hack/boilerplate/go.txt \
		--output-file=zz_generated.validation.go \
		--readonly-pkg=google.golang.org/protobuf/types/known/durationpb \
		--readonly-pkg=google.golang.org/protobuf/types/known/structpb \
		--readonly-pkg=google.golang.org/protobuf/types/known/timestamppb \
		./internal/apivalidation

## --------------------------------------
## Test & Verification Targets
## --------------------------------------

# Presubmit checks. Updates generated code, go.mod and formatting in place; CI
# runs this and then requires a clean tree.
verify: generate
	go mod tidy
	go fmt ./...
	$(GOLANGCI_LINT) run ./...

test:
	@echo "==> Running tests..."
	go test -v ./...

# Provision Kind + Substrate + AX, then run the E2E suite. Substrate is installed
# from its published release images: the version in go.mod, or SUBSTRATE_REF=<release tag>.
# Pass extra `go test` flags via E2E_ARGS, e.g. E2E_ARGS='-test.run TestE2E_ModelCRUD'.
test-e2e:
	./hack/run-e2e-kind.sh $(E2E_ARGS)

# Provision Kind + Substrate + AX without running tests (accepts SUBSTRATE_REF).
e2e-setup:
	./hack/setup-e2e-kind.sh

# Run the E2E suite against a cluster provisioned by e2e-setup.
e2e-run:
	./hack/run-e2e.sh $(E2E_ARGS)

# Delete the Kind E2E cluster.
e2e-cleanup:
	./hack/cleanup-e2e-kind.sh

# Smoke test of `ax local start`: build the images into a registry on
# localhost, start a local cluster from them, and run a task. It deletes the
# cluster on exit.
test-local-install:
	./hack/test-local-install.sh

# Build ax and the AX images from this checkout into a registry on localhost
# (podman or docker) and start the local cluster from them, or roll the new
# ax-server out to the running one. Keeps the cluster and the registry; set
# AX_LOCAL_CLUSTER to use another cluster than ax-local.
build-local-cluster:
	CONTAINER_CLI=$(CONTAINER_CLI) ./hack/build-local-cluster.sh

clean:
	@echo "==> Cleaning build artifacts..."
	rm -rf bin/ .tmp/
