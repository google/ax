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

package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Client interacts with the in-actor guest daemon.
type Client struct {
	grpcConn   *grpc.ClientConn
	process    ateenvv1alpha.ProcessServiceClient
	filesystem ateenvv1alpha.FileSystemServiceClient
}

// Dial connects to the guest daemon at the specified target (e.g., "127.0.0.1:80").
func Dial(target string) (*Client, error) {
	return DialTarget(target, "")
}

// DialTarget connects to the guest daemon at the specified target, optionally injecting
// the 'ate-target-actor' metadata header on all calls (required when routing via atenet-router).
func DialTarget(target string, targetActor string) (*Client, error) {
	target = strings.TrimPrefix(target, "http://")
	target = strings.TrimPrefix(target, "https://")

	dialOpts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if targetActor != "" {
		dialOpts = append(dialOpts,
			grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
				ctx = metadata.AppendToOutgoingContext(ctx, "ate-target-actor", targetActor)
				return invoker(ctx, method, req, reply, cc, opts...)
			}),
			grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
				ctx = metadata.AppendToOutgoingContext(ctx, "ate-target-actor", targetActor)
				return streamer(ctx, desc, cc, method, opts...)
			}),
		)
	}

	conn, err := grpc.NewClient(target, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to guest at %s: %w", target, err)
	}

	return &Client{
		grpcConn:   conn,
		process:    ateenvv1alpha.NewProcessServiceClient(conn),
		filesystem: ateenvv1alpha.NewFileSystemServiceClient(conn),
	}, nil
}

// Close terminates the underlying gRPC client connection.
func (c *Client) Close() error {
	if c.grpcConn != nil {
		return c.grpcConn.Close()
	}
	return nil
}

// ExecOptions holds parameters for running a command in the task actor.
type ExecOptions struct {
	Command []string
	Cwd     string
	Env     map[string]string
	// Stdin, if set, is copied to the process's standard input, which is closed
	// once Stdin reaches EOF. If nil, the process reads an empty stdin.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Signals, if set, delivers each signal received on it to the process group.
	// Only SIGHUP, SIGINT, SIGQUIT and SIGTERM are forwarded.
	Signals <-chan os.Signal
}

// Exec runs a command inside the task container, feeding it opts.Stdin and
// streaming stdout/stderr until completion. It returns the process exit code.
func (c *Client) Exec(ctx context.Context, opts ExecOptions) (int, error) {
	if len(opts.Command) == 0 {
		return 1, errors.New("exec: command cannot be empty")
	}

	proc, err := c.process.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{
		Command: opts.Command,
		Cwd:     opts.Cwd,
		Env:     opts.Env,
		Stdin:   opts.Stdin != nil,
	})
	if err != nil {
		return 1, fmt.Errorf("starting process: %w", err)
	}

	pid := proc.GetProcessId()

	// Input and signal forwarding stop when the command finishes.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if opts.Stdin != nil {
		go c.forwardStdin(ctx, pid, opts.Stdin)
	}
	if opts.Signals != nil {
		go c.forwardSignals(ctx, pid, opts.Signals)
	}

	stream, err := c.process.StreamProcessOutput(ctx, &ateenvv1alpha.StreamProcessOutputRequest{
		ProcessId: pid,
		Follow:    true,
	})
	if err != nil {
		return 1, fmt.Errorf("streaming process output: %w", err)
	}

	// With Follow set, the stream ends with the final process state.
	for {
		out, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 1, fmt.Errorf("receiving output stream: %w", err)
		}
		if exit := out.GetExit(); exit != nil {
			return int(exit.GetExitCode()), nil
		}
		if data := out.GetStdout(); len(data) > 0 && opts.Stdout != nil {
			_, _ = opts.Stdout.Write(data)
		}
		if data := out.GetStderr(); len(data) > 0 && opts.Stderr != nil {
			_, _ = opts.Stderr.Write(data)
		}
	}

	// The stream closed without reporting an exit; fall back to polling.
	for {
		proc, err := c.process.GetProcess(ctx, &ateenvv1alpha.GetProcessRequest{ProcessId: pid})
		if err != nil {
			return 1, fmt.Errorf("getting process state: %w", err)
		}
		if proc.GetState() != ateenvv1alpha.ProcessState_PROCESS_STATE_RUNNING {
			return int(proc.GetExitCode()), nil
		}
		select {
		case <-ctx.Done():
			return 1, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// forwardStdin copies r to the stdin of process pid and closes it when r ends.
// Errors are dropped: they mean the process has exited or the connection is
// gone, and Exec reports either from the output stream.
func (c *Client) forwardStdin(ctx context.Context, pid string, r io.Reader) {
	stream, err := c.process.WriteProcessInput(ctx)
	if err != nil {
		return
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			if err := stream.Send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: pid, Data: buf[:n]}); err != nil {
				return
			}
		}
		if readErr != nil {
			if err := stream.Send(&ateenvv1alpha.WriteProcessInputRequest{ProcessId: pid, Close: true}); err != nil {
				return
			}
			_, _ = stream.CloseAndRecv()
			return
		}
	}
}

// forwardedSignals maps the local signals Exec forwards to their guest equivalents.
var forwardedSignals = map[os.Signal]ateenvv1alpha.Signal{
	syscall.SIGHUP:  ateenvv1alpha.Signal_SIGNAL_HUP,
	syscall.SIGINT:  ateenvv1alpha.Signal_SIGNAL_INT,
	syscall.SIGQUIT: ateenvv1alpha.Signal_SIGNAL_QUIT,
	syscall.SIGTERM: ateenvv1alpha.Signal_SIGNAL_TERM,
}

// forwardSignals delivers each signal from sigs to process pid until ctx ends.
func (c *Client) forwardSignals(ctx context.Context, pid string, sigs <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case sig := <-sigs:
			if guestSig, ok := forwardedSignals[sig]; ok {
				_, _ = c.process.SignalProcess(ctx, &ateenvv1alpha.SignalProcessRequest{ProcessId: pid, Signal: guestSig})
			}
		}
	}
}
