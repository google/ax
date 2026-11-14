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
	"math"
	"strings"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// ValidateTask reports the first problem with a task that would make it
// impossible to run correctly. It is called by the API server before saving.
func ValidateTask(t *v1alpha1.Task) error {
	if err := ValidateObjectMeta(t.GetMetadata()); err != nil {
		return err
	}
	spec := t.GetSpec()
	if err := validateProfile("spec.profile", spec.GetProfile()); err != nil {
		return err
	}
	if ref := spec.GetWorkspace(); ref != nil {
		if ref.GetName() == "" {
			return errors.New("spec.workspace: name is required")
		}
		if p := ref.GetPath(); p != "" && !strings.HasPrefix(p, "/") {
			return fmt.Errorf("spec.workspace: path %q must be absolute", p)
		}
	}
	if err := validateGatewayRef("spec.gateway", spec.GetGateway()); err != nil {
		return err
	}
	if err := validateRetry("spec.retry", spec.GetRetry()); err != nil {
		return err
	}
	if err := ValidateTaskConfig(spec); err != nil {
		return err
	}
	return ValidateTaskSize(t)
}

func validateRetry(field string, r *v1alpha1.RetryConfig) error {
	if r == nil {
		return nil
	}
	if r.GetMaxRetries() < 0 {
		return fmt.Errorf("%s.maxRetries: must not be negative", field)
	}
	if d := r.GetInitialBackoff(); d != nil {
		if !d.IsValid() || d.AsDuration() < 0 {
			return fmt.Errorf("%s.initialBackoff: must not be negative", field)
		}
	}
	if d := r.GetMaxBackoff(); d != nil {
		if !d.IsValid() || d.AsDuration() < 0 {
			return fmt.Errorf("%s.maxBackoff: must not be negative", field)
		}
	}
	if r.GetInitialBackoff() != nil && r.GetMaxBackoff() != nil &&
		r.GetMaxBackoff().AsDuration() > 0 &&
		r.GetInitialBackoff().AsDuration() > r.GetMaxBackoff().AsDuration() {
		return fmt.Errorf("%s.initialBackoff: must not exceed maxBackoff", field)
	}
	if m := r.GetBackoffMultiplier(); m != 0 && (math.IsNaN(m) || math.IsInf(m, 0) || m < 1.0) {
		return fmt.Errorf("%s.backoffMultiplier: must be >= 1.0", field)
	}
	return nil
}

// MaxEnvValueSize is the longest environment variable value Substrate accepts
// (EnvVar.value in ateapi.proto caps it at 32,768 characters). It is applied
// to the byte length, which is never less than the character count.
const MaxEnvValueSize = 32 << 10

// MaxTaskYAMLSize is the largest a task's launch configuration may be once
// encoded as YAML, the form in which it reaches the runner through the
// AX_TASK_YAML environment variable. It keeps 1 KiB of MaxEnvValueSize in
// reserve for the fields the control plane fills in after validation, such as
// metadata.atespace, metadata.uid, metadata.createTime, and spec.sessionId.
const MaxTaskYAMLSize = MaxEnvValueSize - 1<<10

// MaxTaskConfigSize is the largest spec.config a task may have, measured as
// its JSON encoding. The config travels inside AX_TASK_YAML, so it must fit
// within MaxTaskYAMLSize together with the rest of the spec; this limit leaves
// room for that and gives a more specific error than ValidateTaskSize. The
// config's YAML encoding can be somewhat larger than its JSON encoding, so a
// config near this limit can still be rejected by ValidateTaskSize.
const MaxTaskConfigSize = 16 << 10

// ValidateTaskSize checks that the task's launch configuration, encoded as the
// control plane encodes it for AX_TASK_YAML, fits within MaxTaskYAMLSize.
// Larger tasks would be rejected by Substrate when their template is created.
func ValidateTaskSize(t *v1alpha1.Task) error {
	launch := proto.Clone(t).(*v1alpha1.Task)
	launch.Status = nil
	data, err := yaml.Marshal(launch)
	if err != nil {
		return fmt.Errorf("encoding task: %w", err)
	}
	if len(data) > MaxTaskYAMLSize {
		return fmt.Errorf("task: encoded launch configuration is %d bytes, which exceeds the limit of %d bytes", len(data), MaxTaskYAMLSize)
	}
	return nil
}

// ValidateTaskConfig checks that spec.config fits within MaxTaskConfigSize.
// The config is otherwise opaque: its schema belongs to the runner, and AX does
// not interpret it.
func ValidateTaskConfig(spec *v1alpha1.TaskSpec) error {
	config := spec.GetConfig()
	if config == nil {
		return nil
	}
	data, err := protojson.Marshal(config)
	if err != nil {
		return fmt.Errorf("spec.config: %w", err)
	}
	if len(data) > MaxTaskConfigSize {
		return fmt.Errorf("spec.config: %d bytes exceeds the limit of %d bytes", len(data), MaxTaskConfigSize)
	}
	return nil
}
