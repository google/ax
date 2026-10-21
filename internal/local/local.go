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

// Package local runs the AX control plane inside the ax CLI process for local
// development. There is no daemon: every invocation serves the regular AX
// gRPC API over an in-memory connection, keeps state in a SQLite database
// under ~/.ax, and runs tasks as containers through Podman (or another
// Docker-compatible runtime). Containers outlive the CLI process, so a task
// resumed by one command keeps running for the next.
package local

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"github.com/google/ax/internal/container"
	"github.com/google/ax/internal/lock"
	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store/sqlite"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// Home returns the AX state directory: $AX_HOME, or ~/.ax.
func Home() (string, error) {
	if dir := os.Getenv("AX_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ax"), nil
}

// DBPath returns the path of the local database inside home.
func DBPath(home string) string { return filepath.Join(home, "db.sqlite") }

// Options configures Open.
type Options struct {
	// Home is the state directory. Empty means Home().
	Home string
	// Progress receives user-facing progress such as image pulls.
	Progress io.Writer
	// Runtime is the container CLI. Nil detects one when first needed.
	Runtime *container.Runtime
}

// Env is a running in-process AX control plane.
type Env struct {
	store *sqlite.Store
	grpc  *grpc.Server
	lis   *bufconn.Listener
}

// Open starts an in-process control plane backed by the local database.
func Open(opts Options) (*Env, error) {
	home := opts.Home
	if home == "" {
		var err error
		if home, err = Home(); err != nil {
			return nil, fmt.Errorf("locating AX home: %w", err)
		}
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", home, err)
	}

	st, err := sqlite.Open(DBPath(home))
	if err != nil {
		return nil, err
	}
	locker, err := lock.NewFileLocker(filepath.Join(home, "locks"))
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	srv := server.NewServer(st, server.Options{
		Locker:     locker,
		Reconciler: container.NewReconciler(container.Options{Progress: opts.Progress, Runtime: opts.Runtime}),
	})

	lis := bufconn.Listen(1 << 20)
	g := srv.GRPCServer()
	go func() { _ = g.Serve(lis) }()

	return &Env{store: st, grpc: g, lis: lis}, nil
}

// Dial returns a client connection to the in-process server.
func (e *Env) Dial() (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///ax-local",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return e.lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
}

// Close stops the server and closes the database.
func (e *Env) Close() error {
	e.grpc.Stop()
	return e.store.Close()
}
