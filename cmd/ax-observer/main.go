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

// Command ax-observer records how running tasks ended: it runs an
// observer.Observer over ax-server's store, which asks the runners of the
// working tasks for their status files. It serves no API. Run one: more would
// ask every runner more than once.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/observer"
	"github.com/google/ax/internal/store/postgres"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/internal/telemetry"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc"
)

func main() {
	var (
		metricsAddr          string
		storeURL             string
		substrateEndpoint    string
		substrateAuthority   string
		substrateTokenFile   string
		substrateCAFile      string
		substrateInsecureTLS bool
		substratePlaintext   bool
	)

	flag.StringVar(&metricsAddr, "metrics-addr", ":9090", "Listen address for Prometheus /metrics; empty disables it")
	flag.StringVar(&storeURL, "store", "postgres://ax@localhost:5432/ax?sslmode=disable", "ax-server's PostgreSQL connection URL (env: AX_STORE). libpq variables such as PGPASSWORD fill in what it leaves out")
	flag.StringVar(&substrateEndpoint, "substrate-endpoint", "api.ate-system.svc.cluster.local:443", "Agent Substrate Control API endpoint")
	flag.StringVar(&substrateAuthority, "substrate-authority", "api.ate-system.svc", "Authority / TLS ServerName for Substrate endpoint")
	flag.StringVar(&substrateTokenFile, "substrate-token-file", "", "Path to bearer token file for Substrate auth")
	flag.StringVar(&substrateCAFile, "substrate-ca-file", "", "Path to CA PEM file for Substrate TLS")
	flag.BoolVar(&substrateInsecureTLS, "substrate-insecure-tls", false, "Skip Substrate TLS verification")
	flag.BoolVar(&substratePlaintext, "substrate-plaintext", false, "Use insecure plaintext gRPC connection to Substrate")
	flag.Parse()

	if envStore := os.Getenv("AX_STORE"); envStore != "" {
		storeURL = envStore
	}

	logger := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.SetDefault(logger)

	tel, err := telemetry.Init(context.Background(), "ax-observer")
	if err != nil {
		slog.Error("initializing telemetry", "error", err)
		os.Exit(1)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tel.Shutdown(ctx); err != nil {
			slog.Warn("flushing telemetry", "error", err)
		}
	}()

	slog.Info("starting ax-observer",
		"metricsAddr", metricsAddr,
		"store", describeStore(storeURL),
		"substrateEndpoint", substrateEndpoint,
	)

	openCtx, openCancel := context.WithTimeout(context.Background(), time.Minute)
	backend, err := postgres.Open(openCtx, storeURL, postgres.Options{})
	openCancel()
	if err != nil {
		slog.Error("opening store", "store", describeStore(storeURL), "error", err)
		os.Exit(1)
	}
	defer backend.Close()

	subClient, err := substrate.NewClientWithOptions(substrate.ClientOptions{
		Target:      substrateEndpoint,
		Authority:   substrateAuthority,
		TokenFile:   substrateTokenFile,
		CAFile:      substrateCAFile,
		InsecureTLS: substrateInsecureTLS,
		Plaintext:   substratePlaintext,
	}, grpc.WithStatsHandler(telemetry.ClientStatsHandler()))
	if err != nil {
		slog.Error("initializing substrate client", "error", err)
		os.Exit(1)
	}
	defer subClient.Close()

	// The observer only asks runners for their status, so the task manager
	// needs no templates.
	o := observer.New(backend, backend, backend, controller.NewTaskManager(subClient, "", ""))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var metricsServer *http.Server
	if metricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", tel.MetricsHandler())
		metricsServer = &http.Server{Addr: metricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("metrics server failed", "error", err)
				os.Exit(1)
			}
		}()
	}

	o.Run(ctx)
	slog.Info("shutting down ax-observer")

	if metricsServer != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = metricsServer.Shutdown(shutdownCtx)
	}
}

// describeStore summarizes a store connection string for logs, as ax-server
// does: only the user, host, port and database, never the password.
func describeStore(connString string) string {
	cfg, err := pgconn.ParseConfig(connString)
	if err != nil {
		return "<unparseable>"
	}
	return fmt.Sprintf("%s@%s/%s", cfg.User, net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))), cfg.Database)
}
