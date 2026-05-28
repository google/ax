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

# Check if GEMINI_API_KEY is set
if [ -z "$GEMINI_API_KEY" ]; then
  echo "ERROR: GEMINI_API_KEY environment variable is not set."
  echo "Please set it using: export GEMINI_API_KEY=\"your-key\""
  exit 1
fi

PORT=50053
ADDRESS="localhost:$PORT"
AGENT_FILE="examples/antigravity_agent/agent.py"

# 1. Start Python gRPC server in the background
echo "Starting Python gRPC Harness Server on port $PORT..."
PYTHONPATH=python:. .venv/bin/python -m python.antigravity.harness_server --agent_file "$AGENT_FILE" --port "$PORT" > /tmp/antigravity_harness.log 2>&1 &
SERVER_PID=$!

# Register trap to ensure server is killed on script exit
cleanup() {
  echo "Cleaning up: killing Python server (PID: $SERVER_PID)..."
  kill "$SERVER_PID" || true
  wait "$SERVER_PID" 2>/dev/null || true
  echo "Cleanup complete!"
}
trap cleanup EXIT

# 2. Wait for the Python server to be healthy
echo "Waiting for Python server to become healthy..."
MAX_ATTEMPTS=30
ATTEMPT=1
HEALTHY=false

while [ $ATTEMPT -le $MAX_ATTEMPTS ]; do
  # We can check if the port is open using nc (netcat)
  if nc -z localhost "$PORT"; then
    HEALTHY=true
    break
  fi
  sleep 0.2
  ATTEMPT=$((ATTEMPT + 1))
done

if [ "$HEALTHY" = false ]; then
  echo "ERROR: Python server failed to start within 6 seconds."
  echo "Server logs (/tmp/antigravity_harness.log):"
  cat /tmp/antigravity_harness.log
  exit 1
fi
echo "Python server is active!"

# 3. Build and run the Go E2E V2 demonstration
echo "Building e2e..."
/opt/homebrew/bin/go build -o bin/e2e ./cmd/e2e

echo "Executing E2E Demo with Antigravity gRPC Harness..."
bin/e2e

echo "Success!"
