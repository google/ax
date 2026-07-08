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

package pythonsidecar

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SetupOptions configures asset extraction and environment setup for a Python sidecar.
type SetupOptions struct {
	// FS is the embedded filesystem containing Python assets (e.g., python.FS). (Required)
	FS fs.FS
	// TargetDir is the directory on disk where assets will be extracted.
	// If empty, it defaults to filepath.Join(os.UserHomeDir(), ".ax"). (Optional)
	TargetDir string
}

// Setup extracts the embedded filesystem assets to TargetDir.
// It returns TargetDir, which can be set as PythonPath in Config.
func Setup(ctx context.Context, opts SetupOptions) (string, error) {
	if opts.FS == nil {
		return "", fmt.Errorf("SetupOptions.FS cannot be nil")
	}

	targetDir := opts.TargetDir
	if targetDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolving home directory for python target dir: %w", err)
		}
		targetDir = filepath.Join(home, ".ax")
	}

	extractDir := filepath.Join(targetDir, "python")
	reqPath := filepath.Join(extractDir, "antigravity", "requirements.txt")

	result := targetDir + string(os.PathListSeparator) + extractDir

	if err := extractFS(ctx, opts.FS, extractDir); err != nil {
		return "", fmt.Errorf("failed to extract embedded assets: %w", err)
	}

	pkgPath, err := install(ctx, reqPath)
	if err != nil {
		return "", err
	}

	return result + string(os.PathListSeparator) + pkgPath, nil
}

func extractFS(ctx context.Context, filesystem fs.FS, destDir string) error {
	return fs.WalkDir(filesystem, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if d.IsDir() {
			return nil
		}

		destPath := filepath.Join(destDir, filepath.FromSlash(path))
		rel, err := filepath.Rel(destDir, destPath)
		if err != nil || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." {
			return fmt.Errorf("illegal file path in embedded FS: %s", path)
		}

		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return fmt.Errorf("creating directory for %s: %w", destPath, err)
		}

		src, err := filesystem.Open(path)
		if err != nil {
			return fmt.Errorf("opening embedded file %s: %w", path, err)
		}
		defer src.Close()

		info, err := d.Info()
		mode := os.FileMode(0644)
		if err == nil && info.Mode().Perm() != 0 {
			mode = info.Mode().Perm() | 0200
		}

		_ = os.Chmod(destPath, 0644)

		dst, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			return fmt.Errorf("creating destination file %s: %w", destPath, err)
		}

		if _, err := io.Copy(dst, src); err != nil {
			_ = dst.Close()
			return fmt.Errorf("writing file %s: %w", destPath, err)
		}

		if err := dst.Close(); err != nil {
			return fmt.Errorf("closing file %s: %w", destPath, err)
		}

		return nil
	})
}

func install(ctx context.Context, reqPath string) (string, error) {
	pkgDir := filepath.Join(filepath.Dir(reqPath), "site-packages")
	cmd := exec.CommandContext(ctx, "python3", "-m", "pip", "install", "--extra-index-url", "https://pypi.org/simple", "--target", pkgDir, "-r", reqPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("pip install failed for %s: %w\nOutput:\n%s", reqPath, err, string(out))
	}
	return pkgDir, nil
}
