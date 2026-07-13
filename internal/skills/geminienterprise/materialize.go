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

// Package geminienterprise turns a harness's skills configuration into on-disk
// skill folders. It sources agentskills.io skills from the Gemini Enterprise
// Skill Registry (a managed, versioned catalog exposed over the Vertex AI
// v1beta1 REST API) and writes each skill to <target_dir>/<skill-id>/.
//
// It is harness-agnostic: it only writes files and reports what it wrote (see
// Result). It knows nothing about specific harnesses, SKILLS_DIR, or discovery
// pointers — callers decide how a given harness is told where its skills are.
//
// Scope: read-only. This package never creates, updates, or deletes registry
// skills (authoring is out of scope).
package geminienterprise

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/google/ax/internal/config"
)

// Environment fallbacks for project/location (the registry target_dir is a
// required config field, so it has no env fallback).
const (
	envCloudProject  = "GOOGLE_CLOUD_PROJECT"
	envCloudLocation = "GOOGLE_CLOUD_LOCATION"

	defaultRegistryLocation = "us-central1"
)

// Result reports what Materialize wrote: one Written entry per registry that
// produced skills, grouping the skills with the directory they landed in.
type Result struct {
	Written []Written
}

// Written groups the skills materialized from one registry with the directory
// they were written into (each skill at <Dir>/<skill-id>/).
type Written struct {
	Dir    string
	Skills []MaterializedSkill
}

// Empty reports whether nothing was materialized.
func (r Result) Empty() bool { return len(r.Written) == 0 }

// MaterializedSkill records one skill written to disk.
type MaterializedSkill struct {
	SkillID  string
	Revision string
	Dir      string // path of the written skill folder (<target_dir>/<skill-id>/)
}

// Materialize materializes every enabled registry in sc into its configured
// target_dir (each skill at <target_dir>/<skill-id>/) and reports what it wrote.
//
// target_dir is a required, validated config field (see config.SkillsConfig), so
// this does not fall back to any env var or the working directory. It is
// fail-safe: disabled registries are skipped, and any registry error is logged
// and swallowed so a skill problem never blocks harness creation. When the same
// skill id would be written into the same dir more than once (within one
// registry's selection, or across registries sharing a dir), the FIRST writer
// wins and later duplicates are skipped with a warning. Substrate/pod
// materialization is a separate, later path; this wires the local flow only.
func Materialize(ctx context.Context, sc config.SkillsConfig) Result {
	var res Result
	// claimed tracks (target_dir, skill-id) pairs already written across all
	// registries so the FIRST writer of an id into a dir wins; later duplicates
	// (within one registry's selection, or across registries sharing a dir) are
	// skipped with a warning instead of silently overwriting.
	claimed := newClaimSet()
	// TODO: fetches are sequential (both here and per-skill in client.fetch);
	// consider bounded-concurrent download/unzip to speed up materialization.
	// TODO: bound overall materialization with a deadline (only per-call HTTP
	// timeouts exist today); e.g. a default ~120s that users can override.
	for i := range sc.Registries {
		rc := sc.Registries[i]
		if !rc.Enabled {
			continue
		}
		project := firstNonEmpty(rc.Project, os.Getenv(envCloudProject))
		if project == "" {
			log.Printf("skills: registries[%d] enabled but no project (config or %s); skipping", i, envCloudProject)
			continue
		}
		location := firstNonEmpty(rc.Location, os.Getenv(envCloudLocation), defaultRegistryLocation)

		c, err := newClient(clientOptions{Project: project, Location: location})
		if err != nil {
			log.Printf("skills: registries[%d] client init failed: %v; skipping", i, err)
			continue
		}
		out, err := c.fetch(ctx, selectionFromConfig(rc), rc.TargetDir, claimed, i)
		if err != nil {
			log.Printf("skills: registries[%d] fetch failed: %v; continuing", i, err)
			continue
		}
		for _, s := range out.skipped {
			log.Printf("skills: registries[%d] skipped %q: %v", i, s.skillID, s.reason)
		}
		if len(out.materialized) == 0 {
			continue
		}
		log.Printf("skills: registries[%d] materialized %d skill(s) into %s (skipped %d)",
			i, len(out.materialized), rc.TargetDir, len(out.skipped))
		res.Written = append(res.Written, Written{Dir: rc.TargetDir, Skills: out.materialized})
	}
	return res
}

// claimSet tracks which (dir, skill-id) pairs have already been materialized, so
// the first writer of a given id into a given dir wins.
type claimSet struct {
	seen map[string]int // key -> registry index that first claimed it
}

func newClaimSet() *claimSet { return &claimSet{seen: map[string]int{}} }

func claimKey(dir, id string) string { return dir + "\x00" + id }

// claim records (dir, id) as owned by registry regIdx and returns true if this
// is the first claim. If already claimed, it returns false and the index of the
// registry that won.
func (c *claimSet) claim(dir, id string, regIdx int) (won bool, byRegistry int) {
	key := claimKey(dir, id)
	if prev, ok := c.seen[key]; ok {
		return false, prev
	}
	c.seen[key] = regIdx
	return true, regIdx
}

// selectionFromConfig maps a SkillsRegistryConfig to a selection:
//   - explicit Skills list => by-id (with optional revision pin)
//   - else Query => by-query
//   - else => all
func selectionFromConfig(rc config.SkillsRegistryConfig) selection {
	if len(rc.Skills) > 0 {
		refs := make([]skillRef, 0, len(rc.Skills))
		for _, s := range rc.Skills {
			refs = append(refs, skillRef{SkillID: s.ID, Revision: s.Revision})
		}
		return selection{SkillRefs: refs}
	}
	if rc.Query != nil && strings.TrimSpace(rc.Query.Text) != "" {
		return selection{Query: rc.Query.Text, TopK: rc.Query.TopK}
	}
	return selection{All: true}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
