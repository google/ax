#!/bin/bash
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

set -e

COLOR_CYAN='\033[1;36m'
COLOR_RED='\033[1;31m'
COLOR_RESET='\033[0m'

log_step() {
  echo -e "${COLOR_CYAN}[step]: $1${COLOR_RESET}"
}

log_error() {
  echo -e "${COLOR_RED}ERROR: $1${COLOR_RESET}" >&2
}

usage() {
  echo "AX Developer Utility Tool"
  echo ""
  echo "Usage: $0 <command> [options]"
  echo ""
  echo "Commands:"
  echo "  local          Start the local AX development environment (Harness + Server + Monitor Dashboard)"
  echo "  test           Run local end-to-end integration tests (Harness + E2E client)"
  echo "  cloud deploy   Deploy AX server and harnesses to a SubstrATE Kubernetes cluster"
  echo "  cloud delete   Tear down AX server and harnesses from a SubstrATE Kubernetes cluster"
  echo ""
  echo "Cloud Options:"
  echo "  --harness, --v2   Target the experimental harness-path configuration (ax-deployment2.yaml)"
  echo ""
  echo "Options:"
  echo "  -h, --help     Show this help message"
}

# Find appropriate Python virtualenv or fallback to python3
resolve_python() {
  if [ -f ".venv/bin/python" ]; then
    echo ".venv/bin/python"
  elif [ -f "/Users/anjalisridhar/ax/.venv/bin/python" ]; then
    echo "/Users/anjalisridhar/ax/.venv/bin/python"
  else
    echo "python3"
  fi
}

check_gemini_key() {
  if [ -z "$GEMINI_API_KEY" ]; then
    log_error "GEMINI_API_KEY environment variable is not set."
    echo "Please set it using: export GEMINI_API_KEY=\"your-key\""
    exit 1
  fi
}

# Wait for a local TCP port to open
wait_for_port() {
  local port=$1
  local name=$2
  local max_attempts=30
  local attempt=1
  while [ $attempt -le $max_attempts ]; do
    if nc -z localhost "$port"; then
      return 0
    fi
    sleep 0.2
    attempt=$((attempt + 1))
  done
  log_error "$name failed to bind to port $port."
  return 1
}

run_local() {
  check_gemini_key
  
  local port=50053
  local agent_file="examples/antigravity_agent/agent.py"
  local config_file="internal/ax2.yaml"
  local python_bin
  python_bin=$(resolve_python)

  if [ ! -f "bin/ax" ]; then
    log_step "Building AX CLI..."
    go build -tags harness -o bin/ax ./cmd/ax
  fi

  local server_pid=""
  local ax_pid=""

  cleanup() {
    echo ""
    log_step "Shutting down local AX environment..."
    if [ -n "$server_pid" ]; then
      kill "$server_pid" 2>/dev/null || true
    fi
    if [ -n "$ax_pid" ]; then
      kill "$ax_pid" 2>/dev/null || true
    fi
    wait "$server_pid" 2>/dev/null || true
    wait "$ax_pid" 2>/dev/null || true
    log_step "Shutdown complete!"
  }
  trap cleanup EXIT

  log_step "Starting Python gRPC Harness Server on port $port..."
  PYTHONPATH=python:. "$python_bin" -m python.antigravity.harness_server --agent_file "$agent_file" --port "$port" > /tmp/antigravity_harness.log 2>&1 &
  server_pid=$!

  log_step "Waiting for Python Harness Server to become healthy..."
  if ! wait_for_port "$port" "Python Harness Server"; then
    cat /tmp/antigravity_harness.log
    exit 1
  fi
  echo "Python Harness Server is active!"

  log_step "Starting AX Orchestrator Server (ax serve)..."
  ./bin/ax serve --config "$config_file" > /tmp/ax_serve.log 2>&1 &
  ax_pid=$!

  log_step "Waiting for AX Orchestrator to bind on port 8494..."
  if ! wait_for_port 8494 "AX Orchestrator"; then
    cat /tmp/ax_serve.log
    exit 1
  fi
  echo "AX Orchestrator is active!"

  log_step "Starting AX Monitor Dashboard (ax monitor)..."
  echo "Press Ctrl+C to terminate all services."
  ./bin/ax monitor --config "$config_file" --addr localhost:8080
}

run_test() {
  check_gemini_key

  local port=50053
  local agent_file="examples/antigravity_agent/agent.py"
  local python_bin
  python_bin=$(resolve_python)

  local server_pid=""

  cleanup() {
    if [ -n "$server_pid" ]; then
      log_step "Killing Python server (PID: $server_pid)..."
      kill "$server_pid" 2>/dev/null || true
      wait "$server_pid" 2>/dev/null || true
    fi
  }
  trap cleanup EXIT

  log_step "Starting Python gRPC Harness Server on port $port..."
  PYTHONPATH=python:. "$python_bin" -m python.antigravity.harness_server --agent_file "$agent_file" --port "$port" > /tmp/antigravity_harness.log 2>&1 &
  server_pid=$!

  log_step "Waiting for Python Harness Server to become healthy..."
  if ! wait_for_port "$port" "Python Harness Server"; then
    cat /tmp/antigravity_harness.log
    exit 1
  fi

  log_step "Building E2E test client..."
  go build -o bin/e2e ./cmd/e2e

  log_step "Executing E2E integration test suite..."
  bin/e2e
  
  echo "Success!"
}

# SubstrATE Cloud commands helper
run_kubectl() {
  kubectl ${KUBECTL_CONTEXT:+--context=${KUBECTL_CONTEXT}} "$@"
}

run_ko() {
  GOFLAGS="-tags=ate" ko apply ${KUBECTL_CONTEXT:+--context=${KUBECTL_CONTEXT}} "$@"
}

cloud_deploy() {
  check_gemini_key
  
  if [ -z "$BUCKET_NAME" ]; then
    log_error "BUCKET_NAME environment variable is not set."
    echo "Please set it using: export BUCKET_NAME=\"your-gcs-bucket\""
    exit 1
  fi

  local manifest="manifests/ax-deployment.yaml.tmpl"
  local service="manifests/ax-service.yaml"
  
  while [[ "$#" -gt 0 ]]; do
    case $1 in
      --harness|--v2)
        manifest="internal/manifests/ax-deployment2.yaml"
        service=""
        shift
        ;;
      *)
        shift
        ;;
    esac
  done

  log_step "Deploying AX Server and Harnesses from $manifest to Kubernetes cluster..."
  sed -e "s|\${GEMINI_API_KEY}|${GEMINI_API_KEY}|g" \
      -e "s|\${BUCKET_NAME}|${BUCKET_NAME}|g" \
      "$manifest" \
      | run_ko -f -

  if [ -n "$service" ]; then
    run_kubectl apply -f "$service"
  fi
  log_step "Deployment applied successfully!"
}

cloud_delete() {
  local manifest="manifests/ax-deployment.yaml.tmpl"
  local service="manifests/ax-service.yaml"
  
  while [[ "$#" -gt 0 ]]; do
    case $1 in
      --harness|--v2)
        manifest="internal/manifests/ax-deployment2.yaml"
        service=""
        shift
        ;;
      *)
        shift
        ;;
    esac
  done

  log_step "Deleting AX Server and Harnesses ($manifest) from Kubernetes cluster..."
  sed -e "s|\${GEMINI_API_KEY}|dummy-key|g" \
      -e "s|\${BUCKET_NAME}|dummy-bucket|g" \
      "$manifest" \
      | run_kubectl delete --ignore-not-found -f -

  if [ -n "$service" ]; then
    run_kubectl delete --ignore-not-found -f "$service"
  fi
  log_step "Deletion complete!"
}

# Main routing logic
if [ "$#" -eq 0 ]; then
  usage
  exit 1
fi

case $1 in
  -h|--help)
    usage
    exit 0
    ;;
  local)
    run_local
    ;;
  test)
    run_test
    ;;
  cloud)
    if [ "$#" -lt 2 ]; then
      log_error "Missing cloud action (deploy or delete)."
      usage
      exit 1
    fi
    action=$2
    shift 2
    case $action in
      deploy)
        cloud_deploy "$@"
        ;;
      delete)
        cloud_delete "$@"
        ;;
      *)
        log_error "Unknown cloud command: $action"
        usage
        exit 1
        ;;
    esac
    ;;
  *)
    log_error "Unknown command: $1"
    usage
    exit 1
    ;;
esac
