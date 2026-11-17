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

# A pure task image for TestE2E_PureImageLauncher: no AX runner inside, only a
# stand-in for a prebuilt agent image's launcher. The runner comes from the
# runner volume ax-server mounts at /opt/ax.
ARG BASE_IMAGE=python:3.12-slim
FROM ${BASE_IMAGE}
COPY test/e2e/launcher/ax-harness /usr/local/bin/ax-harness
ENTRYPOINT ["/usr/local/bin/ax-harness"]
