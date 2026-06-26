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
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/google/ax/cmd/ax/internal/cliutil"
	"github.com/google/ax/internal/controller/eventlog"
	"github.com/google/ax/proto"
	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"
)

//go:embed web/index.html
var dashboardHTML string

var (
	dashboardAddr       string
	dashboardConfigFile string
)

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Start the AX Dashboard",
	Long:  `Start a local HTTP server to display AX conversations and executions dashboard.`,
	RunE:  runDashboard,
}

func init() {
	dashboardCmd.Flags().StringVar(&dashboardAddr, "addr", "localhost:8080", "Server address to listen on")
	dashboardCmd.Flags().StringVar(&dashboardConfigFile, "config", "ax.yaml", "Path to YAML configuration file")
}

type ConversationResponse struct {
	ID       string `json:"id"`
	Harness  string `json:"harness"`
	Status   string `json:"status"`
	LastSeq  int32  `json:"last_seq"`
	Duration string `json:"duration"`
}

func runDashboard(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := newConfig(cmd, dashboardConfigFile)
	if err != nil {
		return err
	}

	dbPath := cfg.EventLog.SQLiteConfig.Filename
	slog.InfoContext(ctx, "Opening event log database", slog.String("path", dbPath))
	
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return fmt.Errorf("failed to create database directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open sqlite database: %w", err)
	}
	defer db.Close()

	// Verify database connection
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("failed to ping database: %w", err)
	}

	// Create tables if they don't exist (to avoid crashes on fresh setup)
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS conversation_log (
			conversation_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (conversation_id, seq)
		)`); err != nil {
		return fmt.Errorf("failed to initialize conversation_log table: %w", err)
	}

	// Setup API handlers
	mux := http.NewServeMux()
	mux.HandleFunc("/api/conversations", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		convs, err := fetchConversations(r.Context(), db)
		if err != nil {
			slog.ErrorContext(r.Context(), "Failed to fetch conversations", slog.Any("error", err))
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(convs); err != nil {
			slog.ErrorContext(r.Context(), "Failed to encode conversations response", slog.Any("error", err))
		}
	})

	mux.HandleFunc("/api/trace", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		convID := r.URL.Query().Get("conversation")
		if convID == "" {
			http.Error(w, "Missing conversation ID", http.StatusBadRequest)
			return
		}

		data, err := loadTraceData(r.Context(), cfg, convID)
		if err != nil {
			slog.ErrorContext(r.Context(), "Failed to load trace data", slog.String("conversation_id", convID), slog.Any("error", err))
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(data); err != nil {
			slog.ErrorContext(r.Context(), "Failed to encode trace data response", slog.Any("error", err))
		}
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, dashboardHTML)
	})

	listener, err := net.Listen("tcp", dashboardAddr)
	if err != nil {
		return fmt.Errorf("failed to bind server to %s: %w", dashboardAddr, err)
	}
	defer listener.Close()

	addr := listener.Addr().String()
	host, port, err := net.SplitHostPort(addr)
	if err == nil && (host == "::" || host == "0.0.0.0" || host == "" || host == "[::]") {
		addr = fmt.Sprintf("localhost:%s", port)
	}
	url := fmt.Sprintf("http://%s", addr)

	slog.InfoContext(ctx, "AX Dashboard started", slog.String("url", url))

	go openBrowser(url)

	server := &http.Server{
		Handler: mux,
	}

	return server.Serve(listener)
}

func fetchConversations(ctx context.Context, db *sql.DB) ([]ConversationResponse, error) {
	query := `
SELECT 
    conversation_id,
    seq AS last_seq,
    json_extract(payload, '$.state') AS state,
    json_extract(payload, '$.harness_id') AS harness_id
FROM conversation_log
WHERE (conversation_id, seq) IN (
    SELECT conversation_id, MAX(seq)
    FROM conversation_log
    GROUP BY conversation_id
);
`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	convs := []ConversationResponse{}
	for rows.Next() {
		var id string
		var lastSeq int32
		var state string
		var harnessID sql.NullString

		err := rows.Scan(&id, &lastSeq, &state, &harnessID)
		if err != nil {
			return nil, err
		}

		harnessName := "unknown"
		if harnessID.Valid && harnessID.String != "" {
			harnessName = harnessID.String
			// Strip special prefix if it starts with "__"
			if len(harnessName) > 2 && harnessName[:2] == "__" {
				harnessName = harnessName[2:]
			}
		}

		status := state
		if len(status) > 6 && status[:6] == "STATE_" {
			status = status[6:]
		}
		if status == "PENDING" {
			status = "RUNNING"
		}

		convs = append(convs, ConversationResponse{
			ID:       id,
			Harness:  harnessName,
			Status:   status,
			LastSeq:  lastSeq,
			Duration: "N/A",
		})
	}

	return convs, nil
}

func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	}
	if err != nil {
		fmt.Printf("Failed to open browser: %v\n", err)
	}
}

type Text struct {
	Text string `json:"text"`
}

type Approval struct {
	Approved bool `json:"approved"`
}

type Confirmation struct {
	ID       string    `json:"id"`
	Question string    `json:"question,omitempty"`
	Approval *Approval `json:"approval,omitempty"`
}

type Content struct {
	Role         string        `json:"role"`
	Text         *Text         `json:"text,omitempty"`
	Confirmation *Confirmation `json:"confirmation,omitempty"`
}

type EventResponse struct {
	Seq       int32     `json:"seq"`
	HarnessID string    `json:"harness_id,omitempty"`
	State     string    `json:"state"`
	Messages  []Content `json:"messages"`
}

type TraceData struct {
	ConversationID string          `json:"conversation_id"`
	Events         []EventResponse `json:"events"`
}

func loadTraceData(ctx context.Context, cfg *cliutil.Config, convID string) (*TraceData, error) {
	evLog, err := eventlog.OpenSQLiteEventLog(cfg.EventLog.SQLiteConfig.Filename)
	if err != nil {
		return nil, fmt.Errorf("could not open sqlite eventlog: %w", err)
	}
	defer evLog.Close()

	convEvents, err := evLog.Events(ctx, convID)
	if err != nil {
		return nil, fmt.Errorf("failed to query conversation events: %w", err)
	}

	var events []EventResponse
	for _, protoEv := range convEvents {
		events = append(events, EventResponse{
			Seq:       protoEv.Seq,
			HarnessID: protoEv.HarnessId,
			State:     fmt.Sprint(protoEv.State),
			Messages:  extractMsgs(protoEv.Messages),
		})
	}

	return &TraceData{
		ConversationID: convID,
		Events:         events,
	}, nil
}

func extractMsgs(protoContents []*proto.Message) []Content {
	var results []Content
	for _, c := range protoContents {
		// Skip messages flagged as internal-only.
		if c.GetInternalOnly() {
			continue
		}
		content := Content{Role: c.Role}
		msgContent := c.GetContent()
		if msgContent == nil {
			continue
		}
		if textC := msgContent.GetText(); textC != nil {
			content.Text = &Text{Text: textC.Text}
		} else if conf := msgContent.GetConfirmation(); conf != nil {
			content.Confirmation = &Confirmation{
				ID:       conf.Id,
				Question: conf.Question,
			}
			if app := conf.GetApproval(); app != nil {
				content.Confirmation.Approval = &Approval{Approved: app.Approved}
			} else if dec := conf.GetDecline(); dec != nil {
				content.Confirmation.Approval = &Approval{Approved: !dec.Declined}
			}
		}
		results = append(results, content)
	}
	return results
}
