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

package geminienterprise

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// unzipCaps bounds a defensive local unzip of an untrusted skill payload. Even
// though the registry validates payloads server-side, the local unzip must
// independently guard against Zip-Slip (entries with "..", absolute paths, or
// symlinks) and resource exhaustion.
type unzipCaps struct {
	// MaxFiles caps the number of entries per skill archive.
	MaxFiles int
	// MaxTotalUnzippedBytes caps the total unzipped size per skill archive.
	MaxTotalUnzippedBytes int64
	// MaxDepth caps directory nesting depth within a skill archive.
	MaxDepth int
}

// The registry's documented payload caps, used as defaults.
const (
	defaultMaxFiles      = 10_000
	defaultMaxTotalBytes = 500 << 20 // 500 MiB
	defaultMaxDepth      = 8
)

// withDefaults fills any zero-valued cap with the registry's documented limit.
func (c unzipCaps) withDefaults() unzipCaps {
	if c.MaxFiles <= 0 {
		c.MaxFiles = defaultMaxFiles
	}
	if c.MaxTotalUnzippedBytes <= 0 {
		c.MaxTotalUnzippedBytes = defaultMaxTotalBytes
	}
	if c.MaxDepth <= 0 {
		c.MaxDepth = defaultMaxDepth
	}
	return c
}

// safeUnzip extracts a zip archive into destDir, defensively rejecting unsafe
// entries and enforcing caps. destDir must not exist yet or must be safe to
// write into; callers clear it beforehand.
//
// Guards:
//   - Zip-Slip: every entry must resolve to a path inside destDir.
//   - Absolute paths and paths containing ".." are rejected.
//   - Symlinks (and any non-regular, non-dir mode) are rejected.
//   - MaxFiles / MaxDepth / MaxTotalUnzippedBytes are enforced; the running
//     total is checked while copying so a lying uncompressed-size can't be used
//     to blow past the cap.
func safeUnzip(archive []byte, destDir string, caps unzipCaps) error {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return fmt.Errorf("opening zip: %w", err)
	}
	if len(zr.File) > caps.MaxFiles {
		return fmt.Errorf("archive has %d entries, exceeds cap %d", len(zr.File), caps.MaxFiles)
	}

	// Resolve destDir to an absolute, clean base for containment checks.
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolving dest: %w", err)
	}
	if err := os.MkdirAll(absDest, 0o755); err != nil {
		return fmt.Errorf("creating dest: %w", err)
	}

	var total int64
	for _, f := range zr.File {
		if err := validateEntryName(f.Name, caps.MaxDepth); err != nil {
			return err
		}
		// Reject symlinks and any special modes; only dirs and regular files.
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 {
			return fmt.Errorf("entry %q is a symlink (rejected)", f.Name)
		}
		if !mode.IsDir() && !mode.IsRegular() {
			return fmt.Errorf("entry %q has unsupported mode %v (rejected)", f.Name, mode)
		}

		target := filepath.Join(absDest, filepath.FromSlash(f.Name))
		// Containment: target must be within absDest.
		if !withinBase(absDest, target) {
			return fmt.Errorf("entry %q escapes destination (zip-slip)", f.Name)
		}

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("creating dir %q: %w", f.Name, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("creating parent of %q: %w", f.Name, err)
		}
		written, err := writeCappedFile(f, target, caps.MaxTotalUnzippedBytes-total)
		if err != nil {
			return err
		}
		total += written
		if total > caps.MaxTotalUnzippedBytes {
			return fmt.Errorf("archive exceeds unzipped size cap %d bytes", caps.MaxTotalUnzippedBytes)
		}
	}
	return nil
}

// validateEntryName rejects absolute paths, "..", and over-deep nesting.
func validateEntryName(name string, maxDepth int) error {
	if name == "" {
		return fmt.Errorf("empty entry name")
	}
	// Reject Windows-style and POSIX absolute paths.
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
		return fmt.Errorf("entry %q is an absolute path (rejected)", name)
	}
	slashed := strings.ReplaceAll(name, `\`, "/")
	for _, seg := range strings.Split(slashed, "/") {
		if seg == ".." {
			return fmt.Errorf("entry %q contains '..' (rejected)", name)
		}
	}
	depth := 0
	for _, seg := range strings.Split(strings.Trim(slashed, "/"), "/") {
		if seg != "" && seg != "." {
			depth++
		}
	}
	if depth > maxDepth {
		return fmt.Errorf("entry %q nesting depth %d exceeds cap %d", name, depth, maxDepth)
	}
	return nil
}

// withinBase reports whether target is inside base (after cleaning).
func withinBase(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// writeCappedFile copies one zip entry to disk, refusing to write more than
// remaining bytes (so a mismatched declared size can't overrun the cap).
func writeCappedFile(f *zip.File, target string, remaining int64) (int64, error) {
	if remaining < 0 {
		return 0, fmt.Errorf("unzipped size cap exceeded before %q", f.Name)
	}
	rc, err := f.Open()
	if err != nil {
		return 0, fmt.Errorf("opening entry %q: %w", f.Name, err)
	}
	defer rc.Close()

	// Preserve the archive entry's permission bits so executable skill scripts
	// (e.g. under scripts/) stay executable after extraction. Chmod after create
	// so the mode sticks regardless of the process umask.
	perm := f.Mode().Perm()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return 0, fmt.Errorf("creating file %q: %w", f.Name, err)
	}
	defer out.Close()
	if err := out.Chmod(perm); err != nil {
		return 0, fmt.Errorf("setting mode on %q: %w", f.Name, err)
	}

	// Limit the copy to remaining+1 so we can detect overrun deterministically.
	n, err := io.Copy(out, io.LimitReader(rc, remaining+1))
	if err != nil {
		return n, fmt.Errorf("writing %q: %w", f.Name, err)
	}
	if n > remaining {
		return n, fmt.Errorf("entry %q exceeds remaining unzipped size budget", f.Name)
	}
	return n, nil
}
