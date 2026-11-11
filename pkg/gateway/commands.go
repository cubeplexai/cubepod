package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type runCommandReq struct {
	Cmd        string            `json:"cmd"`
	Command    string            `json:"command"`
	Workdir    string            `json:"workdir"`
	Envs       map[string]string `json:"envs"`
	Background bool              `json:"background"`
}

func (s *Server) handleRunCommand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	s.mu.RLock()
	rec, ok := s.sandboxes[id]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	// Forward to live podd daemon if registered
	if poddURL != "" {
		target := strings.TrimSuffix(poddURL, "/") + "/v1/commands/run"
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback execution
	var req runCommandReq
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	cmdStr := req.Cmd
	if cmdStr == "" {
		cmdStr = req.Command
	}
	if cmdStr == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}

	workdir := rec.WorkspaceDir
	if req.Workdir != "" && req.Workdir != "/workspace" {
		if strings.HasPrefix(req.Workdir, "/workspace/") {
			rel := strings.TrimPrefix(req.Workdir, "/workspace/")
			workdir = filepath.Join(rec.WorkspaceDir, rel)
		} else {
			workdir = req.Workdir
		}
	}
	_ = os.MkdirAll(workdir, 0755)

	cmdID := fmt.Sprintf("cmd-%d", time.Now().UnixNano())

	if req.Background {
		ctx, cancel := context.WithCancel(context.Background())
		execCmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
		execCmd.Dir = workdir
		execCmd.Env = os.Environ()
		for k, v := range req.Envs {
			execCmd.Env = append(execCmd.Env, fmt.Sprintf("%s=%s", k, v))
		}

		entry := &commandEntry{
			id:      cmdID,
			cmdID:   cmdID,
			running: true,
			buf:     &bytes.Buffer{},
			cancel:  cancel,
		}
		writer := &lockedWriter{entry: entry}
		execCmd.Stdout = writer
		execCmd.Stderr = writer

		if err := execCmd.Start(); err != nil {
			cancel()
			http.Error(w, "failed to start command: "+err.Error(), http.StatusInternalServerError)
			return
		}

		entry.pid = execCmd.Process.Pid
		s.mu.Lock()
		s.commands[cmdID] = entry
		s.mu.Unlock()

		go func() {
			waitErr := execCmd.Wait()
			entry.mu.Lock()
			entry.running = false
			if waitErr != nil {
				if exitErr, ok := waitErr.(*exec.ExitError); ok {
					entry.exitCode = exitErr.ExitCode()
				} else {
					entry.exitCode = 1
				}
			} else {
				entry.exitCode = 0
			}
			entry.mu.Unlock()
		}()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      cmdID,
			"cmdID":   cmdID,
			"running": true,
			"pid":     entry.pid,
		})
		return
	}

	// Synchronous / Foreground Execution
	execCmd := exec.CommandContext(r.Context(), "sh", "-c", cmdStr)
	execCmd.Dir = workdir
	execCmd.Env = os.Environ()
	for k, v := range req.Envs {
		execCmd.Env = append(execCmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	var stdout, stderr bytes.Buffer
	execCmd.Stdout = &stdout
	execCmd.Stderr = &stderr

	err := execCmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	combined := append(stdout.Bytes(), stderr.Bytes()...)
	entry := &commandEntry{
		id:       cmdID,
		cmdID:    cmdID,
		running:  false,
		exitCode: exitCode,
		buf:      bytes.NewBuffer(combined),
	}
	s.mu.Lock()
	s.commands[cmdID] = entry
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":       cmdID,
		"cmdID":    cmdID,
		"exitCode": exitCode,
		"stdout":   stdout.String(),
		"stderr":   stderr.String(),
	})
}

func (s *Server) handleGetCommandLogs(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	cmdID := r.PathValue("cmdID")

	s.mu.RLock()
	rec, ok := s.sandboxes[sandboxID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	// Forward to live podd daemon if registered
	if poddURL != "" {
		target := fmt.Sprintf("%s/v1/commands/%s/logs", strings.TrimSuffix(poddURL, "/"), cmdID)
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	s.mu.RLock()
	entry, ok := s.commands[cmdID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "command not found", http.StatusNotFound)
		return
	}

	cursorStr := r.URL.Query().Get("cursor")
	cursor := 0
	if cursorStr != "" {
		if c, err := strconv.Atoi(cursorStr); err == nil {
			cursor = c
		}
	}

	entry.mu.RLock()
	all := entry.buf.Bytes()
	running := entry.running
	exitCode := entry.exitCode

	var content string
	nextCursor := len(all)
	if cursor < 0 {
		cursor = 0
	}
	if cursor < len(all) {
		content = string(all[cursor:])
	}
	entry.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":       cmdID,
		"cmdID":    cmdID,
		"content":  content,
		"cursor":   nextCursor,
		"running":  running,
		"exitCode": exitCode,
	})
}

func (s *Server) handleGetCommandStatus(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	cmdID := r.PathValue("cmdID")

	s.mu.RLock()
	rec, ok := s.sandboxes[sandboxID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	// Forward to live podd daemon if registered
	if poddURL != "" {
		target := fmt.Sprintf("%s/v1/commands/%s/status", strings.TrimSuffix(poddURL, "/"), cmdID)
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	s.mu.RLock()
	entry, ok := s.commands[cmdID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "command not found", http.StatusNotFound)
		return
	}

	entry.mu.RLock()
	running := entry.running
	exitCode := entry.exitCode
	cursor := entry.buf.Len()
	pid := entry.pid
	entry.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":        cmdID,
		"cmdID":     cmdID,
		"pid":       pid,
		"running":   running,
		"exit_code": exitCode,
		"exitCode":  exitCode,
		"cursor":    cursor,
	})
}

func (s *Server) handleStreamCommandLogs(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	cmdID := r.PathValue("cmdID")

	s.mu.RLock()
	rec, ok := s.sandboxes[sandboxID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	cursor := 0
	client := &http.Client{Timeout: 5 * time.Second}

	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}

		var content string
		var nextCursor int
		var running bool
		var exitCode int

		if poddURL != "" {
			reqURL := fmt.Sprintf("%s/v1/commands/%s/logs?cursor=%d", strings.TrimSuffix(poddURL, "/"), cmdID, cursor)
			req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, reqURL, nil)
			if err != nil {
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				return
			}
			var logResp struct {
				ID       string `json:"id"`
				Content  string `json:"content"`
				Cursor   int    `json:"cursor"`
				Running  bool   `json:"running"`
				ExitCode *int   `json:"exit_code"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&logResp)
			_ = resp.Body.Close()

			content = logResp.Content
			nextCursor = logResp.Cursor
			running = logResp.Running
			if logResp.ExitCode != nil {
				exitCode = *logResp.ExitCode
			}
		} else {
			s.mu.RLock()
			entry, ok := s.commands[cmdID]
			s.mu.RUnlock()
			if !ok {
				return
			}

			entry.mu.RLock()
			all := entry.buf.Bytes()
			running = entry.running
			exitCode = entry.exitCode
			nextCursor = len(all)
			if cursor < len(all) {
				content = string(all[cursor:])
			}
			entry.mu.RUnlock()
		}

		cursor = nextCursor

		frameData, _ := json.Marshal(map[string]interface{}{
			"id":        cmdID,
			"cmdID":     cmdID,
			"content":   content,
			"cursor":    cursor,
			"running":   running,
			"exit_code": exitCode,
			"exitCode":  exitCode,
		})

		_, _ = fmt.Fprintf(w, "data: %s\n\n", frameData)
		flusher.Flush()

		if !running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *Server) handleKillCommand(w http.ResponseWriter, r *http.Request) {
	sandboxID := r.PathValue("sandboxID")
	cmdID := r.PathValue("cmdID")

	s.mu.RLock()
	rec, ok := s.sandboxes[sandboxID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	if err := s.ensureRunning(r.Context(), rec); err != nil {
		http.Error(w, "failed to resume sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	poddURL := rec.PoddURL
	s.mu.RUnlock()

	// Forward to live podd daemon if registered
	if poddURL != "" {
		target := fmt.Sprintf("%s/v1/commands/%s/kill", strings.TrimSuffix(poddURL, "/"), cmdID)
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	s.mu.RLock()
	entry, ok := s.commands[cmdID]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, "command not found", http.StatusNotFound)
		return
	}

	entry.mu.Lock()
	if entry.running {
		if entry.cancel != nil {
			entry.cancel()
		}
		entry.running = false
		entry.exitCode = -1
	}
	entry.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"id":     cmdID,
		"cmdID":  cmdID,
		"killed": true,
	})
}
