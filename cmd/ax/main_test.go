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
	"context"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
)

func TestRunGetResourceAliases(t *testing.T) {
	const atespace = "test-space"
	s := memory.NewStore()
	for _, name := range []string{"target", "other"} {
		meta := &v1alpha1.ObjectMeta{Name: name, Atespace: atespace}
		if err := s.SaveTask(context.Background(), &v1alpha1.Task{Metadata: meta}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveWorkspace(context.Background(), &v1alpha1.Workspace{Metadata: meta}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveModel(context.Background(), &v1alpha1.Model{Metadata: meta}); err != nil {
			t.Fatal(err)
		}
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := server.NewServer(s).GRPCServer()
	t.Cleanup(srv.Stop)
	go func() { _ = srv.Serve(listener) }()

	for _, resource := range []string{"task", "workspace", "model"} {
		for _, alias := range []string{resource, resource + "s"} {
			for _, tc := range []struct {
				name         string
				resourceName string
			}{
				{name: "list"},
				{name: "get", resourceName: "target"},
				{name: "missing", resourceName: "missing"},
			} {
				t.Run(alias+"/"+tc.name, func(t *testing.T) {
					// runGet writes to os.Stdout, so these subtests must remain sequential.
					output, err := os.CreateTemp(t.TempDir(), "stdout")
					if err != nil {
						t.Fatal(err)
					}
					stdout := os.Stdout
					os.Stdout = output
					t.Cleanup(func() {
						os.Stdout = stdout
						_ = output.Close()
					})

					args := []string{alias}
					if tc.resourceName != "" {
						args = append(args, tc.resourceName)
					}
					runErr := runGet(listener.Addr().String(), atespace, args)
					data, err := os.ReadFile(output.Name())
					if err != nil {
						t.Fatal(err)
					}
					if tc.name == "missing" {
						if status.Code(runErr) != codes.NotFound {
							t.Fatalf("expected NotFound, got %v; output: %s", runErr, data)
						}
						if len(data) != 0 {
							t.Fatalf("unexpected output for missing resource: %s", data)
						}
						return
					}
					if runErr != nil {
						t.Fatal(runErr)
					}
					if tc.name == "get" {
						var got struct {
							Metadata struct {
								Name     string `yaml:"name"`
								Atespace string `yaml:"atespace"`
							} `yaml:"metadata"`
						}
						if err := yaml.Unmarshal(data, &got); err != nil {
							t.Fatalf("expected resource YAML, got %q: %v", data, err)
						}
						if got.Metadata.Name != tc.resourceName || got.Metadata.Atespace != atespace {
							t.Fatalf("unexpected resource metadata: %+v", got.Metadata)
						}
						return
					}
					rows := strings.Split(strings.TrimSpace(string(data)), "\n")
					if len(rows) != 3 {
						t.Fatalf("expected header and two resources, got %q", data)
					}
					seen := make(map[string]bool)
					for _, row := range rows[1:] {
						fields := strings.Fields(row)
						if len(fields) < 2 || fields[1] != atespace {
							t.Fatalf("unexpected resource row: %q", row)
						}
						seen[fields[0]] = true
					}
					if !seen["target"] || !seen["other"] {
						t.Fatalf("expected both resources, got %q", data)
					}
				})
			}
		}
	}
}
