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

package guest_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	envguest "github.com/agent-substrate/env/guest"
	"github.com/google/ax/internal/guest"
)

// startGuest serves the real guest services on a local port and returns a
// client connected to them.
func startGuest(t *testing.T) *guest.Client {
	t.Helper()
	cfg := envguest.DefaultConfig()
	cfg.Workspace = t.TempDir()
	cfg.LogDir = t.TempDir()
	srv, cleanup, err := envguest.NewServer(cfg)
	if err != nil {
		t.Fatalf("creating guest server: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		cleanup()
	})

	client, err := guest.Dial(lis.Addr().String())
	if err != nil {
		t.Fatalf("dialing guest: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func execContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// newPipe returns an OS pipe standing in for a terminal's stdin.
func newPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	return r, w
}

// syncBuffer is a bytes.Buffer that can be written and read from different goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestExecForwardsStdin(t *testing.T) {
	client := startGuest(t)

	var stdout bytes.Buffer
	code, err := client.Exec(execContext(t), guest.ExecOptions{
		Command: []string{"cat"},
		Stdin:   strings.NewReader("hello from stdin\n"),
		Stdout:  &stdout,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if got := stdout.String(); got != "hello from stdin\n" {
		t.Errorf("stdout = %q, want %q", got, "hello from stdin\n")
	}
}

func TestExecStreamsInteractiveInput(t *testing.T) {
	client := startGuest(t)

	// Each line is only written after the reply to the previous one arrives,
	// so this passes only if input and output stream while the process runs.
	stdinR, stdinW := newPipe(t)
	stdout := &syncBuffer{}
	done := make(chan struct{})
	var (
		code int
		err  error
	)
	go func() {
		defer close(done)
		code, err = client.Exec(execContext(t), guest.ExecOptions{
			Command: []string{"sh", "-c", "while read line; do echo \"got $line\"; done; echo bye"},
			Stdin:   stdinR,
			Stdout:  stdout,
		})
	}()

	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(stdout.String(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %q, stdout so far %q", want, stdout.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	_, _ = io.WriteString(stdinW, "one\n")
	waitFor("got one\n")
	_, _ = io.WriteString(stdinW, "two\n")
	waitFor("got two\n")

	// Closing stdin ends the read loop, so the shell prints bye and exits.
	_ = stdinW.Close()
	<-done
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if got, want := stdout.String(), "got one\ngot two\nbye\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestExecPropagatesExitCode(t *testing.T) {
	client := startGuest(t)

	code, err := client.Exec(execContext(t), guest.ExecOptions{
		Command: []string{"sh", "-c", "read code; exit $code"},
		Stdin:   strings.NewReader("7\n"),
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d, want 7", code)
	}
}

func TestExecWithoutStdinReadsEmpty(t *testing.T) {
	client := startGuest(t)

	var stdout bytes.Buffer
	code, err := client.Exec(execContext(t), guest.ExecOptions{
		Command: []string{"sh", "-c", "cat; echo done"},
		Stdout:  &stdout,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 0 || stdout.String() != "done\n" {
		t.Errorf("got exit code %d, stdout %q; want 0, %q", code, stdout.String(), "done\n")
	}
}

func TestExecForwardsSignals(t *testing.T) {
	client := startGuest(t)

	// The process prints once it is running, then waits on stdin, which the
	// test keeps open. Only the forwarded SIGINT can end it.
	stdinR, _ := newPipe(t)
	stdout := &syncBuffer{}
	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})
	var (
		code int
		err  error
	)
	go func() {
		defer close(done)
		code, err = client.Exec(execContext(t), guest.ExecOptions{
			Command: []string{"sh", "-c", "echo ready; cat"},
			Stdin:   stdinR,
			Stdout:  stdout,
			Signals: sigs,
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stdout.String(), "ready") {
		if time.Now().After(deadline) {
			t.Fatalf("process never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	sigs <- os.Interrupt
	<-done

	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 130 {
		t.Errorf("exit code = %d, want 130 (killed by SIGINT)", code)
	}
}
