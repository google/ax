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

// Package tasklogs reads live worker output from the Kubernetes API.
package tasklogs

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Source opens one live worker log stream. Closing it must unblock reads.
type Source interface {
	Open(context.Context, string, string, time.Time) (io.ReadCloser, error)
}

// Client reads pods/log using a rotating service-account token.
type Client struct {
	URL       string
	HTTP      *http.Client
	TokenFile string
}

// InCluster constructs the log client only when log streaming is enabled.
func InCluster() (*Client, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	if host == "" {
		return nil, fmt.Errorf("task logs require an in-cluster Kubernetes service account")
	}
	port := os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS")
	if port == "" {
		port = "443"
	}
	ca, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("invalid Kubernetes CA bundle")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	transport.ResponseHeaderTimeout = 10 * time.Second
	return &Client{URL: "https://" + net.JoinHostPort(host, port), TokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token", HTTP: &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Open follows only new records from the worker container. No log history is requested.
func (c *Client) Open(ctx context.Context, namespace, pod string, since time.Time) (io.ReadCloser, error) {
	token, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "reading Kubernetes service-account token")
	}
	query := url.Values{"container": {"worker"}, "follow": {"true"}, "sinceTime": {since.UTC().Format(time.RFC3339Nano)}}
	endpoint := c.URL + "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods/" + url.PathEscape(pod) + "/log?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, status.Error(codes.Internal, "constructing worker log request")
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "opening worker logs")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		code := codes.Unavailable
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
			code = codes.PermissionDenied
		}
		return nil, status.Errorf(code, "worker logs returned HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// MatchActorLog accepts only the trusted UID label from Substrate actorlog.
// Conflicting label groups are rejected rather than choosing one identity.
func MatchActorLog(line []byte, uid string) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(line, &envelope) != nil {
		return false
	}
	found := false
	for _, key := range []string{"labels", "logging.googleapis.com/labels"} {
		raw, ok := envelope[key]
		if !ok {
			continue
		}
		var labels map[string]json.RawMessage
		if json.Unmarshal(raw, &labels) != nil {
			return false
		}
		raw, ok = labels["ate.actor.uid"]
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil || value != uid {
			return false
		}
		found = true
	}
	return found && uid != ""
}

// Record is a filtered actorlog line or the terminal reader error.
type Record struct {
	Line string
	Err  error
}

// Read filters a bounded worker stream. The caller must stop it before opening
// another stream; stop closes the body and waits for the reader to exit.
func Read(ctx context.Context, body io.ReadCloser, uid string) (<-chan Record, func()) {
	ctx, cancel := context.WithCancel(ctx)
	output := make(chan Record)
	done := make(chan struct{})
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			if !MatchActorLog(scanner.Bytes(), uid) {
				continue
			}
			select {
			case output <- Record{Line: scanner.Text()}:
			case <-ctx.Done():
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = io.EOF
		}
		select {
		case output <- Record{Err: err}:
		case <-ctx.Done():
		}
	}()
	return output, func() { cancel(); body.Close(); <-done }
}
