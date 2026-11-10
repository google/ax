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

package resources_test

import (
	"testing"

	"github.com/google/ax/internal/resources"
)

func TestParseCPUMillicores(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"2", 2000, false},
		{"500m", 500, false},
		{"1.5", 1500, false},
		{"1n", 1, false}, // ceiling to 1m
		{"0", 0, true},
		{"1000", 0, true},
	}
	for _, tt := range tests {
		got, err := resources.ParseCPUMillicores(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseCPUMillicores(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseCPUMillicores(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestParseMemoryBytes(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"4Gi", 4 << 30, false},
		{"512Mi", 512 << 20, false},
		{"500M", 500_000_000, false},
		{"1u", 1, false}, // ceiling to 1 byte
		{"0Gi", 0, true},
	}
	for _, tt := range tests {
		got, err := resources.ParseMemoryBytes(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseMemoryBytes(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseMemoryBytes(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}
