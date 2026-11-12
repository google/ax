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

package apivalidation

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/google/ax/internal/resources"
	"github.com/google/ax/pkg/apis/v1alpha1"
)

// Resource names.
//
// A resource's name and atespace become Substrate resource names (the task's
// actor and ActorTemplate, and the atespace itself), which must be lowercase
// RFC 1123 labels. Checking them at apply time turns an asynchronous
// ActorCreationFailed condition into an immediate error.

// MaxNameLength is the longest name or atespace a resource may have.
const MaxNameLength = 63

var nameRegexp = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidateName reports whether s can be used as a resource name or atespace: a
// lowercase RFC 1123 label of at most MaxNameLength characters.
func ValidateName(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	if len(s) > MaxNameLength || !nameRegexp.MatchString(s) {
		return fmt.Errorf("must be a lowercase RFC 1123 label: at most %d lowercase alphanumeric characters or '-', starting and ending with an alphanumeric character", MaxNameLength)
	}
	return nil
}

// ValidateObjectMeta checks a resource's name and, when set, its atespace. An
// empty atespace is fine here; the API server defaults it.
func ValidateObjectMeta(meta *v1alpha1.ObjectMeta) error {
	if err := ValidateName(meta.GetName()); err != nil {
		return fmt.Errorf("metadata.name: invalid value %q: %w", meta.GetName(), err)
	}
	if atespace := meta.GetAtespace(); atespace != "" {
		if err := ValidateName(atespace); err != nil {
			return fmt.Errorf("metadata.atespace: invalid value %q: %w", atespace, err)
		}
	}
	return nil
}

// Resource requirements.
//
// spec.resources.limits is copied onto the Substrate ActorTemplate, which
// enforces the same rules as below: cpu and memory only, each quantity greater
// than zero, and a cpu limit below 1000 cores. Checking them here means a bad
// value fails at apply time instead of when the template is created.

// ValidateResources reports the first problem with a task's resource
// requirements. A nil or empty value is valid: omitted cpu and memory limits
// default to 1 CPU (1000m) and 512Mi when the ActorTemplate is built.
func ValidateResources(reqs *v1alpha1.ResourcesSpec) error {
	return validateResources("spec.resources", reqs)
}

// TODO: Once TaskSpec migrates to spec.profile, drop the field parameter
// and hardcode "spec.profile.resources".
func validateResources(field string, reqs *v1alpha1.ResourcesSpec) error {
	limits := reqs.GetLimits()
	if cpu := limits.GetCpu(); cpu != "" {
		if _, err := resources.ParseCPUMillicores(cpu); err != nil {
			return fmt.Errorf("%s.limits.cpu: %w", field, err)
		}
	}
	if memory := limits.GetMemory(); memory != "" {
		if _, err := resources.ParseMemoryBytes(memory); err != nil {
			return fmt.Errorf("%s.limits.memory: %w", field, err)
		}
	}
	return nil
}

func validateProfile(field string, p *v1alpha1.Profile) error {
	if p == nil {
		return nil
	}
	if err := validateResources(field+".resources", p.GetResources()); err != nil {
		return err
	}
	if seed := p.GetAteActorTemplate(); seed != "" {
		if err := ValidateName(seed); err != nil {
			return fmt.Errorf("%s.ateActorTemplate: invalid value %q: %w", field, seed, err)
		}
	}
	return nil
}

func validateGatewayRef(field string, ref *v1alpha1.GatewayRef) error {
	if ref == nil {
		return nil
	}
	if err := ValidateName(ref.GetName()); err != nil {
		return fmt.Errorf("%s.name: invalid value %q: %w", field, ref.GetName(), err)
	}
	return nil
}
