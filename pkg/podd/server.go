package podd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// ManagedProcess tracks a foreground or background command managed by podd.
type ManagedProcess struct {
	ID        string    `json:"id"`
	Command   string    `json:"command"`
	PID       int       `json:"pid"`
	Running   bool      `json:"running"`
	ExitCode  *int      `json:"exitCode,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	mu        sync.Mutex
	output    bytes.Buffer
	cancel    context.CancelFunc
}

// Server provides the in-sandbox execution, file management, and lifecycle daemon API.
type Server struct {
	Addr      string
	Workdir   string
	procsMu   sync.RWMutex
	processes map[string]*ManagedProcess
}

// NewServer creates a new podd Server instance.
func NewServer(addr string, workdir string) *Server {
	return &Server{
		Addr:      addr,
		Workdir:   workdir,
		processes: make(map[string]*ManagedProcess),
	}
}

// Handler returns the HTTP multiplexer for podd.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /v1/commands/run", s.handleRunCommand)
	mux.HandleFunc("GET /v1/commands/{id}/status", s.handleCommandStatus)
	mux.HandleFunc("GET /v1/commands/{id}/logs", s.handleCommandLogs)
	mux.HandleFunc("POST /v1/commands/{id}/kill", s.handleCommandKill)
	mux.HandleFunc("POST /v1/files/archive-sync", s.handleArchiveSync)
	mux.HandleFunc("POST /v1/lifecycle/prepare-stop", s.handlePrepareStop)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"workdir": s.Workdir,
	})
}

type runCommandRequest struct {
	Command    string            `json:"command"`
	Workdir    string            `json:"workdir,omitempty"`
	Envs       map[string]string `json:"envs,omitempty"`
	Background bool              `json:"background,omitempty"`
}

func (s *Server) handleRunCommand(w http.ResponseWriter, r *http.Request) {
	var req runCommandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cwd := req.Workdir
	if cwd == "" {
		cwd = s.Workdir
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "bash", "-c", req.Command)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	for k, v := range req.Envs {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	procID := fmt.Sprintf("exec-%d", time.Now().UnixNano())
	proc := &ManagedProcess{
		ID:        procID,
		Command:   req.Command,
		Running:   true,
		StartedAt: time.Now().UTC(),
		cancel:    cancel,
	}
	cmd.Stdout = &proc.output
	cmd.Stderr = &proc.output

	if err := cmd.Start(); err != nil {
		cancel()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	proc.PID = cmd.Process.Pid

	s.procsMu.Lock()
	s.processes[procID] = proc
	s.procsMu.Unlock()

	done := make(chan struct{})
	go func() {
		err := cmd.Wait()
		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				code = -1
			}
		}
		proc.mu.Lock()
		proc.Running = false
		proc.ExitCode = &code
		proc.mu.Unlock()
		close(done)
	}()

	if !req.Background {
		<-done
	}

	proc.mu.Lock()
	resp := map[string]any{
		"id":       proc.ID,
		"pid":      proc.PID,
		"running":  proc.Running,
		"exitCode": proc.ExitCode,
		"output":   proc.output.String(),
	}
	proc.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleCommandStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.procsMu.RLock()
	proc, ok := s.processes[id]
	s.procsMu.RUnlock()
	if !ok {
		http.Error(w, "process not found", http.StatusNotFound)
		return
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":       proc.ID,
		"running":  proc.Running,
		"exitCode": proc.ExitCode,
	})
}

func (s *Server) handleCommandLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cursor, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
	s.procsMu.RLock()
	proc, ok := s.processes[id]
	s.procsMu.RUnlock()
	if !ok {
		http.Error(w, "process not found", http.StatusNotFound)
		return
	}
	proc.mu.Lock()
	data := proc.output.Bytes()
	if cursor < 0 || cursor > len(data) {
		cursor = len(data)
	}
	chunk := string(data[cursor:])
	nextCursor := len(data)
	proc.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"content": chunk,
		"cursor":  nextCursor,
	})
}

func (s *Server) handleCommandKill(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.procsMu.RLock()
	proc, ok := s.processes[id]
	s.procsMu.RUnlock()
	if !ok {
		http.Error(w, "process not found", http.StatusNotFound)
		return
	}
	proc.cancel()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleArchiveSync(w http.ResponseWriter, r *http.Request) {
	targetDir := r.URL.Query().Get("dest")
	if targetDir == "" {
		targetDir = s.Workdir
	}
	gz, err := gzip.NewReader(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid gzip stream: %v", err), http.StatusBadRequest)
		return
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	extracted := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("tar error: %v", err), http.StatusBadRequest)
			return
		}
		cleanRel := filepath.Clean(hdr.Name)
		if filepath.IsAbs(cleanRel) || cleanRel == ".." {
			continue
		}
		destPath := filepath.Join(targetDir, cleanRel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			_ = os.MkdirAll(destPath, 0o755)
		case tar.TypeReg:
			_ = os.MkdirAll(filepath.Dir(destPath), 0o755)
			f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_, _ = io.Copy(f, tr)
			_ = f.Close()
			extracted++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"extractedFiles": extracted})
}

func (s *Server) handlePrepareStop(w http.ResponseWriter, _ *http.Request) {
	slog.Info("flushing filesystems before Pod suspend")
	_ = exec.Command("sync").Run()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "synced"})
}
