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

// This file handles host prerequisite checks and CLI tool bootstrapping for
// `ax local` and `ax install` in three parts:
//  1. Container runtime discovery (finding a running `docker` or `podman` daemon).
//  2. Fail-fast preflight validation of the container engine's underlying Linux
//     host (or macOS Linux VM): rootful mode, kernel >= 6.8, and >= 4 GiB RAM.
//  3. Zero-sudo auto-download of pinned `kubectl` and `kind` binaries into
//     `~/.ax/bin` when missing from PATH or older than Kubernetes 1.36 requires.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	// Minimum Linux kernel version on the container daemon host (or macOS Linux VM)
	// required by Agent Substrate's OverlayFS `lowerdir+` image cache.
	minKernelMajor = 6
	minKernelMinor = 8

	// Memory thresholds (in bytes) on the container daemon host / macOS VM for
	// running a local Kind + Agent Substrate + AX stack.
	minHostMemoryBytes         int64 = 4 * 1024 * 1024 * 1024 // 4 GiB hard floor
	recommendedHostMemoryBytes int64 = 6 * 1024 * 1024 * 1024 // 6 GiB recommended
)

var (
	kindSemverRe   = regexp.MustCompile(`v?(\d+)\.(\d+)\.(\d+)`)
	kernelMajorMin = regexp.MustCompile(`(\d+)\.(\d+)`)

	// Official SHA-256 digests from https://kind.sigs.k8s.io/dl/v0.32.0/kind-<os>-<arch>.sha256sum
	pinnedKindSHA256 = map[string]string{
		"linux/amd64":  "50030de23cf40a18505f20426f6a8506bedf13c6e509244bd1fa9463721b0f54",
		"linux/arm64":  "b92cd615e97585de8ddade28ed5cd7feb4248d717c233eea5b03c37298900f5d",
		"darwin/amd64": "295ac6d0d634c9819c9907df45e3017d1f13166bd13c3404c45e79f7faa47498",
		"darwin/arm64": "dca67911095a110c2b5c36e26df6cac860c602033e456c0db47be498cdef1ebb",
	}

	// Official SHA-256 digests from https://dl.k8s.io/release/v1.36.0/bin/<os>/<arch>/kubectl.sha256
	pinnedKubectlSHA256 = map[string]string{
		"linux/amd64":  "123d8c8844f46b1244c547fffb3c17180c0c26dac9890589fe7e67763298748e",
		"linux/arm64":  "9f9d9c44a7b5264515ac9da5991584e2395bd50662e651132337e7b4d0c56f8f",
		"darwin/amd64": "06d7e9a3a26a326d43102c70b19f9d233db219d09890e558dbdc3647db732f06",
		"darwin/arm64": "4bcf268eacdc1d2df74e37d86f639f27ca7dea3ae185b7b452b73b9fb5ddc14e",
	}
)

// ToolURLs allows tests to point auto-downloads at an httptest.Server with
// test-specific SHA-256 digests.
type ToolURLs struct {
	KindBaseURL    string // Default: https://kind.sigs.k8s.io/dl
	KindSHA256     string // Optional override for custom test KindBaseURL
	KubectlBaseURL string // Default: https://dl.k8s.io/release
	KubectlSHA256  string // Optional override for custom test KubectlBaseURL
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

// -----------------------------------------------------------------------------
// 1. Container Runtime Discovery
// -----------------------------------------------------------------------------

// ContainerRuntime identifies the host container engine CLI and whether it is Podman.
type ContainerRuntime struct {
	Binary   string
	IsPodman bool
}

// ResolveContainerRuntime locates a working docker or podman CLI.
func ResolveContainerRuntime(ctx context.Context, r Runner) (ContainerRuntime, error) {
	// Honor an explicit override (matching Makefile's CONTAINER_CLI convention).
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

	// Probe `docker info`, then `podman info`, to verify not only that the CLI is
	// installed, but that its daemon / Linux VM is actively running.
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
	if runtime.GOOS == "darwin" {
		return ContainerRuntime{}, fmt.Errorf("no running container engine found on macOS: start OrbStack or Docker Desktop (>= 6 GiB RAM), or install and start Colima:\n  brew install docker colima\n  colima start --cpu 4 --memory 8 --disk 60 --vm-type vz --mount-type virtiofs")
	}
	return ContainerRuntime{}, fmt.Errorf("no running container engine found: install and start Docker or rootful Podman")
}

// -----------------------------------------------------------------------------
// 2. Container Host / macOS VM Preflight Checks
// -----------------------------------------------------------------------------

// ContainerHostInfo summarizes the container daemon's underlying Linux host or VM.
type ContainerHostInfo struct {
	KernelVersion string
	MemTotalBytes int64
	Rootless      bool
}

// CheckContainerHost inspects the running Docker or Podman engine and verifies
// that its Linux host (or macOS Linux VM) meets Agent Substrate's kernel,
// privilege, and memory requirements. Unparseable or empty fields (such as in
// unit test fakes) are skipped gracefully.
func CheckContainerHost(ctx context.Context, r Runner, rt ContainerRuntime, logOut io.Writer) error {
	info, ok := InspectContainerHost(ctx, r, rt)
	if !ok {
		return nil
	}
	return ValidateContainerHost(info, rt.IsPodman, runtime.GOOS, logOut)
}

// InspectContainerHost queries the container engine for its kernel, memory, and
// rootless security options. Note that on macOS (Colima, OrbStack, Docker Desktop,
// Podman Machine), `info` reports the Linux VM's kernel and RAM rather than XNU.
func InspectContainerHost(ctx context.Context, r Runner, rt ContainerRuntime) (ContainerHostInfo, bool) {
	// Docker and Podman expose slightly different Go template schemas for `info`.
	format := "{{.KernelVersion}}|{{.MemTotal}}|{{.SecurityOptions}}"
	if rt.IsPodman {
		format = "{{.Host.Kernel}}|{{.Host.MemTotal}}|{{.Host.Security.Rootless}}"
	}
	res, err := r.Run(ctx, Spec{
		Name: rt.Binary,
		Args: []string{"info", "--format", format},
	})
	if err != nil {
		return ContainerHostInfo{}, false
	}
	line := strings.TrimSpace(res.Stdout)
	if line == "" {
		return ContainerHostInfo{}, false
	}
	parts := strings.SplitN(line, "|", 3)
	if len(parts) < 3 {
		return ContainerHostInfo{}, false
	}
	memBytes, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	secField := strings.ToLower(strings.TrimSpace(parts[2]))
	// Podman prints "true"/"false" for Host.Security.Rootless; Docker includes
	// "name=rootless" inside its SecurityOptions slice.
	rootless := secField == "true" || strings.Contains(secField, "name=rootless")
	return ContainerHostInfo{
		KernelVersion: strings.TrimSpace(parts[0]),
		MemTotalBytes: memBytes,
		Rootless:      rootless,
	}, true
}

// ValidateContainerHost checks kernel (>= 6.8), rootful daemon mode, and memory
// capacity against Agent Substrate's local cluster requirements.
func ValidateContainerHost(info ContainerHostInfo, isPodman bool, hostOS string, logOut io.Writer) error {
	// Check 1: Rootless container engines cannot apply Kind node network sysctls
	// (`proxy_arp=1`) or run Substrate's privileged `atelet` gVisor DaemonSet.
	if info.Rootless {
		if isPodman {
			return fmt.Errorf("rootless Podman is not supported for 'ax local' (Kind node sysctls and Substrate's atelet DaemonSet require rootful mode): switch Podman Machine to rootful ('podman machine stop && podman machine set --rootful && podman machine start') or use Colima / OrbStack / Docker Desktop")
		}
		return fmt.Errorf("rootless Docker is not supported for 'ax local' (Kind node sysctls and Substrate's atelet DaemonSet require rootful mode): use a rootful Docker engine (or Colima / OrbStack / Docker Desktop on macOS)")
	}

	// Check 2: Substrate's image cache mounts layers using Linux 6.8's `fsconfig`
	// OverlayFS `lowerdir+` API; older kernels fail with EINVAL only when the
	// first sandbox starts, so fail fast before creating the Kind cluster.
	if maj, min, ok := parseKernelMajorMinor(info.KernelVersion); ok {
		if maj < minKernelMajor || (maj == minKernelMajor && min < minKernelMinor) {
			vmHint := "upgrade your host Linux kernel to >= 6.8"
			if hostOS == "darwin" {
				vmHint = "upgrade your macOS container Linux VM (Colima / OrbStack / Docker Desktop) to a Linux >= 6.8 kernel"
			}
			return fmt.Errorf("container host Linux kernel %q is older than %d.%d (required by Agent Substrate's OverlayFS lowerdir+ image cache): %s",
				info.KernelVersion, minKernelMajor, minKernelMinor, vmHint)
		}
	}

	// Check 3: Ensure the container host / VM has enough memory for K8s +
	// Substrate + Postgres + AX (Colima's default 2 GiB VM will OOM/Pend pods).
	if info.MemTotalBytes > 0 {
		gib := float64(info.MemTotalBytes) / (1024 * 1024 * 1024)
		if info.MemTotalBytes < minHostMemoryBytes {
			resizeHint := "allocate at least 6 GiB of RAM to the container host"
			if hostOS == "darwin" {
				resizeHint = "allocate at least 6–8 GiB of RAM to your macOS container VM (e.g. 'colima stop && colima start --cpu 4 --memory 8' or Docker Desktop -> Settings -> Resources)"
			}
			return fmt.Errorf("container host has only %.1f GiB RAM (< 4.0 GiB minimum required for Kubernetes + Agent Substrate + AX): %s", gib, resizeHint)
		}
		if info.MemTotalBytes < recommendedHostMemoryBytes && logOut != nil {
			fmt.Fprintf(logOut, "==> Warning: container host has %.1f GiB RAM (< 6.0 GiB recommended); pods may stay Pending under load.\n", gib)
		}
	}
	return nil
}

func parseKernelMajorMinor(kernel string) (major, minor int, ok bool) {
	m := kernelMajorMin.FindStringSubmatch(kernel)
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

// -----------------------------------------------------------------------------
// 3. Auto-Bootstrapping `kubectl` and `kind` into ~/.ax/bin
// -----------------------------------------------------------------------------

// EnsureKubectl locates kubectl on PATH first, then ~/.ax/bin (checksum-verified),
// downloading and SHA-256-verifying a pinned release binary into ~/.ax/bin if absent.
func EnsureKubectl(ctx context.Context, r Runner, urls ToolURLs, logOut io.Writer) (string, error) {
	if p, err := r.LookPath("kubectl"); err == nil {
		return p, nil
	}

	expectedSHA, err := expectedToolSHA256(urls.KubectlSHA256, pinnedKubectlSHA256, "kubectl")
	if err != nil {
		return "", err
	}
	if dir, err := BinDir(); err == nil {
		localBin := filepath.Join(dir, "kubectl")
		if isExecutable(localBin) && fileMatchesSHA256(localBin, expectedSHA) {
			return localBin, nil
		}
	}

	// Fall back to downloading a pinned release binary into user-owned ~/.ax/bin
	// (no sudo or global PATH mutation required).
	dir, err := BinDir()
	if err != nil {
		return "", fmt.Errorf("kubectl not found and cannot create ~/.ax/bin: %w", err)
	}
	baseURL := urls.KubectlBaseURL
	if baseURL == "" {
		baseURL = DefaultToolURLs().KubectlBaseURL
	}
	dlURL := fmt.Sprintf("%s/%s/bin/%s/%s/kubectl",
		strings.TrimRight(baseURL, "/"), defaultKubectlVersion, runtime.GOOS, runtime.GOARCH)
	dest := filepath.Join(dir, "kubectl")
	if logOut != nil {
		fmt.Fprintf(logOut, "Downloading kubectl %s to %s...\n", defaultKubectlVersion, dest)
	}
	if err := downloadExecutable(ctx, dlURL, dest, expectedSHA); err != nil {
		return "", fmt.Errorf("downloading kubectl: %w", err)
	}
	return dest, nil
}

// EnsureKind locates a Kubernetes 1.36-compatible kind CLI (>= v0.32.0) on PATH
// first, then ~/.ax/bin (checksum-verified), downloading and SHA-256-verifying
// defaultKindVersion into ~/.ax/bin if missing or too old (for example, hosts
// shipping kind 0.11.x).
func EnsureKind(ctx context.Context, r Runner, urls ToolURLs, logOut io.Writer) (string, error) {
	if p, err := r.LookPath("kind"); err == nil && kindMeetsMinVersion(ctx, r, p) {
		return p, nil
	}

	expectedSHA, err := expectedToolSHA256(urls.KindSHA256, pinnedKindSHA256, "kind")
	if err != nil {
		return "", err
	}
	if dir, err := BinDir(); err == nil {
		localBin := filepath.Join(dir, "kind")
		if isExecutable(localBin) && fileMatchesSHA256(localBin, expectedSHA) && kindMeetsMinVersion(ctx, r, localBin) {
			return localBin, nil
		}
	}

	dir, err := BinDir()
	if err != nil {
		return "", fmt.Errorf("compatible kind (>= v0.%d.0) not found and cannot create ~/.ax/bin: %w", minKindMinorVersion, err)
	}
	baseURL := urls.KindBaseURL
	if baseURL == "" {
		baseURL = DefaultToolURLs().KindBaseURL
	}
	dlURL := fmt.Sprintf("%s/%s/kind-%s-%s",
		strings.TrimRight(baseURL, "/"), defaultKindVersion, runtime.GOOS, runtime.GOARCH)
	dest := filepath.Join(dir, "kind")
	if logOut != nil {
		fmt.Fprintf(logOut, "Downloading kind %s to %s...\n", defaultKindVersion, dest)
	}
	if err := downloadExecutable(ctx, dlURL, dest, expectedSHA); err != nil {
		return "", fmt.Errorf("downloading kind: %w", err)
	}
	return dest, nil
}

// ResolveStudioBinary locates the standalone `ax-studio` binary installed by
// `install.sh`, checking $AX_STUDIO_BIN first, then ~/.ax/bin/ax-studio, then PATH.
func ResolveStudioBinary(r Runner) (string, error) {
	if r == nil {
		r = OSRunner{}
	}
	if explicit := strings.TrimSpace(os.Getenv("AX_STUDIO_BIN")); explicit != "" {
		if isExecutable(explicit) {
			return explicit, nil
		}
		if p, err := r.LookPath(explicit); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("AX_STUDIO_BIN=%q is not an executable file", explicit)
	}
	if dir, err := BinDir(); err == nil {
		localBin := filepath.Join(dir, "ax-studio")
		if isExecutable(localBin) {
			return localBin, nil
		}
	}
	if p, err := r.LookPath("ax-studio"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("ax-studio binary not found in ~/.ax/bin or PATH: re-run install.sh or download from https://github.com/agentexecutor/studio/releases")
}

func expectedToolSHA256(override string, table map[string]string, tool string) (string, error) {
	if s := strings.ToLower(strings.TrimSpace(override)); s != "" {
		return s, nil
	}
	plat := runtime.GOOS + "/" + runtime.GOARCH
	sum, ok := table[plat]
	if !ok || sum == "" {
		return "", fmt.Errorf("no pinned SHA-256 digest configured for %s on %s", tool, plat)
	}
	return sum, nil
}

func fileMatchesSHA256(path, expectedHex string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == strings.ToLower(strings.TrimSpace(expectedHex))
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

// downloadExecutable streams url to a unique temporary sibling file, verifies
// its SHA-256 digest against expectedSHA256 before setting executable bits or
// renaming, and atomically moves it into place.
func downloadExecutable(ctx context.Context, url, dest, expectedSHA256 string) error {
	expectedSHA256 = strings.ToLower(strings.TrimSpace(expectedSHA256))
	if expectedSHA256 == "" {
		return fmt.Errorf("expected SHA-256 digest is required for %s", url)
	}

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

	f, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if actualSHA256 != expectedSHA256 {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("SHA-256 checksum mismatch for %s (expected %s, got %s)", url, expectedSHA256, actualSHA256)
	}
	if err := f.Chmod(0o755); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
