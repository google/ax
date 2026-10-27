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

package main

import (
	"strings"
	"testing"
)

func TestDescribeStore(t *testing.T) {
	tests := []struct {
		name string
		conn string
		want string
	}{
		{
			name: "password in userinfo",
			conn: "postgres://ax:secret@db:5432/ax?sslmode=disable",
			want: "ax@db:5432/ax",
		},
		{
			name: "password in query",
			conn: "postgres://ax@db:5432/ax?sslmode=disable&password=secret",
			want: "ax@db:5432/ax",
		},
		{
			name: "keyword DSN",
			conn: "host=db port=5432 user=ax password=secret dbname=ax sslmode=disable",
			want: "ax@db:5432/ax",
		},
		{
			name: "unparseable",
			conn: "postgres://ax:secret@db:notaport/ax",
			want: "<unparseable>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := describeStore(tt.conn)
			if strings.Contains(got, "secret") {
				t.Fatalf("describeStore(%q) = %q, leaks the password", tt.conn, got)
			}
			if got != tt.want {
				t.Errorf("describeStore(%q) = %q, want %q", tt.conn, got, tt.want)
			}
		})
	}
}
