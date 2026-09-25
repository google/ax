# How to Contribute

We welcome contributions to AX!

## Before You Begin

### Sign our Contributor License Agreement

All submissions to this project need to follow Google’s [Contributor License Agreement (CLA)](https://cla.developers.google.com/about), which covers any original work of authorship included in the submission. This doesn't prohibit the use of coding assistance tools, including tool-, AI-, or machine-generated code, as long as these submissions abide by the CLA's requirements.

You (or your employer) retain the copyright to your contribution; this simply gives us permission to use and redistribute your contributions as part of the project.

If you or your current employer have already signed the Google CLA (even if it was for a different project), you probably don't need to do it again. Visit <https://cla.developers.google.com/> to see your current agreements or sign a new one.

### Community Guidelines

This project follows [Google's Open Source Community Guidelines](https://opensource.google/conduct/).

### Code Reviews

All submissions, including submissions by project members, require review. We use GitHub pull requests for this purpose. Consult [GitHub Help](https://help.github.com/articles/about-pull-requests/) for more information on using pull requests.

---

## Development Workflow

### Prerequisites

- **Go 1.27+**
- **[`ko`](https://ko.build/)** (for building and deploying control plane images)
- **Docker** or **Podman** (for building the Linux task-runner container image)
- A Kubernetes cluster with [Agent Substrate](https://github.com/agent-substrate/substrate) installed

### Building Binaries

Build all local binaries (`bin/ax`, `bin/ax-controller`, `bin/ax-server`):

```bash
make build
```

Or install the `ax` CLI directly into `$(go env GOPATH)/bin`:

```bash
make install
```

Make sure that `$(go env GOPATH)/bin` is on your `$PATH`:

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
```

### Running Tests

Run all unit and integration tests (including the mock Substrate gRPC server and API server tests):

```bash
make test
```

Or run via `go test`:

```bash
go test -v ./...
```

### Building the Task Runner Image

Cross-compile the task runner for `linux/amd64` and package the container image:

```bash
make build-task-runner
```

To build and push to a remote container registry:

```bash
make push-task-runner TASK_RUNNER_REPO=gcr.io/<your-project>/ax-task-runner
```

### Deploying to Kubernetes

Deploy Redis, controller, and server components to your cluster in the `ax-system` namespace:

```bash
make deploy AX_IMAGE_REPO=gcr.io/<your-project>/ax-images
```

---

## Creating a Pull Request

1. **Fork and Clone the Repository**:
   Fork [google/ax](https://github.com/google/ax) on GitHub, then clone your fork. Replace `YOUR_USERNAME` with your GitHub username:
   ```bash
   git clone git@github.com:YOUR_USERNAME/ax.git
   cd ax
   git remote add upstream git@github.com:google/ax.git
   ```

2. **Ensure `main` is up to date**:
   ```bash
   git fetch upstream
   git checkout main
   git merge --ff-only upstream/main
   ```

3. **Create a feature branch**:
   ```bash
   git checkout -b my-feature
   ```

4. **Make changes and verify**:
   - Run tests: `make test`
   - Ensure clean modules: `go mod tidy` and verify with `git diff --exit-code go.mod go.sum`
   - Build binaries: `make build`

5. **Commit and open a PR**:
   ```bash
   git add .
   git commit -m "feat: describe your changes"
   git push -u origin my-feature
   ```
   Open a pull request from your fork's `my-feature` branch to `google/ax`'s `main` branch, describing the motivation and changes.
