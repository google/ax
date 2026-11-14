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
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/internal/lock"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxUint64Digits = 20
	// maxTagWorkspacePrefix leaves room for "ws-" (3) + "-" (1) + 8-char ws
	// hash + "-" (1) + 16-char template hash + "-r" (2) + up to 20 revision
	// digits within apivalidation.MaxNameLength (63).
	maxTagWorkspacePrefix = apivalidation.MaxNameLength - len("ws-") - 1 - 8 - 1 - 16 - len("-r") - maxUint64Digits

	// maxSetupActorPrefix leaves room for "ws-setup-" (9) + "-" (1) + 8-char ws
	// hash + "-" (1) + 8-char random nonce within apivalidation.MaxNameLength (63).
	maxSetupActorPrefix = apivalidation.MaxNameLength - len("ws-setup-") - 1 - 8 - 1 - 8

	initialSnapshotRevision uint64 = 1
	defaultMaxSetupAttempts        = 2
	setupRetryBackoff              = 200 * time.Millisecond
)

// WorkspaceTagPrefix returns the deterministic Tag prefix
// ("ws-<prefix>-<8hex(ws)>-") shared by every snapshot Tag owned by
// workspaceName across all compute templates (default + lazy-cached) and
// snapshot revisions.
func WorkspaceTagPrefix(workspaceName string) string {
	wsSum := sha256.Sum256([]byte(workspaceName))
	prefix := strings.TrimRight(workspaceName[:min(len(workspaceName), maxTagWorkspacePrefix)], "-")
	return fmt.Sprintf("ws-%s-%x-", prefix, wsSum[:4])
}

// WorkspaceTagName returns a deterministic RFC 1123 Substrate Tag name
// ("ws-<prefix>-<8hex(ws)>-<16hex(tpl)>-r<revision>") for (workspaceName,
// templateName, revision).
//
// Substrate Tags cannot repoint their snapshot in place (UpdateTag does not
// change the underlying snapshot, and DeleteTag invalidates unsuspended
// running Actors created from that Tag), so refreshing a Workspace snapshot
// mints a new Tag under an incremented "-r<revision>" suffix while keeping the
// "ws-<prefix>-<8hex(ws)>-" owner prefix intact.
//
// TODO: Persist the active snapshot revision on Workspace and increment it
// once periodic or on-demand snapshot refresh is implemented.
func WorkspaceTagName(workspaceName, templateName string, revision uint64) string {
	tplSum := sha256.Sum256([]byte(templateName))
	return fmt.Sprintf("%s%x-r%d", WorkspaceTagPrefix(workspaceName), tplSum[:8], revision)
}

func setupActorPrefix(workspaceName string) string {
	wsSum := sha256.Sum256([]byte(workspaceName))
	prefix := strings.TrimRight(workspaceName[:min(len(workspaceName), maxSetupActorPrefix)], "-")
	if prefix == "" {
		return fmt.Sprintf("ws-setup-%x-", wsSum[:4])
	}
	return fmt.Sprintf("ws-setup-%s-%x-", prefix, wsSum[:4])
}

func newSetupActorName(workspaceName string) (string, error) {
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generating setup actor nonce: %w", err)
	}
	return fmt.Sprintf("%s%x", setupActorPrefix(workspaceName), nonce), nil
}

// NeedsPrewarmSnapshot reports whether spec declares any setup steps or compute
// profile that produce disk/rootfs state worth capturing in a Workspace Tag.
// Pure spec.mcp or empty Workspaces require no setup actor.
func NeedsPrewarmSnapshot(spec *v1alpha1.WorkspaceSpec) bool {
	if spec == nil {
		return false
	}
	if !isProfileEmpty(spec.GetProfile()) {
		return true
	}
	s := spec.GetSetup()
	return len(s.GetGit()) > 0 || len(s.GetFiles()) > 0 || s.GetSkills() != nil || len(s.GetCommands()) > 0
}

// SetupCoordinator is the rendezvous between WorkspaceManager and the
// pull-based runner HTTP service (pkg/apis/runner/v1):
//  1. WorkspaceManager calls AwaitSetup right after ResumeActor(setupActor),
//     registering ws in memory keyed by "<atespace>/<setupActor>" and blocking.
//  2. Inside the sandbox, ax-task-runner calls POST /runner/v1/assignment:claim
//     (authenticated via the Substrate Egress Gateway's Actor JWT) and receives
//     ASSIGNMENT_MODE_SETUP + ws + a one-time runner_token.
//  3. After running workspace.SetupWorkspace, ax-task-runner calls
//     POST /runner/v1/events with SetupFinished{ok, error_message}, which
//     unblocks AwaitSetup with nil or the setup stderr before the setup actor
//     is suspended and tagged.
type SetupCoordinator interface {
	AwaitSetup(ctx context.Context, atespace, setupActor string, ws *v1alpha1.Workspace) error
}

// WorkspaceManagerOptions configures a WorkspaceManager.
type WorkspaceManagerOptions struct {
	Locker           lock.Locker
	SetupCoordinator SetupCoordinator
	// RunnerImage is a static server-startup setting (changed only when
	// ax-server is redeployed with a new flag/env digest, never mutated
	// dynamically at runtime).
	RunnerImage string
	// MaxSetupAttempts bounds how many times buildSnapshotWithSetupActor tries
	// running a fresh setup actor on non-spec runtime failures (defaults to 2).
	MaxSetupAttempts int
}

// WorkspaceManager executes synchronous Substrate operations for Workspace
// snapshot pre-warming, lazy Tag resolution, and Tag cleanup. Unlike a
// background controller loop, its methods are called inline by gRPC handlers.
type WorkspaceManager struct {
	client           *substrate.Client
	locker           lock.Locker
	coordinator      SetupCoordinator
	runnerImage      string
	maxSetupAttempts int
}

// NewWorkspaceManager creates a WorkspaceManager backed by client.
func NewWorkspaceManager(client *substrate.Client, opts ...WorkspaceManagerOptions) *WorkspaceManager {
	var opt WorkspaceManagerOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	locker := opt.Locker
	if locker == nil {
		locker = lock.NewMemoryLocker()
	}
	attempts := opt.MaxSetupAttempts
	if attempts <= 0 {
		attempts = defaultMaxSetupAttempts
	}
	return &WorkspaceManager{
		client:           client,
		locker:           locker,
		coordinator:      opt.SetupCoordinator,
		runnerImage:      opt.RunnerImage,
		maxSetupAttempts: attempts,
	}
}

// PrewarmEnabled reports whether a SetupCoordinator has been wired into
// ax-server.
//
// TODO: Remove this temporary wiring gate (and firstUnimplementedWorkspaceField
// in server/workspace.go) once the pull-based runner SetupCoordinator is always
// injected in cmd/ax-server/main.go.
func (m *WorkspaceManager) PrewarmEnabled() bool {
	return m != nil && m.coordinator != nil
}

// stageError attaches structured lifecycle metadata to an error returned inside
// EnsureSnapshotTag so Prepare can populate Condition.Reason accurately (for
// example AtespaceCreationFailed vs TemplateCreationFailed vs SetupFailed) and
// distinguish whether spec.setup already completed before a tagging error.
type stageError struct {
	reason        string
	setupDone     bool
	nonRetryable  bool
	underlyingErr error
}

func (e *stageError) Error() string { return e.underlyingErr.Error() }
func (e *stageError) Unwrap() error { return e.underlyingErr }

// Prepare is the CreateWorkspace entry point: it builds the default snapshot
// Tag for ws (when ws has setup steps or an explicit profile) and translates
// the outcome into the WorkspaceStatus persisted in the store:
//   - No setup/profile -> Phase=Ready immediately (no setup actor needed).
//   - Tag created      -> Phase=Ready, SetupComplete=True, SnapshotReady=True.
//   - Transient error  -> Phase=Preparing, reason=SubstrateUnavailable so re-applying retries.
//   - Permanent error  -> Phase=Failed with stage-specific reason (TemplateCreationFailed,
//     GatewayBindingFailed, SetupFailed, or SnapshotTagFailed) and LastSetupError recorded.
func (m *WorkspaceManager) Prepare(ctx context.Context, ws *v1alpha1.Workspace, setupGateway *v1alpha1.Gateway) (*v1alpha1.WorkspaceStatus, error) {
	now := time.Now()
	st := &v1alpha1.WorkspaceStatus{Phase: v1alpha1.WorkspacePhasePreparing}

	// Skip spinning up a setup actor for empty or MCP-only Workspaces that
	// write nothing to /workspace or the rootfs.
	if !NeedsPrewarmSnapshot(ws.GetSpec()) {
		st.Phase = v1alpha1.WorkspacePhaseReady
		return st, nil
	}

	tagName, err := m.EnsureSnapshotTag(ctx, ws, ws.GetSpec().GetProfile(), setupGateway)
	if err != nil {
		msg := statusErrorMessage(err)
		if substrate.IsTransientError(err) || errors.Is(err, context.Canceled) {
			setWorkspaceCondition(st, v1alpha1.WorkspaceConditionSetupComplete, "False", v1alpha1.ReasonSubstrateUnavailable, msg, now)
			setWorkspaceCondition(st, v1alpha1.WorkspaceConditionSnapshotReady, "False", v1alpha1.ReasonSubstrateUnavailable, msg, now)
			return st, err
		}

		reason := v1alpha1.WorkspaceReasonSetupFailed
		setupDone := false
		var se *stageError
		if errors.As(err, &se) {
			reason = se.reason
			setupDone = se.setupDone
		}
		st.Phase = v1alpha1.WorkspacePhaseFailed
		st.LastSetupError = msg
		if setupDone {
			setWorkspaceCondition(st, v1alpha1.WorkspaceConditionSetupComplete, "True", v1alpha1.WorkspaceReasonSetupComplete, "Workspace setup completed", now)
		} else {
			setWorkspaceCondition(st, v1alpha1.WorkspaceConditionSetupComplete, "False", reason, msg, now)
		}
		setWorkspaceCondition(st, v1alpha1.WorkspaceConditionSnapshotReady, "False", reason, msg, now)
		return st, err
	}

	st.Phase = v1alpha1.WorkspacePhaseReady
	setWorkspaceCondition(st, v1alpha1.WorkspaceConditionSetupComplete, "True", v1alpha1.WorkspaceReasonSetupComplete, "Workspace setup completed", now)
	setWorkspaceCondition(st, v1alpha1.WorkspaceConditionSnapshotReady, "True", v1alpha1.WorkspaceReasonSnapshotReady, fmt.Sprintf("Snapshot tag %s ready", tagName), now)
	return st, nil
}

// EnsureSnapshotTag resolves or builds the cached Substrate Tag for a given
// (Workspace, Profile) pair. It serves two callers:
//  1. Eager pre-warming in Prepare (using ws.Spec.Profile).
//  2. Future lazy snapshot caching in CreateTask (when a Task specifies a
//     custom Profile that has no Tag for this Workspace yet).
//
// Why it locks on (atespace, tagName): if 10 Tasks with the same new Profile
// arrive at once on a cold miss, only the first lock holder runs a setup actor
// while the remaining 9 wait on the lock, re-check GetTag, and reuse the Tag.
func (m *WorkspaceManager) EnsureSnapshotTag(ctx context.Context, ws *v1alpha1.Workspace, prof *v1alpha1.Profile, setupGateway *v1alpha1.Gateway) (string, error) {
	if !m.PrewarmEnabled() {
		return "", errors.New("workspace snapshot pre-warming is not configured")
	}

	atespace := ws.GetMetadata().GetAtespace()
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	wsName := ws.GetMetadata().GetName()

	// Step 1: Ensure the atespace and the Profile's shared ActorTemplate exist.
	// Wrapped in stageError so Prepare records AtespaceCreationFailed /
	// TemplateCreationFailed as the Condition.Reason rather than SetupFailed.
	if err := m.client.EnsureAtespace(ctx, atespace); err != nil {
		return "", &stageError{reason: v1alpha1.ReasonAtespaceCreationFailed, underlyingErr: fmt.Errorf("ensuring atespace %q: %w", atespace, err)}
	}
	templateName, err := m.ensureProfileTemplate(ctx, atespace, prof)
	if err != nil {
		return "", &stageError{reason: v1alpha1.ReasonTemplateCreationFailed, underlyingErr: err}
	}

	// Step 2: Fast path (warm hit) — if a COMPLETED Tag already exists on
	// Substrate, return immediately without taking a lock or spawning a setup actor.
	tagName := WorkspaceTagName(wsName, templateName, initialSnapshotRevision)
	if tag, err := m.client.GetTag(ctx, atespace, tagName); err == nil && isTagCompleted(tag) {
		return tagName, nil
	} else if err != nil && status.Code(err) != codes.NotFound {
		return "", fmt.Errorf("checking tag %s/%s: %w", atespace, tagName, err)
	}

	// Step 3: Cold miss (or incomplete stranded Tag) — acquire a per-Tag
	// distributed lock so concurrent requests coalesce onto one setup actor.
	unlock, err := m.locker.Lock(ctx, "workspace-tag", atespace, tagName)
	if err != nil {
		return "", fmt.Errorf("locking workspace tag %s/%s: %w", atespace, tagName, err)
	}
	defer unlock()

	// Double-check after acquiring the lock: reuse a COMPLETED Tag built by an
	// earlier holder, or delete an incomplete PENDING Tag stranded by an
	// interrupted CreateTag before rebuilding.
	tag, err := m.client.GetTag(ctx, atespace, tagName)
	if err != nil && status.Code(err) != codes.NotFound {
		return "", fmt.Errorf("checking tag %s/%s: %w", atespace, tagName, err)
	}
	if err == nil {
		if isTagCompleted(tag) {
			return tagName, nil
		}
		if err := m.client.DeleteTag(ctx, atespace, tagName); err != nil {
			return "", &stageError{reason: v1alpha1.WorkspaceReasonSnapshotTagFailed, underlyingErr: err}
		}
	}

	// Step 4: Run a temporary setup actor (retrying up to maxSetupAttempts on
	// retryable runtime hiccups, never on deterministic spec/gateway errors).
	var lastErr error
	for attempt := 1; attempt <= m.maxSetupAttempts; attempt++ {
		lastErr = m.buildSnapshotWithSetupActor(ctx, ws, atespace, templateName, tagName, setupGateway)
		if lastErr == nil {
			return tagName, nil
		}
		if ctx.Err() != nil || isNonRetryableSetupError(lastErr) || attempt == m.maxSetupAttempts {
			break
		}
		slog.Warn("workspace setup attempt failed, retrying with a fresh setup actor",
			"atespace", atespace, "workspace", wsName, "attempt", attempt, "error", lastErr)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(setupRetryBackoff):
		}
	}
	return "", lastErr
}

// isTagCompleted reports whether tag's durable snapshot copy has finished. In
// Substrate, CreateTag spans two transactions around an object copy; a tag with
// an unset Snapshot or SNAPSHOT_STORAGE_STATE_PENDING from an interrupted
// CreateTag is incomplete and must be deleted before retrying CreateTag.
func isTagCompleted(tag *ateapipb.Tag) bool {
	return tag.GetStatus().GetSnapshot().GetDurableSnapshot().GetState() == ateapipb.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED
}

func isNonRetryableSetupError(err error) bool {
	var se *stageError
	if errors.As(err, &se) && se.nonRetryable {
		return true
	}
	switch status.Code(err) {
	case codes.InvalidArgument, codes.PermissionDenied, codes.Unauthenticated:
		return true
	}
	return false
}

func (m *WorkspaceManager) ensureProfileTemplate(ctx context.Context, atespace string, prof *v1alpha1.Profile) (string, error) {
	tplName, tplSpec, err := ResolveProfileTemplate(atespace, prof, m.runnerImage)
	if err != nil {
		return "", err
	}
	if tplSpec == nil {
		// User-supplied ate_actor_template: verify it exists without creating or mutating it.
		if _, err := m.client.GetActorTemplate(ctx, atespace, tplName); err != nil {
			return "", fmt.Errorf("getting actor template %s/%s: %w", atespace, tplName, err)
		}
		return tplName, nil
	}
	if _, err := m.client.EnsureActorTemplate(ctx, tplSpec); err != nil {
		return "", err
	}
	return tplName, nil
}

// buildSnapshotWithSetupActor runs one attempt of the setup actor lifecycle:
//  1. Create temporary setup actor from the shared Profile ActorTemplate.
//  2. Bind optional setupGateway egress rules (scoped only to this setup actor).
//  3. Resume the setup actor and wait for the in-sandbox runner to pull and
//     finish workspace.SetupWorkspace via AwaitSetup.
//  4. Suspend synchronously (waiting for durable snapshot upload) and CreateTag.
//  5. Always delete the temporary setup actor on exit (success or failure).
func (m *WorkspaceManager) buildSnapshotWithSetupActor(ctx context.Context, ws *v1alpha1.Workspace, atespace, templateName, tagName string, setupGateway *v1alpha1.Gateway) error {
	wsName := ws.GetMetadata().GetName()
	actorName, err := newSetupActorName(wsName)
	if err != nil {
		return &stageError{reason: v1alpha1.ReasonActorCreationFailed, nonRetryable: true, underlyingErr: err}
	}

	slog.Info("provisioning workspace setup actor", "atespace", atespace, "workspace", wsName, "actor", actorName, "template", templateName, "tag", tagName)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if delErr := m.client.DeleteActor(cleanupCtx, atespace, actorName); delErr != nil {
			slog.Warn("failed to clean up workspace setup actor", "atespace", atespace, "actor", actorName, "error", delErr)
		}
	}()

	if _, err := m.client.EnsureActor(ctx, atespace, actorName, atespace, templateName); err != nil {
		return &stageError{reason: v1alpha1.ReasonActorCreationFailed, underlyingErr: err}
	}
	if setupGateway != nil {
		if err := m.client.EnsureActorEgressPolicy(ctx, atespace, actorName, setupGateway.GetSpec().GetEgress()); err != nil {
			return &stageError{reason: v1alpha1.ReasonGatewayBindingFailed, nonRetryable: !substrate.IsTransientError(err), underlyingErr: err}
		}
	}
	if _, _, err := m.client.ResumeActor(ctx, atespace, actorName); err != nil {
		return &stageError{reason: v1alpha1.ReasonActorResumeFailed, underlyingErr: err}
	}
	if err := m.coordinator.AwaitSetup(ctx, atespace, actorName, ws); err != nil {
		return &stageError{reason: v1alpha1.WorkspaceReasonSetupFailed, underlyingErr: fmt.Errorf("running workspace setup: %w", err)}
	}
	if err := m.client.SuspendActorAndWaitForSnapshot(ctx, atespace, actorName); err != nil {
		return &stageError{reason: v1alpha1.WorkspaceReasonSnapshotTagFailed, setupDone: true, underlyingErr: err}
	}
	if _, err := m.client.CreateTag(ctx, atespace, tagName, actorName); err != nil {
		if status.Code(err) == codes.AlreadyExists {
			// A prior attempt in this retry loop left an incomplete Tag behind;
			// delete it and create the Tag once more.
			if delErr := m.client.DeleteTag(ctx, atespace, tagName); delErr != nil {
				return &stageError{reason: v1alpha1.WorkspaceReasonSnapshotTagFailed, setupDone: true, underlyingErr: delErr}
			}
			if _, err = m.client.CreateTag(ctx, atespace, tagName, actorName); err == nil {
				return nil
			}
		}
		return &stageError{reason: v1alpha1.WorkspaceReasonSnapshotTagFailed, setupDone: true, underlyingErr: err}
	}
	return nil
}

// Delete removes every Substrate snapshot Tag (both the default Tag and any
// lazy-cached Tags created by Task profile overrides) and any stranded setup
// actor belonging to wsName.
func (m *WorkspaceManager) Delete(ctx context.Context, atespace, wsName string) error {
	if m == nil || m.client == nil {
		return nil
	}
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}

	var errs []error
	tagPrefix := WorkspaceTagPrefix(wsName)
	tags, err := m.client.ListTags(ctx, atespace)
	if err != nil {
		errs = append(errs, err)
	} else {
		for _, t := range tags {
			name := t.GetMetadata().GetName()
			if strings.HasPrefix(name, tagPrefix) {
				if delErr := m.client.DeleteTag(ctx, atespace, name); delErr != nil {
					errs = append(errs, delErr)
				}
			}
		}
	}

	actorPrefix := setupActorPrefix(wsName)
	actors, err := m.client.ListActors(ctx, atespace)
	if err != nil {
		errs = append(errs, err)
	} else {
		for _, a := range actors {
			name := a.GetMetadata().GetName()
			if strings.HasPrefix(name, actorPrefix) {
				if delErr := m.client.DeleteActor(ctx, atespace, name); delErr != nil {
					errs = append(errs, delErr)
				}
			}
		}
	}
	return errors.Join(errs...)
}

func setWorkspaceCondition(st *v1alpha1.WorkspaceStatus, condType, condStatus, reason, message string, t time.Time) {
	ts := timestamppb.New(t)
	for i, c := range st.Conditions {
		if c.Type == condType {
			st.Conditions[i].Status = condStatus
			st.Conditions[i].Reason = reason
			st.Conditions[i].Message = message
			st.Conditions[i].UpdateTime = ts
			return
		}
	}
	st.Conditions = append(st.Conditions, &v1alpha1.Condition{
		Type:       condType,
		Status:     condStatus,
		Reason:     reason,
		Message:    message,
		UpdateTime: ts,
	})
}
