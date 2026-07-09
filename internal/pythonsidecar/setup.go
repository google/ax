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

	"github.com/google/ax/internal/config"
)

// SetupOptions configures asset extraction and environment setup for a Python sidecar.
type SetupOptions struct {
	// FS is the embedded filesystem containing Python assets (e.g., python.FS). (Required)
	FS fs.FS
}

// Setup extracts the embedded filesystem assets to TargetDir.
// It returns TargetDir, which can be set as PythonPath in Config.
func Setup(ctx context.Context, opts SetupOptions) (string, error) {
	if opts.FS == nil {
		return "", fmt.Errorf("SetupOptions.FS cannot be nil")
	}
	axDir, err := config.AXAssetsDir()
	if err != nil {
		return "", err
	}
	extractDir := filepath.Join(axDir, "python")
	reqPath := filepath.Join(extractDir, "antigravity", "requirements.txt")

	pythonPath := axDir + string(os.PathListSeparator) + extractDir
	updated, err := extractFS(ctx, opts.FS, extractDir)
	if err != nil {
		return "", fmt.Errorf("failed to extract embedded assets: %w", err)
	}

	pkgPath, err := install(ctx, reqPath, updated)
	if err != nil {
		return "", err
	}
	return pythonPath + string(os.PathListSeparator) + pkgPath, nil
}

func extractFS(ctx context.Context, filesystem fs.FS, destDir string) (bool, error) {
	var updated bool
	err := fs.WalkDir(filesystem, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		destPath := filepath.Join(destDir, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(destPath, 0755)
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		stat, err := os.Stat(destPath)
		// If the file already exists on disk with the exact same size,
		// skip copying to preserve its timestamp and avoid false re-installations.
		if err == nil {
			if stat.Size() == info.Size() {
				return nil
			}
		}

		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return fmt.Errorf("creating directory for %s: %w", destPath, err)
		}

		// There is at least, one file updated.
		updated = true
		src, err := filesystem.Open(path)
		if err != nil {
			return fmt.Errorf("opening embedded file %s: %w", path, err)
		}
		defer src.Close()

		mode := os.FileMode(0644)
		if info.Mode().Perm() != 0 {
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
	return updated, err
}

func install(ctx context.Context, reqPath string, install bool) (string, error) {
	pkgDir := filepath.Join(filepath.Dir(reqPath), "site-packages")
	if !install {
		// Make sure that pip install ran once and we have
		// the dependencies installed under site-packages.
		if _, err := os.Stat(pkgDir); err == nil {
			return pkgDir, nil
		}
	}
	fmt.Println("Setting up Antigravity SDK, this may take a while...")

	// Some corp machines are overriding the indexes, ensure that
	// simple index is included where Antigravity SDK is distributed from.
	cmd := exec.CommandContext(ctx, "python3", "-m", "pip", "install", "--extra-index-url", "https://pypi.org/simple", "--target", pkgDir, "-r", reqPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("pip install failed for %s: %w\nOutput:\n%s", reqPath, err, string(out))
	}
	return pkgDir, nil
}
