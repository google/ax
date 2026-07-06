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
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func getFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to get free port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestReadyzEndpoint(t *testing.T) {
	addr := getFreePort(t)
	s := New(nil)
	go func() {
		_ = s.Serve(addr)
	}()
	defer s.GracefulStop()

	url := fmt.Sprintf("http://%s/readyz", addr)

	// Wait until server is listening and ready
	var success bool
	for i := 0; i < 50; i++ {
		resp, err := http.Get(url)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				if err == nil && string(body) == "ok\n" {
					success = true
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !success {
		t.Fatal("server failed to serve /readyz with expected response")
	}
}
