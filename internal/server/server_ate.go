//go:build !harness

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

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log"
	"os"
	"time"

	"github.com/google/ax/internal/experimental/k8s/ate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func createATEClient() *ate.Client {
	if os.Getenv("AX_SUBSTRATE") != "1" {
		return nil
	}

	ns := os.Getenv("AX_SUBSTRATE_NAMESPACE")
	if ns == "" {
		ns = "ax"
	}
	template := os.Getenv("AX_SUBSTRATE_TEMPLATE")
	if template == "" {
		template = "ax-template"
	}
	endpoint := os.Getenv("AX_SUBSTRATE_ENDPOINT")
	credBundle := os.Getenv("AX_SUBSTRATE_CRED_BUNDLE")

	clientTLSConfig := &tls.Config{
		InsecureSkipVerify: true,
	}

	if credBundle != "" {
		bundleBytes, err := os.ReadFile(credBundle)
		if err != nil {
			log.Printf("Warning: Failed to read bundle %s: %v", credBundle, err)
		} else {
			certPool := x509.NewCertPool()
			if ok := certPool.AppendCertsFromPEM(bundleBytes); ok {
				clientTLSConfig.RootCAs = certPool
				log.Printf("Successfully loaded CA bundle from %s", credBundle)
			}
		}
	}

	log.Printf("Creating SubstrATE client (Namespace: %s, Template: %s, Endpoint: %s)", ns, template, endpoint)
	client, err := ate.NewClient(ns, template, endpoint, grpc.WithTransportCredentials(credentials.NewTLS(clientTLSConfig)))
	if err != nil {
		log.Printf("Warning: Failed to create SubstrATE client: %v", err)
		return nil
	}
	return client
}

func suspendActor(actorID string) {
	client := createATEClient()
	if client == nil {
		return
	}
	defer client.Close()

	log.Printf("Automatically suspending actor %s in 50 milliseconds...", actorID)
	time.Sleep(50 * time.Millisecond)

	suspendCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.SuspendActor(suspendCtx, actorID); err != nil {
		log.Printf("Failed to automatically suspend actor %s: %v", actorID, err)
	} else {
		log.Printf("Successfully suspended actor %s", actorID)
	}
}
