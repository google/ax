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

const (
	maxUint64Digits = 20
	// maxTagWorkspacePrefix leaves room for "ws-" (3) + "-" (1) + 8-char ws
	// hash + "-" (1) + 16-char template hash + "-r" (2) + up to 20 revision
	// digits within apivalidation.MaxNameLength (63).
	maxTagWorkspacePrefix = apivalidation.MaxNameLength - len("ws-") - 1 - 8 - 1 - 16 - len("-r") - maxUint64Digits
)

// WorkspaceTagName returns a deterministic RFC 1123 Substrate Tag name
// ("ws-<prefix>-<8hex(ws)>-<16hex(tpl)>-r<revision>") for (workspaceName,
// templateName, revision).
//
// Substrate Tags cannot repoint their snapshot in place (UpdateTag does not
// change the underlying snapshot, and DeleteTag invalidates unsuspended
// running Actors created from that Tag), so refreshing a Workspace snapshot
// mints a new Tag under an incremented "-r<revision>" suffix while keeping the
// "ws-<prefix>-<8hex(ws)>-" owner prefix intact.
//
// TODO: Persist the active snapshot revision on Workspace and increment it
// once periodic or on-demand snapshot refresh is implemented.
func WorkspaceTagName(workspaceName, templateName string, revision uint64) string {
	wsSum := sha256.Sum256([]byte(workspaceName))
	tplSum := sha256.Sum256([]byte(templateName))
	prefix := strings.TrimRight(workspaceName[:min(len(workspaceName), maxTagWorkspacePrefix)], "-")
	return fmt.Sprintf("ws-%s-%x-%x-r%d", prefix, wsSum[:4], tplSum[:8], revision)
}
