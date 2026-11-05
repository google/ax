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

package deploy

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRenderPostgres(t *testing.T) {
	out, err := RenderPostgres("ax-dev")
	if err != nil {
		t.Fatalf("RenderPostgres: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "name: ax-system") || strings.Contains(s, "namespace: ax-system") {
		t.Fatalf("expected ax-system to be rewritten, got:\n%s", s)
	}
	if !strings.Contains(s, "namespace: ax-dev") {
		t.Fatalf("expected namespace: ax-dev in output")
	}
	assertValidMultiDocYAML(t, out)
}

func TestRenderServer(t *testing.T) {
	const (
		srvImg  = "localhost:5001/ax-server:local@sha256:1111111111111111111111111111111111111111111111111111111111111111"
		taskImg = "localhost:5001/ax-task-runner:local@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	)

	t.Run("rejects unpinned task image", func(t *testing.T) {
		_, err := RenderServer(ServerConfig{
			Namespace:        "ax-system",
			ServerImage:      srvImg,
			DefaultTaskImage: "localhost:5001/ax-task-runner:latest",
		})
		if err == nil || !strings.Contains(err.Error(), "digest-pinned") {
			t.Fatalf("expected digest-pinned error, got %v", err)
		}
	})

	t.Run("rejects malformed image references", func(t *testing.T) {
		for _, tc := range []ServerConfig{
			{Namespace: "ax-system", ServerImage: "repo/img:tag\n          evil: true", DefaultTaskImage: taskImg},
			{Namespace: "ax-system", ServerImage: srvImg, DefaultTaskImage: "localhost:5001/ax-task-runner@sha256:short"},
		} {
			if _, err := RenderServer(tc); err == nil {
				t.Fatalf("expected error for %+v, got nil", tc)
			}
		}
	})

	t.Run("renders custom namespace cleanly", func(t *testing.T) {
		out, err := RenderServer(ServerConfig{
			Namespace:        "team-a",
			ServerImage:      srvImg,
			DefaultTaskImage: taskImg,
		})
		if err != nil {
			t.Fatalf("RenderServer: %v", err)
		}
		s := string(out)
		for _, bad := range []string{"${AX_SERVER_IMAGE}", "${AX_DEFAULT_TASK_IMAGE}", "ax-system"} {
			if strings.Contains(s, bad) {
				t.Fatalf("unexpected %q in rendered server manifest:\n%s", bad, s)
			}
		}
		for _, want := range []string{
			"name: ax-server-team-a",
			"ax-postgres.team-a.svc.cluster.local:5432",
			"--template-atespace=team-a",
			srvImg,
			taskImg,
		} {
			if !strings.Contains(s, want) {
				t.Fatalf("missing %q in rendered server manifest:\n%s", want, s)
			}
		}
		assertValidMultiDocYAML(t, out)
	})
}

func assertValidMultiDocYAML(t *testing.T, data []byte) {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	docs := 0
	for {
		var m map[string]any
		err := dec.Decode(&m)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("invalid YAML document %d: %v", docs+1, err)
		}
		if len(m) > 0 {
			docs++
		}
	}
	if docs == 0 {
		t.Fatal("expected at least 1 YAML document")
	}
}
