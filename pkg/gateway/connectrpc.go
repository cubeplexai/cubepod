package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// makeConnectFrame wraps payload with Connect-RPC envelope:
// 1-byte flag (0 = message data, 2 = end-of-stream trailer) + 4-byte big-endian length.
func makeConnectFrame(data []byte, isEnd bool) []byte {
	prefix := byte(0)
	if isEnd {
		prefix = 2
	}
	frame := make([]byte, 5+len(data))
	frame[0] = prefix
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(data)))
	copy(frame[5:], data)
	return frame
}

// unpackConnectRPCBody unmarshals request payload, automatically stripping 5-byte
// Connect-RPC framing headers if present.
func unpackConnectRPCBody(r *http.Request) ([]byte, bool, error) {
	if r.Body == nil {
		return nil, false, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, false, err
	}
	isFramed := false
	if len(body) >= 5 && (body[0] == 0 || body[0] == 1) {
		msgLen := binary.BigEndian.Uint32(body[1:5])
		if int(msgLen)+5 <= len(body) {
			isFramed = true
			return body[5 : 5+msgLen], isFramed, nil
		}
	}
	return body, isFramed, nil
}

// writeConnectError serializes a standard Connect-RPC error JSON payload.
func (s *Server) writeConnectError(w http.ResponseWriter, statusCode int, code string, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"code":    code,
		"message": msg,
	})
}

// writeUnaryResponse sends a unary Connect-RPC response, respecting requested framing/Content-Type.
func (s *Server) writeUnaryResponse(w http.ResponseWriter, r *http.Request, data interface{}) {
	payload, err := json.Marshal(data)
	if err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to marshal response: "+err.Error())
		return
	}

	reqCT := r.Header.Get("Content-Type")
	if strings.Contains(reqCT, "application/connect+json") {
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(makeConnectFrame(payload, false))
		_, _ = w.Write(makeConnectFrame([]byte("{}"), true))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func (s *Server) writeStreamData(w io.Writer, mu *sync.Mutex, flusher http.Flusher, field string, chunk []byte) bool {
	b64 := base64.StdEncoding.EncodeToString(chunk)
	msg, err := json.Marshal(map[string]interface{}{
		"event": map[string]interface{}{
			"data": map[string]interface{}{
				field: b64,
			},
		},
	})
	if err != nil {
		return false
	}
	return s.writeConnectFrameSafe(w, mu, msg, false, flusher) == nil
}

func (s *Server) writeStreamError(w io.Writer, mu *sync.Mutex, flusher http.Flusher, code, message string) {
	payload, _ := json.Marshal(map[string]interface{}{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
	_ = s.writeConnectFrameSafe(w, mu, payload, true, flusher)
}

// streamLocalPTY forwards queued PTY bytes. Client disconnect does not kill the process.
// A subscriber that falls behind is ended with resource_exhausted, not a short success stream.
func (s *Server) streamLocalPTY(w http.ResponseWriter, r *http.Request, entry *commandEntry, sub *ptySubscriber, flusher http.Flusher, writeMu *sync.Mutex) {
	if sub == nil {
		return
	}
	defer func() {
		entry.mu.Lock()
		delete(entry.subscribers, sub)
		entry.mu.Unlock()
	}()
	doneCh := entry.done
	if doneCh == nil {
		doneCh = make(chan struct{})
		close(doneCh)
	}
	writeEnd := func() {
		if sub.overflowed() {
			s.writeStreamError(w, writeMu, flusher, "resource_exhausted", "pty subscriber queue overflow")
			return
		}
		entry.mu.RLock()
		exitCode := entry.exitCode
		entry.mu.RUnlock()
		endPayload, _ := json.Marshal(map[string]interface{}{
			"event": map[string]interface{}{
				"end": map[string]interface{}{
					"exitCode": exitCode,
					"exited":   true,
				},
			},
		})
		_ = s.writeConnectFrameSafe(w, writeMu, endPayload, false, flusher)
		_ = s.writeConnectFrameSafe(w, writeMu, []byte("{}"), true, flusher)
	}
	for {
		select {
		case <-sub.failed:
			s.writeStreamError(w, writeMu, flusher, "resource_exhausted", "pty subscriber queue overflow")
			return
		case chunk := <-sub.ch:
			if !s.writeStreamData(w, writeMu, flusher, "pty", chunk) {
				return
			}
		case <-doneCh:
			for {
				select {
				case <-sub.failed:
					s.writeStreamError(w, writeMu, flusher, "resource_exhausted", "pty subscriber queue overflow")
					return
				case chunk := <-sub.ch:
					if !s.writeStreamData(w, writeMu, flusher, "pty", chunk) {
						return
					}
				default:
					writeEnd()
					return
				}
			}
		case <-r.Context().Done():
			return
		}
	}
}

// writeConnectFrameSafe writes a frame to the writer with an optional mutex lock and flushes.
func (s *Server) writeConnectFrameSafe(w io.Writer, mu *sync.Mutex, data []byte, isEnd bool, flusher http.Flusher) error {
	frame := makeConnectFrame(data, isEnd)
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	_, err := w.Write(frame)
	if flusher != nil {
		flusher.Flush()
	}
	return err
}

// resolveConnectRPCSandbox extracts the target SandboxRecord from headers, query params, path, or subdomain.
func (s *Server) resolveConnectRPCSandbox(r *http.Request) (*SandboxRecord, error) {
	sandboxID := r.PathValue("sandboxID")
	if sandboxID == "" {
		sandboxID = r.Header.Get("E2b-Sandbox-Id")
	}
	if sandboxID == "" {
		sandboxID = r.Header.Get("e2b-sandbox-id")
	}
	if sandboxID == "" {
		sandboxID = r.Header.Get("X-Sandbox-ID")
	}
	if sandboxID == "" {
		sandboxID = r.Header.Get("X-Sandbox-Id")
	}
	if sandboxID == "" {
		sandboxID = r.URL.Query().Get("sandboxID")
	}
	if sandboxID == "" {
		sandboxID = r.URL.Query().Get("sandbox_id")
	}
	if sandboxID == "" {
		sandboxID = r.URL.Query().Get("sandboxId")
	}
	if sandboxID == "" && r.Host != "" {
		matches := subdomainHostRegex.FindStringSubmatch(r.Host)
		if len(matches) == 4 {
			sandboxID = matches[2]
		}
	}

	if sandboxID != "" {
		rec, ok := s.resolveSandbox(r.Context(), sandboxID)
		if !ok {
			return nil, fmt.Errorf("sandbox %q not found", sandboxID)
		}
		return rec, nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	// Fallback if exactly one sandbox is active
	if len(s.sandboxes) == 1 {
		for _, rec := range s.sandboxes {
			return rec, nil
		}
	}

	return nil, fmt.Errorf("target sandbox ID required (via E2b-Sandbox-Id header, query param, or path)")
}

// shouldForwardConnectRPC checks if a live poddURL is available or live pod routing is enabled.
func (s *Server) shouldForwardConnectRPC(rec *SandboxRecord) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if rec.PoddURL != "" {
		return rec.PoddURL, true
	}
	if s.livePodRouting && s.reconciler != nil {
		if boxStatus, ok := s.reconciler.GetBox(rec.SandboxID); ok && boxStatus.PodIP != "" && boxStatus.PodIP != "10.244.0.42" {
			return fmt.Sprintf("http://%s:49983", boxStatus.PodIP), true
		}
	}
	return "", false
}

// forwardConnectRPCToPodd proxies the Connect-RPC request upstream to the live podd daemon.
func (s *Server) forwardConnectRPCToPodd(w http.ResponseWriter, r *http.Request, poddURL string) {
	targetURL, err := url.Parse(poddURL)
	if err != nil {
		s.writeConnectError(w, http.StatusBadGateway, "internal", "invalid podd URL: "+err.Error())
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.Host = targetURL.Host

			cleanPath := r.URL.Path
			if idx := strings.Index(cleanPath, "/process.Process/"); idx >= 0 {
				cleanPath = cleanPath[idx:]
			} else if idx := strings.Index(cleanPath, "/filesystem.Filesystem/"); idx >= 0 {
				cleanPath = cleanPath[idx:]
			}
			req.URL.Path = cleanPath

			if targetURL.RawQuery != "" {
				if req.URL.RawQuery != "" {
					req.URL.RawQuery = targetURL.RawQuery + "&" + req.URL.RawQuery
				} else {
					req.URL.RawQuery = targetURL.RawQuery
				}
			}
		},
		FlushInterval: 10 * time.Millisecond,
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
			slog.Error("podd connectrpc reverse proxy failed", "target", poddURL, "err", proxyErr)
			s.writeConnectError(rw, http.StatusBadGateway, "unavailable", "podd connectrpc proxy error: "+proxyErr.Error())
		},
	}

	proxy.ServeHTTP(w, r)
}

// handleConnectRPCDispatch dispatches an incoming Connect-RPC request on a given sandbox.
func (s *Server) handleConnectRPCDispatch(w http.ResponseWriter, r *http.Request, rec *SandboxRecord) {
	r.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	switch r.URL.Path {
	case "/process.Process/Start":
		s.handleConnectRPCProcessStart(w, r)
	case "/process.Process/SendSignal":
		s.handleConnectRPCProcessSendSignal(w, r)
	case "/process.Process/Connect":
		s.handleConnectRPCProcessConnect(w, r)
	case "/process.Process/List":
		s.handleConnectRPCProcessList(w, r)
	case "/process.Process/SendInput":
		s.handleConnectRPCProcessSendInput(w, r)
	case "/process.Process/Update":
		s.handleConnectRPCProcessUpdate(w, r)
	case "/process.Process/StreamInput":
		s.handleConnectRPCProcessStreamInput(w, r)
	case "/process.Process/CloseStdin":
		s.handleConnectRPCProcessCloseStdin(w, r)
	case "/filesystem.Filesystem/Read":
		s.handleConnectRPCFilesystemRead(w, r)
	case "/filesystem.Filesystem/Write":
		s.handleConnectRPCFilesystemWrite(w, r)
	case "/filesystem.Filesystem/List", "/filesystem.Filesystem/ListDir":
		s.handleConnectRPCFilesystemList(w, r)
	case "/filesystem.Filesystem/Stat":
		s.handleConnectRPCFilesystemStat(w, r)
	case "/filesystem.Filesystem/Remove":
		s.handleConnectRPCFilesystemRemove(w, r)
	case "/filesystem.Filesystem/MakeDir":
		s.handleConnectRPCFilesystemMakeDir(w, r)
	default:
		http.NotFound(w, r)
	}
}

func safeSandboxEnv(envs map[string]string, isPTY bool) []string {
	pathVal := os.Getenv("PATH")
	if pathVal == "" {
		pathVal = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	homeVal := os.Getenv("HOME")
	if homeVal == "" {
		homeVal = "/workspace"
	}
	out := []string{
		"PATH=" + pathVal,
		"HOME=" + homeVal,
		"LANG=C.UTF-8",
	}
	if _, hasTerm := envs["TERM"]; isPTY && !hasTerm {
		out = append(out, "TERM=xterm-256color")
	}
	for k, v := range envs {
		out = append(out, k+"="+v)
	}
	return out
}

// handleConnectRPCProcessStart handles POST /process.Process/Start.
func (s *Server) handleConnectRPCProcessStart(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}

	bodyBytes, _, err := unpackConnectRPCBody(r)
	if err != nil {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", "failed to read body: "+err.Error())
		return
	}

	var rpcReq struct {
		Process struct {
			Cmd  string            `json:"cmd"`
			Args []string          `json:"args"`
			Envs map[string]string `json:"envs"`
			Cwd  string            `json:"cwd"`
		} `json:"process"`
		PTY *struct {
			Size struct {
				Cols int `json:"cols"`
				Rows int `json:"rows"`
			} `json:"size"`
		} `json:"pty"`
		Tag   string            `json:"tag"`
		Stdin bool              `json:"stdin"`
		Cmd   string            `json:"cmd"`
		Args  []string          `json:"args"`
		Envs  map[string]string `json:"envs"`
		Cwd   string            `json:"cwd"`
	}
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &rpcReq)
	}

	envs := rpcReq.Process.Envs
	if envs == nil {
		envs = rpcReq.Envs
	}

	cwd := rpcReq.Process.Cwd
	if cwd == "" {
		cwd = rpcReq.Cwd
	}
	workdir := rec.WorkspaceDir
	if cwd != "" && cwd != "/workspace" {
		if strings.HasPrefix(cwd, "/workspace/") {
			rel := strings.TrimPrefix(cwd, "/workspace/")
			workdir = filepath.Join(rec.WorkspaceDir, rel)
		} else {
			workdir = cwd
		}
	}
	if workdir == "" {
		workdir = s.workspaceDirForSandbox(rec.SandboxID, rec.VolumeKey)
		rec.WorkspaceDir = workdir
	}
	_ = os.MkdirAll(workdir, 0755)

	if rpcReq.PTY != nil {
		cols := rpcReq.PTY.Size.Cols
		if cols <= 0 {
			cols = 80
		}
		rows := rpcReq.PTY.Size.Rows
		if rows <= 0 {
			rows = 24
		}
		ws := &unix.Winsize{
			Row: uint16(rows),
			Col: uint16(cols),
		}
		masterFile, slaveFile, err := openOSPTY(ws)
		if err != nil {
			s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to openpty: "+err.Error())
			return
		}

		ptyCmdName := rpcReq.Process.Cmd
		if ptyCmdName == "" {
			ptyCmdName = rpcReq.Cmd
		}
		if ptyCmdName == "" {
			ptyCmdName = "/bin/bash"
		}
		ptyCmdArgs := rpcReq.Process.Args
		if len(ptyCmdArgs) == 0 {
			ptyCmdArgs = rpcReq.Args
		}

		ptyCtx, ptyCancel := context.WithCancel(context.Background())
		execCmd := exec.CommandContext(ptyCtx, ptyCmdName, ptyCmdArgs...)
		execCmd.Dir = workdir
		execCmd.Env = safeSandboxEnv(envs, true)
		execCmd.Stdin = slaveFile
		execCmd.Stdout = slaveFile
		execCmd.Stderr = slaveFile
		execCmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid:  true,
			Setctty: true,
			Ctty:    0,
		}

		if err := execCmd.Start(); err != nil {
			_ = masterFile.Close()
			_ = slaveFile.Close()
			ptyCancel()
			s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to start pty process: "+err.Error())
			return
		}
		_ = slaveFile.Close()

		pid := execCmd.Process.Pid
		cmdID := fmt.Sprintf("cmd-%d", pid)
		tag := rpcReq.Tag
		entry := &commandEntry{
			id:        cmdID,
			cmdID:     cmdID,
			pid:       pid,
			sandboxID: rec.SandboxID,
			tag:       tag,
			config: map[string]interface{}{
				"cmd":  ptyCmdName,
				"args": ptyCmdArgs,
				"envs": envs,
				"cwd":  workdir,
			},
			running:     true,
			buf:         &bytes.Buffer{},
			cancel:      ptyCancel,
			masterFile:  masterFile,
			isPTY:       true,
			subscribers: make(map[*ptySubscriber]struct{}),
			done:        make(chan struct{}),
		}
		s.mu.Lock()
		s.pruneLocalCommandsLocked()
		s.commands[cmdID] = entry
		s.commands[strconv.Itoa(pid)] = entry
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)

		flusher, _ := w.(http.Flusher)
		var writeMu sync.Mutex

		// 1. Send start event
		startPayload, _ := json.Marshal(map[string]interface{}{
			"event": map[string]interface{}{
				"start": map[string]interface{}{
					"pid": pid,
				},
			},
		})
		s.writeConnectFrameSafe(w, &writeMu, startPayload, false, flusher)

		// Subscribe before the reader starts so early PTY bytes are queued.
		_, _, _, sub := entry.snapshotAndSubscribe()

		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := masterFile.Read(buf)
				if n > 0 {
					chunk := make([]byte, n)
					copy(chunk, buf[:n])
					entry.broadcastPTY(chunk)
				}
				if err != nil {
					break
				}
			}
		}()

		go func() {
			_ = execCmd.Wait()
			exitCode := 0
			if execCmd.ProcessState != nil {
				exitCode = execCmd.ProcessState.ExitCode()
			}
			entry.mu.Lock()
			entry.running = false
			entry.exitCode = exitCode
			entry.exitedAt = time.Now()
			if entry.masterFile != nil {
				_ = entry.masterFile.Close()
				entry.masterFile = nil
			}
			entry.mu.Unlock()
			close(entry.done)
		}()

		// timeoutMs is a client ConnectRPC stream deadline, not a server-side kill.
		s.streamLocalPTY(w, r, entry, sub, flusher, &writeMu)
		return
	}

	var cmdName string
	var cmdArgs []string
	args := rpcReq.Process.Args
	if len(args) == 0 {
		args = rpcReq.Args
	}
	cmd := rpcReq.Process.Cmd
	if cmd == "" {
		cmd = rpcReq.Cmd
	}

	if len(args) >= 3 && args[0] == "-l" && args[1] == "-c" {
		cmdName = "sh"
		cmdArgs = []string{"-c", args[2]}
	} else if len(args) > 0 {
		cmdName = cmd
		if cmdName == "" {
			cmdName = args[0]
			cmdArgs = args[1:]
		} else {
			cmdArgs = args
		}
	} else if cmd != "" {
		cmdName = "sh"
		cmdArgs = []string{"-c", cmd}
	} else {
		cmdName = "echo"
		cmdArgs = []string{""}
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	execCmd := exec.CommandContext(ctx, cmdName, cmdArgs...)
	execCmd.Dir = workdir
	execCmd.Env = safeSandboxEnv(envs, false)

	stdoutPipe, err := execCmd.StdoutPipe()
	if err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to create stdout pipe: "+err.Error())
		return
	}
	stderrPipe, err := execCmd.StderrPipe()
	if err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to create stderr pipe: "+err.Error())
		return
	}

	if err := execCmd.Start(); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to start process: "+err.Error())
		return
	}

	pid := execCmd.Process.Pid
	cmdID := fmt.Sprintf("cmd-%d", pid)
	entry := &commandEntry{
		id:        cmdID,
		cmdID:     cmdID,
		pid:       pid,
		sandboxID: rec.SandboxID,
		tag:       rpcReq.Tag,
		config: map[string]interface{}{
			"cmd":  cmdName,
			"args": cmdArgs,
			"envs": envs,
			"cwd":  workdir,
		},
		running: true,
		buf:     &bytes.Buffer{},
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	s.mu.Lock()
	s.pruneLocalCommandsLocked()
	s.commands[cmdID] = entry
	s.commands[strconv.Itoa(pid)] = entry
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/connect+json")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	var writeMu sync.Mutex

	// 1. Send start event
	startPayload, _ := json.Marshal(map[string]interface{}{
		"event": map[string]interface{}{
			"start": map[string]interface{}{
				"pid": pid,
			},
		},
	})
	s.writeConnectFrameSafe(w, &writeMu, startPayload, false, flusher)

	// 2. Stream stdout and stderr
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				entry.mu.Lock()
				entry.buf.Write(chunk)
				entry.mu.Unlock()

				b64 := base64.StdEncoding.EncodeToString(chunk)
				msg, _ := json.Marshal(map[string]interface{}{
					"event": map[string]interface{}{
						"data": map[string]interface{}{
							"stdout": b64,
						},
					},
				})
				s.writeConnectFrameSafe(w, &writeMu, msg, false, flusher)
			}
			if err != nil {
				break
			}
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				entry.mu.Lock()
				entry.buf.Write(chunk)
				entry.mu.Unlock()

				b64 := base64.StdEncoding.EncodeToString(chunk)
				msg, _ := json.Marshal(map[string]interface{}{
					"event": map[string]interface{}{
						"data": map[string]interface{}{
							"stderr": b64,
						},
					},
				})
				s.writeConnectFrameSafe(w, &writeMu, msg, false, flusher)
			}
			if err != nil {
				break
			}
		}
	}()

	wg.Wait()
	waitErr := execCmd.Wait()
	exitCode := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	entry.mu.Lock()
	entry.running = false
	entry.exitCode = exitCode
	entry.exitedAt = time.Now()
	entry.mu.Unlock()
	if entry.done != nil {
		close(entry.done)
	}

	// 3. Send end event
	endPayload, _ := json.Marshal(map[string]interface{}{
		"event": map[string]interface{}{
			"end": map[string]interface{}{
				"exitCode": exitCode,
				"exited":   true,
			},
		},
	})
	s.writeConnectFrameSafe(w, &writeMu, endPayload, false, flusher)

	// 4. Send end-of-stream trailer frame (flag 2, {})
	s.writeConnectFrameSafe(w, &writeMu, []byte("{}"), true, flusher)
}

// handleConnectRPCProcessSendSignal handles POST /process.Process/SendSignal.
func (s *Server) handleConnectRPCProcessSendSignal(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}

	bodyBytes, _, err := unpackConnectRPCBody(r)
	if err != nil {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", "failed to read body: "+err.Error())
		return
	}

	var rpcReq struct {
		Process struct {
			Pid      int    `json:"pid"`
			Tag      string `json:"tag"`
			Selector struct {
				Pid int    `json:"pid"`
				Tag string `json:"tag"`
			} `json:"selector"`
		} `json:"process"`
		Signal any `json:"signal"`
	}
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &rpcReq)
	}

	pid := rpcReq.Process.Pid
	if pid == 0 {
		pid = rpcReq.Process.Selector.Pid
	}
	tag := rpcReq.Process.Tag
	if tag == "" {
		tag = rpcReq.Process.Selector.Tag
	}

	entry := s.findLocalCommandEntry(rec.SandboxID, pid, tag)
	if entry == nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", "process not found")
		return
	}

	entry.mu.RLock()
	running := entry.running
	entryPid := entry.pid
	entry.mu.RUnlock()
	if !running {
		s.writeConnectError(w, http.StatusNotFound, "not_found", "process not found")
		return
	}

	sig := syscall.SIGKILL
	if strSig, ok := rpcReq.Signal.(string); ok && (strings.Contains(strSig, "TERM") || strings.Contains(strSig, "15")) {
		sig = syscall.SIGTERM
	} else if numSig, ok := rpcReq.Signal.(float64); ok && int(numSig) == 15 {
		sig = syscall.SIGTERM
	}

	if entryPid > 0 {
		_ = syscall.Kill(-entryPid, sig)
		if proc, findErr := os.FindProcess(entryPid); findErr == nil {
			_ = proc.Signal(sig)
		}
	}

	s.writeUnaryResponse(w, r, map[string]interface{}{})
}

// handleConnectRPCProcessConnect handles POST /process.Process/Connect.
func (s *Server) handleConnectRPCProcessConnect(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}

	bodyBytes, _, _ := unpackConnectRPCBody(r)
	var rpcReq struct {
		Process struct {
			Pid      int    `json:"pid"`
			Tag      string `json:"tag"`
			Selector struct {
				Pid int    `json:"pid"`
				Tag string `json:"tag"`
			} `json:"selector"`
		} `json:"process"`
	}
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &rpcReq)
	}

	pid := rpcReq.Process.Pid
	if pid == 0 {
		pid = rpcReq.Process.Selector.Pid
	}
	tag := rpcReq.Process.Tag
	if tag == "" {
		tag = rpcReq.Process.Selector.Tag
	}

	entry := s.findLocalCommandEntry(rec.SandboxID, pid, tag)
	if entry == nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", "process not found")
		return
	}

	w.Header().Set("Content-Type", "application/connect+json")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	var writeMu sync.Mutex

	startPayload, _ := json.Marshal(map[string]interface{}{
		"event": map[string]interface{}{
			"start": map[string]interface{}{
				"pid": entry.pid,
			},
		},
	})
	s.writeConnectFrameSafe(w, &writeMu, startPayload, false, flusher)

	snap, running, exitCode, sub := entry.snapshotAndSubscribe()
	entry.mu.RLock()
	isPTY := entry.isPTY
	doneCh := entry.done
	entry.mu.RUnlock()

	if len(snap) > 0 {
		field := "stdout"
		if isPTY {
			field = "pty"
		}
		if !s.writeStreamData(w, &writeMu, flusher, field, snap) {
			if sub != nil {
				entry.mu.Lock()
				delete(entry.subscribers, sub)
				entry.mu.Unlock()
			}
			return
		}
	}

	if sub != nil {
		s.streamLocalPTY(w, r, entry, sub, flusher, &writeMu)
		return
	} else {
		if running && doneCh != nil {
			sentLen := len(snap)
			ticker := time.NewTicker(25 * time.Millisecond)
			defer ticker.Stop()
		waitLoop:
			for {
				select {
				case <-r.Context().Done():
					return
				case <-doneCh:
					entry.mu.RLock()
					var tail []byte
					if entry.buf != nil && entry.buf.Len() > sentLen {
						b := entry.buf.Bytes()
						tail = append([]byte(nil), b[sentLen:]...)
					}
					exitCode = entry.exitCode
					entry.mu.RUnlock()
					if len(tail) > 0 {
						_ = s.writeStreamData(w, &writeMu, flusher, "stdout", tail)
					}
					break waitLoop
				case <-ticker.C:
					entry.mu.RLock()
					var chunk []byte
					if entry.buf != nil && entry.buf.Len() > sentLen {
						b := entry.buf.Bytes()
						chunk = append([]byte(nil), b[sentLen:]...)
						sentLen += len(chunk)
					}
					entry.mu.RUnlock()
					if len(chunk) > 0 {
						if !s.writeStreamData(w, &writeMu, flusher, "stdout", chunk) {
							return
						}
					}
				}
			}
		}
		endPayload, _ := json.Marshal(map[string]interface{}{
			"event": map[string]interface{}{
				"end": map[string]interface{}{
					"exitCode": exitCode,
					"exited":   true,
				},
			},
		})
		s.writeConnectFrameSafe(w, &writeMu, endPayload, false, flusher)
		s.writeConnectFrameSafe(w, &writeMu, []byte("{}"), true, flusher)
	}
}

// handleConnectRPCProcessList handles POST /process.Process/List.
func (s *Server) handleConnectRPCProcessList(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}

	type procInfo struct {
		Pid    int                    `json:"pid"`
		Tag    string                 `json:"tag,omitempty"`
		Config map[string]interface{} `json:"config,omitempty"`
	}

	s.mu.RLock()
	var procs []procInfo
	seen := make(map[int]bool)
	for _, cmd := range s.commands {
		if cmd.sandboxID != rec.SandboxID {
			continue
		}
		cmd.mu.RLock()
		if cmd.running && cmd.pid > 0 && !seen[cmd.pid] {
			seen[cmd.pid] = true
			cfg := cmd.config
			if cfg == nil {
				cfg = map[string]interface{}{
					"cmd":  cmd.cmdID,
					"args": []string{},
					"envs": map[string]string{},
				}
			}
			procs = append(procs, procInfo{
				Pid:    cmd.pid,
				Tag:    cmd.tag,
				Config: cfg,
			})
		}
		cmd.mu.RUnlock()
	}
	s.mu.RUnlock()

	if procs == nil {
		procs = []procInfo{}
	}
	s.writeUnaryResponse(w, r, map[string]interface{}{
		"processes": procs,
	})
}

// handleConnectRPCProcessSendInput handles POST /process.Process/SendInput.
func (s *Server) handleConnectRPCProcessSendInput(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}

	bodyBytes, _, err := unpackConnectRPCBody(r)
	if err != nil {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", "failed to read body: "+err.Error())
		return
	}

	var rpcReq struct {
		Process struct {
			Pid      int    `json:"pid"`
			Tag      string `json:"tag"`
			Selector struct {
				Pid int    `json:"pid"`
				Tag string `json:"tag"`
			} `json:"selector"`
		} `json:"process"`
		Input struct {
			Pty   string `json:"pty"`
			Stdin string `json:"stdin"`
		} `json:"input"`
		Pty   string `json:"pty"`
		Stdin string `json:"stdin"`
	}
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &rpcReq)
	}

	pid := rpcReq.Process.Pid
	if pid == 0 {
		pid = rpcReq.Process.Selector.Pid
	}
	tag := rpcReq.Process.Tag
	if tag == "" {
		tag = rpcReq.Process.Selector.Tag
	}

	entry := s.findLocalCommandEntry(rec.SandboxID, pid, tag)
	if entry == nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", "process not found")
		return
	}

	entry.mu.RLock()
	running := entry.running
	mf := entry.masterFile
	entry.mu.RUnlock()
	if !running {
		s.writeConnectError(w, http.StatusNotFound, "not_found", "process not found")
		return
	}

	rawB64 := rpcReq.Input.Pty
	if rawB64 == "" {
		rawB64 = rpcReq.Input.Stdin
	}
	if rawB64 == "" {
		rawB64 = rpcReq.Pty
	}
	if rawB64 == "" {
		rawB64 = rpcReq.Stdin
	}
	data, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", "invalid base64 input: "+err.Error())
		return
	}

	if mf != nil && len(data) > 0 {
		if _, err := mf.Write(data); err != nil {
			s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to write pty input: "+err.Error())
			return
		}
	}

	s.writeUnaryResponse(w, r, map[string]interface{}{})
}

// handleConnectRPCProcessUpdate handles POST /process.Process/Update.
func (s *Server) handleConnectRPCProcessUpdate(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}

	bodyBytes, _, err := unpackConnectRPCBody(r)
	if err != nil {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", "failed to read body: "+err.Error())
		return
	}

	var rpcReq struct {
		Process struct {
			Pid      int    `json:"pid"`
			Tag      string `json:"tag"`
			Selector struct {
				Pid int    `json:"pid"`
				Tag string `json:"tag"`
			} `json:"selector"`
		} `json:"process"`
		PTY struct {
			Size struct {
				Cols int `json:"cols"`
				Rows int `json:"rows"`
			} `json:"size"`
		} `json:"pty"`
	}
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &rpcReq)
	}

	pid := rpcReq.Process.Pid
	if pid == 0 {
		pid = rpcReq.Process.Selector.Pid
	}
	tag := rpcReq.Process.Tag
	if tag == "" {
		tag = rpcReq.Process.Selector.Tag
	}

	entry := s.findLocalCommandEntry(rec.SandboxID, pid, tag)
	if entry == nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", "process not found")
		return
	}

	cols := rpcReq.PTY.Size.Cols
	if cols <= 0 {
		cols = 80
	}
	rows := rpcReq.PTY.Size.Rows
	if rows <= 0 {
		rows = 24
	}

	entry.mu.RLock()
	running := entry.running
	mf := entry.masterFile
	entryPid := entry.pid
	entry.mu.RUnlock()
	if !running {
		s.writeConnectError(w, http.StatusNotFound, "not_found", "process not found")
		return
	}

	if mf != nil {
		ws := &unix.Winsize{
			Row: uint16(rows),
			Col: uint16(cols),
		}
		_ = unix.IoctlSetWinsize(int(mf.Fd()), unix.TIOCSWINSZ, ws)
	}

	if entryPid > 0 {
		_ = syscall.Kill(-entryPid, syscall.SIGWINCH)
		_ = syscall.Kill(entryPid, syscall.SIGWINCH)
	}

	s.writeUnaryResponse(w, r, map[string]interface{}{})
}

// handleConnectRPCProcessStreamInput handles POST /process.Process/StreamInput.
func (s *Server) handleConnectRPCProcessStreamInput(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	var currentEntry *commandEntry
	idx := 0
	for idx+5 <= len(bodyBytes) {
		flag := bodyBytes[idx]
		msgLen := binary.BigEndian.Uint32(bodyBytes[idx+1 : idx+5])
		start := idx + 5
		end := start + int(msgLen)
		if end > len(bodyBytes) {
			break
		}
		if flag == 2 {
			break
		}
		frameData := bodyBytes[start:end]
		idx = end

		type procSel struct {
			Pid      int    `json:"pid"`
			Tag      string `json:"tag"`
			Selector struct {
				Pid int    `json:"pid"`
				Tag string `json:"tag"`
			} `json:"selector"`
		}
		type startPart struct {
			Process procSel `json:"process"`
		}
		type dataPart struct {
			Input struct {
				Pty   string `json:"pty"`
				Stdin string `json:"stdin"`
			} `json:"input"`
			Pty   string `json:"pty"`
			Stdin string `json:"stdin"`
		}
		var event struct {
			Event struct {
				Start *startPart `json:"start"`
				Data  *dataPart  `json:"data"`
			} `json:"event"`
			Start *startPart `json:"start"`
			Data  *dataPart  `json:"data"`
		}
		if err := json.Unmarshal(frameData, &event); err == nil {
			startObj := event.Start
			if startObj == nil {
				startObj = event.Event.Start
			}
			dataObj := event.Data
			if dataObj == nil {
				dataObj = event.Event.Data
			}

			if startObj != nil {
				pid := startObj.Process.Pid
				if pid == 0 {
					pid = startObj.Process.Selector.Pid
				}
				tag := startObj.Process.Tag
				if tag == "" {
					tag = startObj.Process.Selector.Tag
				}
				currentEntry = s.findLocalCommandEntry(rec.SandboxID, pid, tag)
			} else if dataObj != nil && currentEntry != nil {
				rawB64 := dataObj.Input.Pty
				if rawB64 == "" {
					rawB64 = dataObj.Input.Stdin
				}
				if rawB64 == "" {
					rawB64 = dataObj.Pty
				}
				if rawB64 == "" {
					rawB64 = dataObj.Stdin
				}
				data, err := base64.StdEncoding.DecodeString(rawB64)
				if err != nil {
					s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", "invalid base64 input: "+err.Error())
					return
				}
				currentEntry.mu.RLock()
				running := currentEntry.running
				mf := currentEntry.masterFile
				currentEntry.mu.RUnlock()
				if running && mf != nil && len(data) > 0 {
					if _, err := mf.Write(data); err != nil {
						s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to write pty input: "+err.Error())
						return
					}
				}
			}
		}
	}

	s.writeUnaryResponse(w, r, map[string]interface{}{})
}

// handleConnectRPCProcessCloseStdin handles POST /process.Process/CloseStdin.
func (s *Server) handleConnectRPCProcessCloseStdin(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}
	s.writeConnectError(w, http.StatusNotImplemented, "unimplemented", "close stdin is not supported")
}

// handleConnectRPCFilesystemRead handles POST /filesystem.Filesystem/Read.
func (s *Server) handleConnectRPCFilesystemRead(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}

	bodyBytes, _, _ := unpackConnectRPCBody(r)
	var rpcReq struct {
		Path string `json:"path"`
	}
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &rpcReq)
	}
	targetPath := rpcReq.Path
	if targetPath == "" {
		targetPath = r.URL.Query().Get("path")
	}
	if targetPath == "" {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", "path is required")
		return
	}

	fullPath, err := s.resolveFilePath(rec, targetPath)
	if err != nil {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeConnectError(w, http.StatusNotFound, "not_found", "file not found")
			return
		}
		s.writeConnectError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	resp := map[string]interface{}{
		"content": string(data),
		"data":    base64.StdEncoding.EncodeToString(data),
	}
	s.writeUnaryResponse(w, r, resp)
}

// handleConnectRPCFilesystemWrite handles POST /filesystem.Filesystem/Write.
func (s *Server) handleConnectRPCFilesystemWrite(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}

	bodyBytes, _, _ := unpackConnectRPCBody(r)
	var rpcReq struct {
		Path    string `json:"path"`
		Data    string `json:"data"`
		Content string `json:"content"`
	}
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &rpcReq)
	}

	targetPath := rpcReq.Path
	if targetPath == "" {
		targetPath = r.URL.Query().Get("path")
	}
	if targetPath == "" {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", "path is required")
		return
	}

	var contentBytes []byte
	if rpcReq.Content != "" {
		contentBytes = []byte(rpcReq.Content)
	} else if rpcReq.Data != "" {
		if decoded, err := base64.StdEncoding.DecodeString(rpcReq.Data); err == nil && len(decoded) > 0 {
			contentBytes = decoded
		} else {
			contentBytes = []byte(rpcReq.Data)
		}
	}

	fullPath, err := s.resolveFilePath(rec, targetPath)
	if err != nil {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}

	_ = os.MkdirAll(filepath.Dir(fullPath), 0755)
	if err := os.WriteFile(fullPath, contentBytes, 0644); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to write file: "+err.Error())
		return
	}

	resp := map[string]interface{}{
		"success": true,
		"entry": map[string]interface{}{
			"name": filepath.Base(targetPath),
			"type": "FILE_TYPE_FILE",
			"path": targetPath,
			"size": len(contentBytes),
		},
	}
	s.writeUnaryResponse(w, r, resp)
}

// handleConnectRPCFilesystemList handles POST /filesystem.Filesystem/List and ListDir.
func (s *Server) handleConnectRPCFilesystemList(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}

	bodyBytes, _, _ := unpackConnectRPCBody(r)
	var rpcReq struct {
		Path  string `json:"path"`
		Depth int    `json:"depth"`
	}
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &rpcReq)
	}
	targetPath := rpcReq.Path
	if targetPath == "" {
		targetPath = r.URL.Query().Get("path")
	}
	if targetPath == "" {
		targetPath = "/workspace"
	}

	fullPath, err := s.resolveFilePath(rec, targetPath)
	if err != nil {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeConnectError(w, http.StatusNotFound, "not_found", "directory not found")
			return
		}
		s.writeConnectError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	type entryInfo struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Path        string `json:"path"`
		Size        int64  `json:"size"`
		Mode        uint32 `json:"mode"`
		Permissions string `json:"permissions"`
		IsDir       bool   `json:"isDir"`
		IsDirSnake  bool   `json:"is_dir"`
	}

	list := make([]entryInfo, 0, len(entries))
	for _, e := range entries {
		fi, _ := e.Info()
		t := "FILE_TYPE_FILE"
		if e.IsDir() {
			t = "FILE_TYPE_DIRECTORY"
		}
		var sz int64
		var mode uint32
		var perm string
		if fi != nil {
			sz = fi.Size()
			mode = uint32(fi.Mode().Perm())
			perm = fi.Mode().String()
		}
		itemPath := path.Join(targetPath, e.Name())
		list = append(list, entryInfo{
			Name:        e.Name(),
			Type:        t,
			Path:        itemPath,
			Size:        sz,
			Mode:        mode,
			Permissions: perm,
			IsDir:       e.IsDir(),
			IsDirSnake:  e.IsDir(),
		})
	}

	resp := map[string]interface{}{
		"entries": list,
	}
	s.writeUnaryResponse(w, r, resp)
}

// handleConnectRPCFilesystemStat handles POST /filesystem.Filesystem/Stat.
func (s *Server) handleConnectRPCFilesystemStat(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}

	bodyBytes, _, _ := unpackConnectRPCBody(r)
	var rpcReq struct {
		Path string `json:"path"`
	}
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &rpcReq)
	}
	targetPath := rpcReq.Path
	if targetPath == "" {
		targetPath = r.URL.Query().Get("path")
	}
	if targetPath == "" {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", "path is required")
		return
	}

	fullPath, err := s.resolveFilePath(rec, targetPath)
	if err != nil {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}

	fi, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeConnectError(w, http.StatusNotFound, "not_found", "file or directory not found")
			return
		}
		s.writeConnectError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	t := "FILE_TYPE_FILE"
	if fi.IsDir() {
		t = "FILE_TYPE_DIRECTORY"
	}

	resp := map[string]interface{}{
		"entry": map[string]interface{}{
			"name":        fi.Name(),
			"type":        t,
			"path":        targetPath,
			"size":        fi.Size(),
			"mode":        uint32(fi.Mode().Perm()),
			"permissions": fi.Mode().String(),
			"isDir":       fi.IsDir(),
			"is_dir":      fi.IsDir(),
		},
	}
	s.writeUnaryResponse(w, r, resp)
}

// handleConnectRPCFilesystemMakeDir handles POST /filesystem.Filesystem/MakeDir.
func (s *Server) handleConnectRPCFilesystemMakeDir(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}

	bodyBytes, _, _ := unpackConnectRPCBody(r)
	var rpcReq struct {
		Path string `json:"path"`
	}
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &rpcReq)
	}
	targetPath := rpcReq.Path
	if targetPath == "" {
		targetPath = r.URL.Query().Get("path")
	}
	if targetPath == "" {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", "path is required")
		return
	}

	fullPath, err := s.resolveFilePath(rec, targetPath)
	if err != nil {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}

	if err := os.MkdirAll(fullPath, 0755); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to make directory: "+err.Error())
		return
	}

	resp := map[string]interface{}{
		"entry": map[string]interface{}{
			"name":        filepath.Base(targetPath),
			"type":        "FILE_TYPE_DIRECTORY",
			"path":        targetPath,
			"isDir":       true,
			"is_dir":      true,
			"permissions": "drwxr-xr-x",
		},
	}
	s.writeUnaryResponse(w, r, resp)
}

// handleConnectRPCFilesystemRemove handles POST /filesystem.Filesystem/Remove.
func (s *Server) handleConnectRPCFilesystemRemove(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()

	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}

	bodyBytes, _, _ := unpackConnectRPCBody(r)
	var rpcReq struct {
		Path string `json:"path"`
	}
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &rpcReq)
	}
	targetPath := rpcReq.Path
	if targetPath == "" {
		targetPath = r.URL.Query().Get("path")
	}
	if targetPath == "" {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", "path is required")
		return
	}

	fullPath, err := s.resolveFilePath(rec, targetPath)
	if err != nil {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}

	_ = os.RemoveAll(fullPath)
	s.writeUnaryResponse(w, r, map[string]interface{}{})
}

// openOSPTY opens a real OS pseudo-terminal using /dev/ptmx.
func openOSPTY(ws *unix.Winsize) (master *os.File, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		_ = m.Close()
		return nil, nil, err
	}
	ptn, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		_ = m.Close()
		return nil, nil, err
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", ptn), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		_ = m.Close()
		return nil, nil, err
	}
	if ws != nil {
		_ = unix.IoctlSetWinsize(int(m.Fd()), unix.TIOCSWINSZ, ws)
	}
	return m, s, nil
}
