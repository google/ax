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
	"github.com/google/ax/internal/server"
	"github.com/google/ax/internal/store/postgres"
	"github.com/google/ax/internal/substrate"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/jackc/pgx/v5/pgconn"
)

func main() {
	var (
		listenAddr              string
		storeURL                string
		substrateEndpoint       string
		substrateAuthority      string
		substrateTokenFile      string
		substrateCAFile         string
		substrateInsecureTLS    bool
		substratePlaintext      bool
		defaultTemplate         string
		defaultTemplateAtespace string
		defaultTaskImage        string
	)

	flag.StringVar(&listenAddr, "addr", ":8080", "HTTP listen address")
	flag.StringVar(&storeURL, "store", "postgres://ax@localhost:5432/ax?sslmode=disable", "PostgreSQL connection URL (env: AX_STORE). libpq variables such as PGPASSWORD fill in what it leaves out")
	flag.StringVar(&substrateEndpoint, "substrate-endpoint", "api.ate-system.svc.cluster.local:443", "Agent Substrate Control API endpoint")
	flag.StringVar(&substrateAuthority, "substrate-authority", "api.ate-system.svc", "Authority / TLS ServerName for Substrate endpoint")
	flag.StringVar(&substrateTokenFile, "substrate-token-file", "", "Path to bearer token file for Substrate auth")
	flag.StringVar(&substrateCAFile, "substrate-ca-file", "", "Path to CA PEM file for Substrate TLS")
	flag.BoolVar(&substrateInsecureTLS, "substrate-insecure-tls", false, "Skip Substrate TLS verification")
	flag.BoolVar(&substratePlaintext, "substrate-plaintext", false, "Use insecure plaintext gRPC connection to Substrate")
	flag.StringVar(&defaultTemplate, "template", "default-template", "Default Substrate ActorTemplate name")
	flag.StringVar(&defaultTemplateAtespace, "template-atespace", v1alpha1.DefaultSystemNamespace, "Default Substrate ActorTemplate atespace")
	flag.StringVar(&defaultTaskImage, "default-task-image", v1alpha1.DefaultTaskImage, "Container image for tasks that do not set spec.image (env: AX_DEFAULT_TASK_IMAGE)")
	flag.Parse()

	if envAddr := os.Getenv("ADDR"); envAddr != "" {
		listenAddr = envAddr
	}
	// TODO: Reject or warn on passwords embedded in the store URL. They end up
	// in process args and pod specs; credentials should come from PGPASSWORD,
	// a password file, or a Secret instead.
	if envStore := os.Getenv("AX_STORE"); envStore != "" {
		storeURL = envStore
	}
	if envImage := os.Getenv("AX_DEFAULT_TASK_IMAGE"); envImage != "" {
		defaultTaskImage = envImage
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	slog.Info("starting ax-server",
		"listenAddr", listenAddr,
		"store", describeStore(storeURL),
		"substrateEndpoint", substrateEndpoint,
		"template", defaultTemplate,
		"defaultTaskImage", defaultTaskImage,
	)

	openCtx, openCancel := context.WithTimeout(context.Background(), time.Minute)
	backend, err := postgres.Open(openCtx, storeURL, postgres.Options{})
	openCancel()
	if err != nil {
		slog.Error("opening store", "store", describeStore(storeURL), "error", err)
		os.Exit(1)
	}
	defer backend.Close()

	var reconciler server.Reconciler
	subClient, err := substrate.NewClientWithOptions(substrate.ClientOptions{
		Target:      substrateEndpoint,
		Authority:   substrateAuthority,
		TokenFile:   substrateTokenFile,
		CAFile:      substrateCAFile,
		InsecureTLS: substrateInsecureTLS,
		Plaintext:   substratePlaintext,
	})
	if err != nil {
		slog.Warn("could not initialize substrate client; running without substrate reconciliation", "error", err)
	} else {
		defer subClient.Close()
		reconciler = controller.NewTaskReconciler(subClient, defaultTemplate, defaultTemplateAtespace, defaultTaskImage)
	}

	srv := server.NewServer(backend, server.Options{
		Locker:     backend,
		Reconciler: reconciler,
		Substrate:  subClient,
	})

	httpServer := &http.Server{
		Addr:    listenAddr,
		Handler: srv.Handler(),
	}
	httpServer.Protocols = new(http.Protocols)
	httpServer.Protocols.SetHTTP1(true)
	httpServer.Protocols.SetUnencryptedHTTP2(true)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down ax-server")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

// describeStore summarizes a store connection string for logs. It parses the
// string the way the store does, so URLs, query parameters and keyword/value
// DSNs are all handled, and reports only the user, host, port and database.
// Passwords and other connection parameters are never included.
func describeStore(connString string) string {
	cfg, err := pgconn.ParseConfig(connString)
	if err != nil {
		return "<unparseable>"
	}
	return fmt.Sprintf("%s@%s/%s", cfg.User, net.JoinHostPort(cfg.Host, strconv.Itoa(int(cfg.Port))), cfg.Database)
}
