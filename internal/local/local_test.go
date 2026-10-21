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

package local

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

func TestHomeHonorsAXHome(t *testing.T) {
	t.Setenv("AX_HOME", "/tmp/somewhere")
	if got, _ := Home(); got != "/tmp/somewhere" {
		t.Errorf("Home() = %q", got)
	}
	t.Setenv("AX_HOME", "")
	home, _ := os.UserHomeDir()
	if got, _ := Home(); got != filepath.Join(home, ".ax") {
		t.Errorf("Home() = %q, want ~/.ax", got)
	}
}

// TestStatePersistsAcrossInvocations opens the local control plane twice on
// the same home, as two ax commands would, and checks that resources applied
// by the first are visible to the second.
func TestStatePersistsAcrossInvocations(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()

	env, err := Open(Options{Home: home})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	conn, err := env.Dial()
	if err != nil {
		t.Fatal(err)
	}
	client := v1alpha1.NewAXClient(conn)
	if _, err := client.UpdateWorkspace(ctx, &v1alpha1.UpdateWorkspaceRequest{Workspace: &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "ws"},
		Spec:     &v1alpha1.WorkspaceSpec{Git: []*v1alpha1.GitRepo{{Name: "r", Repo: "https://example.com/r.git"}}},
	}}); err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	_ = conn.Close()
	if err := env.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(DBPath(home)); err != nil {
		t.Fatalf("database not at %s: %v", DBPath(home), err)
	}

	env, err = Open(Options{Home: home})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer env.Close()
	conn, err = env.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	got, err := v1alpha1.NewAXClient(conn).GetWorkspace(ctx, &v1alpha1.GetWorkspaceRequest{Name: "ws"})
	if err != nil {
		t.Fatalf("GetWorkspace after reopen: %v", err)
	}
	if len(got.GetSpec().GetGit()) != 1 {
		t.Errorf("workspace spec lost: %v", got)
	}
}
