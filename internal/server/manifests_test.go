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

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/ax/internal/model"
	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store/memory"
	"github.com/google/ax/internal/workspace"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

func TestManifestGuideModels(t *testing.T) {
	guide, err := os.ReadFile("../../docs/manifests.md")
	if err != nil {
		t.Fatal(err)
	}
	var examples []*v1alpha1.Model
	for _, block := range strings.Split(string(guide), "```yaml\n")[1:] {
		source := strings.SplitN(block, "```", 2)[0]
		var meta struct {
			Kind string `yaml:"kind"`
		}
		if err := yaml.Unmarshal([]byte(source), &meta); err != nil {
			t.Fatal(err)
		}
		if meta.Kind != v1alpha1.KindModel {
			continue
		}
		var m v1alpha1.Model
		if err := yaml.Unmarshal([]byte(source), &m); err != nil {
			t.Fatal(err)
		}
		if m.Metadata == nil || m.Spec == nil || m.Spec.SecretKey == nil {
			t.Fatal("Model example requires metadata, spec and secretKey")
		}
		examples = append(examples, &m)
	}
	if len(examples) == 0 {
		t.Fatal("guide has no Model examples")
	}

	for _, example := range examples {
		t.Run(example.Metadata.Name, func(t *testing.T) {
			for _, unsupported := range []bool{false, true} {
				name := "documented"
				if unsupported {
					name = "unsupported-provider"
				}
				t.Run(name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					t.Cleanup(cancel)
					st := memory.NewStore()
					srv := server.NewServer(st)
					ln, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					done := make(chan error, 1)
					go func() { done <- srv.GRPCServer().Serve(ln) }()
					t.Cleanup(func() {
						srv.GRPCServer().Stop()
						if err := <-done; err != nil {
							t.Errorf("gRPC server cleanup: %v", err)
						}
						t.Log("gRPC fixture stopped")
					})
					conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := conn.Close(); err != nil {
							t.Error(err)
						}
					})
					api := v1alpha1.NewAXClient(conn)
					m := proto.Clone(example).(*v1alpha1.Model)
					if unsupported {
						m.Spec.Provider = "anthropic"
					}
					if _, err := api.UpdateModel(ctx, &v1alpha1.UpdateModelRequest{Model: m}); err != nil {
						t.Fatal(err)
					}
					saved, err := api.GetModel(ctx, &v1alpha1.GetModelRequest{Atespace: m.Metadata.Atespace, Name: m.Metadata.Name})
					if err != nil {
						t.Fatal(err)
					}
					if !proto.Equal(saved.Spec, m.Spec) {
						t.Fatal("API/store changed Model spec")
					}
					t.Logf("YAML -> gRPC UpdateModel/GetModel: provider=%s model=%s", saved.Spec.Provider, saved.Spec.Model)

					var calls atomic.Int32
					fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if r.Method != http.MethodPost || r.URL.Path != "/v1beta/models/"+m.Spec.Model+":generateContent" || r.URL.Query().Get("key") != "fixture-key" {
							t.Errorf("unexpected provider request method/path or fixture authentication")
						}
						var body struct {
							Contents          []struct{ Parts []struct{ Text string } }
							SystemInstruction struct{ Parts []struct{ Text string } }
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if len(body.Contents) != 1 || len(body.Contents[0].Parts) != 1 || !strings.Contains(body.Contents[0].Parts[0].Text, "Install dependencies") || len(body.SystemInstruction.Parts) != 1 {
							t.Error("request did not contain workspace goal and system instruction")
						}
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"fixture bootstrap plan"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`)
					}))
					t.Cleanup(func() { fixture.Close(); t.Log("HTTP provider fixture stopped") })
					client, err := model.NewClientFromStore(ctx, st, m.Metadata.Atespace, m.Metadata.Name,
						model.WithBaseURL(fixture.URL), model.WithSecretResolver(func(secretName, key string) (string, error) {
							if secretName != m.Spec.SecretKey.Name || key != m.Spec.SecretKey.Key {
								return "", fmt.Errorf("unexpected secret reference")
							}
							return "fixture-key", nil
						}))
					if err != nil {
						t.Fatal(err)
					}
					plan, err := workspace.NewPlanner(client).PlanEnvironment(ctx, &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "guide-workspace"}}, "Install dependencies")
					if unsupported {
						if err == nil || !strings.Contains(err.Error(), `unsupported provider "anthropic"`) {
							t.Fatalf("expected unsupported provider error, got %v", err)
						}
						if calls.Load() != 0 {
							t.Fatal("unsupported provider contacted HTTP fixture")
						}
						t.Logf("negative control: %v; HTTP requests=0", err)
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if plan.PlanSummary != "fixture bootstrap plan" || plan.ModelUsed != m.Spec.Model || calls.Load() != 1 {
						t.Fatalf("expected fixture plan and one HTTP request, got %+v; requests=%d", plan, calls.Load())
					}
					t.Logf("store -> workspace planner -> Google HTTP: model=%s summary=%q; requests=%d", plan.ModelUsed, plan.PlanSummary, calls.Load())
				})
			}
		})
	}
}
