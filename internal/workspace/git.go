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

package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

const (
	defaultGitRef      = "main"
	defaultRepoDirName = "repo"

	gitSuccessLog = "git-success.log"
	gitErrorLog   = "git-error.log"
	gitRetries    = 5
	gitRetryDelay = 2 * time.Second
)

// cloneRepos fetches each declared repository into targetPath.
func cloneRepos(ctx context.Context, repos []*v1alpha1.GitRepo, targetPath string) ([]string, error) {
	var cloned []string
	for _, repo := range repos {
		if repo == nil || repo.GetRepo() == "" {
			continue
		}

		dest, err := cloneDestination(repo, targetPath)
		if err != nil {
			return cloned, err
		}
		if err := os.MkdirAll(dest, dirPerm); err != nil {
			return cloned, fmt.Errorf("creating git checkout directory %s: %w", dest, err)
		}

		ref := repo.GetRef()
		if ref == "" {
			ref = defaultGitRef
		}

		slog.Info("initializing and fetching git repo", "repo", repo.GetRepo(), "ref", ref, "dir", dest, "depth", repo.GetDepth())
		out, err := retry(ctx, gitRetries, gitRetryDelay, func() ([]byte, error) {
			return fetchRepo(ctx, dest, repo.GetRepo(), ref, repo.GetDepth())
		})
		if err != nil {
			writeStateFile(gitErrorLog, fmt.Appendf(nil, "error: %v\noutput: %s\n", err, out))
			return cloned, fmt.Errorf("cloning %s (ref %s): %w", repo.GetRepo(), ref, err)
		}

		cloned = append(cloned, repo.GetRepo())
		writeStateFile(gitSuccessLog, out)
	}
	return cloned, nil
}

// cloneDestination resolves where a repository is checked out under targetPath.
//
// An explicit Dir wins: "." means the workspace root, and anything else must be
// relative to the workspace. Otherwise the directory is named after the repository's
// Name, unless that is empty or a generic placeholder such as "origin", in which
// case the name is derived from the repository URL.
func cloneDestination(repo *v1alpha1.GitRepo, targetPath string) (string, error) {
	rel := repo.GetDir()
	if rel == "" {
		name := repo.GetName()
		if name == "" || name == defaultRepoDirName || name == "origin" {
			if derived := RepoDirName(repo.GetRepo()); derived != "" {
				name = derived
			} else if name == "" {
				name = defaultRepoDirName
			}
		}
		rel = name
	}
	if rel == "." {
		return targetPath, nil
	}
	if !isRelativeWithinRoot(rel) {
		return "", fmt.Errorf("git checkout path %q must stay within workspace %s", rel, targetPath)
	}
	return filepath.Join(targetPath, rel), nil
}

// fetchRepo initializes dir as a git repository pointed at repoURL, fetches
// ref (branch, tag, or commit SHA), and checks it out. If depth > 0, it
// performs a shallow fetch. It is idempotent so it can be retried against a
// partially initialized directory. Arguments are passed directly to git, never
// through a shell.
func fetchRepo(ctx context.Context, dir, repoURL, ref string, depth int32) ([]byte, error) {
	var combined []byte
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		combined = append(combined, out...)
		if err != nil {
			return fmt.Errorf("git %s: %w", args[0], err)
		}
		return nil
	}

	if err := run("init"); err != nil {
		return combined, err
	}
	// A retry lands on an already initialized directory, so fall back to updating the remote.
	if err := run("remote", "add", "--", "origin", repoURL); err != nil {
		if err := run("remote", "set-url", "--", "origin", repoURL); err != nil {
			return combined, err
		}
	}
	fetchArgs := []string{"fetch"}
	if depth > 0 {
		fetchArgs = append(fetchArgs, fmt.Sprintf("--depth=%d", depth))
	}
	fetchArgs = append(fetchArgs, "--", "origin", ref)
	if err := run(fetchArgs...); err != nil {
		// Servers that disallow fetching unadvertised abbreviated commit SHAs
		// directly need a full fetch followed by checking out the target SHA.
		if !looksLikeCommitSHA(ref) {
			return combined, err
		}
		if fetchErr := run("fetch", "--", "origin"); fetchErr != nil {
			return combined, fetchErr
		}
		if coErr := run("checkout", "-f", ref, "--"); coErr != nil {
			return combined, coErr
		}
		return combined, nil
	}
	if err := run("checkout", "-f", "FETCH_HEAD", "--"); err != nil {
		return combined, err
	}
	return combined, nil
}

func looksLikeCommitSHA(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// retry runs op up to attempts times, waiting delay between failures. It stops early
// when ctx is done and never sleeps after the final attempt.
func retry(ctx context.Context, attempts int, delay time.Duration, op func() ([]byte, error)) ([]byte, error) {
	var (
		out []byte
		err error
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		out, err = op()
		if err == nil {
			return out, nil
		}
		if attempt == attempts {
			break
		}
		slog.Warn("git operation failed, will retry", "attempt", attempt, "max", attempts, "error", err, "output", string(out))
		select {
		case <-ctx.Done():
			return out, errors.Join(err, ctx.Err())
		case <-time.After(delay):
		}
	}
	return out, err
}

// RepoDirName extracts the repository name from a git URL or local path.
// For example "https://github.com/chalk/chalk.git" yields "chalk".
func RepoDirName(repoURL string) string {
	trimmed := strings.TrimSuffix(strings.TrimSpace(repoURL), "/")
	trimmed = strings.TrimSuffix(trimmed, ".git")
	idx := strings.LastIndexAny(trimmed, "/:")
	var name string
	if idx < 0 {
		name = trimmed
	} else {
		name = trimmed[idx+1:]
	}
	if name == "." || name == ".." {
		return ""
	}
	return name
}
