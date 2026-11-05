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

package executil

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRunner struct {
	paths map[string]string
	runFn func(Spec) (Result, error)
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	if p, ok := f.paths[name]; ok {
		return p, nil
	}
	return "", fmt.Errorf("not found: %s", name)
}

func (f *fakeRunner) Run(_ context.Context, spec Spec) (Result, error) {
	return f.runFn(spec)
}

func TestParseKindMajorMinor(t *testing.T) {
	cases := []struct {
		in      string
		maj     int
		min     int
		wantOK  bool
		meets27 bool
	}{
		{"kind version 0.11.1\n", 0, 11, true, false},
		{"kind v0.29.0 go1.24.2 linux/amd64\n", 0, 29, true, true},
		{"kind v1.0.0\n", 1, 0, true, true},
		{"garbage", 0, 0, false, false},
	}
	for _, tc := range cases {
		maj, min, ok := parseKindMajorMinor(tc.in)
		if ok != tc.wantOK || maj != tc.maj || min != tc.min {
			t.Fatalf("parseKindMajorMinor(%q) = (%d, %d, %v), want (%d, %d, %v)",
				tc.in, maj, min, ok, tc.maj, tc.min, tc.wantOK)
		}
	}
}

func TestEnsureKindDownloadsWhenSystemBinaryTooOld(t *testing.T) {
	axHome := t.TempDir()
	t.Setenv("AX_HOME", axHome)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("#!/bin/sh\necho 'kind v0.32.0'\n"))
	}))
	defer srv.Close()

	r := &fakeRunner{
		paths: map[string]string{"kind": "/usr/local/bin/kind"},
		runFn: func(spec Spec) (Result, error) {
			if spec.Name == "/usr/local/bin/kind" {
				return Result{Stdout: "kind version 0.11.1\n"}, nil
			}
			return Result{Stdout: "kind v0.32.0\n"}, nil
		},
	}

	var logBuf bytes.Buffer
	got, err := EnsureKind(context.Background(), r, ToolURLs{KindBaseURL: srv.URL}, &logBuf)
	if err != nil {
		t.Fatalf("EnsureKind: %v", err)
	}
	want := filepath.Join(axHome, "bin", "kind")
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if !strings.Contains(logBuf.String(), "Downloading kind") {
		t.Fatalf("expected download log message, got %q", logBuf.String())
	}
}
