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

package controller_test

import (
	"math"
	"strings"
	"testing"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/internal/controller"
)

func TestWorkspaceTagName(t *testing.T) {
	tpl1 := "ax-3f9c1a2b5d7e9f01"
	tpl2 := "ax-9999999999999999"

	short1 := controller.WorkspaceTagName("my-repo-env", tpl1, 1)
	short2 := controller.WorkspaceTagName("my-repo-env", tpl2, 1)
	refreshed := controller.WorkspaceTagName("my-repo-env", tpl1, 12)
	if err := apivalidation.ValidateName(short1); err != nil {
		t.Fatalf("short tag %q invalid: %v", short1, err)
	}
	if !strings.HasSuffix(short1, "-r1") || !strings.HasSuffix(refreshed, "-r12") {
		t.Fatalf("unexpected revision suffixes: %q, %q", short1, refreshed)
	}
	// Tags for the same Workspace across templates and multi-digit revisions
	// share the exact "ws-<prefix>-<8hex(ws)>-" owner prefix.
	wsOwnerPrefix := func(tag string) string {
		revSep := strings.LastIndexByte(tag, '-')
		tplSep := strings.LastIndexByte(tag[:revSep], '-')
		return tag[:tplSep+1]
	}
	if wsOwnerPrefix(short1) != wsOwnerPrefix(short2) || wsOwnerPrefix(short1) != wsOwnerPrefix(refreshed) {
		t.Errorf("same workspace produced different owner prefixes: %q, %q, %q", short1, short2, refreshed)
	}

	longWS1 := strings.Repeat("a", 62) + "1"
	longWS2 := strings.Repeat("a", 62) + "2"
	tag1 := controller.WorkspaceTagName(longWS1, tpl1, 1)
	tag2 := controller.WorkspaceTagName(longWS2, tpl1, math.MaxUint64)
	if err := apivalidation.ValidateName(tag1); err != nil {
		t.Fatalf("long tag1 %q invalid: %v", tag1, err)
	}
	if err := apivalidation.ValidateName(tag2); err != nil {
		t.Fatalf("long tag2 %q invalid at MaxUint64: %v", tag2, err)
	}
	if wsOwnerPrefix(tag1) == wsOwnerPrefix(tag2) {
		t.Errorf("distinct 63-char workspace names collided on owner prefix: %q vs %q", tag1, tag2)
	}
}
