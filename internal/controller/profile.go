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

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/google/ax/internal/resources"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
)

// MergeProfile overlays optional Task-level overrides onto a Workspace base
// Profile without mutating either input or filling server defaults.
func MergeProfile(base, override *v1alpha1.Profile) *v1alpha1.Profile {
	out := &v1alpha1.Profile{}
	applyProfile(out, base)
	applyProfile(out, override)
	return out
}

func applyProfile(dst, src *v1alpha1.Profile) {
	if src == nil {
		return
	}
	if tpl := src.GetAteActorTemplate(); tpl != "" {
		dst.AteActorTemplate = tpl
		dst.Image = ""
		dst.Resources = nil
		return
	}

	img := src.GetImage()
	cpu := src.GetResources().GetLimits().GetCpu()
	mem := src.GetResources().GetLimits().GetMemory()
	if img == "" && cpu == "" && mem == "" {
		return
	}

	dst.AteActorTemplate = ""
	if img != "" {
		dst.Image = img
	}
	if cpu != "" || mem != "" {
		if dst.Resources == nil {
			dst.Resources = &v1alpha1.ResourcesSpec{}
		}
		if dst.Resources.Limits == nil {
			dst.Resources.Limits = &v1alpha1.ResourceList{}
		}
		if cpu != "" {
			dst.Resources.Limits.Cpu = cpu
		}
		if mem != "" {
			dst.Resources.Limits.Memory = mem
		}
	}
}

// DefaultProfileSnapshotFidelity is the Substrate snapshot fidelity configured
// on AX-managed Profile ActorTemplates so rootfs package installs from
// workspace setup commands survive snapshots.
const DefaultProfileSnapshotFidelity = ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS

type profile struct {
	image         string
	cpuMillicores int64
	memoryBytes   int64
	// Defaults to DefaultProfileSnapshotFidelity (SNAPSHOT_FIDELITY_ROOTFS).
	snapshotFidelity ateapipb.SnapshotFidelity
}

func (p profile) templateName(runnerImage string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\x00%s",
		p.image,
		runnerImage,
		p.cpuMillicores,
		p.memoryBytes,
		p.snapshotFidelity.String(),
	)
	sum := h.Sum(nil)
	return "ax-" + hex.EncodeToString(sum[:8])
}

// ResolveProfileTemplate resolves p into a deterministic Substrate
// ActorTemplate name and specification. Callers in controller fill p.Image with
// the server's configured default task image before calling when omitted.
//
// When p sets ate_actor_template, ResolveProfileTemplate returns that template
// name with a nil *ateapipb.ActorTemplate so the caller looks up the existing
// user-managed template on Substrate (surfacing NotFound if it is missing)
// rather than creating an AX-managed template.
//
// Otherwise, omitted resource limits fall back to Substrate's default CPU/memory
// limits, SnapshotConfig.PreferredFidelity defaults to SNAPSHOT_FIDELITY_ROOTFS,
// and the content-addressed template ("ax-<16-char-sha256>", hashed over the
// profile and runner volume image) is returned ready for EnsureActorTemplate.
func ResolveProfileTemplate(atespace string, p *v1alpha1.Profile, runnerImage string) (string, *ateapipb.ActorTemplate, error) {
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	if tpl := p.GetAteActorTemplate(); tpl != "" {
		return tpl, nil, nil
	}

	image := p.GetImage()
	if image == "" {
		image = v1alpha1.DefaultTaskImage
	}

	cpuMillicores := substrate.DefaultCPUMillicores
	if cpu := p.GetResources().GetLimits().GetCpu(); cpu != "" {
		parsed, err := resources.ParseCPUMillicores(cpu)
		if err != nil {
			return "", nil, fmt.Errorf("profile cpu limit: %w", err)
		}
		cpuMillicores = parsed
	}

	memoryBytes := substrate.DefaultMemoryBytes
	if mem := p.GetResources().GetLimits().GetMemory(); mem != "" {
		parsed, err := resources.ParseMemoryBytes(mem)
		if err != nil {
			return "", nil, fmt.Errorf("profile memory limit: %w", err)
		}
		memoryBytes = parsed
	}

	resolved := profile{
		image:            image,
		cpuMillicores:    cpuMillicores,
		memoryBytes:      memoryBytes,
		snapshotFidelity: DefaultProfileSnapshotFidelity,
	}
	name := resolved.templateName(runnerImage)

	tmpl := substrate.BuildActorTemplate(atespace, name, image, nil, nil, "", &ateapipb.ResourceRequirements{
		Limits: &ateapipb.Resources{
			CpuMillicores: cpuMillicores,
			MemoryBytes:   memoryBytes,
		},
	})
	tmpl.SnapshotConfig.PreferredFidelity = resolved.snapshotFidelity
	return name, tmpl, nil
}
