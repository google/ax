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
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/ax/internal/controller"
	"github.com/google/ax/internal/store/redis"
	"github.com/google/ax/internal/substrate"
	goredis "github.com/redis/go-redis/v9"
)

func main() {
	var (
		substrateEndpoint       string
		substrateAuthority      string
		substrateTokenFile      string
		substrateCAFile         string
		substrateInsecureTLS    bool
		substratePlaintext      bool
		defaultTemplate         string
		defaultTemplateAtespace string
		redisAddr               string
		redisPassword           string
		redisGroup              string
		redisConsumer           string
		concurrency             int
	)

	flag.StringVar(&redisAddr, "redis-addr", "localhost:6379", "Redis server address (e.g. localhost:6379)")
	flag.StringVar(&redisPassword, "redis-password", "", "Redis password")
	flag.StringVar(&redisGroup, "redis-group", "ax-controllers", "Redis stream consumer group")
	flag.StringVar(&redisConsumer, "redis-consumer", "", "Redis stream consumer ID (defaults to hostname)")
	flag.IntVar(&concurrency, "concurrency", 16, "Number of tasks reconciled in parallel by this controller")
	flag.StringVar(&substrateEndpoint, "substrate-endpoint", "api.ate-system.svc.cluster.local:443", "Agent Substrate Control API endpoint")
	flag.StringVar(&substrateAuthority, "substrate-authority", "api.ate-system.svc", "Authority / TLS ServerName for Substrate endpoint")
	flag.StringVar(&substrateTokenFile, "substrate-token-file", "", "Path to bearer token file for Substrate auth")
	flag.StringVar(&substrateCAFile, "substrate-ca-file", "", "Path to CA PEM file for Substrate TLS")
	flag.BoolVar(&substrateInsecureTLS, "substrate-insecure-tls", false, "Skip Substrate TLS verification")
	flag.BoolVar(&substratePlaintext, "substrate-plaintext", false, "Use insecure plaintext gRPC connection to Substrate")
	flag.StringVar(&defaultTemplate, "template", "default-template", "Default Substrate ActorTemplate name")
	flag.StringVar(&defaultTemplateAtespace, "template-atespace", "ax-system", "Default Substrate ActorTemplate atespace")
	flag.Parse()

	if envRedis := os.Getenv("REDIS_ADDR"); envRedis != "" {
		redisAddr = envRedis
	}
	if envPass := os.Getenv("REDIS_PASSWORD"); envPass != "" && redisPassword == "" {
		redisPassword = envPass
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	slog.Info("starting ax-controller",
		"redisAddr", redisAddr,
		"group", redisGroup,
		"substrateEndpoint", substrateEndpoint,
		"authority", substrateAuthority,
		"templateAtespace", defaultTemplateAtespace,
		"template", defaultTemplate,
	)

	subClient, err := substrate.NewClientWithOptions(substrate.ClientOptions{
		Target:      substrateEndpoint,
		Authority:   substrateAuthority,
		TokenFile:   substrateTokenFile,
		CAFile:      substrateCAFile,
		InsecureTLS: substrateInsecureTLS,
		Plaintext:   substratePlaintext,
	})
	if err != nil {
		slog.Error("failed to initialize substrate client", "error", err)
		os.Exit(1)
	}
	defer subClient.Close()

	reconciler := controller.NewTaskReconciler(subClient, defaultTemplate, defaultTemplateAtespace)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	rClient := goredis.NewClient(&goredis.Options{
		Addr:     redisAddr,
		Password: redisPassword,
	})
	defer rClient.Close()

	rStore := redis.NewStore(rClient, redis.Options{})
	worker := controller.NewWorker(rStore, reconciler, redisGroup, redisConsumer)
	worker.Concurrency = concurrency
	if err := worker.Run(ctx); err != nil && err != context.Canceled {
		slog.Error("redis worker stopped with error", "error", err)
		os.Exit(1)
	}
}
