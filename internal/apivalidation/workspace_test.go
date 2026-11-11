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

package apivalidation_test

import (
	"strings"
	"testing"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestValidateWorkspace_Files(t *testing.T) {
	ws := &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "valid-ws"},
		Spec: &v1alpha1.WorkspaceSpec{
			Files: []*v1alpha1.File{
				{Path: "", Content: "missing path"},
			},
		},
	}
	if err := apivalidation.ValidateWorkspace(ws); err == nil || !strings.Contains(err.Error(), "spec.files[0]: path is required") {
		t.Errorf("expected path is required error, got %v", err)
	}
}
