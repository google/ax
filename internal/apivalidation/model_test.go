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
	"context"
	"testing"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestValidateUpdateModelRequest(t *testing.T) {
	tests := []struct {
		name string
		req  *v1alpha1.UpdateModelRequest
		want field.ErrorList
	}{
		{name: "valid", req: &v1alpha1.UpdateModelRequest{Model: &v1alpha1.Model{Metadata: &v1alpha1.ObjectMeta{Name: "model"}}}},
		{name: "missing model", req: &v1alpha1.UpdateModelRequest{}, want: field.ErrorList{field.Required(field.NewPath("model"), "")}},
		{name: "missing model.metadata", req: &v1alpha1.UpdateModelRequest{Model: &v1alpha1.Model{}}, want: field.ErrorList{field.Required(field.NewPath("model", "metadata"), "")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidateErr(t, apivalidation.ValidateUpdateModelRequest(context.Background(), tt.req), tt.want)
		})
	}
}
