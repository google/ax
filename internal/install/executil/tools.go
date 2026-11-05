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
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// Pinned fallback CLI versions downloaded into ~/.ax/bin when absent or
	// incompatible with Kubernetes 1.36+.
	defaultKindVersion    = "v0.32.0"
	defaultKubectlVersion = "v1.36.0"
	minKindMinorVersion   = 32
)

var kindSemverRe = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)

// ToolURLs allows tests to point auto-downloads at an httptest.Server.
type ToolURLs struct {
	KindBaseURL    string // Default: https://kind.sigs.k8s.io/dl
	KubectlBaseURL string // Default: https://dl.k8s.io/release
}

// DefaultToolURLs returns upstream release base URLs for kind and kubectl.
func DefaultToolURLs() ToolURLs {
	return ToolURLs{
		KindBaseURL:    "https://kind.sigs.k8s.io/dl",
		KubectlBaseURL: "https://dl.k8s.io/release",
	}
}

// BinDir returns ~/.ax/bin (or $AX_HOME/bin), creating it if necessary.
func BinDir() (string, error) {
	if axHome := os.Getenv("AX_HOME"); axHome != "" {
		dir := filepath.Join(axHome, "bin")
		return dir, os.MkdirAll(dir, 0o755)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".ax", "bin")
	return dir, os.MkdirAll(dir, 0o755)
}

// ContainerRuntime identifies the host container engine CLI and whether it is Podman.
type ContainerRuntime struct {
	Binary   string
	IsPodman bool
}

// ResolveContainerRuntime locates a working docker or podman CLI.
func ResolveContainerRuntime(ctx context.Context, r Runner) (ContainerRuntime, error) {
	if explicit := os.Getenv("CONTAINER_CLI"); explicit != "" {
		p, err := r.LookPath(explicit)
		if err != nil {
			return ContainerRuntime{}, fmt.Errorf("CONTAINER_CLI=%q not found: %w", explicit, err)
		}
		res, _ := r.Run(ctx, Spec{Name: p, Args: []string{"--version"}})
		return ContainerRuntime{
			Binary:   p,
			IsPodman: strings.Contains(strings.ToLower(res.Stdout+filepath.Base(p)), "podman"),
		}, nil
	}

	for _, candidate := range []struct {
		name     string
		isPodman bool
	}{
		{name: "docker", isPodman: false},
		{name: "podman", isPodman: true},
	} {
		p, err := r.LookPath(candidate.name)
		if err != nil {
			continue
		}
		if _, err := r.Run(ctx, Spec{Name: p, Args: []string{"info"}}); err == nil {
			return ContainerRuntime{Binary: p, IsPodman: candidate.isPodman}, nil
		}
	}
	return ContainerRuntime{}, fmt.Errorf("no running container engine found: install and start Docker or Podman")
}

// EnsureKubectl locates kubectl in ~/.ax/bin or PATH, downloading a pinned
// release binary into ~/.ax/bin if absent.
func EnsureKubectl(ctx context.Context, r Runner, urls ToolURLs, logOut io.Writer) (string, error) {
	if dir, err := BinDir(); err == nil {
		localBin := filepath.Join(dir, "kubectl")
		if isExecutable(localBin) {
			return localBin, nil
		}
	}
	if p, err := r.LookPath("kubectl"); err == nil {
		return p, nil
	}
	dir, err := BinDir()
	if err != nil {
		return "", fmt.Errorf("kubectl not found and cannot create ~/.ax/bin: %w", err)
	}
	if urls.KubectlBaseURL == "" {
		urls = DefaultToolURLs()
	}
	dlURL := fmt.Sprintf("%s/%s/bin/%s/%s/kubectl",
		strings.TrimRight(urls.KubectlBaseURL, "/"), defaultKubectlVersion, runtime.GOOS, runtime.GOARCH)
	dest := filepath.Join(dir, "kubectl")
	if logOut != nil {
		fmt.Fprintf(logOut, "Downloading kubectl %s to %s...\n", defaultKubectlVersion, dest)
	}
	if err := downloadExecutable(ctx, dlURL, dest); err != nil {
		return "", fmt.Errorf("downloading kubectl: %w", err)
	}
	return dest, nil
}

// EnsureKind locates a Kubernetes 1.36-compatible kind CLI (>= v0.32.0) in
// ~/.ax/bin or PATH, downloading defaultKindVersion into ~/.ax/bin if missing
// or too old (for example, Cloudtop hosts shipping kind 0.11.x).
func EnsureKind(ctx context.Context, r Runner, urls ToolURLs, logOut io.Writer) (string, error) {
	if dir, err := BinDir(); err == nil {
		localBin := filepath.Join(dir, "kind")
		if isExecutable(localBin) && kindMeetsMinVersion(ctx, r, localBin) {
			return localBin, nil
		}
	}
	if p, err := r.LookPath("kind"); err == nil && kindMeetsMinVersion(ctx, r, p) {
		return p, nil
	}

	dir, err := BinDir()
	if err != nil {
		return "", fmt.Errorf("compatible kind (>= v0.%d.0) not found and cannot create ~/.ax/bin: %w", minKindMinorVersion, err)
	}
	if urls.KindBaseURL == "" {
		urls = DefaultToolURLs()
	}
	dlURL := fmt.Sprintf("%s/%s/kind-%s-%s",
		strings.TrimRight(urls.KindBaseURL, "/"), defaultKindVersion, runtime.GOOS, runtime.GOARCH)
	dest := filepath.Join(dir, "kind")
	if logOut != nil {
		fmt.Fprintf(logOut, "Downloading kind %s to %s...\n", defaultKindVersion, dest)
	}
	if err := downloadExecutable(ctx, dlURL, dest); err != nil {
		return "", fmt.Errorf("downloading kind: %w", err)
	}
	return dest, nil
}

func kindMeetsMinVersion(ctx context.Context, r Runner, bin string) bool {
	res, err := r.Run(ctx, Spec{Name: bin, Args: []string{"version"}})
	if err != nil {
		return false
	}
	major, minor, ok := parseKindMajorMinor(res.Stdout)
	if !ok {
		return false
	}
	return major > 0 || minor >= minKindMinorVersion
}

func parseKindMajorMinor(versionOutput string) (major, minor int, ok bool) {
	m := kindSemverRe.FindStringSubmatch(versionOutput)
	if len(m) < 3 {
		return 0, 0, false
	}
	maj, err1 := strconv.Atoi(m[1])
	min, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return maj, min, true
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func downloadExecutable(ctx context.Context, url, dest string) error {
	client := &http.Client{Timeout: 2 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}
