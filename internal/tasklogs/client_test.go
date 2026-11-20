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

package tasklogs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestClientErrorsAndTokenRotation(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	c := Client{TokenFile: token}
	for _, tt := range []struct {
		httpCode int
		want     codes.Code
	}{
		{http.StatusOK, codes.OK}, {http.StatusForbidden, codes.PermissionDenied}, {http.StatusUnauthorized, codes.PermissionDenied}, {http.StatusNotFound, codes.Unavailable},
	} {
		t.Run(http.StatusText(tt.httpCode), func(t *testing.T) {
			if err := os.WriteFile(token, []byte(tt.want.String()), 0600); err != nil {
				t.Fatal(err)
			}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+tt.want.String() {
					t.Error("stale token")
				}
				w.WriteHeader(tt.httpCode)
				_, _ = io.WriteString(w, "upstream-secret")
			}))
			defer api.Close()
			c.URL, c.HTTP = api.URL, api.Client()
			body, err := c.Open(t.Context(), "workers", "pod", time.Now())
			if status.Code(err) != tt.want {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			if body != nil {
				body.Close()
			}
			if err != nil && strings.Contains(err.Error(), "upstream-secret") {
				t.Fatal("upstream error body was exposed")
			}
		})
	}
	c = Client{TokenFile: filepath.Join(t.TempDir(), "missing")}
	if _, err := c.Open(t.Context(), "workers", "pod", time.Now()); status.Code(err) != codes.Unavailable {
		t.Fatalf("missing token: %v", err)
	}
}

func TestInClusterRequiresConfiguration(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := InCluster(); err == nil {
		t.Fatal("enabled logs silently accepted missing Kubernetes configuration")
	}
}

func TestMatchActorLog(t *testing.T) {
	for _, tt := range []struct {
		line string
		want bool
	}{
		{`{"labels":{"ate.actor.uid":"uid"},"message":"ok"}`, true},
		{`{"logging.googleapis.com/labels":{"ate.actor.uid":"uid"}}`, true},
		{`{"labels":{"ate.actor.uid":"uid-other"},"message":"uid"}`, false},
		{`{"ate.actor.uid":"uid"}`, false},
		{`{"labels":{"ate.actor.uid":3}}`, false},
		{`{"labels":{"ate.actor.uid":"uid"},"logging.googleapis.com/labels":{"ate.actor.uid":"other"}}`, false},
		{`{"labels":{"ate.actor.uid":"uid"}} trailing`, false},
		{`not json`, false},
		{`null`, false},
	} {
		t.Run(tt.line, func(t *testing.T) {
			if got := MatchActorLog([]byte(tt.line), "uid"); got != tt.want {
				t.Errorf("match=%v, want %v", got, tt.want)
			}
		})
	}
	if MatchActorLog([]byte(`{"labels":{"ate.actor.uid":""}}`), "") {
		t.Fatal("matched an absent UID")
	}
}

func TestReadFiltersAndReachesEOF(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	input := io.NopCloser(strings.NewReader("not json\n" + `{"labels":{"ate.actor.uid":"other"}}` + "\n" + `{"labels":{"ate.actor.uid":"uid"},"message":"yes"}` + "\n"))
	records, stop := Read(ctx, input, "uid")
	defer stop()
	for _, eof := range []bool{false, true} {
		select {
		case record := <-records:
			if eof && record.Err != io.EOF {
				t.Fatalf("terminal error: %v", record.Err)
			}
			if !eof && (record.Err != nil || !strings.Contains(record.Line, "yes")) {
				t.Fatalf("record: %v", record)
			}
		case <-ctx.Done():
			t.Fatal("reader blocked")
		}
	}
}

func TestReadRecordSize(t *testing.T) {
	const limit = 1 << 20
	const target = `{"labels":{"ate.actor.uid":"uid"},"message":"yes"}`
	const foreign = `{"labels":{"ate.actor.uid":"other"}}`
	atLimit := target + strings.Repeat(" ", limit-len(target))
	for _, tt := range []struct {
		name  string
		input string
		want  string
	}{
		{"foreign oversized record", foreign + strings.Repeat(" ", limit) + "\n" + target + "\n", target},
		{"matching oversized record", atLimit + " \n" + target + "\n", target},
		{"exact limit with CRLF", atLimit + "\r\n", atLimit},
		{"exact limit without newline", atLimit, atLimit},
		{"oversized record without newline", atLimit + " ", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			records, stop := Read(ctx, io.NopCloser(strings.NewReader(tt.input)), "uid")
			defer stop()
			if tt.want != "" {
				select {
				case got := <-records:
					if got.Err != nil || got.Line != tt.want {
						t.Fatalf("got %d bytes and error %v, want %d bytes", len(got.Line), got.Err, len(tt.want))
					}
				case <-ctx.Done():
					t.Fatal("timed out waiting for matching record")
				}
			}
			select {
			case got := <-records:
				if got.Err != io.EOF || got.Line != "" {
					t.Fatalf("got %d bytes and error %v, want EOF", len(got.Line), got.Err)
				}
			case <-ctx.Done():
				t.Fatal("timed out waiting for EOF")
			}
		})
	}
}
