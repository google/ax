.PHONY: all build proto test test-python clean install

# Build all binaries
all: proto build

# Build binaries
build:
	@echo "Building ax..."
	@mkdir -p bin
	@go build -o bin/ax ./cmd/ax
	@echo "Build complete!"


# Generate protobuf code
proto:
	@echo "Generating protobuf code..."
	@export PATH=$$PATH:$$(go env GOPATH)/bin && \
		protoc --go_out=. --go_opt=paths=source_relative \
		       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
		       proto/ax.proto proto/content.proto
	@python3 -m grpc_tools.protoc -I. --python_out=python --grpc_python_out=python proto/ax.proto proto/content.proto
	@echo "Protobuf generation complete!"

# Run Go tests
test:
	@echo "Running Go tests..."
	@go test -v ./...

# Run Python tests for the antigravity harness sidecar.
# Assumes deps are installed for the same interpreter as `python3`, e.g.:
#   python3 -m pip install -r python/antigravity/requirements.txt \
#     'pytest>=7.0' 'pytest-timeout>=2.0'
# --timeout guards against hung gRPC servers.
test-python:
	@echo "Running Python tests..."
	@python3 -m pytest python/antigravity/ --timeout=30 --timeout-method=thread

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
