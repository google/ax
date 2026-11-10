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

package apivalidation

import (
	"fmt"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

// ValidateWorkspace reports the first problem with a workspace that would make
// it unusable. It is called by the API server before saving.
func ValidateWorkspace(w *v1alpha1.Workspace) error {
	if err := ValidateObjectMeta(w.GetMetadata()); err != nil {
		return err
	}
	if w.GetSpec() != nil {
		for i, f := range w.GetSpec().GetFiles() {
			field := fmt.Sprintf("spec.files[%d]", i)
			if f == nil || f.GetPath() == "" {
				return fmt.Errorf("%s: path is required", field)
			}
		}
	}
	return nil
}
