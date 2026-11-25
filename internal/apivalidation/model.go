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
	"context"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"k8s.io/apimachinery/pkg/api/operation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

func ValidateUpdateModelRequest(ctx context.Context, req *v1alpha1.UpdateModelRequest) field.ErrorList {
	// Call the generated validation.
	// We model this as a create rather than an update because updates assume
	// the existence of a "current" value, which we do not have. This is
	// validating the request itself.
	op := operation.Operation{Type: operation.Create}
	return Validate_UpdateModelRequest(ctx, op, nil, req, nil)
}
