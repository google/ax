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
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
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
	// resumed actor to finish workspace setup.
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

// TaskReconciler reconciles Task resources by provisioning and orchestrating
// sandboxed Actors on Agent Substrate.
type TaskReconciler struct {
	client                  *substrate.Client
	httpClient              *http.Client
	defaultTemplate         string
	defaultTemplateAtespace string
	// defaultTaskImage is used when the effective profile omits image and ateTemplate.name.
	defaultTaskImage string

	// WorkspaceReadyTimeout bounds how long Resume waits for the actor's workspace
	// to report ready before recording it as still initializing.
	WorkspaceReadyTimeout time.Duration

	// RunnerVolumeImage, when set, is a digest-pinned runner volume image
	// (Dockerfile.runner-volume) that task templates mount read-only and run
	// the runner from, so task images do not need the runner baked in. It is
	// part of the template name, so a new digest provisions new templates.
	// Empty keeps running the runner baked into the task image.
	RunnerVolumeImage string

	// RunnerEndpoint is the in-cluster host:port of ax-server's :8081 runner
	// service bound on every task actor's EgressPolicy (defaults to
	// substrate.DefaultRunnerEndpoint).
	RunnerEndpoint string

	// RunnerAudience is the OIDC audience bound to Actor JWTs injected for
	// RunnerEndpoint (defaults to substrate.DefaultRunnerAudience).
	RunnerAudience string

	// WorkspaceManager, when configured with pre-warming enabled, resolves warm
	// Workspace snapshot Tags so task actors start from pre-warmed snapshots.
	WorkspaceManager *WorkspaceManager

	// GatewayStore resolves Workspace.spec.setup.gateway if a task profile
	// override triggers a lazy Workspace snapshot build during CreateTask.
	GatewayStore GatewayStore
}

// NewTaskReconciler creates a new TaskReconciler. Empty defaults fall back to
// "default-template", v1alpha1.DefaultSystemNamespace and
// v1alpha1.DefaultTaskImage respectively.
func NewTaskReconciler(client *substrate.Client, defaultTemplate, defaultTemplateAtespace, defaultTaskImage string) *TaskReconciler {
	if defaultTemplate == "" {
		defaultTemplate = "default-template"
	}
	if defaultTemplateAtespace == "" {
		defaultTemplateAtespace = v1alpha1.DefaultSystemNamespace
	}
	if defaultTaskImage == "" {
		defaultTaskImage = v1alpha1.DefaultTaskImage
	}
	return &TaskReconciler{
		client:                  client,
		httpClient:              newProbeHTTPClient(),
		defaultTemplate:         defaultTemplate,
		defaultTemplateAtespace: defaultTemplateAtespace,
		defaultTaskImage:        defaultTaskImage,
		WorkspaceReadyTimeout:   defaultWorkspaceReadyTimeout,
		RunnerEndpoint:          substrate.DefaultRunnerEndpoint,
		RunnerAudience:          substrate.DefaultRunnerAudience,
	}
}

// Client returns the Substrate client used by the reconciler.
func (r *TaskReconciler) Client() *substrate.Client {
	return r.client
}

// DefaultTaskImage returns the fallback container image configured on the reconciler.
func (r *TaskReconciler) DefaultTaskImage() string {
	return r.defaultTaskImage
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
func (r *TaskReconciler) ensureSubstrateResources(ctx context.Context, task *v1alpha1.Task, ws *v1alpha1.Workspace) error {
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
			r.setNotReady(task, v1alpha1.ReasonInvalidResources, err.Error(), now)
			return fmt.Errorf("validating resources: %w", err)
		}
	}

	// 1. Ensure the Atespace exists in Substrate.
	if err := r.client.EnsureAtespace(ctx, atespace); err != nil {
		if substrate.IsTransientError(err) {
			r.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
			return fmt.Errorf("ensuring atespace: %w", err)
		}
		r.setNotReady(task, v1alpha1.ReasonAtespaceCreationFailed, err.Error(), now)
		return fmt.Errorf("ensuring atespace: %w", err)
	}

	// 2. The actor is always named after the task, so the two can be used
	// interchangeably (for example in the router's ate-target-actor header).
	actorName := task.Metadata.Name

	// 3. Resolve or provision the ActorTemplate for the task.
	templateAtespace, templateName, err := ensureProfileTemplate(ctx, r.client, atespace, effective, r.defaultTaskImage, r.RunnerVolumeImage)
	if err != nil {
		if substrate.IsTransientError(err) {
			r.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
			return fmt.Errorf("ensuring actor template: %w", err)
		}
		r.setNotReady(task, v1alpha1.ReasonTemplateCreationFailed, statusErrorMessage(err), now)
		return fmt.Errorf("ensuring actor template: %w", err)
	}

	// 4. Resolve the warm Workspace snapshot Tag when the bound Workspace has
	// setup steps or a compute profile and snapshot pre-warming is enabled.
	// Tasks mounting a Workspace at a non-default path skip the pre-warmed
	// snapshot so setup runs directly at their target path.
	wsPath := task.GetSpec().WorkspacePath()
	usesDefaultWorkspacePath := wsPath == "" || wsPath == v1alpha1.DefaultWorkspacePath
	var sourceTag string
	if usesDefaultWorkspacePath && ws != nil && needsPrewarmSnapshot(ws.GetSpec()) && r.WorkspaceManager != nil && r.WorkspaceManager.PrewarmEnabled() {
		var setupGW *v1alpha1.Gateway
		if gwName := ws.GetSpec().GetSetup().GetGateway().GetName(); gwName != "" && r.GatewayStore != nil {
			setupGW, _ = r.GatewayStore.GetGateway(ctx, atespace, gwName)
		}
		sourceTag, err = r.WorkspaceManager.EnsureSnapshotTag(ctx, ws, effective, setupGW)
		if err != nil {
			if substrate.IsTransientError(err) {
				r.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
				return fmt.Errorf("resolving workspace snapshot tag: %w", err)
			}
			r.setNotReady(task, v1alpha1.ReasonActorCreationFailed, statusErrorMessage(err), now)
			return fmt.Errorf("resolving workspace snapshot tag: %w", err)
		}
	}

	_, err = r.client.EnsureActorFromTag(ctx, atespace, actorName, templateAtespace, templateName, sourceTag)
	if err != nil {
		if substrate.IsTransientError(err) {
			r.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
			return fmt.Errorf("ensuring actor: %w", err)
		}
		r.setNotReady(task, v1alpha1.ReasonActorCreationFailed, err.Error(), now)
		return fmt.Errorf("ensuring actor: %w", err)
	}

	task.Status.AteTemplate = &v1alpha1.ObjectRef{
		Name:     templateName,
		Atespace: templateAtespace,
	}
	if len(task.Spec.Commands) > 0 && len(task.Status.ResolvedArgv) == 0 {
		task.Status.ResolvedArgv = slices.Clone(task.Spec.Commands)
	}
	if usesDefaultWorkspacePath && r.WorkspaceManager != nil && r.WorkspaceManager.PrewarmEnabled() && !conditionTrue(task, v1alpha1.ConditionWorkspaceReady) {
		msg := "Workspace requires no setup"
		if sourceTag != "" {
			msg = fmt.Sprintf("Workspace seeded from snapshot tag %s", sourceTag)
		}
		setCondition(task, v1alpha1.ConditionWorkspaceReady, "True", v1alpha1.ReasonSetupComplete, msg, now)
	}
	return nil
}

// IsMemoryFidelityTemplate reports whether the referenced ActorTemplate uses
// SNAPSHOT_FIDELITY_MEMORY.
func (r *TaskReconciler) IsMemoryFidelityTemplate(ctx context.Context, defaultAtespace string, ref *v1alpha1.ObjectRef) (bool, error) {
	if ref.GetName() == "" {
		return DefaultProfileSnapshotFidelity == ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, nil
	}
	atespace := ref.GetAtespace()
	if atespace == "" {
		atespace = defaultAtespace
	}
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	tmpl, err := r.client.GetActorTemplate(ctx, atespace, ref.GetName())
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return false, nil
		}
		return false, err
	}
	return tmpl.GetSnapshotConfig().GetPreferredFidelity() == ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, nil
}

// bindGateway ensures the task's actor has an EgressPolicy containing the
// built-in :8081 runner control rule plus any rules from the task's bound
// Gateway. An actor that already has a policy keeps it unchanged.
func (r *TaskReconciler) bindGateway(ctx context.Context, task *v1alpha1.Task, gw *v1alpha1.Gateway) error {
	gwName := task.Spec.GetGateway().GetName()
	atespace, actorName := task.Metadata.Atespace, task.Metadata.Name
	_, err := r.client.GetActorEgressPolicy(ctx, atespace, actorName)
	if status.Code(err) == codes.NotFound {
		rules := []*ateapipb.EgressRule{
			substrate.RunnerEgressRule(r.RunnerEndpoint, r.RunnerAudience),
		}
		if gwName != "" {
			if gw == nil {
				err = fmt.Errorf("not available in atespace %q", atespace)
			} else {
				rules = append(rules, gw.GetSpec().GetEgress()...)
				err = r.client.EnsureActorEgressPolicy(ctx, atespace, actorName, rules)
			}
		} else {
			err = r.client.EnsureActorEgressPolicy(ctx, atespace, actorName, rules)
		}
	}
	if err != nil {
		now := time.Now()
		if substrate.IsTransientError(err) {
			r.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
			if gwName != "" {
				return fmt.Errorf("binding gateway %q: %w", gwName, err)
			}
			return fmt.Errorf("binding runner egress policy: %w", err)
		}
		if gwName != "" {
			r.setNotReady(task, v1alpha1.ReasonGatewayBindingFailed, fmt.Sprintf("gateway %q: %s", gwName, statusErrorMessage(err)), now)
			return fmt.Errorf("binding gateway %q: %w", gwName, err)
		}
		r.setNotReady(task, v1alpha1.ReasonGatewayBindingFailed, statusErrorMessage(err), now)
		return fmt.Errorf("binding runner egress policy: %w", err)
	}
	return nil
}

// Suspend runs ensureSubstrateResources, suspends the task's actor, and then
// runs bindGateway, so the actor is suspended even when bindGateway fails. A
// failure to suspend the actor is returned without changing the task's status.
func (r *TaskReconciler) Suspend(ctx context.Context, task *v1alpha1.Task, ws *v1alpha1.Workspace, gw *v1alpha1.Gateway) error {
	var prevStatus *v1alpha1.TaskStatus
	if task.GetStatus() != nil {
		prevStatus = proto.Clone(task.Status).(*v1alpha1.TaskStatus)
	}
	if err := r.ensureSubstrateResources(ctx, task, ws); err != nil {
		return err
	}
	atespace, actorName := task.Metadata.Atespace, task.Metadata.Name
	slog.Info("suspending actor on Substrate", "actor", actorName)
	if err := r.client.SuspendActor(ctx, atespace, actorName); err != nil {
		task.Status = prevStatus
		return fmt.Errorf("suspending actor: %w", err)
	}
	if err := r.bindGateway(ctx, task, gw); err != nil {
		task.Status.AteTemplate = prevStatus.GetAteTemplate()
		return err
	}
	slog.Info("task successfully suspended", "name", task.Metadata.Name, "actor", actorName)
	return nil
}

// Resume runs ensureSubstrateResources and bindGateway, resumes the task's
// actor, and waits up to WorkspaceReadyTimeout for the task's workspace to be
// set up, recording the result in the WorkspaceReady condition. A failure to
// resume the actor is recorded the way ensureSubstrateResources records its own.
func (r *TaskReconciler) Resume(ctx context.Context, task *v1alpha1.Task, ws *v1alpha1.Workspace, gw *v1alpha1.Gateway) error {
	if err := r.ensureSubstrateResources(ctx, task, ws); err != nil {
		return err
	}
	if err := r.bindGateway(ctx, task, gw); err != nil {
		return err
	}
	atespace, actorName := task.Metadata.Atespace, task.Metadata.Name
	now := time.Now()

	// Resume the Actor to activate container execution.
	slog.Info("resuming actor on Substrate worker", "actor", actorName)
	_, workerIP, err := r.client.ResumeActor(ctx, atespace, actorName)
	if err != nil {
		if substrate.IsTransientError(err) {
			r.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
			return fmt.Errorf("resuming actor: %w", err)
		}
		r.setNotReady(task, v1alpha1.ReasonActorResumeFailed, err.Error(), now)
		return fmt.Errorf("resuming actor: %w", err)
	}

	// Check if workspace setup inside the actor has completed.
	host := workerIP
	port := "80"
	if h, p, err := net.SplitHostPort(workerIP); err == nil {
		host = h
		port = p
	}
	readyURL := fmt.Sprintf("http://%s/readyz?check=workspace", net.JoinHostPort(host, port))
	// Workspace setup happens once per task. After it has completed, WorkspaceReady stays
	// True across suspend/resume cycles, so only poll while it is still initializing.
	workspaceReady := conditionTrue(task, v1alpha1.ConditionWorkspaceReady)
	if workerIP != "" && !workspaceReady {
		// Poll briefly for workspace setup completion.
		pollCtx, cancel := context.WithTimeout(ctx, r.WorkspaceReadyTimeout)
		defer cancel()

		ticker := time.NewTicker(workspaceReadyPollInterval)
		defer ticker.Stop()

		checkReady := func() bool {
			// 1. Direct readyz check
			req, _ := http.NewRequestWithContext(pollCtx, http.MethodGet, readyURL, nil)
			if resp, err := r.httpClient.Do(req); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return true
				}
			}

			// 2. Router-based readyz check (in-cluster via atenet-router)
			if routerAddr := os.Getenv("ATENET_ROUTER_ADDR"); routerAddr != "" {
				routerURL := routerAddr
				if !strings.HasPrefix(routerURL, "http://") && !strings.HasPrefix(routerURL, "https://") {
					if strings.HasSuffix(routerURL, ":443") {
						routerURL = "https://" + routerURL
					} else {
						routerURL = "http://" + routerURL
					}
				}
				routerURL = strings.TrimRight(routerURL, "/") + "/readyz?check=workspace"
				rReq, _ := http.NewRequestWithContext(pollCtx, http.MethodGet, routerURL, nil)
				if rReq != nil {
					rReq.Header.Set("ate-target-actor", fmt.Sprintf("%s/%s", atespace, actorName))
					if resp, err := r.httpClient.Do(rReq); err == nil {
						_ = resp.Body.Close()
						if resp.StatusCode == http.StatusOK {
							return true
						}
					}
				}
			}
			return false
		}

		// Initial check
		workspaceReady = checkReady()

		for !workspaceReady {
			select {
			case <-pollCtx.Done():
				goto DonePolling
			case <-ticker.C:
				if checkReady() {
					workspaceReady = true
					goto DonePolling
				}
			}
		}
	}
DonePolling:

	// The server derives Ready from the actor's state and WorkspaceReady.
	if workspaceReady {
		if !conditionTrue(task, v1alpha1.ConditionWorkspaceReady) {
			setCondition(task, v1alpha1.ConditionWorkspaceReady, "True", v1alpha1.ReasonSetupComplete, fmt.Sprintf("Workspace setup completed at %s", workerIP), time.Now())
		}
	} else {
		setCondition(task, v1alpha1.ConditionWorkspaceReady, "False", v1alpha1.ReasonInitializing, fmt.Sprintf("Workspace is initializing at %s", workerIP), time.Now())
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
func (r *TaskReconciler) setNotReady(task *v1alpha1.Task, reason, message string, t time.Time) {
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
func (r *TaskReconciler) Delete(ctx context.Context, atespace, taskName string) error {
	return r.delete(ctx, atespace, taskName, false)
}

// DeleteTask cleans up Substrate resources for task. If the task runs directly
// from a user-supplied ActorTemplate (rather than a per-task template), template
// cleanup is skipped.
func (r *TaskReconciler) DeleteTask(ctx context.Context, task *v1alpha1.Task) error {
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
	return r.delete(ctx, atespace, taskName, skipTemplates)
}

func (r *TaskReconciler) delete(ctx context.Context, atespace, taskName string, skipTemplates bool) error {
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	slog.Info("deleting Substrate actor for task", "atespace", atespace, "task", taskName)
	if err := r.client.DeleteActor(ctx, atespace, taskName); err != nil {
		return err
	}
	if skipTemplates {
		return nil
	}
	if err := r.deleteTaskTemplates(ctx, atespace, taskName); err != nil {
		slog.Warn("could not clean up actor templates for task", "task", taskName, "error", err)
	}
	return nil
}

// deleteTaskTemplates removes all ActorTemplates belonging to the task. Templates
// accumulate across spec revisions, so this matches by name pattern rather than
// recomputing a single digest. If an actor deletion is still finishing in Substrate,
// template deletion may briefly return Aborted, so we retry with backoff.
func (r *TaskReconciler) deleteTaskTemplates(ctx context.Context, atespace, taskName string) error {
	templates, err := r.client.ListActorTemplates(ctx, atespace)
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
			delErr = r.client.DeleteActorTemplate(ctx, atespace, name)
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
