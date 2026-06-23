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

// Command interactionsapi-harness-server runs the Interactions API harness as a
// standalone HarnessService gRPC server. The harness's main logic lives in the
// server; any HarnessService client (AntigravityHarness, SubstrateHarness) can
// dial it, which is how the harness would run isolated on substrate.
//
//	go run ./cmd/interactionsapi-harness-server \
//	  --port=50053 --project=<project> --agent=<agent-name>
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/google/ax/internal/harness"
	"github.com/google/ax/proto"
	"google.golang.org/grpc"
)

var (
	port        = flag.Int("port", 50053, "Port to serve HarnessService on.")
	endpoint    = flag.String("endpoint", "", "Vertex GenAI dataplane HTTPS endpoint (default: public production).")
	project     = flag.String("project", "", "Cloud project id (required).")
	location    = flag.String("location", "global", "Cloud location.")
	agent       = flag.String("agent", "", "Interactions API agent name (required).")
	impersonate = flag.String("impersonate_service_account", "",
		"If set, mint the token by impersonating this service account.")
	debug = flag.Bool("debug", false, "Log concise per-conversation tool activity (FC/FR) to stderr.")
)

func main() {
	flag.Parse()
	if *project == "" || *agent == "" {
		slog.Error("--project and --agent are required")
		os.Exit(1)
	}

	server := harness.NewInteractionsAPIServer(harness.InteractionsAPIConfig{
		Endpoint:                  *endpoint,
		Project:                   *project,
		Location:                  *location,
		Agent:                     *agent,
		ImpersonateServiceAccount: *impersonate,
		Debug:                     *debug,
	})

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		slog.Error("failed to listen", slog.Int("port", *port), slog.Any("error", err))
		os.Exit(1)
	}

	grpcServer := grpc.NewServer()
	proto.RegisterHarnessServiceServer(grpcServer, server)

	slog.Info("Interactions API HarnessService listening", slog.Any("address", lis.Addr()))
	if err := grpcServer.Serve(lis); err != nil {
		slog.Error("failed to serve", slog.Any("error", err))
		os.Exit(1)
	}
}
