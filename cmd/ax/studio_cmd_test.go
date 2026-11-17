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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBuildStudioChildArgs(t *testing.T) {
	t.Run("forwards_arbitrary_studio_flags_verbatim", func(t *testing.T) {
		got := buildStudioChildArgs("custom-ax", "/usr/local/bin/ax", []string{"--port", "18080", "--no-open", "--future-flag", "v"})
		want := []string{"-n", "custom-ax", "--ax-bin", "/usr/local/bin/ax", "--port", "18080", "--no-open", "--future-flag", "v"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("buildStudioChildArgs = %#v, want %#v", got, want)
		}
	})

	t.Run("maps_bare_help_to_flag", func(t *testing.T) {
		got := buildStudioChildArgs("", "/usr/local/bin/ax", []string{"help"})
		want := []string{"-n", "ax-system", "--ax-bin", "/usr/local/bin/ax", "--help"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("buildStudioChildArgs(help) = %#v, want %#v", got, want)
		}
	})

	t.Run("respects_explicit_overrides", func(t *testing.T) {
		got := buildStudioChildArgs("ax-system", "/usr/local/bin/ax", []string{"-n", "other-ns", "--ax-bin=/custom/ax", "--help"})
		want := []string{"-n", "other-ns", "--ax-bin=/custom/ax", "--help"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("buildStudioChildArgs(overrides) = %#v, want %#v", got, want)
		}
	})
}

func TestRunStudioDelegatesToStudioBinary(t *testing.T) {
	tmp := t.TempDir()
	outFile := filepath.Join(tmp, "recorded_args.txt")
	fakeStudio := filepath.Join(tmp, "ax-studio")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + outFile + "\n"
	if err := os.WriteFile(fakeStudio, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AX_STUDIO_BIN", fakeStudio)

	if err := runStudio("ax-system", []string{"--port", "18088", "--no-open", "--custom-studio-opt"}); err != nil {
		t.Fatalf("runStudio failed: %v", err)
	}

	recorded, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading recorded args: %v", err)
	}
	text := string(recorded)
	for _, expected := range []string{"-n\nax-system", "--port\n18088", "--no-open", "--custom-studio-opt"} {
		if !strings.Contains(text, expected) {
			t.Errorf("expected delegated args to contain %q, got:\n%s", expected, text)
		}
	}
}

func buildTestStudioTarGz(t *testing.T, content string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: "ax-studio",
		Mode: 0o755,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

func TestInstallShStudioPinning(t *testing.T) {
	for _, tool := range []string{"sh", "curl", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available in PATH", tool)
		}
	}

	installScript, err := filepath.Abs(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}

	archiveBytes, archiveSHA := buildTestStudioTarGz(t, "#!/bin/sh\necho ax-studio-ok\n")
	assetName := fmt.Sprintf("ax-studio_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	t.Run("skips_cleanly_when_unpinned_and_never_hits_latest", func(t *testing.T) {
		tmp := t.TempDir()
		// Place a failing curl shim first in PATH to prove no HTTP call is made when unpinned.
		shimDir := filepath.Join(tmp, "shim")
		if err := os.MkdirAll(shimDir, 0o755); err != nil {
			t.Fatal(err)
		}
		curlMarker := filepath.Join(tmp, "curl_called")
		curlShim := "#!/bin/sh\ntouch " + curlMarker + "\nexit 1\n"
		if err := os.WriteFile(filepath.Join(shimDir, "curl"), []byte(curlShim), 0o755); err != nil {
			t.Fatal(err)
		}

		installDir := filepath.Join(tmp, "bin")
		cmd := exec.Command("sh", "-c", `. "$1" && detect_os && detect_arch && resolve_install_dir && install_studio`, "sh", installScript)
		cmd.Env = append(os.Environ(),
			"__INSTALL_SH_SOURCED=1",
			"AX_INSTALL_DIR="+installDir,
			"AX_STUDIO_VERSION=",
			"AX_STUDIO_BASE_URL=",
			"AX_STUDIO_LOCAL_BIN=",
			"AX_SKIP_STUDIO=0",
			"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("expected unpinned install_studio to skip cleanly, got err=%v:\n%s", err, string(out))
		}
		if _, err := os.Stat(curlMarker); err == nil {
			t.Fatal("expected curl not to be invoked when PINNED_STUDIO_VERSION and AX_STUDIO_VERSION are empty")
		}
		if _, err := os.Stat(filepath.Join(installDir, "ax-studio")); err == nil {
			t.Fatal("expected ax-studio not to be installed when unpinned")
		}
	})

	t.Run("verifies_embedded_pinned_sha256_and_fails_closed_on_mismatch", func(t *testing.T) {
		var checksumHits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/releases/download/v0.1.0/" + assetName:
				_, _ = w.Write(archiveBytes)
			case "/releases/download/v0.1.0/checksums.txt":
				checksumHits.Add(1)
				http.NotFound(w, r)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		tmp := t.TempDir()
		realCurl, err := exec.LookPath("curl")
		if err != nil {
			t.Fatal(err)
		}
		// Redirect github.com/agentexecutor/studio requests to the local httptest server.
		shimDir := filepath.Join(tmp, "shim")
		if err := os.MkdirAll(shimDir, 0o755); err != nil {
			t.Fatal(err)
		}
		rewriteCurl := fmt.Sprintf("#!/bin/sh\nargs=\"\"\nfor a in \"$@\"; do\n  a=$(printf '%%s' \"$a\" | sed 's|https://github.com/agentexecutor/studio|%s|g')\n  args=\"$args '$a'\"\ndone\neval exec '%s' $args\n", srv.URL, realCurl)
		if err := os.WriteFile(filepath.Join(shimDir, "curl"), []byte(rewriteCurl), 0o755); err != nil {
			t.Fatal(err)
		}

		// 1. Matching embedded SHA-256 succeeds without fetching checksums.txt.
		goodDir := filepath.Join(tmp, "good-bin")
		goodSnippet := fmt.Sprintf(`. "$1"
PINNED_STUDIO_VERSION="v0.1.0"
AX_STUDIO_VERSION="v0.1.0"
pinned_studio_sha256() { printf '%%s' %q; }
detect_os && detect_arch && resolve_install_dir && install_studio`, archiveSHA)
		goodCmd := exec.Command("sh", "-c", goodSnippet, "sh", installScript)
		goodCmd.Env = append(os.Environ(),
			"__INSTALL_SH_SOURCED=1",
			"AX_INSTALL_DIR="+goodDir,
			"AX_STUDIO_GITHUB_REPO=agentexecutor/studio",
			"AX_STUDIO_BASE_URL=",
			"AX_STUDIO_SHA256=",
			"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		)
		if out, err := goodCmd.CombinedOutput(); err != nil {
			t.Fatalf("expected pinned SHA-256 install to succeed, got %v:\n%s", err, string(out))
		}
		if _, err := os.Stat(filepath.Join(goodDir, "ax-studio")); err != nil {
			t.Fatalf("expected ax-studio binary installed: %v", err)
		}
		if checksumHits.Load() != 0 {
			t.Fatalf("expected pinned SHA-256 path not to fetch checksums.txt, got %d hits", checksumHits.Load())
		}

		// 2. Mismatched embedded SHA-256 fails closed.
		badDir := filepath.Join(tmp, "bad-bin")
		badSnippet := `. "$1"
PINNED_STUDIO_VERSION="v0.1.0"
AX_STUDIO_VERSION="v0.1.0"
pinned_studio_sha256() { printf '%s' "0000000000000000000000000000000000000000000000000000000000000000"; }
detect_os && detect_arch && resolve_install_dir && install_studio`
		badCmd := exec.Command("sh", "-c", badSnippet, "sh", installScript)
		badCmd.Env = append(os.Environ(),
			"__INSTALL_SH_SOURCED=1",
			"AX_INSTALL_DIR="+badDir,
			"AX_STUDIO_GITHUB_REPO=agentexecutor/studio",
			"AX_STUDIO_BASE_URL=",
			"AX_STUDIO_SHA256=",
			"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		)
		if out, err := badCmd.CombinedOutput(); err == nil {
			t.Fatalf("expected SHA-256 mismatch to fail closed, but command succeeded:\n%s", string(out))
		}
		if _, err := os.Stat(filepath.Join(badDir, "ax-studio")); err == nil {
			t.Fatal("expected ax-studio not to exist after SHA-256 mismatch")
		}
	})

	t.Run("custom_override_verifies_checksums_txt", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/" + assetName:
				_, _ = w.Write(archiveBytes)
			case "/checksums.txt":
				_, _ = fmt.Fprintf(w, "%s  %s\n", archiveSHA, assetName)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		installDir := filepath.Join(t.TempDir(), "bin")
		cmd := exec.Command("sh", "-c", `. "$1" && detect_os && detect_arch && resolve_install_dir && install_studio`, "sh", installScript)
		cmd.Env = append(os.Environ(),
			"__INSTALL_SH_SOURCED=1",
			"AX_INSTALL_DIR="+installDir,
			"AX_STUDIO_BASE_URL="+srv.URL,
			"AX_STUDIO_SHA256=",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("expected custom AX_STUDIO_BASE_URL install to succeed, got %v:\n%s", err, string(out))
		}
		if _, err := os.Stat(filepath.Join(installDir, "ax-studio")); err != nil {
			t.Fatalf("expected ax-studio binary installed: %v", err)
		}
	})
}
