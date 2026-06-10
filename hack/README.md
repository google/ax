# Helper Scripts (`hack/`)

This directory contains utility scripts to install, run, and test AX Orchestrator and its agent harnesses.

## Available Scripts

### 1. `run-ax-environment.sh`
This script boots the complete local development environment in a single terminal session.

- **What it does**:
  1. Compiles the Go `ax` CLI with `-tags harness`.
  2. Starts the **Python gRPC Harness Server** in the background on port `50053`.
  3. Waits for the harness to become healthy.
  4. Starts the **AX Orchestrator Server** (`ax serve`) in the background on port `8494`.
  5. Waits for the orchestrator to become healthy.
  6. Runs the **AX Monitor Dashboard** (`ax monitor`) in the foreground, opening your web browser to the dashboard interface (`http://localhost:8080`).
  7. On exit or `Ctrl+C`, cleans up all background processes gracefully.

- **Usage**:
  ```bash
  export GEMINI_API_KEY="your-gemini-api-key"
  ./hack/run-ax-environment.sh
  ```

---

### 2. `run-antigravity-streaming.sh`
This script executes a local E2E test turn against a persistent gRPC harness.

- **What it does**:
  1. Boots the Python gRPC server in the background.
  2. Compiles the Go E2E client binary (`cmd/e2e/main.go`).
  3. Runs the E2E verification test suite.
  4. Automatically cleans up the background server on exit.

- **Usage**:
  ```bash
  export GEMINI_API_KEY="your-gemini-api-key"
  ./hack/run-antigravity-streaming.sh
  ```

---

### 3. `install-ax.sh`
This script deploys the AX server components to a Kubernetes/SubstrATE cluster.

- **What it does**:
  - Leverages `ko` to build and deploy AX server containers.
  - Automatically handles namespace and dependency setups.

- **Usage**:
  - **Deploy to Cluster**:
    ```bash
    ./hack/install-ax.sh --deploy-ax-server
    ```
  - **Tear Down**:
    ```bash
    ./hack/install-ax.sh --delete-ax-server
    ```
