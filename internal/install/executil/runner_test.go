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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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

	payload := []byte("#!/bin/sh\necho 'kind v0.32.0'\n")
	sum := sha256.Sum256(payload)
	wantSHA := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
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
	got, err := EnsureKind(context.Background(), r, ToolURLs{
		KindBaseURL: srv.URL,
		KindSHA256:  wantSHA,
	}, &logBuf)
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

func TestEnsureKindRejectsChecksumMismatch(t *testing.T) {
	axHome := t.TempDir()
	t.Setenv("AX_HOME", axHome)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("tampered-binary"))
	}))
	defer srv.Close()

	r := &fakeRunner{paths: map[string]string{}, runFn: func(Spec) (Result, error) { return Result{}, nil }}
	_, err := EnsureKind(context.Background(), r, ToolURLs{
		KindBaseURL: srv.URL,
		KindSHA256:  strings.Repeat("0", 64),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "SHA-256 checksum mismatch") {
		t.Fatalf("expected SHA-256 mismatch error, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(axHome, "bin", "kind")); !os.IsNotExist(statErr) {
		t.Fatalf("expected ~/.ax/bin/kind not to exist after checksum mismatch, statErr=%v", statErr)
	}
}

func TestEnsureToolsPreferSystemPathOverAxBin(t *testing.T) {
	axHome := t.TempDir()
	t.Setenv("AX_HOME", axHome)
	binDir, err := BinDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kubectl", "kind"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	r := &fakeRunner{
		paths: map[string]string{
			"kubectl": "/usr/local/bin/kubectl",
			"kind":    "/usr/local/bin/kind",
		},
		runFn: func(spec Spec) (Result, error) {
			return Result{Stdout: "kind v0.32.0\n"}, nil
		},
	}

	gotKubectl, err := EnsureKubectl(context.Background(), r, DefaultToolURLs(), nil)
	if err != nil || gotKubectl != "/usr/local/bin/kubectl" {
		t.Fatalf("EnsureKubectl = (%q, %v), want /usr/local/bin/kubectl", gotKubectl, err)
	}
	gotKind, err := EnsureKind(context.Background(), r, DefaultToolURLs(), nil)
	if err != nil || gotKind != "/usr/local/bin/kind" {
		t.Fatalf("EnsureKind = (%q, %v), want /usr/local/bin/kind", gotKind, err)
	}
}

func TestValidateContainerHost(t *testing.T) {
	const gib = 1024 * 1024 * 1024

	t.Run("healthy_macos_vm", func(t *testing.T) {
		var warn bytes.Buffer
		err := ValidateContainerHost(ContainerHostInfo{
			KernelVersion: "6.10.14-linuxkit",
			MemTotalBytes: 8 * gib,
			Rootless:      false,
		}, false, "darwin", &warn)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if warn.Len() != 0 {
			t.Fatalf("unexpected warning: %q", warn.String())
		}
	})

	t.Run("rejects_rootless_podman_on_macos", func(t *testing.T) {
		err := ValidateContainerHost(ContainerHostInfo{
			KernelVersion: "6.12.0",
			MemTotalBytes: 8 * gib,
			Rootless:      true,
		}, true, "darwin", nil)
		if err == nil || !strings.Contains(err.Error(), "podman machine set --rootful") {
			t.Fatalf("expected rootful remediation error, got: %v", err)
		}
	})

	t.Run("rejects_kernel_older_than_6_8", func(t *testing.T) {
		err := ValidateContainerHost(ContainerHostInfo{
			KernelVersion: "6.6.12-linuxkit",
			MemTotalBytes: 8 * gib,
		}, false, "darwin", nil)
		if err == nil || !strings.Contains(err.Error(), "6.8") {
			t.Fatalf("expected kernel >= 6.8 error, got: %v", err)
		}
	})

	t.Run("rejects_tiny_2gib_colima_vm", func(t *testing.T) {
		err := ValidateContainerHost(ContainerHostInfo{
			KernelVersion: "6.12.0-ubuntu",
			MemTotalBytes: 2 * gib,
		}, false, "darwin", nil)
		if err == nil || !strings.Contains(err.Error(), "colima") {
			t.Fatalf("expected macOS VM memory error, got: %v", err)
		}
	})

	t.Run("warns_between_4_and_6_gib", func(t *testing.T) {
		var warn bytes.Buffer
		err := ValidateContainerHost(ContainerHostInfo{
			KernelVersion: "6.12.0-ubuntu",
			MemTotalBytes: 5 * gib,
		}, false, "darwin", &warn)
		if err != nil {
			t.Fatalf("unexpected error for 5 GiB VM: %v", err)
		}
		if !strings.Contains(warn.String(), "Warning:") {
			t.Fatalf("expected memory warning, got %q", warn.String())
		}
	})
}

func TestResolveStudioBinary(t *testing.T) {
	t.Run("prefers_AX_STUDIO_BIN_override", func(t *testing.T) {
		tmp := t.TempDir()
		custom := filepath.Join(tmp, "my-ax-studio")
		if err := os.WriteFile(custom, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AX_STUDIO_BIN", custom)

		got, err := ResolveStudioBinary(&fakeRunner{})
		if err != nil || got != custom {
			t.Fatalf("ResolveStudioBinary = (%q, %v), want %q", got, err, custom)
		}
	})

	t.Run("resolves_from_managed_ax_bin_dir", func(t *testing.T) {
		t.Setenv("AX_STUDIO_BIN", "")
		axHome := t.TempDir()
		t.Setenv("AX_HOME", axHome)
		binDir := filepath.Join(axHome, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		installed := filepath.Join(binDir, "ax-studio")
		if err := os.WriteFile(installed, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}

		got, err := ResolveStudioBinary(&fakeRunner{})
		if err != nil || got != installed {
			t.Fatalf("ResolveStudioBinary = (%q, %v), want %q", got, err, installed)
		}
	})
}
