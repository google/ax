# Developer Utility Tool (`hack/`)

This directory contains the unified AX developer CLI utility tool `ax-dev.sh` to compile, run, and deploy the AX Orchestrator and its harnesses.

---

## Unified Command: `ax-dev.sh`

The `ax-dev.sh` script consolidates all local running, E2E testing, and Kubernetes/SubstrATE cluster deployments.

### 1. Local Development Mode (`local`)
Starts the Python harness, AX Orchestrator, and the AX Monitor Dashboard, launching the visual web interface automatically in a single terminal session.

- **What it does**:
  1. Compiles the Go `ax` CLI with `-tags harness`.
  2. Starts the **Python gRPC Harness Server** in the background on port `50053`.
  3. Waits for the harness to become healthy.
  4. Starts the **AX Orchestrator Server** (`ax serve`) in the background on port `8494`.
  5. Waits for the orchestrator to become healthy.
  6. Starts the **AX Monitor Dashboard** (`ax monitor`) in the foreground, opening your web browser to the dashboard interface (`http://localhost:8080`).
  7. On exit or `Ctrl+C`, cleans up all background processes gracefully.

- **Usage**:
  ```bash
  export GEMINI_API_KEY="your-gemini-api-key"
  ./hack/ax-dev.sh local
  ```

---

### 2. Local E2E Integration Tests (`test`)
Runs local end-to-end integration tests using the stateful Python harness and Go E2E test client.

- **What it does**:
  1. Boots the Python gRPC server in the background.
  2. Compiles the Go E2E client binary (`cmd/e2e/main.go`).
  3. Runs the E2E verification test suite.
  4. Automatically cleans up the background server on exit.

- **Usage**:
  ```bash
  export GEMINI_API_KEY="your-gemini-api-key"
  ./hack/ax-dev.sh test
  ```

---

### 3. Cloud / SubstrATE Deployment Mode (`cloud`)
Deploys or deletes the AX Orchestrator server resources on a SubstrATE-enabled Kubernetes cluster.

- **Commands**:
  - **Deploy to Cluster**:
    ```bash
    export GEMINI_API_KEY="your-gemini-api-key"
    export BUCKET_NAME="your-gcs-bucket-name"
    ./hack/ax-dev.sh cloud deploy
    ```
  - **Tear Down / Delete**:
    ```bash
    ./hack/ax-dev.sh cloud delete
    ```
