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

package v1alpha1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

const (
	APIVersion      = "ax.io/v1alpha1"
	KindTask        = "Task"
	KindWorkspace   = "Workspace"
	KindModel       = "Model"
	KindSecretStore = "SecretStore"

	// DefaultSecretStore is the store a secret_ref uses when it names none.
	DefaultSecretStore = "default"

	DefaultTaskImage       = "gcr.io/ax-substrate/ate-images/ax-task-runner"
	DefaultSystemNamespace = "ax-system"

	// PhasePending marks a task whose actor has not been provisioned yet, for
	// example because Substrate was unavailable. Retrying the operation can
	// still succeed.
	PhasePending = "Pending"
	// PhaseFailed marks a task that AX could not provision or operate, or whose
	// actor crashed or disappeared.
	PhaseFailed = "Failed"
	// PhaseTerminating marks a task whose deletion has been requested and whose
	// actor is being torn down. The record disappears once cleanup completes.
	PhaseTerminating = "Terminating"

	// The phases below follow from the state of the task's actor on Agent
	// Substrate. They are never stored.

	// PhaseRunning marks a task whose actor is running.
	PhaseRunning = "Running"
	// PhaseResuming marks a task whose actor is being resumed.
	PhaseResuming = "Resuming"
	// PhaseSuspending marks a task whose actor is being suspended.
	PhaseSuspending = "Suspending"
	// PhaseSuspended marks a task whose actor is suspended.
	PhaseSuspended = "Suspended"
	// PhaseUnknown marks a task whose actor could not be read, or is in a state
	// AX does not recognize.
	PhaseUnknown = "Unknown"
)

// Condition types reported on Task status.
const (
	// ConditionReady reports whether the task as a whole is ready to do work:
	// its actor is running and the workspace inside it has finished setting up.
	ConditionReady = "Ready"
	// ConditionWorkspaceReady reports whether the workspace inside the actor has
	// finished setting up.
	ConditionWorkspaceReady = "WorkspaceReady"
)

// TODO(jbd): ReasonAtespaceCreationFailed, ReasonSubstrateUnavailable,
// ReasonActorNotFound, ReasonActorCreationFailed, ReasonActorResumeFailed
// are invalid states. These should be returned as errors, shouldn't be
// leaking as state.

// Condition reasons. A Ready condition that is False because the actor is
// not running (for example while it is suspended) uses the task's phase as its
// reason instead.
const (
	// ReasonTaskRunning: Ready is True; the actor is running and the workspace
	// is set up.
	ReasonTaskRunning = "TaskRunning"
	// ReasonWorkspaceInitializing: Ready is False; the actor is running but
	// the workspace is still being set up.
	ReasonWorkspaceInitializing = "WorkspaceInitializing"
	// ReasonSubstrateUnavailable: Agent Substrate could not be reached. Ready
	// is False with phase Pending while provisioning, or Unknown with phase
	// Unknown while reading the actor.
	ReasonSubstrateUnavailable = "SubstrateUnavailable"
	// ReasonActorNotFound: Ready is False; the task's actor was removed
	// outside AX.
	ReasonActorNotFound = "ActorNotFound"
	// ReasonAtespaceCreationFailed: Ready is False; the atespace could not be
	// created on Agent Substrate.
	ReasonAtespaceCreationFailed = "AtespaceCreationFailed"
	// ReasonInvalidResources: Ready is False; spec.resources is invalid.
	ReasonInvalidResources = "InvalidResources"
	// ReasonTemplateCreationFailed: Ready is False; Agent Substrate rejected
	// the task's ActorTemplate, for example because of its image.
	ReasonTemplateCreationFailed = "TemplateCreationFailed"
	// ReasonActorCreationFailed: Ready is False; the actor could not be
	// created.
	ReasonActorCreationFailed = "ActorCreationFailed"
	// ReasonActorResumeFailed: Ready is False; the actor could not be resumed.
	ReasonActorResumeFailed = "ActorResumeFailed"

	// ReasonSetupComplete: WorkspaceReady is True.
	ReasonSetupComplete = "SetupComplete"
	// ReasonInitializing: WorkspaceReady is False; setup is still running.
	ReasonInitializing = "Initializing"
)

// IsAXPhase reports whether phase describes the control plane's own work on a
// task (provisioning it, failing to, or tearing it down) rather than the state
// of the task's actor. Only these phases are stored; every other phase is read
// from the actor on Agent Substrate when the task is fetched.
func IsAXPhase(phase string) bool {
	switch phase {
	case PhasePending, PhaseFailed, PhaseTerminating:
		return true
	}
	return false
}

// YAML encoding.
//
// The API kinds are protobuf messages, so protojson is the single source of
// truth for their wire names (lowerCamelCase), timestamp format (RFC 3339), and
// which fields are omitted when unset. YAML is bridged through it: manifests are
// decoded generically, converted to JSON, and then decoded strictly by protojson,
// so unknown or misspelled fields are rejected. Output is protojson rendered as
// a yaml.Node so field order follows the proto definition instead of being
// sorted.
//
// A few legacy manifest shapes are still accepted. They are rewritten on the
// generic document before strict decoding by the per-kind normalizers below.

var (
	protoMarshal   = protojson.MarshalOptions{}
	protoUnmarshal = protojson.UnmarshalOptions{} // unknown fields are errors
	// protoUnmarshalLenient is for specs the control plane hands to a runner,
	// which may come from a newer control plane than the runner image.
	protoUnmarshalLenient = protojson.UnmarshalOptions{DiscardUnknown: true}
)

// normalizer rewrites a generically decoded document in place so legacy shapes
// match the current schema before strict decoding.
type normalizer func(doc map[string]any)

func marshalYAML(m proto.Message) (any, error) {
	data, err := protoMarshal.Marshal(m)
	if err != nil {
		return nil, err
	}
	return jsonToYAMLNode(data)
}

func unmarshalYAML(value *yaml.Node, m proto.Message, normalize normalizer) error {
	return unmarshalYAMLWith(value, m, normalize, protoUnmarshal)
}

// UnmarshalYAMLLenient decodes a Task, Workspace or Model like their
// UnmarshalYAML methods but ignores fields it doesn't know. Runners use it for
// the specs the control plane passes them, so a runner image keeps working
// when a newer control plane adds fields it doesn't use.
func UnmarshalYAMLLenient(value *yaml.Node, m proto.Message) error {
	var normalize normalizer
	switch m.(type) {
	case *Workspace:
		normalize = normalizeWorkspace
	case *Model:
		normalize = normalizeModel
	}
	return unmarshalYAMLWith(value, m, normalize, protoUnmarshalLenient)
}

func unmarshalYAMLWith(value *yaml.Node, m proto.Message, normalize normalizer, opts protojson.UnmarshalOptions) error {
	var doc any
	if err := value.Decode(&doc); err != nil {
		return err
	}
	if obj, ok := doc.(map[string]any); ok && normalize != nil {
		normalize(obj)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("converting yaml to json: %w", err)
	}
	return opts.Unmarshal(data, m)
}

// jsonToYAMLNode converts a JSON document into a yaml.Node tree, preserving
// object key order and tagging scalars so the YAML encoder quotes strings that
// would otherwise read as another type (for example a status of "True").
func jsonToYAMLNode(data []byte) (*yaml.Node, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return readJSONValue(dec)
}

func readJSONValue(dec *json.Decoder) (*yaml.Node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			n := &yaml.Node{Kind: yaml.MappingNode}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := keyTok.(string)
				val, err := readJSONValue(dec)
				if err != nil {
					return nil, err
				}
				n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, val)
			}
			_, err = dec.Token() // closing '}'
			return n, err
		case '[':
			n := &yaml.Node{Kind: yaml.SequenceNode}
			for dec.More() {
				val, err := readJSONValue(dec)
				if err != nil {
					return nil, err
				}
				n.Content = append(n.Content, val)
			}
			_, err = dec.Token() // closing ']'
			return n, err
		}
		return nil, fmt.Errorf("unexpected json delimiter %q", v)
	case string:
		// protojson renders timestamps as RFC 3339 strings. Let those stay plain
		// scalars, which YAML reads as timestamps; every other string is tagged so
		// values like "True" or "1" are quoted and read back as strings.
		tag := "!!str"
		if _, err := time.Parse(time.RFC3339Nano, v); err == nil {
			tag = "!!timestamp"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v}, nil
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(v.String(), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v.String()}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(v)}, nil
	case nil:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
	}
	return nil, fmt.Errorf("unexpected json token %v", tok)
}

// Legacy manifest shapes.

// normalizeWorkspace accepts spec.mcp written as a bare list of servers.
func normalizeWorkspace(doc map[string]any) {
	spec, _ := doc["spec"].(map[string]any)
	if list, ok := spec["mcp"].([]any); ok {
		spec["mcp"] = map[string]any{"servers": list}
	}
}

// normalizeModel accepts older spellings of the secret reference: spec.secretKeyRef,
// spec.apiKey.secretKeyRef, and a bare string meaning the secret name and key are
// the same.
func normalizeModel(doc map[string]any) {
	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		return
	}
	if _, has := spec["secretKey"]; !has {
		if ref, ok := spec["secretKeyRef"]; ok {
			spec["secretKey"] = ref
		} else if apiKey, ok := spec["apiKey"].(map[string]any); ok {
			if ref, ok := apiKey["secretKeyRef"]; ok {
				spec["secretKey"] = ref
			}
		}
	}
	delete(spec, "secretKeyRef")
	delete(spec, "apiKey")
	if s, ok := spec["secretKey"].(string); ok {
		spec["secretKey"] = map[string]any{"name": s, "key": s}
	}
}

// Per-kind YAML methods. Nested messages are handled by protojson as part of
// their parent, so only the kinds that are encoded on their own need these.

func (t *Task) MarshalYAML() (any, error)        { return marshalYAML(t) }
func (t *Task) UnmarshalYAML(n *yaml.Node) error { return unmarshalYAML(n, t, nil) }

func (w *Workspace) MarshalYAML() (any, error)        { return marshalYAML(w) }
func (w *Workspace) UnmarshalYAML(n *yaml.Node) error { return unmarshalYAML(n, w, normalizeWorkspace) }

func (m *Model) MarshalYAML() (any, error)        { return marshalYAML(m) }
func (m *Model) UnmarshalYAML(n *yaml.Node) error { return unmarshalYAML(n, m, normalizeModel) }

func (s *SecretStore) MarshalYAML() (any, error)        { return marshalYAML(s) }
func (s *SecretStore) UnmarshalYAML(n *yaml.Node) error { return unmarshalYAML(n, s, nil) }

// Workspace binding.
//
// A task binds at most one workspace through spec.workspace.

// DefaultWorkspacePath is where the task's workspace is mounted unless the
// binding sets a path. It is the task command's working directory, and a task
// with no workspace gets an empty directory there.
const DefaultWorkspacePath = "/workspace"

// WorkspacePath returns the task command's working directory: the bound
// workspace's path when set, otherwise DefaultWorkspacePath.
func (s *TaskSpec) WorkspacePath() string {
	if p := s.GetWorkspace().GetPath(); p != "" {
		return p
	}
	return DefaultWorkspacePath
}

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
func ValidateObjectMeta(meta *ObjectMeta) error {
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

// ValidateWorkspace reports the first problem with a workspace that would make
// it unusable. It is called by the API server before saving.
func ValidateWorkspace(w *Workspace) error {
	if err := ValidateObjectMeta(w.GetMetadata()); err != nil {
		return err
	}
	if w.GetSpec() != nil {
		for i, f := range w.GetSpec().GetFiles() {
			field := fmt.Sprintf("spec.files[%d]", i)
			if f == nil || f.GetPath() == "" {
				return fmt.Errorf("%s: path is required", field)
			}
		}
	}
	return nil
}

// ValidateModel reports the first problem with a model that would make it
// unusable. It is called by the API server before saving.
func ValidateModel(m *Model) error {
	return ValidateObjectMeta(m.GetMetadata())
}

// ValidateSecretStore checks the store's metadata and that exactly one backend
// is configured.
func ValidateSecretStore(s *SecretStore) error {
	if err := ValidateObjectMeta(s.GetMetadata()); err != nil {
		return err
	}
	k := s.GetSpec().GetKubernetes()
	if k == nil {
		return errors.New("spec: a backend is required (spec.kubernetes)")
	}
	if ns := k.GetNamespace(); ns != "" {
		if err := ValidateName(ns); err != nil {
			return fmt.Errorf("spec.kubernetes.namespace: %w", err)
		}
	}
	return nil
}

// StoreName is the SecretStore a secret_ref resolves against.
func (r *SecretEnvSource) StoreName() string {
	if r.GetStore() != "" {
		return r.GetStore()
	}
	return DefaultSecretStore
}

// ValidateTask reports the first problem with a task that would make it
// impossible to run correctly. It is called by the API server before saving.
func ValidateTask(t *Task) error {
	if err := ValidateObjectMeta(t.GetMetadata()); err != nil {
		return err
	}
	spec := t.GetSpec()
	if ref := spec.GetWorkspace(); ref != nil {
		if ref.GetName() == "" {
			return errors.New("spec.workspace: name is required")
		}
		if p := ref.GetPath(); p != "" && !strings.HasPrefix(p, "/") {
			return fmt.Errorf("spec.workspace: path %q must be absolute", p)
		}
	}
	if err := validateEgress(spec); err != nil {
		return err
	}
	if err := ValidateTaskConfig(spec); err != nil {
		return err
	}
	if err := ValidateTaskSize(t); err != nil {
		return err
	}
	return ValidateResources(spec.GetResources())
}

// MaxEnvValueSize is the longest environment variable value Substrate accepts
// (EnvVar.value in ateapi.proto caps it at 32,768 characters). It is applied
// to the byte length, which is never less than the character count.
const MaxEnvValueSize = 32 << 10

// MaxTaskYAMLSize is the largest a task's launch configuration may be once
// encoded as YAML, the form in which it reaches the runner through the
// AX_TASK_YAML environment variable. It keeps 1 KiB of MaxEnvValueSize in
// reserve for the fields the control plane fills in after validation, such as
// metadata.atespace and metadata.createTime.
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
func ValidateTaskSize(t *Task) error {
	launch := proto.Clone(t).(*Task)
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
func ValidateTaskConfig(spec *TaskSpec) error {
	config := spec.GetConfig()
	if config == nil {
		return nil
	}
	data, err := protoMarshal.Marshal(config)
	if err != nil {
		return fmt.Errorf("spec.config: %w", err)
	}
	if len(data) > MaxTaskConfigSize {
		return fmt.Errorf("spec.config: %d bytes exceeds the limit of %d bytes", len(data), MaxTaskConfigSize)
	}
	return nil
}

// SecretPlaceholder is the value a task sees for an env var backed by a
// secret_ref. The egress gateway replaces it with the secret on the way out.
const SecretPlaceholder = "ax-placeholder"

// ResolvedValue is the value the task's environment gets for e: the literal
// value, or SecretPlaceholder when the variable is backed by a secret_ref.
func (e *EnvVar) ResolvedValue() string {
	if e.GetSecretRef() != nil {
		return SecretPlaceholder
	}
	return e.GetValue()
}

var (
	hostLabel    = `[a-z0-9]([-a-z0-9]*[a-z0-9])?`
	hostRegexp   = regexp.MustCompile(`^` + hostLabel + `(\.` + hostLabel + `)*$`)
	headerRegexp = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	// secretNameRegexp and secretKeyRegexp follow Kubernetes' rules for Secret
	// names (DNS subdomains) and data keys. Both end up as path segments of the
	// credential URI, so neither may contain "/".
	secretNameRegexp = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	secretKeyRegexp  = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
	maxHostLength    = 253
)

// validateHost reports whether h is a lowercase DNS name, optionally with "*"
// as its leftmost label when wildcard is true.
func validateHost(h string, wildcard bool) error {
	name := h
	if wildcard {
		name = strings.TrimPrefix(h, "*.")
	}
	if len(name) == 0 || len(name) > maxHostLength || !hostRegexp.MatchString(name) {
		if wildcard {
			return fmt.Errorf("%q must be a lowercase DNS name, optionally starting with \"*.\"", h)
		}
		return fmt.Errorf("%q must be a lowercase DNS name", h)
	}
	return nil
}

func validateEgress(spec *TaskSpec) error {
	for i, h := range spec.GetEgress().GetHosts() {
		if err := validateHost(h, true); err != nil {
			return fmt.Errorf("spec.egress.hosts[%d]: %w", i, err)
		}
	}
	injected := map[string]string{}
	for i, e := range spec.GetEnv() {
		field := fmt.Sprintf("spec.env[%d]", i)
		ref := e.GetSecretRef()
		if ref == nil {
			continue
		}
		if e.GetValue() != "" {
			return fmt.Errorf("%s: value and secretRef are mutually exclusive", field)
		}
		if ref.GetName() == "" || ref.GetKey() == "" {
			return fmt.Errorf("%s.secretRef: name and key are required", field)
		}
		if len(ref.GetName()) > maxHostLength || !secretNameRegexp.MatchString(ref.GetName()) {
			return fmt.Errorf("%s.secretRef.name: %q must be a lowercase DNS subdomain", field, ref.GetName())
		}
		if len(ref.GetKey()) > maxHostLength || !secretKeyRegexp.MatchString(ref.GetKey()) {
			return fmt.Errorf("%s.secretRef.key: %q may only contain letters, digits, '-', '_', and '.'", field, ref.GetKey())
		}
		if ref.GetStore() != "" {
			if err := ValidateName(ref.GetStore()); err != nil {
				return fmt.Errorf("%s.secretRef.store: %w", field, err)
			}
		}
		inj := ref.GetInject()
		if inj == nil {
			return fmt.Errorf("%s.secretRef.inject: host and header are required", field)
		}
		if err := validateHost(inj.GetHost(), false); err != nil {
			return fmt.Errorf("%s.secretRef.inject.host: %w", field, err)
		}
		if !headerRegexp.MatchString(inj.GetHeader()) || strings.EqualFold(inj.GetHeader(), "host") {
			return fmt.Errorf("%s.secretRef.inject.header: %q is not a valid request header name", field, inj.GetHeader())
		}
		if strings.ContainsAny(inj.GetPrefix(), "\r\n") {
			return fmt.Errorf("%s.secretRef.inject.prefix: must not contain line breaks", field)
		}
		k := inj.GetHost() + " " + strings.ToLower(inj.GetHeader())
		if other, dup := injected[k]; dup {
			return fmt.Errorf("%s.secretRef.inject: header %q for host %q is already injected by env %q", field, inj.GetHeader(), inj.GetHost(), other)
		}
		injected[k] = e.GetName()
	}
	return nil
}
