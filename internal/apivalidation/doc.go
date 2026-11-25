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

// Kubernetes codegen tools require this to be in doc.go, no other name will
// work.

// +k8s:validation-gen=TypesWithSuffix=Request
// +k8s:validation-gen-input=github.com/google/ax/pkg/apis/v1alpha1
// +k8s:validation-gen-scheme-registry=nil
// +k8s:validation-gen-deep-equal-func=axDeepEqual

package apivalidation
