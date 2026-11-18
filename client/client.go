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

// Package client reaches an ax-server the way the ax CLI does: it resolves
// the server's address from a kubeconfig context, starting or reusing the
// port-forward recorded in ~/.ax/tunnels, and dials the AX gRPC API.
//
// The package is alpha. Its API may change in any release without a
// compatibility guarantee.
//
// Unlike the ax CLI, the package never downloads kubectl. When a port-forward
// has to be started and kubectl is not installed, ServerURL and Dial return
// ErrKubectlNotFound; `ax local start` and `ax install` install kubectl into
// ~/.ax/bin.
package client

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/ax/internal/tunnel"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ErrKubectlNotFound reports that a port-forward to ax-server has to be
// started and kubectl is neither on PATH nor in ~/.ax/bin. Test for it with
// errors.Is.
var ErrKubectlNotFound = tunnel.ErrKubectlNotFound

// ErrNoContext reports that no ServerURL was given and no kubeconfig context
// is selected or configured. Test for it with errors.Is.
var ErrNoContext = tunnel.ErrNoContext

// Options selects the ax-server to reach. The zero value is the AX
// installation in namespace ax-system of the current kubeconfig context.
type Options struct {
	// ServerURL is the address of an ax-server that needs no port-forward,
	// such as "http://127.0.0.1:8080". When set, the other fields are unused.
	ServerURL string
	// Context is the kubeconfig context of the cluster. Empty means
	// $KUBECONTEXT, then the current context.
	Context string
	// Namespace is the Kubernetes namespace AX is installed in. Empty means
	// "ax-system" (or $AX_SYSTEM_NAMESPACE first when FromEnv is set).
	Namespace string
	// FromEnv reads $AX_SERVER (when Context is empty) and
	// $AX_SYSTEM_NAMESPACE (when Namespace is empty).
	FromEnv bool
}

// ServerURL returns the address of the ax-server that opts selects, in the
// form "http://host:port". It returns, in order: opts.ServerURL; $AX_SERVER
// (when opts.FromEnv is set and opts.Context is empty); the address of a
// port-forward to svc/ax-server, which it starts unless a healthy one is
// recorded in ~/.ax/tunnels. With no kubeconfig context it returns
// ErrNoContext.
//
// A port-forward that ServerURL starts is shared with the ax CLI and keeps
// running after ServerURL returns and after ctx is cancelled; `ax tunnel stop`
// stops it.
func ServerURL(ctx context.Context, opts Options) (string, error) {
	return tunnel.EnsureServerURL(ctx, tunnel.Options{
		ServerURL: opts.ServerURL,
		Context:   opts.Context,
		Namespace: opts.Namespace,
		FromEnv:   opts.FromEnv,
	})
}

// Dial returns a client for the ax-server at ServerURL(ctx, opts). The caller
// closes the returned connection.
//
// The connection is plaintext, which is what ax-server serves. dialOpts are
// applied after that default.
func Dial(ctx context.Context, opts Options, dialOpts ...grpc.DialOption) (v1alpha1.AXClient, *grpc.ClientConn, error) {
	serverURL, err := ServerURL(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	target := strings.TrimPrefix(serverURL, "http://")
	target = strings.TrimPrefix(target, "https://")
	all := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, dialOpts...)
	conn, err := grpc.NewClient(target, all...)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to %s: %w", target, err)
	}
	return v1alpha1.NewAXClient(conn), conn, nil
}

// Context is a context defined in the user's kubeconfig files.
type Context struct {
	Name    string
	Cluster string
	User    string
	// Current reports whether this is the context that CurrentContext returns.
	Current bool
}

// CurrentContext returns the name of the active kubeconfig context:
// $KUBECONTEXT if set, otherwise current-context of $KUBECONFIG or
// ~/.kube/config. It returns "" when there is none.
func CurrentContext() (string, error) {
	return tunnel.CurrentContext("")
}

// ListContexts returns the contexts defined in $KUBECONFIG or ~/.kube/config,
// sorted by name.
func ListContexts() ([]Context, error) {
	entries, err := tunnel.ListContexts("")
	if err != nil {
		return nil, err
	}
	out := make([]Context, 0, len(entries))
	for _, e := range entries {
		out = append(out, Context{Name: e.Name, Cluster: e.Cluster, User: e.User, Current: e.Current})
	}
	return out, nil
}
