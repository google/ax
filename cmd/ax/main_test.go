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
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
)

func TestApplyAtespace(t *testing.T) {
	// Exercise flag parsing and dispatch as well as the resource RPCs.
	binary := filepath.Join(t.TempDir(), "ax")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("building CLI: %v\n%s", err, output)
	}
	for _, kind := range []string{"Task", "Workspace", "Model"} {
		for _, tc := range []struct {
			name             string
			manifestAtespace string
			flags            []string
			wantAtespace     string
			wantErr          string
		}{
			{name: "default", wantAtespace: "default"},
			{name: "flag only", flags: []string{"-a", "team-a"}, wantAtespace: "team-a"},
			{name: "manifest only", manifestAtespace: "team-a", wantAtespace: "team-a"},
			{name: "matching", manifestAtespace: "team-a", flags: []string{"--atespace", "team-a"}, wantAtespace: "team-a"},
			{name: "conflicting", manifestAtespace: "team-a", flags: []string{"--atespace=team-b"}, wantErr: `metadata.atespace "team-a" does not match --atespace "team-b"`},
			{name: "explicit default", manifestAtespace: "team-a", flags: []string{"-a", "default"}, wantErr: `metadata.atespace "team-a" does not match --atespace "default"`},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				s := memory.NewStore()
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				srv := server.NewServer(s).GRPCServer()
				t.Cleanup(srv.Stop)
				go func() { _ = srv.Serve(listener) }()

				path := filepath.Join(t.TempDir(), "resource.yaml")
				manifest := fmt.Sprintf("apiVersion: ax.io/v1alpha1\nkind: %s\nmetadata:\n  name: example\n", kind)
				if tc.manifestAtespace != "" {
					manifest += fmt.Sprintf("  atespace: %q\n", tc.manifestAtespace)
				}
				manifest += "spec: {}\n"
				if err := os.WriteFile(path, []byte(manifest), 0600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				args := append([]string{"--server", listener.Addr().String(), "apply", "-f", path}, tc.flags...)
				output, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
				if tc.wantErr != "" {
					var exitErr *exec.ExitError
					if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(string(output), tc.wantErr) {
						t.Fatalf("expected exit 1 with %q, got %v\n%s", tc.wantErr, err, output)
					}
				} else {
					wantOutput := strings.ToLower(kind) + ".ax.io/example created\n"
					if err != nil || string(output) != wantOutput {
						t.Fatalf("expected %q, got %v\n%s", wantOutput, err, output)
					}
					// Reapplying must look up the resource in the same atespace.
					output, err = exec.CommandContext(ctx, binary, args...).CombinedOutput()
					wantOutput = strings.ToLower(kind) + ".ax.io/example unchanged\n"
					if err != nil || string(output) != wantOutput {
						t.Fatalf("expected %q on reapply, got %v\n%s", wantOutput, err, output)
					}
				}

				for _, atespace := range []string{"default", "team-a", "team-b"} {
					var meta *v1alpha1.ObjectMeta
					var getErr error
					switch kind {
					case "Task":
						resource, err := s.GetTask(ctx, atespace, "example")
						meta, getErr = resource.GetMetadata(), err
					case "Workspace":
						resource, err := s.GetWorkspace(ctx, atespace, "example")
						meta, getErr = resource.GetMetadata(), err
					case "Model":
						resource, err := s.GetModel(ctx, atespace, "example")
						meta, getErr = resource.GetMetadata(), err
					}
					if atespace == tc.wantAtespace {
						if getErr != nil || meta.GetAtespace() != atespace {
							t.Errorf("expected resource in %q, got metadata %v, error %v", atespace, meta, getErr)
						}
					} else if !errors.Is(getErr, store.ErrNotFound) {
						t.Errorf("expected no resource in %q, got metadata %v, error %v", atespace, meta, getErr)
					}
				}
			})
		}
	}
}

func TestRunApplyEmptyDocuments(t *testing.T) {
	const first = "apiVersion: ax.io/v1alpha1\nkind: Workspace\nmetadata:\n  name: first\nspec: {}\n"
	second := strings.Replace(first, "name: first", "name: second", 1)
	for _, tc := range []struct {
		name      string
		manifest  string
		wantNames []string
		wantErr   string
	}{
		{name: "empty file"},
		{name: "empty documents only", manifest: "---\n# empty\n---\n"},
		{name: "leading empty document", manifest: "---\n---\n" + first, wantNames: []string{"first"}},
		{name: "trailing separator", manifest: first + "---\n", wantNames: []string{"first"}},
		{name: "middle empty documents", manifest: first + "---\n# empty\n---\n---\n" + second, wantNames: []string{"first", "second"}},
		{name: "missing kind", manifest: first + "---\n---\n{}\n---\n" + second, wantNames: []string{"first"}, wantErr: "applying document 3: missing kind"},
		{name: "explicit null", manifest: "null\n", wantErr: "applying document 1: missing kind"},
		{name: "tagged null", manifest: "!!null\n", wantErr: "applying document 1: missing kind"},
		{name: "tagged quoted null", manifest: "!!null \"\"\n", wantErr: "applying document 1: missing kind"},
		{name: "empty string", manifest: "\"\"\n", wantErr: "applying document 1: reading kind"},
		{name: "invalid YAML", manifest: first + "---\n[\n", wantNames: []string{"first"}, wantErr: "decoding document 2:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := memory.NewStore()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := server.NewServer(s).GRPCServer()
			t.Cleanup(srv.Stop)
			go func() { _ = srv.Serve(listener) }()

			path := filepath.Join(t.TempDir(), "manifest.yaml")
			if err := os.WriteFile(path, []byte(tc.manifest), 0600); err != nil {
				t.Fatal(err)
			}
			// runApply writes to os.Stdout, so these subtests must remain sequential.
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

			runErr := runApply(listener.Addr().String(), nil, []string{"-f", path})
			if tc.wantErr == "" {
				if runErr != nil {
					t.Fatal(runErr)
				}
			} else if runErr == nil || !strings.Contains(runErr.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, runErr)
			}
			var wantOutput strings.Builder
			for _, name := range tc.wantNames {
				if _, err := s.GetWorkspace(context.Background(), "default", name); err != nil {
					t.Fatalf("expected workspace %q to be created: %v", name, err)
				}
				wantOutput.WriteString("workspace.ax.io/" + name + " created\n")
			}
			data, err := os.ReadFile(output.Name())
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != wantOutput.String() {
				t.Fatalf("expected output %q, got %q", wantOutput.String(), data)
			}
		})
	}
}

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
