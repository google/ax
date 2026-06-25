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
	"testing"
)

func TestValidateID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{
			name:    "valid lowercase",
			id:      "task123",
			wantErr: false,
		},
		{
			name:    "valid mixed",
			id:      "Task-ID_123",
			wantErr: false,
		},
		{
			name:    "valid simple",
			id:      "Task-ID",
			wantErr: false,
		},
		{
			name:    "valid underscore",
			id:      "task_id",
			wantErr: false,
		},
		{
			name:    "invalid space",
			id:      "task id",
			wantErr: true,
		},
		{
			name:    "invalid char",
			id:      "task!",
			wantErr: true,
		},
		{
			name:    "empty",
			id:      "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateID(tt.id)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateID(%q) error = %v, wantErr %v", tt.id, err, tt.wantErr)
			}
		})
	}
}
