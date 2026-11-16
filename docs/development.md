# Development

## Prerequisites

- Go 1.27+
- Podman or Docker for building container images
- A Kubernetes cluster and kubeconfig

## Build

```bash
make build                 # bin/ax, bin/ax-server
make install               # install the ax CLI into $(go env GOPATH)/bin
make images                # build and push the ax-server and ax-task-runner images to $AX_IMAGE_REPO
```

Pushing a release tag makes CI publish the images to
`ghcr.io/google/ax/<image>:<tag>`; see [Release images](release-images.md).

## Test

Runs everything, including the mock Substrate gRPC server, in-memory store validation, and API server tests:

```bash
make test        # or: go test -v ./...
```

## Contributing

See [CONTRIBUTING.md](../CONTRIBUTING.md) for contribution guidelines, CLA requirements, and PR workflows.
