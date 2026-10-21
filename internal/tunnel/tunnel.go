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

package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/ax/pkg/apis/v1alpha1"
)

var portForwardRegex = regexp.MustCompile(`Forwarding from .*:(\d+)\s+->`)

type TunnelInfo struct {
	Context   string    `json:"context"`
	Namespace string    `json:"namespace"`
	Service   string    `json:"service"`
	Port      int       `json:"port"`
	PID       int       `json:"pid"`
	CreatedAt time.Time `json:"created_at"`
}

type Options struct {
	ServerURL string
	Context   string
	Namespace string // default: ax-system
	Service   string // default: ax-server
	Target    string // e.g. "pod/name" or "svc/name" (overrides Service if set)
	Port      int    // remote port, default: 8080
}

func SanitizeContext(name string) string {
	var sb strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
	}
	s := sb.String()
	if s == "" {
		return "default"
	}
	return s
}

func TunnelDir() (string, error) {
	if dir := os.Getenv("AX_HOME"); dir != "" {
		p := filepath.Join(dir, "tunnels")
		return p, os.MkdirAll(p, 0755)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(home, ".ax", "tunnels")
	return p, os.MkdirAll(p, 0755)
}

func GetTunnel(ctxName string) (*TunnelInfo, error) {
	dir, err := TunnelDir()
	if err != nil {
		return nil, err
	}
	statePath := filepath.Join(dir, SanitizeContext(ctxName)+".json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, err
	}
	var info TunnelInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func SaveTunnel(info *TunnelInfo) error {
	dir, err := TunnelDir()
	if err != nil {
		return err
	}
	statePath := filepath.Join(dir, SanitizeContext(info.Context)+".json")
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath, data, 0644)
}

func StopTunnel(info *TunnelInfo) error {
	if info == nil {
		return nil
	}
	if info.PID > 0 {
		// Attempt to terminate process group and single process
		_ = syscall.Kill(-info.PID, syscall.SIGTERM)
		_ = syscall.Kill(info.PID, syscall.SIGTERM)
	}

	dir, err := TunnelDir()
	if err == nil {
		_ = os.Remove(filepath.Join(dir, SanitizeContext(info.Context)+".json"))
	}
	return nil
}

func StopTunnelByContext(ctxName string) error {
	info, err := GetTunnel(ctxName)
	if err != nil {
		return err
	}
	return StopTunnel(info)
}

func ListTunnels() ([]*TunnelInfo, error) {
	dir, err := TunnelDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var results []*TunnelInfo
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var info TunnelInfo
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}
		results = append(results, &info)
	}
	return results, nil
}

func IsTunnelHealthy(port int) bool {
	if port <= 0 {
		return false
	}
	client := http.Client{Timeout: 400 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// EnsureServerURL resolves the AX server URL.
// If ServerURL or AX_SERVER is explicitly set, it returns that.
// Otherwise, it checks the active Kubernetes context (or opts.Context)
// and establishes/reuses a background port-forward tunnel to svc/ax-server.
func EnsureServerURL(opts Options) (string, error) {
	if opts.ServerURL != "" {
		return opts.ServerURL, nil
	}
	if env := os.Getenv("AX_SERVER"); env != "" {
		return env, nil
	}

	ctxName, err := CurrentContext(opts.Context)
	if err != nil || ctxName == "" {
		// No Kubernetes context found; fallback to local default
		return "http://localhost:8080", nil
	}

	if opts.Namespace == "" {
		if envNs := os.Getenv("AX_SYSTEM_NAMESPACE"); envNs != "" {
			opts.Namespace = envNs
		} else {
			opts.Namespace = v1alpha1.DefaultSystemNamespace
		}
	}
	if opts.Service == "" {
		opts.Service = "ax-server"
	}
	if opts.Port == 0 {
		opts.Port = 8080
	}

	// Check for existing active tunnel
	if existing, err := GetTunnel(ctxName); err == nil && existing != nil {
		// Empty Namespace/Service means the tunnel was recorded before these fields
		// existed; treat it as matching for backward compatibility.
		sameTarget := (existing.Namespace == "" || existing.Namespace == opts.Namespace) &&
			(existing.Service == "" || existing.Service == opts.Service)
		if sameTarget && IsTunnelHealthy(existing.Port) {
			return fmt.Sprintf("http://127.0.0.1:%d", existing.Port), nil
		}
		// Existing tunnel is unhealthy or stale, clean it up
		_ = StopTunnel(existing)
	}

	// Start new background port-forward for this context
	dir, err := TunnelDir()
	if err != nil {
		return "", fmt.Errorf("creating tunnel directory: %w", err)
	}
	logPath := filepath.Join(dir, SanitizeContext(ctxName)+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return "", fmt.Errorf("opening tunnel log: %w", err)
	}
	defer logFile.Close()

	var kArgs []string
	if ctxName != "" {
		kArgs = append(kArgs, "--context", ctxName)
	}
	targetRes := fmt.Sprintf("svc/%s", opts.Service)
	if opts.Target != "" {
		targetRes = opts.Target
	}
	kArgs = append(kArgs, "port-forward", "-n", opts.Namespace, targetRes, fmt.Sprintf(":%d", opts.Port))

	cmd := exec.Command("kubectl", kArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("spawning kubectl port-forward for context %q: %w", ctxName, err)
	}

	// Wait for port assignment from log output
	localPort := 0
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)

		logData, err := os.ReadFile(logPath)
		if err != nil {
			continue
		}
		if match := portForwardRegex.FindStringSubmatch(string(logData)); len(match) > 1 {
			p, err := strconv.Atoi(match[1])
			if err == nil && p > 0 {
				localPort = p
				break
			}
		}

		// Check if command exited prematurely
		if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
			return "", fmt.Errorf("kubectl port-forward exited for context %q: %s", ctxName, strings.TrimSpace(string(logData)))
		}
	}

	if localPort == 0 {
		_ = cmd.Process.Kill()
		logData, _ := os.ReadFile(logPath)
		return "", fmt.Errorf("timeout waiting for kubectl port-forward on context %q: %s", ctxName, strings.TrimSpace(string(logData)))
	}

	// Wait for healthz to respond
	healthy := false
	healthDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(healthDeadline) {
		if IsTunnelHealthy(localPort) {
			healthy = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !healthy {
		_ = cmd.Process.Kill()
		return "", fmt.Errorf("tunnel started on port %d but health check failed for context %q", localPort, ctxName)
	}

	info := &TunnelInfo{
		Context:   ctxName,
		Namespace: opts.Namespace,
		Service:   opts.Service,
		Port:      localPort,
		PID:       cmd.Process.Pid,
		CreatedAt: time.Now(),
	}
	_ = SaveTunnel(info)

	return fmt.Sprintf("http://127.0.0.1:%d", localPort), nil
}

// PortForward starts an ephemeral port-forward to a Kubernetes resource (e.g. pod/name or svc/name)
// and returns the local assigned port and a cleanup function.
func PortForward(ctx context.Context, kubeContext, namespace, targetResource string, remotePort int) (int, func(), error) {
	ctxName, err := CurrentContext(kubeContext)
	if err != nil {
		ctxName = ""
	}

	var kArgs []string
	if ctxName != "" {
		kArgs = append(kArgs, "--context", ctxName)
	}
	if namespace != "" {
		kArgs = append(kArgs, "-n", namespace)
	}
	kArgs = append(kArgs, "port-forward", targetResource, fmt.Sprintf(":%d", remotePort))

	cmd := exec.CommandContext(ctx, "kubectl", kArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	pr, pw, err := os.Pipe()
	if err != nil {
		return 0, nil, fmt.Errorf("creating pipe: %w", err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return 0, nil, fmt.Errorf("spawning kubectl port-forward for %s: %w", targetResource, err)
	}

	portChan := make(chan int, 1)
	errChan := make(chan error, 1)

	go func() {
		buf := make([]byte, 1024)
		var output strings.Builder
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				output.Write(buf[:n])
				if match := portForwardRegex.FindStringSubmatch(output.String()); len(match) > 1 {
					p, convErr := strconv.Atoi(match[1])
					if convErr == nil && p > 0 {
						portChan <- p
					}
				}
			}
			if err != nil {
				select {
				case errChan <- fmt.Errorf("kubectl port-forward exited: %s", strings.TrimSpace(output.String())):
				default:
				}
				return
			}
		}
	}()

	cleanup := func() {
		_ = pr.Close()
		_ = pw.Close()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			_ = cmd.Process.Kill()
		}
	}

	select {
	case p := <-portChan:
		return p, cleanup, nil
	case err := <-errChan:
		cleanup()
		return 0, nil, err
	case <-time.After(8 * time.Second):
		cleanup()
		return 0, nil, fmt.Errorf("timed out waiting for port-forward to %s", targetResource)
	case <-ctx.Done():
		cleanup()
		return 0, nil, ctx.Err()
	}
}
