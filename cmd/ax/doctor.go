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
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/google/ax/internal/config"
	"github.com/spf13/cobra"
)

var doctorEndpoint string

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose and display health of the AX runtime, sidecar process, and system metrics",
	Long: `Display the current diagnostic status of:
- Python sidecar process (PID, CPU, Memory RSS, Uptime, Command)
- gRPC harness endpoint connectivity
- AX config/assets directory status
- Go runtime memory and goroutines`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		printStatus(os.Stdout, doctorEndpoint)
		return nil
	},
}

func init() {
	doctorCmd.Flags().StringVar(&doctorEndpoint, "endpoint", "127.0.0.1:50053", "Harness gRPC endpoint to monitor")
}

// Stats holds the sampled metrics and state of the AX system.
type Stats struct {
	Timestamp      time.Time
	AssetsDir      string
	SidecarPID     int
	SidecarAlive   bool
	SidecarCPU     string
	SidecarRSS     string
	SidecarUptime  string
	SidecarCmd     string
	Endpoint       string
	EndpointActive bool
	PythonVersion  string
	GoVersion      string
	NumGoroutine   int
	MemAllocMB     float64
	NumAssetsFiles int
	AssetsSizeKB   int64
}

// Sampler collects status of the AX runtime and Python sidecar.
type Sampler struct {
	endpoint string
}

// NewSampler creates a new Sampler for the given endpoint.
func NewSampler(endpoint string) *Sampler {
	if endpoint == "" {
		endpoint = "127.0.0.1:50053"
	}
	return &Sampler{
		endpoint: endpoint,
	}
}

// Sample collects current metrics from the system.
func (s *Sampler) Sample() Stats {
	st := Stats{
		Timestamp:    time.Now(),
		Endpoint:     s.endpoint,
		GoVersion:    runtime.Version(),
		NumGoroutine: runtime.NumGoroutine(),
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	st.MemAllocMB = float64(m.Alloc) / (1024 * 1024)

	// 1. Assets dir
	assetsDir, err := config.AXAssetsDir()
	if err == nil {
		st.AssetsDir = assetsDir
		if entries, err := os.ReadDir(assetsDir); err == nil {
			st.NumAssetsFiles = len(entries)
			for _, e := range entries {
				if info, err := e.Info(); err == nil {
					st.AssetsSizeKB += info.Size() / 1024
				}
			}
		}

		pidFile := filepath.Join(assetsDir, "sidecar.pid")
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				st.SidecarPID = pid
				st.SidecarAlive = isProcessAlive(pid)
			}
		}
	}

	// 2. Sidecar process details if alive
	if st.SidecarAlive && st.SidecarPID > 0 {
		out, err := exec.Command("ps", "-p", strconv.Itoa(st.SidecarPID), "-o", "%cpu,rss,etime,command").Output()
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if len(lines) >= 2 {
				fields := strings.Fields(lines[1])
				if len(fields) >= 4 {
					st.SidecarCPU = fields[0] + "%"
					if rssKB, err := strconv.ParseFloat(fields[1], 64); err == nil {
						st.SidecarRSS = fmt.Sprintf("%.1f MB", rssKB/1024)
					} else {
						st.SidecarRSS = fields[1] + " KB"
					}
					st.SidecarUptime = fields[2]
					st.SidecarCmd = strings.Join(fields[3:], " ")
				}
			}
		}
	}

	// 3. Check endpoint connectivity
	conn, err := net.DialTimeout("tcp", s.endpoint, 120*time.Millisecond)
	if err == nil {
		st.EndpointActive = true
		_ = conn.Close()
	}

	// 4. Python version
	if out, err := exec.Command("python3", "--version").Output(); err == nil {
		st.PythonVersion = strings.TrimSpace(string(out))
	} else {
		st.PythonVersion = "python3 not found"
	}

	return st
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Display handles plain text rendering of AX runtime metrics using bold labels and headers.
type Display struct {
	w          io.Writer
	styleBold  lipgloss.Style
	styleTitle lipgloss.Style
}

// NewDisplay creates a new Display writer.
func NewDisplay(w io.Writer) *Display {
	if w == nil {
		w = os.Stdout
	}

	return &Display{
		w:          w,
		styleBold:  lipgloss.NewStyle().Bold(true),
		styleTitle: lipgloss.NewStyle().Bold(true),
	}
}

// Render formats and prints the system status in plain bold text.
func (d *Display) Render(stats Stats) {
	var b strings.Builder

	b.WriteString(d.styleTitle.Render("ax doctor") + " (" + stats.Timestamp.Format("2006-01-02 15:04:05 MST") + ")\n\n")

	b.WriteString(d.styleBold.Render("Sidecar:") + "\n")
	if stats.SidecarAlive {
		b.WriteString(fmt.Sprintf("  %s %s\n", d.styleBold.Render(fmt.Sprintf("%-14s", "Status:")), "RUNNING"))
		b.WriteString(fmt.Sprintf("  %s %d\n", d.styleBold.Render(fmt.Sprintf("%-14s", "PID:")), stats.SidecarPID))
		if stats.SidecarCPU != "" {
			b.WriteString(fmt.Sprintf("  %s %s\n", d.styleBold.Render(fmt.Sprintf("%-14s", "CPU:")), stats.SidecarCPU))
		}
		if stats.SidecarRSS != "" {
			b.WriteString(fmt.Sprintf("  %s %s\n", d.styleBold.Render(fmt.Sprintf("%-14s", "Memory (RSS):")), stats.SidecarRSS))
		}
		if stats.SidecarUptime != "" {
			b.WriteString(fmt.Sprintf("  %s %s\n", d.styleBold.Render(fmt.Sprintf("%-14s", "Elapsed Time:")), stats.SidecarUptime))
		}
		if stats.SidecarCmd != "" {
			b.WriteString(fmt.Sprintf("  %s %s\n", d.styleBold.Render(fmt.Sprintf("%-14s", "Command:")), stats.SidecarCmd))
		}
	} else {
		b.WriteString(fmt.Sprintf("  %s %s\n", d.styleBold.Render(fmt.Sprintf("%-14s", "Status:")), "STOPPED"))
		b.WriteString(fmt.Sprintf("  %s %s\n", d.styleBold.Render(fmt.Sprintf("%-14s", "PID:")), "none"))
	}

	endpointStatus := stats.Endpoint
	if stats.EndpointActive {
		endpointStatus += " (ACTIVE)"
	} else {
		endpointStatus += " (UNREACHABLE)"
	}
	b.WriteString(fmt.Sprintf("  %s %s\n\n", d.styleBold.Render(fmt.Sprintf("%-14s", "Endpoint:")), endpointStatus))

	b.WriteString(d.styleBold.Render("Assets:") + "\n")
	b.WriteString(fmt.Sprintf("  %s %s\n", d.styleBold.Render(fmt.Sprintf("%-14s", "Assets Dir:")), stats.AssetsDir))
	b.WriteString(fmt.Sprintf("  %s %d files (%d KB)\n\n", d.styleBold.Render(fmt.Sprintf("%-14s", "Asset Files:")), stats.NumAssetsFiles, stats.AssetsSizeKB))

	b.WriteString(d.styleBold.Render("Environment:") + "\n")
	b.WriteString(fmt.Sprintf("  %s %s\n", d.styleBold.Render(fmt.Sprintf("%-14s", "Python:")), stats.PythonVersion))
	b.WriteString(fmt.Sprintf("  %s %s\n", d.styleBold.Render(fmt.Sprintf("%-14s", "Go Runtime:")), stats.GoVersion))
	b.WriteString(fmt.Sprintf("  %s %d\n", d.styleBold.Render(fmt.Sprintf("%-14s", "Goroutines:")), stats.NumGoroutine))
	b.WriteString(fmt.Sprintf("  %s %.2f MB\n", d.styleBold.Render(fmt.Sprintf("%-14s", "Go Memory:")), stats.MemAllocMB))

	fmt.Fprint(d.w, b.String())
}

// printStatus samples the system and renders the output to w.
func printStatus(w io.Writer, endpoint string) {
	sampler := NewSampler(endpoint)
	stats := sampler.Sample()
	display := NewDisplay(w)
	display.Render(stats)
}
