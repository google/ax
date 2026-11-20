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

## Test

Runs everything, including the mock Substrate gRPC server, in-memory store validation, and API server tests:

```bash
make test        # or: go test -v ./...
```

Smoke test of `ax local start` on a real Kind cluster, with images built into a registry on `localhost` (needs Docker; about 3 minutes):

```bash
make test-local-install
```

## Contributing

See [CONTRIBUTING.md](../CONTRIBUTING.md) for contribution guidelines, CLA requirements, and PR workflows.
