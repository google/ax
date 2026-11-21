# Development

## Prerequisites

- Go 1.27+
- Podman or Docker for building container images
- A Kubernetes cluster and kubeconfig

## Build

```bash
make build                 # bin/ax, bin/ax-server
make install               # install the ax CLI into $(go env GOPATH)/bin
make images                # build and push the ax-server and ax-runner images to $AX_IMAGE_REPO
```

### Build and push images

When developing from source (unversioned dev builds require `--server-image` or `$AX_SERVER_IMAGE` instead of using official release defaults), build and push your images and pin them by digest:

```bash
export AX_IMAGE_REPO=<your-registry>   # e.g. gcr.io/my-project
make images                            # prints the digest of each pushed image
export AX_SERVER_IMAGE="$AX_IMAGE_REPO/ax-server@<digest>"
export AX_TASK_IMAGE="$AX_IMAGE_REPO/ax-runner@<digest>"
ax local start --server-image "$AX_SERVER_IMAGE"
```

For `ax local start`, you can use a registry on `localhost` instead of a remote one: see [Using Locally Built Images](local_installer.md#using-locally-built-images).

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
