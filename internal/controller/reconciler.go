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
	"crypto/sha256"
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
	"sort"
	"strings"
	"time"

	"github.com/google/ax/internal/apivalidation"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"
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

// TaskReconciler reconciles Task resources by provisioning and orchestrating
// sandboxed Actors on Agent Substrate.
type TaskReconciler struct {
	client                  *substrate.Client
	httpClient              *http.Client
	defaultTemplate         string
	defaultTemplateAtespace string
	// defaultTaskImage is used when the effective profile omits image and ate_actor_template.
	defaultTaskImage string

	// WorkspaceReadyTimeout bounds how long Resume waits for the actor's workspace
	// to report ready before recording it as still initializing.
	WorkspaceReadyTimeout time.Duration
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
	}
}

// Client returns the Substrate client used by the reconciler.
func (r *TaskReconciler) Client() *substrate.Client {
	return r.client
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
	if effective.GetAteActorTemplate() == "" {
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
	var (
		templateAtespace, templateName string
		err                            error
	)
	if tpl := effective.GetAteActorTemplate(); tpl != "" {
		slog.Info("reconciling task with existing ActorTemplate", "name", task.Metadata.Name, "atespace", atespace, "ateActorTemplate", tpl)
		if _, err := r.client.GetActorTemplate(ctx, atespace, tpl); err != nil {
			if substrate.IsTransientError(err) {
				r.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
				return fmt.Errorf("getting actor template: %w", err)
			}
			r.setNotReady(task, v1alpha1.ReasonTemplateCreationFailed, substrateErrorMessage(err), now)
			return fmt.Errorf("getting actor template: %w", err)
		}
		templateAtespace = atespace
		templateName = tpl
	} else {
		image := effective.GetImage()
		if image == "" {
			image = r.defaultTaskImage
		}
		slog.Info("reconciling task", "name", task.Metadata.Name, "atespace", atespace, "image", image)
		templateAtespace, templateName, err = r.ensureTaskTemplate(ctx, task, ws, atespace, image, effective.GetResources(), now)
		if err != nil {
			return err
		}
	}

	_, err = r.client.EnsureActor(ctx, atespace, actorName, templateAtespace, templateName)
	if err != nil {
		if substrate.IsTransientError(err) {
			r.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
			return fmt.Errorf("ensuring actor: %w", err)
		}
		r.setNotReady(task, v1alpha1.ReasonActorCreationFailed, err.Error(), now)
		return fmt.Errorf("ensuring actor: %w", err)
	}

	task.Status.AteActorTemplate = templateName
	if len(task.Spec.Commands) > 0 && len(task.Status.ResolvedArgv) == 0 {
		task.Status.ResolvedArgv = slices.Clone(task.Spec.Commands)
	}
	return nil
}

func (r *TaskReconciler) ensureTaskTemplate(
	ctx context.Context,
	task *v1alpha1.Task,
	ws *v1alpha1.Workspace,
	atespace, image string,
	resSpec *v1alpha1.ResourcesSpec,
	now time.Time,
) (string, string, error) {
	templateName := r.defaultTemplate
	templateAtespace := r.defaultTemplateAtespace
	if parts := strings.SplitN(templateName, "/", 2); len(parts) == 2 {
		templateAtespace = parts[0]
		templateName = parts[1]
	}

	// Prepare container environment variables (task env + specs).
	extraEnv := make(map[string]string)
	for _, e := range task.Spec.Env {
		if e.Name != "" {
			extraEnv[e.Name] = e.Value
		}
	}

	// Only launch configuration belongs in the template; status and mutable
	// lifecycle timestamps must not create new golden snapshots.
	launchTask := proto.Clone(task).(*v1alpha1.Task)
	launchTask.Status = nil
	if launchTask.Metadata != nil {
		launchTask.Metadata.UpdateTime = nil
		launchTask.Metadata.DeleteTime = nil
	}
	if taskYAML, err := yaml.Marshal(launchTask); err == nil {
		extraEnv["AX_TASK_YAML"] = string(taskYAML)
	}

	// Inject the bound Workspace spec, if any.
	if ws != nil {
		if wsYAML, err := yaml.Marshal(ws); err == nil {
			extraEnv["AX_WORKSPACE_YAML"] = string(wsYAML)
		}
	}

	resources := substrate.ResourceLimits(resSpec)

	if image != "" || len(extraEnv) > 0 {
		slog.Info("ensuring custom ActorTemplate for task", "image", image)
		customTemplateName := taskTemplateName(task.Metadata.Name, image, extraEnv)

		// spec.profile.resources rides along in AX_TASK_YAML / AX_WORKSPACE_YAML, so a
		// limits change is already part of the template digest and re-provisions the template.
		tmpl, err := r.client.EnsureActorTemplateWithImage(ctx, templateAtespace, templateName, atespace, customTemplateName, image, resources, extraEnv)
		if err != nil {
			if substrate.IsTransientError(err) {
				r.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
				return "", "", fmt.Errorf("ensuring actor template: %w", err)
			}
			// Falling back to the default template would run a different image than
			// the user asked for, without the limits it asked for, or fail later with
			// a misleading "actor template not found". Surface the real error on the
			// task and stop here.
			r.setNotReady(task, v1alpha1.ReasonTemplateCreationFailed, substrateErrorMessage(err), now)
			return "", "", fmt.Errorf("ensuring actor template: %w", err)
		}
		if tmpl != nil && tmpl.Metadata != nil {
			templateAtespace = tmpl.Metadata.Atespace
			templateName = tmpl.Metadata.Name
			slog.Info("using custom ActorTemplate for actor", "templateAtespace", templateAtespace, "templateName", templateName)
		}
	}

	return templateAtespace, templateName, nil
}

// bindGateway makes sure that the actor of a task that binds a gateway has
// rules. An actor that has rules keeps them. An actor without rules gets the
// rules of gw, or bindGateway fails if gw is nil. A failure is recorded the
// way ensureSubstrateResources records its own.
func (r *TaskReconciler) bindGateway(ctx context.Context, task *v1alpha1.Task, gw *v1alpha1.Gateway) error {
	gwName := task.Spec.GetGateway().GetName()
	if gwName == "" {
		return nil
	}
	atespace, actorName := task.Metadata.Atespace, task.Metadata.Name
	_, err := r.client.GetActorEgressPolicy(ctx, atespace, actorName)
	if status.Code(err) == codes.NotFound {
		if gw == nil {
			err = fmt.Errorf("not available in atespace %q", atespace)
		} else {
			err = r.client.EnsureActorEgressPolicy(ctx, atespace, actorName, gw.GetSpec().GetEgress())
		}
	}
	if err != nil {
		now := time.Now()
		if substrate.IsTransientError(err) {
			r.setNotReady(task, v1alpha1.ReasonSubstrateUnavailable, err.Error(), now)
			return fmt.Errorf("binding gateway %q: %w", gwName, err)
		}
		r.setNotReady(task, v1alpha1.ReasonGatewayBindingFailed, fmt.Sprintf("gateway %q: %s", gwName, substrateErrorMessage(err)), now)
		return fmt.Errorf("binding gateway %q: %w", gwName, err)
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

// substrateErrorMessage returns the message to show users for a failed Substrate call.
// Substrate errors reach the reconciler wrapped (fmt.Errorf("...: %w", grpcErr)), and
// status.Convert on a wrapped error yields the full err.Error() including the
// "rpc error: code = ... desc = " boilerplate. Unwrapping to the underlying gRPC
// status recovers just the description Substrate wrote, e.g.
// `actor_template.containers[0].image: Invalid value: "...": must be pinned by digest`.
// Non-gRPC errors are returned verbatim.
func substrateErrorMessage(err error) string {
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

// taskTemplateName derives the per-task ActorTemplate name from the task name and a
// digest of the image and container environment, so a spec change yields a new template.
func taskTemplateName(taskName, image string, env map[string]string) string {
	h := sha256.New()
	h.Write([]byte(image))
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k + "=" + env[k] + ";"))
	}
	return fmt.Sprintf("%s-tmpl-%x", taskName, h.Sum(nil)[:templateDigestBytes])
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
	tpl := task.GetStatus().GetAteActorTemplate()
	skipTemplates := task.GetSpec().GetProfile().GetAteActorTemplate() != "" ||
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
