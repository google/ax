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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// templateDigestBytes is how many bytes of the spec digest go into a
	// per-task ActorTemplate name.
	templateDigestBytes = 4
	// defaultWorkspaceReadyTimeout is how long Resume waits for a freshly
	// resumed actor's runner to accept the task, finish workspace setup, and
	// start the task.
	defaultWorkspaceReadyTimeout = 15 * time.Second
	workspaceReadyPollInterval   = 500 * time.Millisecond

	defaultServiceDNSCAFile      = "/run/servicedns-ca/trust-bundle.pem"
	defaultPodIdentityBundleFile = "/run/podidentity.podcert.ate.dev/credential-bundle.pem"
	defaultRouterServerName      = "atenet-router.ate-system.svc"
)

// GatewayStore looks up a Gateway resource when lazy workspace snapshot builds
// resolve Workspace.spec.setup.gateway during task creation. Satisfied by
// store.Store.
type GatewayStore interface {
	GetGateway(ctx context.Context, atespace, name string) (*v1alpha1.Gateway, error)
}

// TaskManager provisions, suspends, resumes, and deletes the sandboxed Actors
// that run Tasks on Agent Substrate. ax-server calls it directly; it runs no
// reconcile loop.
type TaskManager struct {
	client     *substrate.Client
	httpClient *http.Client

	// WorkspaceReadyTimeout bounds how long Resume waits for the actor's runner
	// to accept the task, set up its workspace, and start it. A runner that
	// has not accepted the task by then fails the resume; a workspace that is
	// not ready by then is recorded as still initializing.
	WorkspaceReadyTimeout time.Duration

	// RunnerVolumeImage, when set, is a digest-pinned runner volume image
	// (Dockerfile.runner-volume) that task templates mount read-only and run
	// the runner from, so task images do not need the runner baked in. It is
	// part of the template name, so a new digest provisions new templates.
	// Empty keeps running the runner baked into the task image.
	RunnerVolumeImage string

	// WorkspaceManager, when configured with pre-warming enabled, resolves warm
	// Workspace snapshot Tags so task actors start from pre-warmed snapshots.
	WorkspaceManager *WorkspaceManager

	// GatewayStore resolves Workspace.spec.setup.gateway if a task profile
	// override triggers a lazy Workspace snapshot build during CreateTask.
	GatewayStore GatewayStore
}

// NewTaskManager creates a new TaskManager.
func NewTaskManager(client *substrate.Client) *TaskManager {
	return &TaskManager{
		client:                client,
		httpClient:            newProbeHTTPClient(),
		WorkspaceReadyTimeout: defaultWorkspaceReadyTimeout,
	}
}

// Client returns the Substrate client used by the TaskManager.
func (m *TaskManager) Client() *substrate.Client {
	return m.client
}

func newProbeHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: defaultRouterServerName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(defaultPodIdentityBundleFile, defaultPodIdentityBundleFile)
			if err != nil {
				return &tls.Certificate{}, nil
			}
			return &cert, nil
		},
	}
	if caBytes, err := os.ReadFile(defaultServiceDNSCAFile); err == nil {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(caBytes) {
			tlsCfg.RootCAs = pool
		}
	}
	transport.TLSClientConfig = tlsCfg
	return &http.Client{
		Timeout:   2 * time.Second,
		Transport: transport,
	}
}

// ensureSubstrateResources resolves the effective Profile and makes sure the
// task's atespace, ActorTemplate and actor exist on Substrate. It records only
// what AX stores in the task's status: the resolved template name, resolved
// argv, and on failure the reason in a False Ready condition. The caller sets
// the state, and the server reads the rest of the status from the actor.
func (m *TaskManager) ensureSubstrateResources(ctx context.Context, task *v1alpha1.Task, ws *v1alpha1.Workspace) error {
	if task.Metadata == nil {
		task.Metadata = &v1alpha1.ObjectMeta{}
	}
	if task.Status == nil {
		task.Status = &v1alpha1.TaskStatus{}
	}
	atespace := task.Metadata.Atespace
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	task.Metadata.Atespace = atespace

	if task.Spec == nil {
		task.Spec = &v1alpha1.TaskSpec{}
	}

	now := time.Now()

	effective := EffectiveProfile(ws.GetSpec().GetProfile(), task.Spec.GetProfile())

	// Resource limits are enforced by the template, so they are checked here as
	// well as at apply time: a task must never run without limits it asked for.
	if effective.GetAteTemplate().GetName() == "" {
		if err := apivalidation.ValidateResources(effective.GetResources()); err != nil {
			m.setNotReady(task, v1alpha1.ReasonInvalidResources, err.Error(), now)
			return fmt.Errorf("validating resources: %w", err)
		}
	}

	// 1. Ensure the Atespace exists in Substrate.
	if err := m.client.EnsureAtespace(ctx, atespace); err != nil {
		if substrate.IsTransientError(err) {
			m.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
			return fmt.Errorf("ensuring atespace: %w", err)
		}
		m.setNotReady(task, v1alpha1.ReasonAtespaceCreationFailed, err.Error(), now)
		return fmt.Errorf("ensuring atespace: %w", err)
	}

	// 2. The actor is always named after the task, so the two can be used
	// interchangeably (for example in the router's ate-target-actor header).
	actorName := task.Metadata.Name

	// 3. Resolve or provision the ActorTemplate for the task.
	templateAtespace, templateName, err := ensureProfileTemplate(ctx, m.client, atespace, effective, m.RunnerVolumeImage)
	if err != nil {
		if substrate.IsTransientError(err) {
			m.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
			return fmt.Errorf("ensuring actor template: %w", err)
		}
		m.setNotReady(task, v1alpha1.ReasonTemplateCreationFailed, statusErrorMessage(err), now)
		return fmt.Errorf("ensuring actor template: %w", err)
	}

	// 4. Resolve the warm Workspace snapshot Tag when the bound Workspace has
	// setup steps or a compute profile and snapshot pre-warming is enabled.
	// Tasks mounting a Workspace at a non-default path skip the pre-warmed
	// snapshot so setup runs directly at their target path.
	wsPath := task.GetSpec().WorkspacePath()
	usesDefaultWorkspacePath := wsPath == "" || wsPath == v1alpha1.DefaultWorkspacePath
	var sourceTag string
	if usesDefaultWorkspacePath && ws != nil && needsPrewarmSnapshot(ws.GetSpec()) && m.WorkspaceManager != nil && m.WorkspaceManager.PrewarmEnabled() {
		var setupGW *v1alpha1.Gateway
		if gwName := ws.GetSpec().GetSetup().GetGateway().GetName(); gwName != "" && m.GatewayStore != nil {
			setupGW, _ = m.GatewayStore.GetGateway(ctx, atespace, gwName)
		}
		sourceTag, err = m.WorkspaceManager.EnsureSnapshotTag(ctx, ws, effective, setupGW)
		if err != nil {
			if substrate.IsTransientError(err) {
				m.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
				return fmt.Errorf("resolving workspace snapshot tag: %w", err)
			}
			m.setNotReady(task, v1alpha1.ReasonActorCreationFailed, statusErrorMessage(err), now)
			return fmt.Errorf("resolving workspace snapshot tag: %w", err)
		}
	}

	_, err = m.client.EnsureActorFromTag(ctx, atespace, actorName, templateAtespace, templateName, sourceTag)
	if err != nil {
		if substrate.IsTransientError(err) {
			m.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
			return fmt.Errorf("ensuring actor: %w", err)
		}
		m.setNotReady(task, v1alpha1.ReasonActorCreationFailed, err.Error(), now)
		return fmt.Errorf("ensuring actor: %w", err)
	}

	task.Status.AteTemplate = &v1alpha1.ObjectRef{
		Name:     templateName,
		Atespace: templateAtespace,
	}
	if len(task.Spec.Commands) > 0 && len(task.Status.ResolvedArgv) == 0 {
		task.Status.ResolvedArgv = slices.Clone(task.Spec.Commands)
	}
	if usesDefaultWorkspacePath && m.WorkspaceManager != nil && m.WorkspaceManager.PrewarmEnabled() && !conditionTrue(task, v1alpha1.ConditionWorkspaceReady) {
		msg := "Workspace requires no setup"
		if sourceTag != "" {
			msg = fmt.Sprintf("Workspace seeded from snapshot tag %s", sourceTag)
		}
		setCondition(task, v1alpha1.ConditionWorkspaceReady, "True", v1alpha1.ReasonSetupComplete, msg, now)
	}
	return nil
}

// bindGateway ensures the task's actor has an EgressPolicy with the rules of
// the task's bound Gateway. A task without a Gateway gets no policy, and an
// actor that already has a policy keeps it unchanged.
func (m *TaskManager) bindGateway(ctx context.Context, task *v1alpha1.Task, gw *v1alpha1.Gateway) error {
	gwName := task.Spec.GetGateway().GetName()
	if gwName == "" {
		return nil
	}
	atespace, actorName := task.Metadata.Atespace, task.Metadata.Name
	_, err := m.client.GetActorEgressPolicy(ctx, atespace, actorName)
	if status.Code(err) == codes.NotFound {
		if gw == nil {
			err = fmt.Errorf("not available in atespace %q", atespace)
		} else {
			err = m.client.EnsureActorEgressPolicy(ctx, atespace, actorName, gw.GetSpec().GetEgress())
		}
	}
	if err != nil {
		now := time.Now()
		if substrate.IsTransientError(err) {
			m.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
			return fmt.Errorf("binding gateway %q: %w", gwName, err)
		}
		m.setNotReady(task, v1alpha1.ReasonGatewayBindingFailed, fmt.Sprintf("gateway %q: %s", gwName, statusErrorMessage(err)), now)
		return fmt.Errorf("binding gateway %q: %w", gwName, err)
	}
	return nil
}

// Suspend runs ensureSubstrateResources, suspends the task's actor, and then
// runs bindGateway, so the actor is suspended even when bindGateway fails. A
// failure to suspend the actor is returned without changing the task's status.
func (m *TaskManager) Suspend(ctx context.Context, task *v1alpha1.Task, ws *v1alpha1.Workspace, gw *v1alpha1.Gateway) error {
	var prevStatus *v1alpha1.TaskStatus
	if task.GetStatus() != nil {
		prevStatus = proto.Clone(task.Status).(*v1alpha1.TaskStatus)
	}
	if err := m.ensureSubstrateResources(ctx, task, ws); err != nil {
		return err
	}
	atespace, actorName := task.Metadata.Atespace, task.Metadata.Name
	slog.Info("suspending actor on Substrate", "actor", actorName)
	if err := m.client.SuspendActor(ctx, atespace, actorName); err != nil {
		task.Status = prevStatus
		return fmt.Errorf("suspending actor: %w", err)
	}
	if err := m.bindGateway(ctx, task, gw); err != nil {
		task.Status.AteTemplate = prevStatus.GetAteTemplate()
		return err
	}
	slog.Info("task successfully suspended", "name", task.Metadata.Name, "actor", actorName)
	return nil
}

// Resume runs ensureSubstrateResources and bindGateway, resumes the task's
// actor, pushes the task to its runner (v1alpha1.RunnerTaskPath), and waits up
// to WorkspaceReadyTimeout for the runner to set up the task's workspace and
// start the task, recording the result in the WorkspaceReady condition. A
// failure to resume the actor, the runner refusing the task, and the runner
// failing to start a task whose workspace is already set up are recorded the
// way ensureSubstrateResources records its own. So is a runner that does not
// accept the task in time, as a transient error (substrate.IsTransientError):
// the task is not running, and resuming again repeats the push. A runner that
// accepted the task and is still working when the wait ends is not an error.
func (m *TaskManager) Resume(ctx context.Context, task *v1alpha1.Task, ws *v1alpha1.Workspace, gw *v1alpha1.Gateway) error {
	if err := m.ensureSubstrateResources(ctx, task, ws); err != nil {
		return err
	}
	if err := m.bindGateway(ctx, task, gw); err != nil {
		return err
	}
	atespace, actorName := task.Metadata.Atespace, task.Metadata.Name
	now := time.Now()

	// Resume the Actor to activate container execution.
	slog.Info("resuming actor on Substrate worker", "actor", actorName)
	_, workerIP, err := m.client.ResumeActor(ctx, atespace, actorName)
	if err != nil {
		if substrate.IsTransientError(err) {
			m.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
			return fmt.Errorf("resuming actor: %w", err)
		}
		m.setNotReady(task, v1alpha1.ReasonActorResumeFailed, err.Error(), now)
		return fmt.Errorf("resuming actor: %w", err)
	}

	// The actor runs from a template shared by every task with its profile, so
	// the runner learns which task it runs from this push. A runner restored
	// from a memory snapshot already holds the task and accepts it again.
	// Workspace setup happens once per task: after it has completed,
	// WorkspaceReady stays True across suspend/resume cycles. The task starts
	// on every resume, so the runner is polled every time.
	workspaceReady := conditionTrue(task, v1alpha1.ConditionWorkspaceReady)
	pollCtx, cancel := context.WithTimeout(ctx, m.WorkspaceReadyTimeout)
	defer cancel()
	var notReadyMsg string
	body, err := taskPushBody(task, ws)
	if err == nil {
		err = pushToRunner(pollCtx, m.httpClient, atespace, actorName, v1alpha1.RunnerTaskPath, body, workspaceReadyPollInterval)
	}
	switch {
	case errors.Is(err, errNoRouter):
		slog.Warn("cannot push the task to the runner", "actor", actorName, "error", err)
		notReadyMsg = err.Error()
	case err != nil && pollCtx.Err() == nil:
		// The runner refused the task, so it never runs it.
		m.setNotReady(task, v1alpha1.ReasonActorResumeFailed, err.Error(), time.Now())
		return fmt.Errorf("pushing task to runner: %w", err)
	case err != nil:
		// The task reaches the runner only through this push, so without it
		// nothing runs. err wraps the context's error, which makes a timeout
		// transient: resuming again repeats the push.
		err = fmt.Errorf("runner at %s did not accept the task within %s: %w", workerIP, m.WorkspaceReadyTimeout, err)
		m.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), time.Now())
		return err
	default:
		err = waitWorkspaceReady(pollCtx, m.httpClient, atespace, actorName, workspaceReadyPollInterval)
		switch {
		case err == nil:
			workspaceReady = true
		case errors.Is(err, errSetupFailed) && workspaceReady:
			// The workspace was set up on an earlier resume, so what failed
			// is starting the task: nothing runs in the resumed actor. The
			// runner's reason is reported without errSetupFailed's text.
			reason := strings.TrimPrefix(err.Error(), errSetupFailed.Error()+": ")
			m.setNotReady(task, v1alpha1.ReasonActorResumeFailed, reason, time.Now())
			return errors.New(reason)
		case errors.Is(err, errSetupFailed):
			notReadyMsg = err.Error()
		default:
			notReadyMsg = fmt.Sprintf("Workspace is initializing at %s", workerIP)
		}
	}

	// The server derives Ready from the actor's state and WorkspaceReady.
	switch {
	case conditionTrue(task, v1alpha1.ConditionWorkspaceReady):
	case workspaceReady:
		setCondition(task, v1alpha1.ConditionWorkspaceReady, "True", v1alpha1.ReasonSetupComplete, fmt.Sprintf("Workspace setup completed at %s", workerIP), time.Now())
	case errors.Is(err, errSetupFailed):
		setCondition(task, v1alpha1.ConditionWorkspaceReady, "False", v1alpha1.WorkspaceReasonSetupFailed, notReadyMsg, time.Now())
	default:
		setCondition(task, v1alpha1.ConditionWorkspaceReady, "False", v1alpha1.ReasonInitializing, notReadyMsg, time.Now())
	}

	slog.Info("task successfully resumed",
		"name", task.Metadata.Name,
		"actor", actorName,
		"workerIP", workerIP,
		"workspaceReady", workspaceReady,
	)

	return nil
}

// setNotReady marks the task's Ready condition False. WorkspaceReady is left untouched:
// workspace setup is a one-time step whose result outlives actor failures and suspends.
func (m *TaskManager) setNotReady(task *v1alpha1.Task, reason, message string, t time.Time) {
	setCondition(task, v1alpha1.ConditionReady, "False", reason, message, t)
}

// conditionTrue reports whether the task currently has the given condition with status True.
func conditionTrue(task *v1alpha1.Task, condType string) bool {
	if task.Status == nil {
		return false
	}
	for _, c := range task.Status.Conditions {
		if c.Type == condType {
			return c.Status == "True"
		}
	}
	return false
}

// statusErrorMessage returns a clean user-facing error message for status
// conditions and error fields. Wrapped gRPC errors (fmt.Errorf("...: %w", grpcErr))
// have their underlying gRPC status description extracted so the
// "rpc error: code = ... desc = " boilerplate is stripped; non-gRPC errors
// (such as in-sandbox command failures) are returned verbatim.
func statusErrorMessage(err error) string {
	var gs interface{ GRPCStatus() *status.Status }
	if errors.As(err, &gs) {
		if s := gs.GRPCStatus(); s != nil && s.Message() != "" {
			return s.Message()
		}
	}
	return err.Error()
}

func setCondition(task *v1alpha1.Task, condType, status, reason, message string, t time.Time) {
	ts := timestamppb.New(t)
	for i, c := range task.Status.Conditions {
		if c.Type == condType {
			task.Status.Conditions[i].Status = status
			task.Status.Conditions[i].Reason = reason
			task.Status.Conditions[i].Message = message
			task.Status.Conditions[i].UpdateTime = ts
			return
		}
	}
	task.Status.Conditions = append(task.Status.Conditions, &v1alpha1.Condition{
		Type:       condType,
		Status:     status,
		Reason:     reason,
		Message:    message,
		UpdateTime: ts,
	})
}

// taskTemplatePattern matches every ActorTemplate name taskTemplateName can produce
// for the given task, across all spec revisions.
func taskTemplatePattern(taskName string) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf("^%s-tmpl-[0-9a-f]{%d}$", regexp.QuoteMeta(taskName), 2*templateDigestBytes))
}

// Delete cleans up Substrate resources when a Task is deleted: the actor,
// which shares the task's name, then every per-task ActorTemplate provisioned for the task.
func (m *TaskManager) Delete(ctx context.Context, atespace, taskName string) error {
	return m.delete(ctx, atespace, taskName, false)
}

// DeleteTask cleans up Substrate resources for task. If the task runs directly
// from a user-supplied ActorTemplate (rather than a per-task template), template
// cleanup is skipped.
func (m *TaskManager) DeleteTask(ctx context.Context, task *v1alpha1.Task) error {
	if task == nil {
		return nil
	}
	atespace := task.GetMetadata().GetAtespace()
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	taskName := task.GetMetadata().GetName()
	tpl := task.GetStatus().GetAteTemplate().GetName()
	skipTemplates := task.GetSpec().GetProfile().GetAteTemplate().GetName() != "" ||
		(tpl != "" && !taskTemplatePattern(taskName).MatchString(tpl))
	return m.delete(ctx, atespace, taskName, skipTemplates)
}

func (m *TaskManager) delete(ctx context.Context, atespace, taskName string, skipTemplates bool) error {
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	slog.Info("deleting Substrate actor for task", "atespace", atespace, "task", taskName)
	if err := m.client.DeleteActor(ctx, atespace, taskName); err != nil {
		return err
	}
	if skipTemplates {
		return nil
	}
	if err := m.deleteTaskTemplates(ctx, atespace, taskName); err != nil {
		slog.Warn("could not clean up actor templates for task", "task", taskName, "error", err)
	}
	return nil
}

// deleteTaskTemplates removes all ActorTemplates belonging to the task. Templates
// accumulate across spec revisions, so this matches by name pattern rather than
// recomputing a single digest. If an actor deletion is still finishing in Substrate,
// template deletion may briefly return Aborted, so we retry with backoff.
func (m *TaskManager) deleteTaskTemplates(ctx context.Context, atespace, taskName string) error {
	templates, err := m.client.ListActorTemplates(ctx, atespace)
	if err != nil {
		return err
	}
	pattern := taskTemplatePattern(taskName)
	var errs []error
	for _, tmpl := range templates {
		name := tmpl.GetMetadata().GetName()
		if !pattern.MatchString(name) {
			continue
		}
		slog.Info("deleting Substrate actor template for task", "atespace", atespace, "task", taskName, "template", name)
		var delErr error
	Retry:
		for attempt := 0; attempt < 5; attempt++ {
			delErr = m.client.DeleteActorTemplate(ctx, atespace, name)
			if delErr == nil || status.Code(delErr) == codes.NotFound {
				delErr = nil
				break
			}
			select {
			case <-ctx.Done():
				delErr = ctx.Err()
				break Retry
			case <-time.After(500 * time.Millisecond):
			}
		}
		if delErr != nil {
			errs = append(errs, delErr)
		}
	}
	return errors.Join(errs...)
}
