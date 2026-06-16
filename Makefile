.PHONY: all build build-harness proto test clean install install-harness install-ate images

# Default container registry for docker
export KO_DOCKER_REPO ?= gcr.io/ax-container-images

# Build all binaries
all: proto build

# Build binaries
build:
	@echo "Building ax..."
	@mkdir -p bin
	@go build -o bin/ax ./cmd/ax
	@echo "Building remote agent example..."
	@go build -o bin/remote_agent ./examples/remote_agent
	@echo "Build complete!"

# (Dev-only) Build ax with the `harness` build tag.
build-harness:
	@echo "Building ax (harness path)..."
	@mkdir -p bin
	@go build -tags harness -o bin/ax ./cmd/ax
	@echo "Build complete!"

# Generate protobuf code
proto:
	@echo "Generating protobuf code..."
	@export PATH=$$PATH:$$(go env GOPATH)/bin && \
		protoc --go_out=. --go_opt=paths=source_relative \
		       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
		       proto/ax.proto proto/content.proto
	@echo "Protobuf generation complete!"

# Run tests
test:
	@echo "Running tests..."
	@go test -v ./...
	@echo "Running tests (harness path)..."
	@go test -v -tags harness ./...

# Clean build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf bin/
	@rm -rf eventlog/
	@echo "Clean complete!"

# Install ax to GOPATH/bin
install:
	@echo "Installing ax..."
	@go install ./cmd/ax
	@echo "Install complete!"

# Install ax with the `harness` build tag.
install-harness:
	@echo "Installing ax (harness path)..."
	@go install -tags harness ./cmd/ax
	@echo "Install complete!"

# Install ax with ATE support to GOPATH/bin
install-ate:
	@echo "Installing ax with ATE support..."
	@go install -tags ate ./cmd/ax
	@echo "Install complete!"


# Run remote agent example
run-remote:
	@go run ./examples/remote_agent

# Install dependencies
deps:
	@echo "Installing dependencies..."
	@go mod download
	@go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	@echo "Dependencies installed!"

clean-logs:
	@echo "Cleaning the event logs..."
	rm -rf ./eventlog
	mkdir ./eventlog

ax-image:
	@echo "Building container image with ko..."
	ko build --base-import-paths ./cmd/ax

ax-server-image:
	@echo "Building ax-server (harness path) container image with ko..."
	GOFLAGS="-tags=harness" ko build --base-import-paths ./cmd/ax

axepp-image:
	@echo "Building axepp container image with ko..."
	ko build --base-import-paths ./cmd/axepp

ax-shell-image:
	# Used to debug ax servers within a cluster.
	@echo "Building ax shell container image with ko using busybox..."
	KO_DOCKER_REPO=$(KO_DOCKER_REPO)/ax-shell KO_DEFAULTBASEIMAGE=busybox:1.36 ko build --base-import-paths ./cmd/ax

# Build all container images
images: ax-image axepp-image ax-shell-image
