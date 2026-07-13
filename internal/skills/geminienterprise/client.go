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

// This file is the internal Gemini Enterprise Skill Registry transport client: it talks to the
// Vertex AI v1beta1 REST API (ListSkills / GetSkill / GetSkillRevision /
// skills:retrieve), fetches skill payloads, and safe-unzips them to disk. The
// public, config-driven orchestration lives in materialize.go.

package geminienterprise

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// cloudPlatformScope is the OAuth2 scope required to call Vertex AI (read path
// needs roles/aiplatform.viewer).
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// apiVersion is the Vertex AI API version the Skill Registry is exposed under.
const apiVersion = "v1beta1"

// defaultHTTPTimeout bounds a single registry HTTP call.
const defaultHTTPTimeout = 60 * time.Second

// client fetches a selected set of skills from the Gemini Enterprise Skill Registry and
// writes them into a target directory as agentskills.io skill folders.
type client struct {
	baseURL string // .../v1beta1/projects/{project}/locations/{location}/skills
	http    *http.Client
	ts      oauth2.TokenSource
	caps    unzipCaps
}

// clientOptions configures a client. newClient fills defaults for optional fields.
type clientOptions struct {
	// --- Required ---

	// Project is the Google Cloud project that owns the skills.
	Project string
	// Location is the registry region, e.g. "us-central1". Selects the Vertex
	// host (https://{Location}-aiplatform.googleapis.com) unless Endpoint is set.
	Location string

	// --- Optional (newClient fills defaults) ---

	// Endpoint overrides the API host (scheme+host, no trailing slash), e.g. a
	// sandbox host or an httptest.Server URL in unit tests.
	Endpoint string
	// HTTPClient is the client used for all registry calls. Defaults to
	// http.Client{Timeout: 60s}.
	HTTPClient *http.Client
	// TokenSource provides the OAuth2 bearer token. If nil, ADC is used
	// (roles/aiplatform.viewer). Set to a static source in tests.
	TokenSource oauth2.TokenSource
	// Caps bounds a defensive local unzip of untrusted payloads.
	Caps unzipCaps
}

// newClient constructs a client from options. It validates required fields and
// fills defaults; it performs no network I/O.
func newClient(opts clientOptions) (*client, error) {
	if opts.Project == "" {
		return nil, errors.New("registry: Project is required")
	}
	if opts.Location == "" {
		return nil, errors.New("registry: Location is required")
	}

	host := opts.Endpoint
	if host == "" {
		host = fmt.Sprintf("https://%s-aiplatform.googleapis.com", opts.Location)
	}
	host = strings.TrimSuffix(host, "/")
	baseURL := fmt.Sprintf("%s/%s/projects/%s/locations/%s/skills",
		host, apiVersion, opts.Project, opts.Location)

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}

	ts := opts.TokenSource
	if ts == nil {
		creds, err := google.FindDefaultCredentials(context.Background(), cloudPlatformScope)
		if err != nil {
			return nil, fmt.Errorf("registry: finding application default credentials: %w", err)
		}
		ts = creds.TokenSource
	}

	return &client{
		baseURL: baseURL,
		http:    httpClient,
		ts:      ts,
		caps:    opts.Caps.withDefaults(),
	}, nil
}

// selection describes WHICH skills to fetch. Exactly one of the three modes
// should be set (all / by-id / by-query).
type selection struct {
	// All fetches every skill returned by ListSkills for the project/location.
	All bool
	// SkillRefs is an explicit allowlist, each optionally pinned to a revision.
	SkillRefs []skillRef
	// Query is a semantic search string; the top matches are fetched. TopK bounds
	// the result count (<=0 means the server default).
	Query string
	TopK  int
}

// skillRef identifies a single skill to fetch, optionally pinned.
type skillRef struct {
	SkillID  string
	Revision string // empty => latest default revision
}

// fetchResult reports the outcome of a fetch call. fetch is fail-safe: a skill
// that cannot be fetched or unzipped is recorded in skipped.
type fetchResult struct {
	materialized []MaterializedSkill
	skipped      []skippedSkill
}

// skippedSkill records a skill that was intentionally not written, and why.
type skippedSkill struct {
	skillID string
	reason  error
}

// fetch resolves the selection, fetches each skill's payload, and safe-unzips it
// into targetDir as <targetDir>/<skillID>/... targetDir is created if missing.
//
// claimed enforces first-wins across the whole operation: a skill id whose
// (targetDir, id) was already materialized (by this registry's own selection or
// an earlier registry sharing the dir) is skipped with a warning rather than
// overwriting the winner. regIdx is the registry's index, for log context.
//
// It is fail-safe: individual skill failures are collected in fetchResult.skipped;
// fetch returns a non-nil error only for whole-operation failures.
func (c *client) fetch(ctx context.Context, sel selection, targetDir string, claimed *claimSet, regIdx int) (*fetchResult, error) {
	refs, err := c.resolveSelection(ctx, sel)
	if err != nil {
		return nil, fmt.Errorf("registry: resolving selection: %w", err)
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return nil, fmt.Errorf("registry: creating target dir %q: %w", targetDir, err)
	}

	res := &fetchResult{}
	for _, ref := range refs {
		// First-wins: skip (with a warning) any id already materialized into this
		// dir, rather than overwriting it.
		if won, byRegistry := claimed.claim(targetDir, ref.SkillID, regIdx); !won {
			log.Printf("skills: registries[%d] skill %q already materialized into %s by registries[%d]; keeping the first, skipping this one",
				regIdx, ref.SkillID, targetDir, byRegistry)
			continue
		}
		mat, err := c.fetchAndWrite(ctx, ref, targetDir)
		if err != nil {
			res.skipped = append(res.skipped, skippedSkill{skillID: ref.SkillID, reason: err})
			continue
		}
		res.materialized = append(res.materialized, *mat)
	}
	return res, nil
}

// resolveSelection turns a selection into a concrete list of skillRefs to fetch.
func (c *client) resolveSelection(ctx context.Context, sel selection) ([]skillRef, error) {
	switch {
	case len(sel.SkillRefs) > 0:
		return sel.SkillRefs, nil
	case sel.Query != "":
		ids, err := c.retrieveSkillIDs(ctx, sel.Query, sel.TopK)
		if err != nil {
			return nil, err
		}
		return toRefs(ids), nil
	case sel.All:
		ids, err := c.listSkillIDs(ctx)
		if err != nil {
			return nil, err
		}
		return toRefs(ids), nil
	default:
		return nil, errors.New("empty selection: set All, SkillRefs, or Query")
	}
}

// fetchAndWrite fetches one skill's payload (latest or pinned) and unzips it.
func (c *client) fetchAndWrite(ctx context.Context, ref skillRef, targetDir string) (*MaterializedSkill, error) {
	if ref.SkillID == "" {
		return nil, errors.New("skill ref has empty SkillID")
	}
	payloadB64, revision, err := c.fetchPayload(ctx, ref)
	if err != nil {
		return nil, err
	}
	zipped, err := base64.StdEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, fmt.Errorf("decoding payload: %w", err)
	}
	skillDir := filepath.Join(targetDir, ref.SkillID)
	// Replace any prior materialization of this skill so stale files don't linger.
	if err := os.RemoveAll(skillDir); err != nil {
		return nil, fmt.Errorf("clearing %q: %w", skillDir, err)
	}
	if err := safeUnzip(zipped, skillDir, c.caps); err != nil {
		// Leave no partial dir behind on failure.
		_ = os.RemoveAll(skillDir)
		return nil, fmt.Errorf("unzipping: %w", err)
	}
	return &MaterializedSkill{SkillID: ref.SkillID, Revision: revision, Dir: skillDir}, nil
}

// fetchPayload returns the base64 zippedFilesystem and the concrete revision id
// for a skill ref (GetSkill for latest, GetSkillRevision when pinned).
func (c *client) fetchPayload(ctx context.Context, ref skillRef) (payloadB64, revision string, err error) {
	if ref.Revision != "" {
		// GetSkillRevision: payload is nested under "skill".
		var resp skillRevisionResponse
		if err := c.getJSON(ctx, c.baseURL+"/"+ref.SkillID+"/revisions/"+ref.Revision, &resp); err != nil {
			return "", "", err
		}
		if resp.Skill.ZippedFilesystem == "" {
			return "", "", errors.New("GetSkillRevision: empty zippedFilesystem")
		}
		return resp.Skill.ZippedFilesystem, ref.Revision, nil
	}
	// GetSkill: payload at top level.
	var resp skillResponse
	if err := c.getJSON(ctx, c.baseURL+"/"+ref.SkillID, &resp); err != nil {
		return "", "", err
	}
	if resp.ZippedFilesystem == "" {
		return "", "", errors.New("GetSkill: empty zippedFilesystem")
	}
	return resp.ZippedFilesystem, resp.currentRevision(), nil
}

// listSkillIDs pages through ListSkills and returns all skill ids.
func (c *client) listSkillIDs(ctx context.Context) ([]string, error) {
	var ids []string
	pageToken := ""
	for {
		u := c.baseURL
		if pageToken != "" {
			u += "?pageToken=" + url.QueryEscape(pageToken)
		}
		var resp listSkillsResponse
		if err := c.getJSON(ctx, u, &resp); err != nil {
			return nil, err
		}
		for _, s := range resp.Skills {
			if id := s.id(); id != "" {
				ids = append(ids, id)
			}
		}
		if resp.NextPageToken == "" {
			return ids, nil
		}
		pageToken = resp.NextPageToken
	}
}

// retrieveSkillIDs runs semantic search (skills:retrieve) and returns skill ids.
func (c *client) retrieveSkillIDs(ctx context.Context, query string, topK int) ([]string, error) {
	u := c.baseURL + ":retrieve?query=" + url.QueryEscape(query)
	if topK > 0 {
		u += fmt.Sprintf("&topK=%d", topK)
	}
	var resp retrieveSkillsResponse
	if err := c.getJSON(ctx, u, &resp); err != nil {
		return nil, err
	}
	var ids []string
	for _, r := range resp.RetrievedSkills {
		if id := lastSegment(r.SkillName); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// getJSON issues an authenticated GET and decodes the JSON body into out.
func (c *client) getJSON(ctx context.Context, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	tok, err := c.ts.Token()
	if err != nil {
		return fmt.Errorf("obtaining token: %w", err)
	}
	tok.SetAuthHeader(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 512))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// --- Wire types (AIP-standard shapes; see the design doc appendix). ---

type listSkillsResponse struct {
	Skills        []skillResponse `json:"skills"`
	NextPageToken string          `json:"nextPageToken"`
}

// retrieveSkillsResponse is a skills:retrieve result. Each hit carries the skill
// resource name in a flat "skillName" field (not a nested skill object).
type retrieveSkillsResponse struct {
	RetrievedSkills []struct {
		SkillName string `json:"skillName"` // projects/.../skills/{id}
	} `json:"retrievedSkills"`
}

// skillResponse is a Skill resource. GetSkill returns the payload at top level.
type skillResponse struct {
	Name             string `json:"name"` // projects/.../skills/{id}
	DisplayName      string `json:"displayName"`
	Description      string `json:"description"`
	State            string `json:"state"`
	DefaultRevision  string `json:"defaultRevision"`
	ZippedFilesystem string `json:"zippedFilesystem"`
}

// skillRevisionResponse is a GetSkillRevision result; the payload is nested
// under "skill".
type skillRevisionResponse struct {
	Name  string        `json:"name"` // projects/.../skills/{id}/revisions/{rev}
	Skill skillResponse `json:"skill"`
}

// id extracts the trailing skill id from a resource name.
func (s skillResponse) id() string { return lastSegment(s.Name) }

// currentRevision reports the skill's default revision id, if the server provided
// one (best-effort; may be empty for "latest").
func (s skillResponse) currentRevision() string { return lastSegment(s.DefaultRevision) }

func lastSegment(resourceName string) string {
	if resourceName == "" {
		return ""
	}
	parts := strings.Split(resourceName, "/")
	return parts[len(parts)-1]
}

func toRefs(ids []string) []skillRef {
	refs := make([]skillRef, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, skillRef{SkillID: id})
	}
	return refs
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
