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
  * **Standard Deployment**:
    ```bash
    export GEMINI_API_KEY="your-gemini-api-key"
    export BUCKET_NAME="your-gcs-bucket-name"
    ./hack/ax-dev.sh cloud deploy
    ```
  * **Harness E2E Path Deployment (V2 Experimental)**:
    Deploys AX server along with isolated warm harness worker pools (`antigravity` and `hello-world` actors):
    ```bash
    export GEMINI_API_KEY="your-gemini-api-key"
    export BUCKET_NAME="your-gcs-bucket-name"
    export KO_DOCKER_REPO="gcr.io/your-project-id/ate-images"
    export KO_DEFAULTPLATFORMS="linux/amd64"
    ./hack/ax-dev.sh cloud deploy --harness
    ```
  * **Tear Down / Delete**:
    ```bash
    ./hack/ax-dev.sh cloud delete [--harness]
    ```

---

### 4. Running E2E Harness Tests on Cloud / GKE
To test the deployed AX + SubstrATE + Harness stack end-to-end:

1. **Port-Forward AX Server**:
   ```bash
   kubectl port-forward -n ax rs/ax-server 8494:8494
   ```
2. **Execute request targeting local tunnel**:
   Compile the local CLI and execute the plan request (it will launch the default `antigravity` harness actor on SubstrATE automatically):
   ```bash
   go build -o bin/ax ./cmd/ax
   ./bin/ax exec --server localhost:8494 --input "hello"
   ```
