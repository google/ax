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
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/ax/pkg/apis/v1alpha1"
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
	_, err := c.control.CreateAtespace(ctx, req)
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
	return c.control.GetActorTemplate(ctx, req)
}

const (
	DefaultGuestCommand    = "/usr/local/bin/ax-task-runner"
	DefaultSnapshotsBucket = "gs://dberkov-gke-dev3/ate-env/"
)

// ResourceLimits translates a Task's resource limits into the Substrate
// ActorTemplate resources block. Substrate sizes a sandbox by limits alone
// (requests are rejected by v1alpha1.ValidateResources). It returns nil when no
// limit is set so the template inherits the worker defaults.
func ResourceLimits(reqs *v1alpha1.ResourceReqs) *ateapipb.Resources {
	limits := reqs.GetLimits()
	var out []*ateapipb.Limits
	if cpu := limits.GetCpu(); cpu != "" {
		out = append(out, &ateapipb.Limits{Name: "cpu", Quantity: cpu})
	}
	if memory := limits.GetMemory(); memory != "" {
		out = append(out, &ateapipb.Limits{Name: "memory", Quantity: memory})
	}
	if len(out) == 0 {
		return nil
	}
	return &ateapipb.Resources{Limits: out}
}

// BuildActorTemplate constructs a Substrate ActorTemplate based on the standard ate-env specification.
// A nil resources leaves the sandbox sized by the worker defaults.
func BuildActorTemplate(atespace, name, image string, envMap map[string]string, command []string, snapshotsBucket string, resources *ateapipb.Resources) *ateapipb.ActorTemplate {
	if atespace == "" {
		atespace = "default"
	}
	if image == "" {
		image = v1alpha1.DefaultTaskImage
	}
	if len(command) == 0 {
		command = []string{DefaultGuestCommand}
	}
	if snapshotsBucket == "" {
		if envBucket := os.Getenv("AX_SNAPSHOTS_BUCKET"); envBucket != "" {
			snapshotsBucket = envBucket
		} else {
			snapshotsBucket = DefaultSnapshotsBucket
		}
	}

	var envList []*ateapipb.EnvVar
	for k, v := range envMap {
		envList = append(envList, &ateapipb.EnvVar{
			Name:  k,
			Value: v,
		})
	}

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
			Readyz: &ateapipb.ContainerReadyz{
				HttpGet: &ateapipb.HTTPGetAction{
					Path: "/readyz",
					Port: 80,
				},
			},
			VolumeMounts: []*ateapipb.VolumeMount{{
				Name:      "workspace",
				MountPath: "/workspace",
			}},
		}},
		Volumes: []*ateapipb.Volume{{
			Name:       "workspace",
			DurableDir: &ateapipb.DurableDirVolumeSource{},
		}},
		SnapshotsConfig: &ateapipb.SnapshotsConfig{
			StorageLocation: snapshotsBucket,
			OnPause:         ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA,
			OnCommit:        ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA,
			OnResume: &ateapipb.OnResumeConfig{
				FromData: ateapipb.ResumeSource_RESUME_SOURCE_GOLDEN,
			},
		},
		SandboxConfig: &ateapipb.SandboxConfig{
			SandboxClass: ateapipb.SandboxClass_SANDBOX_CLASS_GVISOR,
			ConfigName:   "gvisor-default",
		},
		Resources: resources,
	}
}

// EnsureActorTemplateWithImage creates an ActorTemplate using the specified container image,
// resource limits (nil for worker defaults), and optional environment variables.
func (c *Client) EnsureActorTemplateWithImage(ctx context.Context, baseAtespace, baseTemplate, targetAtespace, targetTemplate, image string, resources *ateapipb.Resources, extraEnv ...map[string]string) (*ateapipb.ActorTemplate, error) {
	existing, err := c.GetActorTemplate(ctx, targetAtespace, targetTemplate)
	if err == nil && existing != nil {
		return existing, nil
	}

	envMap := make(map[string]string)
	for _, envs := range extraEnv {
		for k, v := range envs {
			envMap[k] = v
		}
	}

	tmpl := BuildActorTemplate(targetAtespace, targetTemplate, image, envMap, nil, "", resources)
	req := &ateapipb.CreateActorTemplateRequest{
		ActorTemplate: tmpl,
	}
	created, err := c.control.CreateActorTemplate(ctx, req)
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return nil, fmt.Errorf("creating actor template %s/%s: %w", targetAtespace, targetTemplate, err)
	}
	if created != nil {
		return created, nil
	}
	return c.GetActorTemplate(ctx, targetAtespace, targetTemplate)
}

// EnsureActor creates an Actor in the specified atespace deriving from an ActorTemplate.
func (c *Client) EnsureActor(ctx context.Context, atespace, actorName, templateAtespace, templateName string) (*ateapipb.Actor, error) {
	req := &ateapipb.CreateActorRequest{
		Actor: &ateapipb.Actor{
			Metadata: &ateapipb.ResourceMetadata{
				Atespace: atespace,
				Name:     actorName,
			},
			ActorTemplate: &ateapipb.ObjectRef{
				Atespace: templateAtespace,
				Name:     templateName,
			},
		},
	}
	actor, err := c.control.CreateActor(ctx, req)
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			existing, getErr := c.control.GetActor(ctx, &ateapipb.GetActorRequest{
				Actor: &ateapipb.ObjectRef{
					Atespace: atespace,
					Name:     actorName,
				},
			})
			if getErr == nil && existing != nil {
				state := existing.GetStatus().GetState()
				if state == ateapipb.ActorState_ACTOR_STATE_CRASHED {
					slog.Warn("existing actor is crashed, deleting and recreating", "actor", actorName)
					_, _ = c.control.DeleteActor(ctx, &ateapipb.DeleteActorRequest{
						Actor: &ateapipb.ObjectRef{
							Atespace: atespace,
							Name:     actorName,
						},
						AnyState: true,
					})
					state = ateapipb.ActorState_ACTOR_STATE_DELETING
				}
				if state == ateapipb.ActorState_ACTOR_STATE_DELETING {
					// Wait briefly for previous actor deletion to finalize before recreating
					for i := 0; i < 20; i++ {
						select {
						case <-ctx.Done():
							return nil, ctx.Err()
						case <-time.After(500 * time.Millisecond):
						}
						actor, err = c.control.CreateActor(ctx, req)
						if err == nil {
							return actor, nil
						}
						if status.Code(err) != codes.AlreadyExists {
							break
						}
					}
					return nil, fmt.Errorf("actor %s/%s is still deleting; please retry", atespace, actorName)
				}
			}
			return existing, getErr
		}
		return nil, fmt.Errorf("creating actor %s/%s: %w", atespace, actorName, err)
	}
	return actor, nil
}

// ResumeActor resumes the specified actor onto a worker and returns the worker details.
func (c *Client) ResumeActor(ctx context.Context, atespace, actorName string) (*ateapipb.Actor, string, error) {
	req := &ateapipb.ResumeActorRequest{
		Actor: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     actorName,
		},
	}
	resp, err := c.control.ResumeActor(ctx, req)
	if err != nil {
		return nil, "", fmt.Errorf("resuming actor %s/%s: %w", atespace, actorName, err)
	}
	if resp.Actor == nil {
		return nil, "", fmt.Errorf("nil actor returned when resuming %s/%s", atespace, actorName)
	}
	var workerIP string
	if resp.Actor.Status != nil && resp.Actor.Status.WorkerAssignment != nil {
		workerIP = resp.Actor.Status.WorkerAssignment.WorkerPodIp
	}
	return resp.Actor, workerIP, nil
}

// SuspendActor suspends the specified actor, triggering state checkpointing.
func (c *Client) SuspendActor(ctx context.Context, atespace, actorName string) error {
	req := &ateapipb.SuspendActorRequest{
		Actor: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     actorName,
		},
	}
	_, err := c.control.SuspendActor(ctx, req)
	if err != nil {
		return fmt.Errorf("suspending actor %s/%s: %w", atespace, actorName, err)
	}
	return nil
}

// DeleteActor deletes the specified actor from Substrate.
func (c *Client) DeleteActor(ctx context.Context, atespace, actorName string) error {
	req := &ateapipb.DeleteActorRequest{
		Actor: &ateapipb.ObjectRef{
			Atespace: atespace,
			Name:     actorName,
		},
		AnyState: true,
	}
	_, err := c.control.DeleteActor(ctx, req)
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("deleting actor %s/%s: %w", atespace, actorName, err)
	}
	return nil
}

// ListActorTemplates returns all ActorTemplates in the given atespace.
func (c *Client) ListActorTemplates(ctx context.Context, atespace string) ([]*ateapipb.ActorTemplate, error) {
	resp, err := c.control.ListActorTemplates(ctx, &ateapipb.ListActorTemplatesRequest{Atespace: atespace})
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
	_, err := c.control.DeleteActorTemplate(ctx, req)
	if err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("deleting actor template %s/%s: %w", atespace, templateName, err)
	}
	return nil
}


