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

package substrate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"
	"github.com/google/ax/internal/resources"
	"github.com/google/ax/pkg/apis/v1alpha1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// ClientOptions configures connection to the Substrate Control API server.
type ClientOptions struct {
	Target      string
	Authority   string
	TokenFile   string
	CAFile      string
	InsecureTLS bool
	Plaintext   bool
}

type tokenAuth struct {
	tokenFile string
}

func (t tokenAuth) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	if t.tokenFile == "" {
		return nil, nil
	}
	b, err := os.ReadFile(t.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("reading token file %q: %w", t.tokenFile, err)
	}
	return map[string]string{
		"authorization": "Bearer " + strings.TrimSpace(string(b)),
	}, nil
}

func (t tokenAuth) RequireTransportSecurity() bool {
	return false
}

// Client wraps the Substrate Control API client.
type Client struct {
	conn    *grpc.ClientConn
	control ateapipb.ControlClient
}

// NewClientWithOptions connects to Substrate with full TLS and auth configuration.
func NewClientWithOptions(opts ClientOptions, extraDialOpts ...grpc.DialOption) (*Client, error) {
	if opts.Target == "" {
		opts.Target = "api.ate-system.svc.cluster.local:443"
	}

	// Auto-detect token file if not specified
	if opts.TokenFile == "" {
		if envToken := os.Getenv("SUBSTRATE_TOKEN_FILE"); envToken != "" {
			opts.TokenFile = envToken
		} else if _, err := os.Stat("/var/run/secrets/ateapi/token"); err == nil {
			opts.TokenFile = "/var/run/secrets/ateapi/token"
		}
	}

	// Auto-detect CA file if not specified
	if opts.CAFile == "" {
		if envCA := os.Getenv("SUBSTRATE_CA_FILE"); envCA != "" {
			opts.CAFile = envCA
		} else if _, err := os.Stat("/run/servicedns-ca/trust-bundle.pem"); err == nil {
			opts.CAFile = "/run/servicedns-ca/trust-bundle.pem"
		} else if _, err := os.Stat("/run/servicedns-ca/ca.pem"); err == nil {
			opts.CAFile = "/run/servicedns-ca/ca.pem"
		}
	}

	dialOpts := append([]grpc.DialOption{}, extraDialOpts...)

	if opts.Plaintext {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	} else {
		tlsConfig := &tls.Config{}
		if opts.InsecureTLS {
			tlsConfig.InsecureSkipVerify = true
		}
		if opts.Authority != "" {
			tlsConfig.ServerName = opts.Authority
		} else if strings.Contains(opts.Target, "api.ate-system.svc") {
			tlsConfig.ServerName = "api.ate-system.svc"
		}

		if opts.CAFile != "" {
			caCert, err := os.ReadFile(opts.CAFile)
			if err != nil {
				return nil, fmt.Errorf("reading CA file %q: %w", opts.CAFile, err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caCert) {
				return nil, fmt.Errorf("failed to parse CA certificate from %q", opts.CAFile)
			}
			tlsConfig.RootCAs = pool
		}

		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	}

	authority := opts.Authority
	if authority == "" && strings.Contains(opts.Target, "api.ate-system.svc") {
		authority = "api.ate-system.svc"
	}
	if authority != "" {
		dialOpts = append(dialOpts, grpc.WithAuthority(authority))
	}

	if opts.TokenFile != "" {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(tokenAuth{tokenFile: opts.TokenFile}))
	}

	conn, err := grpc.NewClient(opts.Target, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Substrate control service at %s: %w", opts.Target, err)
	}
	return &Client{
		conn:    conn,
		control: ateapipb.NewControlClient(conn),
	}, nil
}

// NewClient establishes a connection to the Substrate Control API server.
func NewClient(target string, opts ...grpc.DialOption) (*Client, error) {
	if len(opts) > 0 {
		if target == "" {
			target = "api.ate-system.svc.cluster.local:443"
		}
		conn, err := grpc.NewClient(target, opts...)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to Substrate control service at %s: %w", target, err)
		}
		return &Client{
			conn:    conn,
			control: ateapipb.NewControlClient(conn),
		}, nil
	}

	// Auto-configure options when no explicit dial options given
	return NewClientWithOptions(ClientOptions{Target: target})
}

// Close closes the gRPC connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// EnsureAtespace creates the atespace if it does not already exist.
func (c *Client) EnsureAtespace(ctx context.Context, atespace string) error {
	req := &ateapipb.CreateAtespaceRequest{
		Atespace: &ateapipb.Atespace{
			Metadata: &ateapipb.ResourceMetadata{
				Name: atespace,
			},
		},
	}
	_, err := callWithRetry(ctx, func() (*ateapipb.Atespace, error) {
		return c.control.CreateAtespace(ctx, req)
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return fmt.Errorf("creating atespace %q: %w", atespace, err)
	}
	return nil
}

// GetActorTemplate fetches an ActorTemplate by name.
func (c *Client) GetActorTemplate(ctx context.Context, atespace, templateName string) (*ateapipb.ActorTemplate, error) {
	req := &ateapipb.GetActorTemplateRequest{
		ActorTemplate: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     templateName,
		},
	}
	return callWithRetry(ctx, func() (*ateapipb.ActorTemplate, error) {
		return c.control.GetActorTemplate(ctx, req)
	})
}

const (
	DefaultGuestCommand = "/usr/local/bin/ax-task-runner"

	// defaultRetryAttempts is the maximum number of calls retryOnCodes makes. With
	// exponential backoff from defaultRetryInterval capped at maxRetryInterval
	// (±50% jitter), the total wait across all attempts is roughly 6s-20s.
	defaultRetryAttempts = 10

	// retryBackoffFactor is the multiplier applied to the backoff after each failed attempt.
	retryBackoffFactor = 2

	// actorDeletingWaitAttempts bounds how many times EnsureActor retries creation
	// while a previous actor with the same name is still being deleted.
	actorDeletingWaitAttempts = 20
)

var (
	// defaultRetryInterval is the initial backoff for retryOnCodes and the poll
	// interval used while waiting for actor deletion.
	defaultRetryInterval = 200 * time.Millisecond

	// maxRetryInterval caps the exponential backoff in retryOnCodes.
	maxRetryInterval = 2 * time.Second

	// actorDeletingWaitInterval is the interval between creation attempts while a
	// previous actor is still deleting (actorDeletingWaitAttempts * interval ~= 10s).
	actorDeletingWaitInterval = 500 * time.Millisecond
)

// IsTransientError reports whether err represents a transient Substrate
// control-plane unavailability or timeout rather than a permanent failure.
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Unavailable, codes.DeadlineExceeded:
			return true
		}
	}
	return false
}

// retryOnCodes calls fn until it succeeds, returns an error whose gRPC code is not in
// retryable, defaultRetryAttempts is exhausted, or ctx is done. Between attempts it
// waits with jittered exponential backoff starting at defaultRetryInterval and
// capped at maxRetryInterval.
func retryOnCodes(ctx context.Context, fn func() error, retryable ...codes.Code) error {
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = defaultRetryInterval
	b.MaxInterval = maxRetryInterval
	b.Multiplier = retryBackoffFactor

	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		err := fn()
		if err != nil && !slices.Contains(retryable, status.Code(err)) {
			return struct{}{}, backoff.Permanent(err)
		}
		return struct{}{}, err
	}, backoff.WithBackOff(b), backoff.WithMaxTries(defaultRetryAttempts))

	// backoff.Retry returns the *PermanentError wrapper as-is when the attempt limit
	// is hit on the same try, so unwrap it to always surface the original error.
	var permanent *backoff.PermanentError
	if errors.As(err, &permanent) {
		return permanent.Unwrap()
	}
	return err
}

// callWithRetry runs fn with retryOnCodes, always retrying codes.Unavailable
// plus any additional retryable codes (such as codes.Aborted).
func callWithRetry[T any](ctx context.Context, fn func() (T, error), extraRetryable ...codes.Code) (T, error) {
	var out T
	retryable := append([]codes.Code{codes.Unavailable}, extraRetryable...)
	err := retryOnCodes(ctx, func() error {
		var callErr error
		out, callErr = fn()
		return callErr
	}, retryable...)
	return out, err
}

const (
	// DefaultCPUMillicores is the default CPU limit (1 core) applied to an
	// ActorTemplate when a Task omits spec.resources.limits.cpu.
	DefaultCPUMillicores int64 = 1000
	// DefaultMemoryBytes is the default memory limit (512 MiB) applied to an
	// ActorTemplate when a Task omits spec.resources.limits.memory.
	DefaultMemoryBytes int64 = 512 << 20
)

// ResourceLimits translates a Task's resource limits into the Substrate
// ActorTemplate resources block. Substrate sizes a sandbox by limits alone. It
// returns nil when no limit is set so the template uses the default sandbox
// limits.
func ResourceLimits(reqs *v1alpha1.ResourcesSpec) *ateapipb.ResourceRequirements {
	limits := reqs.GetLimits()
	cpuStr := limits.GetCpu()
	memStr := limits.GetMemory()
	if cpuStr == "" && memStr == "" {
		return nil
	}
	out := &ateapipb.Resources{
		CpuMillicores: DefaultCPUMillicores,
		MemoryBytes:   DefaultMemoryBytes,
	}
	if cpuStr != "" {
		if cpu, err := resources.ParseCPUMillicores(cpuStr); err == nil {
			out.CpuMillicores = cpu
		}
	}
	if memStr != "" {
		if mem, err := resources.ParseMemoryBytes(memStr); err == nil {
			out.MemoryBytes = mem
		}
	}
	return &ateapipb.ResourceRequirements{Limits: out}
}

// BuildActorTemplate constructs a Substrate ActorTemplate based on the standard ate-env specification.
// A nil resources applies DefaultCPUMillicores and DefaultMemoryBytes.
// snapshotsBucket sets the template-level snapshot storage location; note that Substrate rejects
// a non-empty storage_location with InvalidArgument (Forbidden) unless the Substrate cluster has
// the PerTemplateSnapshotLocation preview feature gate enabled. Leaving snapshotsBucket empty uses
// Substrate's cluster-wide snapshot storage location.
func BuildActorTemplate(atespace, name, image string, envMap map[string]string, command []string, snapshotsBucket string, resources *ateapipb.ResourceRequirements) *ateapipb.ActorTemplate {
	if atespace == "" {
		atespace = v1alpha1.DefaultAtespace
	}
	if image == "" {
		image = v1alpha1.DefaultTaskImage
	}
	if len(command) == 0 {
		command = []string{DefaultGuestCommand}
	}
	var envList []*ateapipb.EnvVar
	for k, v := range envMap {
		envList = append(envList, &ateapipb.EnvVar{
			Name:  k,
			Value: v,
		})
	}

	cpuMillicores := DefaultCPUMillicores
	memoryBytes := DefaultMemoryBytes
	if limits := resources.GetLimits(); limits != nil {
		if limits.GetCpuMillicores() > 0 {
			cpuMillicores = limits.GetCpuMillicores()
		}
		if limits.GetMemoryBytes() > 0 {
			memoryBytes = limits.GetMemoryBytes()
		}
	}

	snapCfg := &ateapipb.SnapshotConfig{
		PreferredFidelity: ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES,
	}
	if snapshotsBucket != "" {
		snapCfg.StorageLocation = snapshotsBucket
	}
	trustBundleDir, trustBundleFile := path.Split(v1alpha1.EgressTrustBundlePath)

	return &ateapipb.ActorTemplate{
		Metadata: &ateapipb.ResourceMetadata{
			Name:     name,
			Atespace: atespace,
		},
		Containers: []*ateapipb.Container{{
			Name:    "guest",
			Image:   image,
			Command: command,
			Env:     envList,
			WakeupProbe: &ateapipb.WakeupProbe{
				HttpGet: &ateapipb.HttpGetAction{
					Path: "/readyz",
					Port: 80,
				},
			},
			VolumeMounts: []*ateapipb.VolumeMount{{
				Name:      "workspace",
				MountPath: "/workspace",
			}, {
				Name:      "egress-trust-bundle",
				MountPath: path.Clean(trustBundleDir),
			}},
		}},
		Volumes: []*ateapipb.Volume{{
			Name:       "workspace",
			DurableDir: &ateapipb.DurableDirVolumeSource{},
		}, {
			// Mounted for every task, with or without a gateway, so that TLS
			// verifies whether or not something on the way out intercepts it.
			Name: "egress-trust-bundle",
			SystemInfo: &ateapipb.SystemInfoVolumeSource{
				DataSources: []*ateapipb.SystemInfoDataSource{{
					TrustBundle: &ateapipb.TrustBundleDataSource{
						Names: []string{"egress-mitm.ate.dev", "system-roots.ate.dev"},
						Path:  trustBundleFile,
					},
				}},
			},
		}},
		SnapshotConfig: snapCfg,
		Resources: &ateapipb.ResourceRequirements{
			Limits: &ateapipb.Resources{
				CpuMillicores: cpuMillicores,
				MemoryBytes:   memoryBytes,
			},
		},
	}
}

// EnsureActorTemplate returns the named ActorTemplate if it already exists, or
// creates it from tmpl idempotently.
func (c *Client) EnsureActorTemplate(ctx context.Context, tmpl *ateapipb.ActorTemplate) (*ateapipb.ActorTemplate, error) {
	atespace := tmpl.GetMetadata().GetAtespace()
	name := tmpl.GetMetadata().GetName()
	existing, err := c.GetActorTemplate(ctx, atespace, name)
	if err == nil && existing != nil {
		return existing, nil
	}
	if err != nil && status.Code(err) != codes.NotFound {
		return nil, fmt.Errorf("getting actor template %s/%s: %w", atespace, name, err)
	}

	req := &ateapipb.CreateActorTemplateRequest{ActorTemplate: tmpl}
	created, err := callWithRetry(ctx, func() (*ateapipb.ActorTemplate, error) {
		return c.control.CreateActorTemplate(ctx, req)
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return nil, fmt.Errorf("creating actor template %s/%s: %w", atespace, name, err)
	}
	if created != nil {
		return created, nil
	}
	// Another caller created the same template concurrently (AlreadyExists);
	// fetch the winner's template so the returned metadata/UID is populated.
	return c.GetActorTemplate(ctx, atespace, name)
}

// EnsureActorTemplateWithImage creates an ActorTemplate using the specified container image,
// resource limits (nil for default limits), and optional environment variables.
//
// TODO: Remove once TaskReconciler migrates from per-task templates to shared
// Profile ActorTemplates via EnsureActorTemplate.
func (c *Client) EnsureActorTemplateWithImage(ctx context.Context, baseAtespace, baseTemplate, targetAtespace, targetTemplate, image string, resources *ateapipb.ResourceRequirements, extraEnv ...map[string]string) (*ateapipb.ActorTemplate, error) {
	envMap := make(map[string]string)
	for _, envs := range extraEnv {
		maps.Copy(envMap, envs)
	}
	tmpl := BuildActorTemplate(targetAtespace, targetTemplate, image, envMap, nil, "", resources)
	return c.EnsureActorTemplate(ctx, tmpl)
}

// EnsureActor creates an Actor in the specified atespace deriving from an ActorTemplate.
func (c *Client) EnsureActor(ctx context.Context, atespace, actorName, templateAtespace, templateName string) (*ateapipb.Actor, error) {
	return c.EnsureActorFromTag(ctx, atespace, actorName, templateAtespace, templateName, "")
}

// EnsureActorFromTag creates an Actor in atespace deriving from an ActorTemplate,
// seeded from sourceTagName (a Workspace snapshot Tag) when non-empty.
func (c *Client) EnsureActorFromTag(ctx context.Context, atespace, actorName, templateAtespace, templateName, sourceTagName string) (*ateapipb.Actor, error) {
	actorSpec := &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: atespace, Name: actorName},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: templateAtespace, Name: templateName},
	}
	if sourceTagName != "" {
		actorSpec.SourceTag = &ateapipb.ObjectRef{Atespace: atespace, Name: sourceTagName}
	}
	req := &ateapipb.CreateActorRequest{Actor: actorSpec}

	actor, err := callWithRetry(ctx, func() (*ateapipb.Actor, error) {
		return c.control.CreateActor(ctx, req)
	})
	if err == nil {
		return actor, nil
	}
	if status.Code(err) == codes.AlreadyExists {
		return c.resolveExistingActor(ctx, req, atespace, actorName)
	}
	return nil, fmt.Errorf("creating actor %s/%s: %w", atespace, actorName, err)
}

// resolveExistingActor handles AlreadyExists when EnsureActor/EnsureActorFromTag
// is called on subsequent Suspend/Resume cycles or retries for an actor that was
// already provisioned on Substrate.
func (c *Client) resolveExistingActor(ctx context.Context, req *ateapipb.CreateActorRequest, atespace, actorName string) (*ateapipb.Actor, error) {
	existing, err := c.GetActor(ctx, atespace, actorName)
	if status.Code(err) == codes.NotFound {
		// Previous deleting actor finished removal right after CreateActor returned AlreadyExists.
		actor, createErr := callWithRetry(ctx, func() (*ateapipb.Actor, error) {
			return c.control.CreateActor(ctx, req)
		})
		if createErr != nil {
			return nil, fmt.Errorf("creating actor %s/%s: %w", atespace, actorName, createErr)
		}
		return actor, nil
	}
	if err != nil || existing == nil {
		return existing, err
	}
	switch existing.GetStatus().GetState() {
	case ateapipb.ActorState_ACTOR_STATE_CRASHED:
		return c.recoverCrashedActor(ctx, req, atespace, actorName)
	case ateapipb.ActorState_ACTOR_STATE_DELETING:
		return c.recreateDeletingActor(ctx, req, atespace, actorName)
	default:
		return existing, nil
	}
}

// recoverCrashedActor rolls a crashed actor back to its last snapshot via
// RevertActor so the workspace volume survives the crash. If RevertActor fails
// (for example when the actor crashed before taking any snapshot), it deletes
// and recreates the actor from scratch.
func (c *Client) recoverCrashedActor(ctx context.Context, req *ateapipb.CreateActorRequest, atespace, actorName string) (*ateapipb.Actor, error) {
	// Revert to the last snapshot so the workspace survives the crash.
	reverted, revertErr := callWithRetry(ctx, func() (*ateapipb.RevertActorResponse, error) {
		return c.control.RevertActor(ctx, &ateapipb.RevertActorRequest{
			Actor: &ateapipb.ObjectRef{Atespace: atespace, Name: actorName},
		})
	}, codes.Aborted)
	if revertErr == nil {
		slog.Warn("existing actor is crashed, reverted to its last snapshot", "actor", actorName)
		return reverted.GetActor(), nil
	}

	slog.Warn("existing actor is crashed and could not be reverted, deleting and recreating", "actor", actorName, "error", revertErr)
	if err := c.DeleteActor(ctx, atespace, actorName); err != nil {
		return nil, err
	}
	actor, err := callWithRetry(ctx, func() (*ateapipb.Actor, error) {
		return c.control.CreateActor(ctx, req)
	})
	if err != nil {
		return nil, fmt.Errorf("recreating actor %s/%s after crash: %w", atespace, actorName, err)
	}
	return actor, nil
}

// recreateDeletingActor waits briefly for a previous actor of the same name to
// finish deleting before recreating it.
func (c *Client) recreateDeletingActor(ctx context.Context, req *ateapipb.CreateActorRequest, atespace, actorName string) (*ateapipb.Actor, error) {
	// Wait briefly for previous actor deletion to finalize before recreating.
	for i := 0; i < actorDeletingWaitAttempts; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(actorDeletingWaitInterval):
		}
		actor, err := callWithRetry(ctx, func() (*ateapipb.Actor, error) {
			return c.control.CreateActor(ctx, req)
		})
		if err == nil {
			return actor, nil
		}
		if status.Code(err) != codes.AlreadyExists {
			return nil, fmt.Errorf("creating actor %s/%s: %w", atespace, actorName, err)
		}
	}
	return nil, fmt.Errorf("actor %s/%s is still deleting; please retry", atespace, actorName)
}

// ResumeActor resumes the specified actor onto a worker and returns the worker details.
func (c *Client) ResumeActor(ctx context.Context, atespace, actorName string) (*ateapipb.Actor, string, error) {
	req := &ateapipb.ResumeActorRequest{
		Actor: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     actorName,
		},
	}
	resp, err := callWithRetry(ctx, func() (*ateapipb.ResumeActorResponse, error) {
		return c.control.ResumeActor(ctx, req)
	}, codes.Aborted)
	if err != nil {
		return nil, "", fmt.Errorf("resuming actor %s/%s: %w", atespace, actorName, err)
	}
	if resp.GetActor() == nil {
		return nil, "", fmt.Errorf("nil actor returned when resuming %s/%s", atespace, actorName)
	}
	return resp.GetActor(), WorkerIP(resp.GetActor()), nil
}

// WorkerIP returns the pod IP of the worker hosting the actor, or "" when the
// actor has no worker (for example while it is suspended).
func WorkerIP(actor *ateapipb.Actor) string {
	if ips := actor.GetStatus().GetWorkerAssignment().GetWorkerPodIps(); len(ips) > 0 {
		return ips[0]
	}
	return ""
}

// SuspendActor suspends the specified actor, triggering state checkpointing.
func (c *Client) SuspendActor(ctx context.Context, atespace, actorName string) error {
	req := &ateapipb.SuspendActorRequest{
		Actor: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     actorName,
		},
	}
	_, err := callWithRetry(ctx, func() (*ateapipb.SuspendActorResponse, error) {
		return c.control.SuspendActor(ctx, req)
	}, codes.Aborted)
	if err != nil {
		return fmt.Errorf("suspending actor %s/%s: %w", atespace, actorName, err)
	}
	return nil
}

// GetActor fetches an Actor by name.
func (c *Client) GetActor(ctx context.Context, atespace, actorName string) (*ateapipb.Actor, error) {
	req := &ateapipb.GetActorRequest{
		Actor: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     actorName,
		},
	}
	return callWithRetry(ctx, func() (*ateapipb.Actor, error) {
		return c.control.GetActor(ctx, req)
	})
}

// ListActors returns every Actor in the given atespace, or in all atespaces
// when atespace is empty.
func (c *Client) ListActors(ctx context.Context, atespace string) ([]*ateapipb.Actor, error) {
	var actors []*ateapipb.Actor
	req := &ateapipb.ListActorsRequest{Atespace: atespace, Options: &ateapipb.ListOptions{}}
	for {
		resp, err := callWithRetry(ctx, func() (*ateapipb.ListActorsResponse, error) {
			return c.control.ListActors(ctx, req)
		})
		if err != nil {
			return nil, fmt.Errorf("listing actors in %q: %w", atespace, err)
		}
		actors = append(actors, resp.GetActors()...)
		if resp.GetNextPageToken() == "" {
			return actors, nil
		}
		req.Options.PageToken = resp.GetNextPageToken()
	}
}

// egressPolicyName is the name Substrate requires for an actor's egress
// policy. An actor has at most one.
const egressPolicyName = "default"

// GetActorEgressPolicy fetches the actor's egress policy. The error has code
// NotFound when the actor has none.
func (c *Client) GetActorEgressPolicy(ctx context.Context, atespace, actorName string) (*ateapipb.EgressPolicy, error) {
	req := &ateapipb.GetActorEgressPolicyRequest{
		Actor: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     actorName,
		},
		Name: egressPolicyName,
	}
	return callWithRetry(ctx, func() (*ateapipb.EgressPolicy, error) {
		return c.control.GetActorEgressPolicy(ctx, req)
	})
}

// EnsureActorEgressPolicy creates the actor's egress policy with the given
// rules if it does not already exist.
func (c *Client) EnsureActorEgressPolicy(ctx context.Context, atespace, actorName string, rules []*ateapipb.EgressRule) error {
	req := &ateapipb.CreateActorEgressPolicyRequest{
		Actor: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     actorName,
		},
		EgressPolicy: &ateapipb.EgressPolicy{
			Metadata: &ateapipb.ResourceMetadata{
				Atespace: atespace,
				Name:     egressPolicyName,
			},
			Rules: rules,
		},
	}
	_, err := callWithRetry(ctx, func() (*ateapipb.EgressPolicy, error) {
		return c.control.CreateActorEgressPolicy(ctx, req)
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return fmt.Errorf("creating egress policy of actor %s/%s: %w", atespace, actorName, err)
	}
	return nil
}

// DeleteActor deletes the specified actor from Substrate and blocks until the actor
// is fully deleted.
func (c *Client) DeleteActor(ctx context.Context, atespace, actorName string) error {
	req := &ateapipb.DeleteActorRequest{
		Actor: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     actorName,
		},
	}
	_, err := callWithRetry(ctx, func() (*ateapipb.Actor, error) {
		return c.control.DeleteActor(ctx, req)
	}, codes.Aborted)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		return fmt.Errorf("deleting actor %s/%s: %w", atespace, actorName, err)
	}

	getReq := &ateapipb.GetActorRequest{
		Actor: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     actorName,
		},
	}

	// Fast path: check if the actor was deleted synchronously.
	_, err = c.control.GetActor(ctx, getReq)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("waiting for actor %s/%s deletion: %w", atespace, actorName, ctx.Err())
		}
	}

	ticker := time.NewTicker(defaultRetryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for actor %s/%s deletion: %w", atespace, actorName, ctx.Err())
		case <-ticker.C:
			_, err := c.control.GetActor(ctx, getReq)
			if err != nil {
				if status.Code(err) == codes.NotFound {
					return nil
				}
				if ctx.Err() != nil {
					return fmt.Errorf("waiting for actor %s/%s deletion: %w", atespace, actorName, ctx.Err())
				}
			}
		}
	}
}

// ListActorTemplates returns all ActorTemplates in the given atespace.
func (c *Client) ListActorTemplates(ctx context.Context, atespace string) ([]*ateapipb.ActorTemplate, error) {
	resp, err := callWithRetry(ctx, func() (*ateapipb.ListActorTemplatesResponse, error) {
		return c.control.ListActorTemplates(ctx, &ateapipb.ListActorTemplatesRequest{Atespace: atespace})
	})
	if err != nil {
		return nil, fmt.Errorf("listing actor templates in %s: %w", atespace, err)
	}
	if resp.GetNextPageToken() != "" {
		slog.Warn("actor template listing was truncated", "atespace", atespace)
	}
	return resp.GetActorTemplates(), nil
}

// DeleteActorTemplate deletes the specified ActorTemplate. A missing template is not an error.
func (c *Client) DeleteActorTemplate(ctx context.Context, atespace, templateName string) error {
	req := &ateapipb.DeleteActorTemplateRequest{
		ActorTemplate: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     templateName,
		},
	}
	_, err := callWithRetry(ctx, func() (*ateapipb.ActorTemplate, error) {
		return c.control.DeleteActorTemplate(ctx, req)
	}, codes.Aborted)
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("deleting actor template %s/%s: %w", atespace, templateName, err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Snapshot Tags
// -----------------------------------------------------------------------------

// CreateTag creates an atespace-scoped Tag capturing the snapshot of a
// suspended source actor. If a tag of the same name already exists from an
// earlier incomplete attempt, callers should DeleteTag before retrying.
func (c *Client) CreateTag(ctx context.Context, atespace, tagName, sourceActorName string) (*ateapipb.Tag, error) {
	req := &ateapipb.CreateTagRequest{
		Tag: &ateapipb.Tag{
			Metadata:    &ateapipb.ResourceMetadata{Atespace: atespace, Name: tagName},
			Scope:       ateapipb.TagScope_TAG_SCOPE_ATESPACE,
			SourceActor: &ateapipb.ObjectRef{Atespace: atespace, Name: sourceActorName},
		},
	}
	tag, err := callWithRetry(ctx, func() (*ateapipb.Tag, error) {
		return c.control.CreateTag(ctx, req)
	}, codes.Aborted)
	if err != nil {
		return nil, fmt.Errorf("creating tag %s/%s from actor %s: %w", atespace, tagName, sourceActorName, err)
	}
	return tag, nil
}

// GetTag fetches a Tag by atespace and name.
func (c *Client) GetTag(ctx context.Context, atespace, tagName string) (*ateapipb.Tag, error) {
	req := &ateapipb.GetTagRequest{
		Tag: &ateapipb.ObjectRef{Atespace: atespace, Name: tagName},
	}
	return callWithRetry(ctx, func() (*ateapipb.Tag, error) {
		return c.control.GetTag(ctx, req)
	})
}

// ListTags returns every Tag in the given atespace, or in all atespaces when
// atespace is empty.
func (c *Client) ListTags(ctx context.Context, atespace string) ([]*ateapipb.Tag, error) {
	var tags []*ateapipb.Tag
	req := &ateapipb.ListTagsRequest{Atespace: atespace, Options: &ateapipb.ListOptions{}}
	for {
		resp, err := callWithRetry(ctx, func() (*ateapipb.ListTagsResponse, error) {
			return c.control.ListTags(ctx, req)
		})
		if err != nil {
			return nil, fmt.Errorf("listing tags in %q: %w", atespace, err)
		}
		tags = append(tags, resp.GetTags()...)
		if resp.GetNextPageToken() == "" {
			return tags, nil
		}
		req.Options.PageToken = resp.GetNextPageToken()
	}
}

// DeleteTag deletes a Tag and the external snapshot it owns. Deleting a missing
// Tag is not an error.
func (c *Client) DeleteTag(ctx context.Context, atespace, tagName string) error {
	req := &ateapipb.DeleteTagRequest{
		Tag: &ateapipb.ObjectRef{Atespace: atespace, Name: tagName},
	}
	_, err := callWithRetry(ctx, func() (*ateapipb.Tag, error) {
		return c.control.DeleteTag(ctx, req)
	}, codes.Aborted)
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("deleting tag %s/%s: %w", atespace, tagName, err)
	}
	return nil
}
