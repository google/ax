#!/bin/bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -e
set -u
set -o pipefail

ROOT=$(git rev-parse --show-toplevel)
cd "${ROOT}"

# Delegate to the consolidated ax-dev.sh script
case "${1:-}" in
  --deploy-ax-server)
    shift
    ./hack/ax-dev.sh cloud deploy "$@"
    ;;
  --delete-ax-server)
    shift
    ./hack/ax-dev.sh cloud delete "$@"
    ;;
  -h|--help)
    echo "Usage: $0 [options]"
    echo ""
    echo "Options:"
    echo "  --deploy-ax-server                    Deploy AX server and components using ko"
    echo "  --delete-ax-server                    Delete AX server and components from cluster"
    echo "  -h, --help                            Show this help message"
    ;;
  *)
    echo "Usage: $0 --deploy-ax-server | --delete-ax-server"
    exit 1
    ;;
esac
