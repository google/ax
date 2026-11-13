// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/google/ax/internal/apivalidation"
)

// maxTagWorkspacePrefix leaves room for "ws-" (3) + "-" (1) + 8-char ws hash
// + "-" (1) + 16-char tag hash within apivalidation.MaxNameLength (63).
const maxTagWorkspacePrefix = apivalidation.MaxNameLength - len("ws-") - 1 - 8 - 1 - 16

// WorkspaceTagName returns a deterministic RFC 1123 Substrate Tag name
// ("ws-<prefix>-<8hex(ws)>-<16hex(ws+tpl)>") for (workspaceName, templateName).
func WorkspaceTagName(workspaceName, templateName string) string {
	wsSum := sha256.Sum256([]byte(workspaceName))
	tagSum := sha256.Sum256([]byte(workspaceName + "\x00" + templateName))
	prefix := strings.TrimRight(workspaceName[:min(len(workspaceName), maxTagWorkspacePrefix)], "-")
	return fmt.Sprintf("ws-%s-%x-%x", prefix, wsSum[:4], tagSum[:8])
}
