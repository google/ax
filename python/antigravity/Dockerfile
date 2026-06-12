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

# Container image for the Antigravity HarnessService server.
#
# Runtime dependencies -- including the linux/amd64 google-antigravity wheel that
# bundles the linux `localharness` binary -- are installed offline from a
# "wheels" build context, so the build needs no access to the private package
# registry. Populate the wheel cache with `internal/hack/install-ax.sh
# --fetch-wheels` (which also drives the build/deploy).
#
# Build context: repository root. Requires an OCI builder that supports
# additional build contexts and RUN --mount: docker with BuildKit (default since
# Docker 23; older Docker: use `docker buildx build`), or podman/buildah >= 4.0.
# For example:
#   # Docker
#   docker build --platform linux/amd64 \
#     --build-context wheels=$HOME/.cache/ax-antigravity-wheels \
#     -f python/antigravity/Dockerfile -t <ref> . && docker push <ref>
#   # Podman
#   podman build --platform linux/amd64 \
#     --build-context wheels=$HOME/.cache/ax-antigravity-wheels \
#     -f python/antigravity/Dockerfile -t <ref> . && podman push <ref>

FROM python:3.13-slim

WORKDIR /app

# Install runtime deps offline from the pre-downloaded wheels. They are
# bind-mounted (not COPYed) so they never land in an image layer.
COPY python/antigravity/requirements.txt /tmp/requirements.txt
RUN --mount=type=bind,from=wheels,target=/tmp/wheels \
    pip install --no-cache-dir --no-index --find-links=/tmp/wheels -r /tmp/requirements.txt

# Harness server (with generated proto stubs) and the agent it serves.
COPY python/ /app/python
COPY examples/antigravity_agent/ /app/examples/antigravity_agent

# `from python.proto import ...` needs /app on the path; the generated stubs do
# `from proto import ...`, which needs /app/python.
ENV PYTHONPATH=/app:/app/python

# Flush stdout/stderr immediately so server logs surface in container logs.
ENV PYTHONUNBUFFERED=1

EXPOSE 80

# Command for local docker build and test.
# Substrate ignores the image CMD and runs the ActorTemplate `command` instead.
CMD ["python", "-m", "python.antigravity.harness_server", \
     "--agent_file", "examples/antigravity_agent/agent.py", \
     "--host", "0.0.0.0", "--port", "80"]
