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
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// zipFixtureB64 builds an in-memory zip from name->content and returns the base64
// string the registry would return in `zippedFilesystem`.
func zipFixtureB64(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// rawZipFixture returns the raw zip bytes (for direct safeUnzip tests).
func rawZipFixture(t *testing.T, files map[string]string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(zipFixtureB64(t, files))
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return b
}

// newFakeClient returns a client pointed at an httptest.Server whose handler is
// provided by the caller, with a no-op token source.
func newFakeClient(t *testing.T, handler http.HandlerFunc) *client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := newClient(clientOptions{
		Project:     "test-project",
		Location:    "us-central1",
		Endpoint:    srv.URL,
		TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fake"}),
	})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	return c
}

func TestFetchByID_Latest(t *testing.T) {
	payload := zipFixtureB64(t, map[string]string{
		"SKILL.md":     "---\nname: emoji\n---\n# do stuff",
		"scripts/x.sh": "echo hi",
	})
	c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		// GetSkill: .../skills/emoji  -> zippedFilesystem at top level.
		if !strings.HasSuffix(r.URL.Path, "/skills/emoji") {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		writeJSON(t, w, skillResponse{
			Name:             "projects/p/locations/us-central1/skills/emoji",
			DefaultRevision:  "projects/p/locations/us-central1/skills/emoji/revisions/rev-7",
			ZippedFilesystem: payload,
		})
	})

	dir := t.TempDir()
	res, err := c.fetch(context.Background(),
		selection{SkillRefs: []skillRef{{SkillID: "emoji"}}}, dir, newClaimSet(), 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(res.skipped) != 0 {
		t.Fatalf("unexpected skipped: %+v", res.skipped)
	}
	if len(res.materialized) != 1 {
		t.Fatalf("materialized = %d, want 1", len(res.materialized))
	}
	got := res.materialized[0]
	if got.SkillID != "emoji" || got.Revision != "rev-7" {
		t.Errorf("got %+v, want SkillID=emoji Revision=rev-7", got)
	}
	assertFile(t, filepath.Join(dir, "emoji", "SKILL.md"), "---\nname: emoji\n---\n# do stuff")
	assertFile(t, filepath.Join(dir, "emoji", "scripts", "x.sh"), "echo hi")
}

func TestFetchByID_PinnedRevision(t *testing.T) {
	payload := zipFixtureB64(t, map[string]string{"SKILL.md": "pinned"})
	c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		// GetSkillRevision: .../skills/emoji/revisions/rev-3 -> nested under "skill".
		if !strings.HasSuffix(r.URL.Path, "/skills/emoji/revisions/rev-3") {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		writeJSON(t, w, skillRevisionResponse{
			Name:  "projects/p/locations/us-central1/skills/emoji/revisions/rev-3",
			Skill: skillResponse{ZippedFilesystem: payload},
		})
	})

	dir := t.TempDir()
	res, err := c.fetch(context.Background(),
		selection{SkillRefs: []skillRef{{SkillID: "emoji", Revision: "rev-3"}}}, dir, newClaimSet(), 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(res.materialized) != 1 || res.materialized[0].Revision != "rev-3" {
		t.Fatalf("got %+v, want one skill pinned at rev-3", res.materialized)
	}
	assertFile(t, filepath.Join(dir, "emoji", "SKILL.md"), "pinned")
}

func TestFetchAll_Paginated(t *testing.T) {
	pa := zipFixtureB64(t, map[string]string{"SKILL.md": "a"})
	pb := zipFixtureB64(t, map[string]string{"SKILL.md": "b"})
	c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/skills") && r.URL.Query().Get("pageToken") == "":
			writeJSON(t, w, listSkillsResponse{
				Skills:        []skillResponse{{Name: ".../skills/a"}},
				NextPageToken: "TOK",
			})
		case strings.HasSuffix(r.URL.Path, "/skills") && r.URL.Query().Get("pageToken") == "TOK":
			writeJSON(t, w, listSkillsResponse{Skills: []skillResponse{{Name: ".../skills/b"}}})
		case strings.HasSuffix(r.URL.Path, "/skills/a"):
			writeJSON(t, w, skillResponse{Name: ".../skills/a", ZippedFilesystem: pa})
		case strings.HasSuffix(r.URL.Path, "/skills/b"):
			writeJSON(t, w, skillResponse{Name: ".../skills/b", ZippedFilesystem: pb})
		default:
			http.Error(w, "unexpected "+r.URL.String(), http.StatusNotFound)
		}
	})

	dir := t.TempDir()
	res, err := c.fetch(context.Background(), selection{All: true}, dir, newClaimSet(), 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(res.materialized) != 2 {
		t.Fatalf("materialized = %d, want 2 (paged)", len(res.materialized))
	}
	assertFile(t, filepath.Join(dir, "a", "SKILL.md"), "a")
	assertFile(t, filepath.Join(dir, "b", "SKILL.md"), "b")
}

func TestFetchByQuery(t *testing.T) {
	payload := zipFixtureB64(t, map[string]string{"SKILL.md": "found"})
	c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/skills:retrieve"):
			if got := r.URL.Query().Get("query"); got != "emoji stuff" {
				t.Errorf("query = %q, want %q", got, "emoji stuff")
			}
			writeJSON(t, w, retrieveSkillsResponse{
				RetrievedSkills: []struct {
					SkillName string `json:"skillName"`
				}{{SkillName: ".../skills/emoji"}},
			})
		case strings.HasSuffix(r.URL.Path, "/skills/emoji"):
			writeJSON(t, w, skillResponse{Name: ".../skills/emoji", ZippedFilesystem: payload})
		default:
			http.Error(w, "unexpected "+r.URL.String(), http.StatusNotFound)
		}
	})

	dir := t.TempDir()
	res, err := c.fetch(context.Background(),
		selection{Query: "emoji stuff", TopK: 3}, dir, newClaimSet(), 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(res.materialized) != 1 {
		t.Fatalf("materialized = %d, want 1", len(res.materialized))
	}
	assertFile(t, filepath.Join(dir, "emoji", "SKILL.md"), "found")
}

func TestFetchFailSafe_OneBadSkillSkipped(t *testing.T) {
	good := zipFixtureB64(t, map[string]string{"SKILL.md": "ok"})
	c := newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/skills/good"):
			writeJSON(t, w, skillResponse{Name: ".../skills/good", ZippedFilesystem: good})
		case strings.HasSuffix(r.URL.Path, "/skills/bad"):
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	})

	dir := t.TempDir()
	res, err := c.fetch(context.Background(),
		selection{SkillRefs: []skillRef{{SkillID: "good"}, {SkillID: "bad"}}}, dir, newClaimSet(), 0)
	if err != nil {
		t.Fatalf("fetch returned whole-op error, want fail-safe: %v", err)
	}
	if len(res.materialized) != 1 || res.materialized[0].SkillID != "good" {
		t.Errorf("materialized = %+v, want only 'good'", res.materialized)
	}
	if len(res.skipped) != 1 || res.skipped[0].skillID != "bad" {
		t.Errorf("skipped = %+v, want only 'bad'", res.skipped)
	}
	// The bad skill left no directory behind.
	if _, err := os.Stat(filepath.Join(dir, "bad")); !os.IsNotExist(err) {
		t.Errorf("expected no dir for 'bad', stat err = %v", err)
	}
}

func TestFetch_FirstWinsAcrossSharedClaimSet(t *testing.T) {
	// Two fetches (simulating two registries) into the SAME dir with a SHARED
	// claim set. Both offer skill id "dup"; only the first should be written.
	payloadA := zipFixtureB64(t, map[string]string{"SKILL.md": "from-A"})
	payloadB := zipFixtureB64(t, map[string]string{"SKILL.md": "from-B"})

	makeClient := func(payload string) *client {
		return newFakeClient(t, func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/skills/dup") {
				http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
				return
			}
			writeJSON(t, w, skillResponse{Name: ".../skills/dup", ZippedFilesystem: payload})
		})
	}

	dir := t.TempDir()
	claimed := newClaimSet()
	sel := selection{SkillRefs: []skillRef{{SkillID: "dup"}}}

	// Registry 0 wins.
	res0, err := makeClient(payloadA).fetch(context.Background(), sel, dir, claimed, 0)
	if err != nil {
		t.Fatalf("fetch #0: %v", err)
	}
	if len(res0.materialized) != 1 {
		t.Fatalf("registry 0 materialized %d, want 1", len(res0.materialized))
	}

	// Registry 1 offers the same id into the same dir -> skipped (not written).
	res1, err := makeClient(payloadB).fetch(context.Background(), sel, dir, claimed, 1)
	if err != nil {
		t.Fatalf("fetch #1: %v", err)
	}
	if len(res1.materialized) != 0 {
		t.Errorf("registry 1 materialized %d, want 0 (first-wins)", len(res1.materialized))
	}
	// The content on disk must be the FIRST writer's.
	assertFile(t, filepath.Join(dir, "dup", "SKILL.md"), "from-A")
}

func TestNewClientValidation(t *testing.T) {
	if _, err := newClient(clientOptions{Location: "us-central1"}); err == nil {
		t.Error("expected error for missing Project")
	}
	if _, err := newClient(clientOptions{Project: "p"}); err == nil {
		t.Error("expected error for missing Location")
	}
}

// --- safeUnzip unit tests ---

func TestSafeUnzip_Basic(t *testing.T) {
	z := rawZipFixture(t, map[string]string{
		"SKILL.md":         "hi",
		"scripts/tool.sh":  "run",
		"references/a.txt": "ref",
	})
	dir := t.TempDir()
	if err := safeUnzip(z, filepath.Join(dir, "s"), unzipCaps{}.withDefaults()); err != nil {
		t.Fatalf("safeUnzip: %v", err)
	}
	assertFile(t, filepath.Join(dir, "s", "SKILL.md"), "hi")
	assertFile(t, filepath.Join(dir, "s", "scripts", "tool.sh"), "run")
}

func TestSafeUnzip_PreservesExecutableMode(t *testing.T) {
	// Build a zip with an executable script (0755) and a normal file (0644),
	// setting per-entry modes via CreateHeader.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	writeEntry := func(name, content string, mode os.FileMode) {
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		hdr.SetMode(mode)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatalf("CreateHeader %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	writeEntry("scripts/tool.sh", "#!/bin/sh\necho hi\n", 0o755)
	writeEntry("SKILL.md", "hi", 0o644)
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}

	dir := t.TempDir()
	if err := safeUnzip(buf.Bytes(), filepath.Join(dir, "s"), unzipCaps{}.withDefaults()); err != nil {
		t.Fatalf("safeUnzip: %v", err)
	}

	scriptInfo, err := os.Stat(filepath.Join(dir, "s", "scripts", "tool.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if scriptInfo.Mode().Perm()&0o100 == 0 {
		t.Errorf("script mode = %v, want owner-execute bit set (0755 preserved)", scriptInfo.Mode().Perm())
	}
	mdInfo, err := os.Stat(filepath.Join(dir, "s", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if mdInfo.Mode().Perm()&0o111 != 0 {
		t.Errorf("SKILL.md mode = %v, want no execute bits (0644)", mdInfo.Mode().Perm())
	}
}

func TestSafeUnzip_ZipSlipRejected(t *testing.T) {
	for _, bad := range []string{"../evil.txt", "a/../../evil.txt", "/etc/evil"} {
		z := rawZipFixture(t, map[string]string{bad: "x"})
		dir := t.TempDir()
		err := safeUnzip(z, filepath.Join(dir, "s"), unzipCaps{}.withDefaults())
		if err == nil {
			t.Errorf("entry %q: expected rejection, got nil", bad)
			continue
		}
		if _, statErr := os.Stat(filepath.Join(dir, "evil.txt")); !os.IsNotExist(statErr) {
			t.Errorf("entry %q: file escaped destination", bad)
		}
	}
}

func TestSafeUnzip_TooManyFiles(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 5; i++ {
		files[fmt.Sprintf("f%d.txt", i)] = "x"
	}
	z := rawZipFixture(t, files)
	err := safeUnzip(z, t.TempDir(), unzipCaps{MaxFiles: 3}.withDefaults())
	if err == nil || !strings.Contains(err.Error(), "exceeds cap") {
		t.Fatalf("expected MaxFiles cap error, got %v", err)
	}
}

func TestSafeUnzip_TooLarge(t *testing.T) {
	z := rawZipFixture(t, map[string]string{"big.txt": strings.Repeat("A", 1000)})
	err := safeUnzip(z, t.TempDir(), unzipCaps{MaxTotalUnzippedBytes: 100}.withDefaults())
	if err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("expected size cap error, got %v", err)
	}
}

func TestSafeUnzip_TooDeep(t *testing.T) {
	z := rawZipFixture(t, map[string]string{"a/b/c/d/e/f/g/h/i/j/deep.txt": "x"})
	err := safeUnzip(z, t.TempDir(), unzipCaps{MaxDepth: 3}.withDefaults())
	if err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("expected depth cap error, got %v", err)
	}
}

// --- helpers ---

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %q: %v", path, err)
	}
	if string(got) != want {
		t.Errorf("%q = %q, want %q", path, got, want)
	}
}
