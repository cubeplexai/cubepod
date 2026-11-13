package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	cubepodDesktopCtlPath = "/opt/cubepod/desktop/cubepod-desktop-ctl"
	defaultDesktopPort    = 8080
	defaultTerminalPort   = 7681
	defaultCDPPort        = 9222
)

var desktopResolutionRegex = regexp.MustCompile(`^[0-9]{3,5}x[0-9]{3,5}x(16|24|32)$`)

type desktopStartReq struct {
	Resolution       string `json:"resolution,omitempty"`
	InitialURL       string `json:"initialUrl,omitempty"`
	ExpiresInSeconds int    `json:"expiresInSeconds,omitempty"`
}

type desktopCtlPorts struct {
	Desktop  int `json:"desktop"`
	Terminal int `json:"terminal"`
	CDP      int `json:"cdp"`
}

type desktopCtlStatus struct {
	Status     string          `json:"status"`
	Display    string          `json:"display,omitempty"`
	Resolution string          `json:"resolution,omitempty"`
	Ports      desktopCtlPorts `json:"ports"`
	PIDs       map[string]int  `json:"pids,omitempty"`
}

type desktopStartResp struct {
	Status      string `json:"status"`
	DesktopURL  string `json:"desktopUrl"`
	TerminalURL string `json:"terminalUrl"`
	CDPURL      string `json:"cdpUrl"`
	ExpiresAt   string `json:"expiresAt"`
}

type desktopCmdResult struct {
	ExitCode int
	Output   string
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func isDesktopNotInstalled(res *desktopCmdResult) bool {
	if res == nil {
		return false
	}
	if res.ExitCode == 127 {
		return true
	}
	lower := strings.ToLower(res.Output)
	return strings.Contains(lower, "no such file or directory") ||
		strings.Contains(lower, "not found") ||
		strings.Contains(lower, "cannot execute")
}

func writeDesktopNotInstalled(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   "desktop_not_installed",
		"message": "sandbox image does not include cubepod-desktop stack",
	})
}

func parseDesktopCtlJSON(output string) (*desktopCtlStatus, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil, fmt.Errorf("empty output from cubepod-desktop-ctl")
	}
	var st desktopCtlStatus
	if err := json.Unmarshal([]byte(trimmed), &st); err == nil {
		applyDesktopPortDefaults(&st)
		return &st, nil
	}
	// Scan lines in reverse in case wrapper output precedes the JSON line.
	lines := strings.Split(trimmed, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			if err := json.Unmarshal([]byte(line), &st); err == nil {
				applyDesktopPortDefaults(&st)
				return &st, nil
			}
		}
	}
	return nil, fmt.Errorf("invalid JSON from cubepod-desktop-ctl: %q", trimmed)
}

func applyDesktopPortDefaults(st *desktopCtlStatus) {
	if st.Ports.Desktop <= 0 {
		st.Ports.Desktop = defaultDesktopPort
	}
	if st.Ports.Terminal <= 0 {
		st.Ports.Terminal = defaultTerminalPort
	}
	if st.Ports.CDP <= 0 {
		st.Ports.CDP = defaultCDPPort
	}
}

func (s *Server) runDesktopCtl(ctx context.Context, rec *SandboxRecord, cmdStr string) (*desktopCmdResult, error) {
	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	if poddURL != "" {
		payload, err := json.Marshal(map[string]any{
			"command": cmdStr,
			"uid":     1000,
			"gid":     1000,
		})
		if err != nil {
			return nil, err
		}
		target := strings.TrimSuffix(poddURL, "/") + "/v1/commands/run"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		client := s.httpClient
		if client == nil {
			client = &http.Client{Timeout: 60 * time.Second}
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("podd returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var raw struct {
			ExitCode      *int   `json:"exit_code"`
			CamelExitCode *int   `json:"exitCode"`
			Output        string `json:"output"`
			Stdout        string `json:"stdout"`
			Stderr        string `json:"stderr"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("decode podd command response: %w", err)
		}
		exitCode := 0
		if raw.ExitCode != nil {
			exitCode = *raw.ExitCode
		} else if raw.CamelExitCode != nil {
			exitCode = *raw.CamelExitCode
		}
		output := raw.Output
		if output == "" {
			output = raw.Stdout
			if raw.Stderr != "" {
				if output != "" {
					output += "\n"
				}
				output += raw.Stderr
			}
		}
		return &desktopCmdResult{ExitCode: exitCode, Output: output}, nil
	}

	// Local fallback execution
	execCmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	if rec.WorkspaceDir != "" {
		execCmd.Dir = rec.WorkspaceDir
	}
	var stdout, stderr bytes.Buffer
	execCmd.Stdout = &stdout
	execCmd.Stderr = &stderr
	err := execCmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return nil, err
		}
	}
	combined := stdout.String()
	if stderr.Len() > 0 {
		if combined != "" {
			combined += "\n"
		}
		combined += stderr.String()
	}
	return &desktopCmdResult{ExitCode: exitCode, Output: combined}, nil
}

// handleDesktopStart handles POST /sandboxes/{sandboxID}/desktop/start.
func (s *Server) handleDesktopStart(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	rec, ok := s.resolveSandbox(r.Context(), sandboxID)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	var req desktopStartReq
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid JSON request body", http.StatusBadRequest)
			return
		}
	}
	if req.Resolution != "" && !desktopResolutionRegex.MatchString(req.Resolution) {
		http.Error(w, "invalid resolution format (expected WxHxD where D is 16, 24, or 32)", http.StatusBadRequest)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.Metrics().DesktopActivationsError.Add(1)
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	args := []string{cubepodDesktopCtlPath, "start"}
	if req.Resolution != "" {
		args = append(args, "--resolution", shellQuote(req.Resolution))
	}
	if req.InitialURL != "" {
		args = append(args, "--url", shellQuote(req.InitialURL))
	}
	cmdStr := strings.Join(args, " ")

	res, err := s.runDesktopCtl(r.Context(), rec, cmdStr)
	if err != nil {
		s.Metrics().DesktopActivationsError.Add(1)
		http.Error(w, "failed to execute desktop start: "+err.Error(), http.StatusBadGateway)
		return
	}
	if isDesktopNotInstalled(res) {
		s.Metrics().DesktopActivationsUnsupported.Add(1)
		writeDesktopNotInstalled(w)
		return
	}
	if res.ExitCode != 0 {
		s.Metrics().DesktopActivationsError.Add(1)
		http.Error(w, fmt.Sprintf("desktop start failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Output)), http.StatusInternalServerError)
		return
	}

	ctlStatus, err := parseDesktopCtlJSON(res.Output)
	if err != nil {
		s.Metrics().DesktopActivationsError.Add(1)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if host := s.resolveSandboxHost(rec); host != "" {
		s.RegisterPort(sandboxID, ctlStatus.Ports.Desktop, fmt.Sprintf("http://%s:%d", host, ctlStatus.Ports.Desktop))
		s.RegisterPort(sandboxID, ctlStatus.Ports.Terminal, fmt.Sprintf("http://%s:%d", host, ctlStatus.Ports.Terminal))
		s.RegisterPort(sandboxID, ctlStatus.Ports.CDP, fmt.Sprintf("http://%s:%d", host, ctlStatus.Ports.CDP))
	}

	expiresIn := req.ExpiresInSeconds
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	tokDesktop, expTime := s.mintPanelToken(sandboxID, ctlStatus.Ports.Desktop, expiresIn)
	tokTerminal, _ := s.mintPanelToken(sandboxID, ctlStatus.Ports.Terminal, expiresIn)
	tokCDP, _ := s.mintPanelToken(sandboxID, ctlStatus.Ports.CDP, expiresIn)

	statusVal := ctlStatus.Status
	if statusVal == "" {
		statusVal = "running"
	}

	s.Metrics().DesktopActivationsOK.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(desktopStartResp{
		Status:      statusVal,
		DesktopURL:  fmt.Sprintf("/panel/%s/vnc.html?autoconnect=true&resize=scale", tokDesktop),
		TerminalURL: fmt.Sprintf("/panel/%s/", tokTerminal),
		CDPURL:      fmt.Sprintf("/panel/%s/", tokCDP),
		ExpiresAt:   expTime.Format(time.RFC3339),
	})
}

// handleDesktopStatus handles GET /sandboxes/{sandboxID}/desktop/status.
func (s *Server) handleDesktopStatus(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	rec, ok := s.resolveSandbox(r.Context(), sandboxID)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	res, err := s.runDesktopCtl(r.Context(), rec, cubepodDesktopCtlPath+" status")
	if err != nil {
		http.Error(w, "failed to query desktop status: "+err.Error(), http.StatusBadGateway)
		return
	}
	if isDesktopNotInstalled(res) {
		writeDesktopNotInstalled(w)
		return
	}
	if res.ExitCode != 0 {
		http.Error(w, fmt.Sprintf("desktop status failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Output)), http.StatusInternalServerError)
		return
	}

	ctlStatus, err := parseDesktopCtlJSON(res.Output)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ctlStatus)
}

// handleDesktopStop handles POST /sandboxes/{sandboxID}/desktop/stop.
func (s *Server) handleDesktopStop(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	rec, ok := s.resolveSandbox(r.Context(), sandboxID)
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	res, err := s.runDesktopCtl(r.Context(), rec, cubepodDesktopCtlPath+" stop")
	if err != nil {
		http.Error(w, "failed to stop desktop: "+err.Error(), http.StatusBadGateway)
		return
	}
	if isDesktopNotInstalled(res) {
		writeDesktopNotInstalled(w)
		return
	}
	if res.ExitCode != 0 {
		http.Error(w, fmt.Sprintf("desktop stop failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Output)), http.StatusInternalServerError)
		return
	}

	if ctlStatus, err := parseDesktopCtlJSON(res.Output); err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ctlStatus)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "stopped",
	})
}
