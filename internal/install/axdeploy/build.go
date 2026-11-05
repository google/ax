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

package axdeploy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/ax/internal/install/executil"
	"github.com/google/go-containerregistry/pkg/name"
)

// BuildOptions configures `--from-source` image building and pushing.
type BuildOptions struct {
	SourceDir  string
	ImageRepo  string
	ImageTag   string
	Arch       string
	Runner     executil.Runner
	HTTPClient *http.Client
	Out        io.Writer
}

// BuiltImages holds the pushed, digest-pinned AX container image references.
type BuiltImages struct {
	ServerImage     string
	TaskRunnerImage string
}

// BuildAndPushFromSource cross-compiles ax-server and ax-task-runner from
// SourceDir, builds their container images, pushes them to ImageRepo, and
// returns digest-pinned image references.
func BuildAndPushFromSource(ctx context.Context, opts BuildOptions) (BuiltImages, error) {
	r := opts.Runner
	if r == nil {
		r = executil.OSRunner{}
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	srcDir, err := filepath.Abs(opts.SourceDir)
	if err != nil {
		return BuiltImages{}, err
	}
	for _, reqFile := range []string{"go.mod", "Dockerfile.server", "Dockerfile.task-runner"} {
		if _, err := os.Stat(filepath.Join(srcDir, reqFile)); err != nil {
			return BuiltImages{}, fmt.Errorf("--from-source directory %q is missing %s", srcDir, reqFile)
		}
	}

	repo := strings.TrimRight(strings.TrimSpace(opts.ImageRepo), "/")
	if repo == "" {
		return BuiltImages{}, fmt.Errorf("image repository is required for --from-source (pass --image-repo for remote clusters)")
	}
	tag := strings.TrimSpace(opts.ImageTag)
	if tag == "" {
		tag = "local"
	}
	arch := opts.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}

	goBin, err := r.LookPath("go")
	if err != nil {
		return BuiltImages{}, fmt.Errorf("--from-source requires Go toolchain in PATH: %w", err)
	}
	rt, err := executil.ResolveContainerRuntime(ctx, r)
	if err != nil {
		return BuiltImages{}, err
	}

	outBinDir := filepath.Join(srcDir, "bin", "linux_"+arch)
	if err := os.MkdirAll(outBinDir, 0o755); err != nil {
		return BuiltImages{}, err
	}

	fmt.Fprintf(out, "==> Cross-compiling ax-server and ax-task-runner (linux/%s) from %s...\n", arch, srcDir)
	buildEnv := []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + arch, "GOTOOLCHAIN=auto"}
	for _, target := range []struct {
		bin string
		pkg string
	}{
		{"ax-server", "./cmd/ax-server"},
		{"ax-task-runner", "./cmd/ax-task-runner"},
	} {
		if _, err := r.Run(ctx, executil.Spec{
			Name: goBin,
			Args: []string{"build", "-trimpath", "-ldflags=-s -w", "-o", filepath.Join(outBinDir, target.bin), target.pkg},
			Dir:  srcDir,
			Env:  buildEnv,
		}); err != nil {
			return BuiltImages{}, fmt.Errorf("compiling %s: %w", target.bin, err)
		}
	}

	srvTag := fmt.Sprintf("%s/ax-server:%s", repo, tag)
	runnerTag := fmt.Sprintf("%s/ax-task-runner:%s", repo, tag)

	for _, img := range []struct {
		tag        string
		dockerfile string
	}{
		{srvTag, "Dockerfile.server"},
		{runnerTag, "Dockerfile.task-runner"},
	} {
		fmt.Fprintf(out, "==> Building and pushing %s...\n", img.tag)
		if _, err := r.Run(ctx, executil.Spec{
			Name: rt.Binary,
			Args: []string{
				"build",
				"--platform", "linux/" + arch,
				"--build-arg", "TARGETARCH=" + arch,
				"-t", img.tag,
				"-f", filepath.Join(srcDir, img.dockerfile),
				srcDir,
			},
		}); err != nil {
			return BuiltImages{}, fmt.Errorf("building %s: %w", img.tag, err)
		}
		if _, err := r.Run(ctx, executil.Spec{
			Name: rt.Binary,
			Args: []string{"push", img.tag},
		}); err != nil {
			return BuiltImages{}, fmt.Errorf("pushing %s: %w", img.tag, err)
		}
	}

	srvPinned, err := resolvePushedDigest(ctx, r, rt, srvTag, opts.HTTPClient)
	if err != nil {
		return BuiltImages{}, err
	}
	runnerPinned, err := resolvePushedDigest(ctx, r, rt, runnerTag, opts.HTTPClient)
	if err != nil {
		return BuiltImages{}, err
	}

	return BuiltImages{
		ServerImage:     srvPinned,
		TaskRunnerImage: runnerPinned,
	}, nil
}

func resolvePushedDigest(ctx context.Context, r executil.Runner, rt executil.ContainerRuntime, taggedRef string, httpClient *http.Client) (string, error) {
	// Prefer local container inspect first, then fall back to crane OCI v2 HEAD.
	if tagRef, err := name.NewTag(taggedRef); err == nil {
		if res, err := r.Run(ctx, executil.Spec{
			Name: rt.Binary,
			Args: []string{"image", "inspect", "--format", "{{range .RepoDigests}}{{println .}}{{end}}", taggedRef},
		}); err == nil {
			for _, line := range strings.Split(res.Stdout, "\n") {
				if d, err := name.NewDigest(strings.TrimSpace(line)); err == nil && d.Context().Name() == tagRef.Context().Name() {
					return taggedRef + "@" + d.DigestStr(), nil
				}
			}
		}
	}
	return EnsureDigestPinned(ctx, taggedRef, httpClient)
}
