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
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite"
)

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
	Agent    string `json:"agent"`
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
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open sqlite database: %w", err)
	}
	defer db.Close()

	// Verify database connection
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("failed to ping database: %w", err)
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

	listener, err := net.Listen("tcp", dashboardAddr)
	if err != nil {
		return fmt.Errorf("failed to bind server to %s: %w", dashboardAddr, err)
	}
	defer listener.Close()

	slog.InfoContext(ctx, "AX Dashboard started", slog.String("url", fmt.Sprintf("http://%s", listener.Addr().String())))

	server := &http.Server{
		Handler: mux,
	}

	return server.Serve(listener)
}

func fetchConversations(ctx context.Context, db *sql.DB) ([]ConversationResponse, error) {
	query := `
SELECT 
    c.conversation_id,
    c.last_seq,
    c.state,
    e.agent_id,
    e.start_time,
    e.end_time
FROM (
    SELECT conversation_id, seq AS last_seq,
           json_extract(payload, '$.exec_id') AS exec_id,
           json_extract(payload, '$.state') AS state
    FROM conversation_log
    WHERE (conversation_id, seq) IN (
        SELECT conversation_id, MAX(seq)
        FROM conversation_log
        GROUP BY conversation_id
    )
) c
LEFT JOIN (
    SELECT 
        exec_id,
        json_extract(payload, '$.agent_id') AS agent_id,
        MIN(timestamp) AS start_time,
        MAX(timestamp) AS end_time
    FROM execution_log
    GROUP BY exec_id
) e ON c.exec_id = e.exec_id;
`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var convs []ConversationResponse
	for rows.Next() {
		var id string
		var lastSeq int32
		var state string
		var agentID sql.NullString
		var startTimeStr, endTimeStr sql.NullString

		err := rows.Scan(&id, &lastSeq, &state, &agentID, &startTimeStr, &endTimeStr)
		if err != nil {
			return nil, err
		}

		agent := "unknown"
		if agentID.Valid && agentID.String != "" {
			agent = agentID.String
			// Strip special prefix if it starts with "__"
			if len(agent) > 2 && agent[:2] == "__" {
				agent = agent[2:]
			}
		}

		durationStr := "N/A"
		if startTimeStr.Valid && endTimeStr.Valid {
			startTime, err1 := parseSQLiteTime(startTimeStr.String)
			endTime, err2 := parseSQLiteTime(endTimeStr.String)
			if err1 == nil && err2 == nil {
				duration := endTime.Sub(startTime)
				durationStr = fmt.Sprintf("%.1fs", duration.Seconds())
			} else {
				slog.WarnContext(ctx, "Failed to parse sqlite timestamps", slog.String("start", startTimeStr.String), slog.String("end", endTimeStr.String), slog.Any("err1", err1), slog.Any("err2", err2))
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
			Agent:    agent,
			Status:   status,
			LastSeq:  lastSeq,
			Duration: durationStr,
		})
	}

	return convs, nil
}

func parseSQLiteTime(s string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05",
	}
	var err error
	var t time.Time
	for _, layout := range layouts {
		t, err = time.Parse(layout, s)
		if err == nil {
			return t, nil
		}
	}
	return time.Time{}, err
}
