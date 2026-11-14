package gateway

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func (s *Server) resolveFilePath(rec *SandboxRecord, userPath string) (string, error) {
	if userPath == "" {
		return "", fmt.Errorf("empty file path")
	}
	baseDir := rec.WorkspaceDir
	if baseDir == "" {
		baseDir = s.workspaceDirForSandbox(rec.SandboxID, rec.VolumeKey)
		_ = os.MkdirAll(baseDir, 0755)
		rec.WorkspaceDir = baseDir
	}

	p := path.Clean("/" + userPath)
	var rel string
	if strings.HasPrefix(p, "/workspace/") {
		rel = strings.TrimPrefix(p, "/workspace/")
	} else if p == "/workspace" {
		rel = ""
	} else {
		rel = strings.TrimPrefix(p, "/")
	}

	fullPath := filepath.Join(baseDir, filepath.FromSlash(rel))
	cleanBase := filepath.Clean(baseDir)
	cleanFull := filepath.Clean(fullPath)
	if cleanFull != cleanBase && !strings.HasPrefix(cleanFull, cleanBase+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid path traversal attempt")
	}
	return cleanFull, nil
}

func (s *Server) resolveSandboxFromRequest(r *http.Request) (*SandboxRecord, error) {
	id := r.PathValue("sandboxID")
	if id != "" {
		rec, ok := s.resolveSandbox(r.Context(), id)
		if !ok {
			return nil, fmt.Errorf("sandbox not found")
		}
		return rec, nil
	}
	return s.resolveConnectRPCSandbox(r)
}

func (s *Server) handlePutFile(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveSandboxFromRequest(r)
	if err != nil {
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
		target := strings.TrimSuffix(poddURL, "/") + "/v1/files"
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	targetPath := r.URL.Query().Get("path")
	var bodyBytes []byte
	if r.Body != nil {
		bodyBytes, _ = io.ReadAll(r.Body)
	}

	var contentBytes []byte
	var fileJSON struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if len(bodyBytes) > 0 && bodyBytes[0] == '{' && json.Unmarshal(bodyBytes, &fileJSON) == nil && (fileJSON.Path != "" || fileJSON.Content != "") {
		if targetPath == "" {
			targetPath = fileJSON.Path
		}
		contentBytes = []byte(fileJSON.Content)
	} else {
		contentBytes = bodyBytes
	}

	if targetPath == "" {
		http.Error(w, "path parameter or json field is required", http.StatusBadRequest)
		return
	}

	fullPath, err := s.resolveFilePath(rec, targetPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := os.WriteFile(fullPath, contentBytes, 0644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/files" {
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"name":     filepath.Base(targetPath),
				"type":     "file",
				"path":     targetPath,
				"metadata": map[string]string{},
			},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "ok",
		"path":   targetPath,
		"size":   len(contentBytes),
	})
}

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveSandboxFromRequest(r)
	if err != nil {
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
		target := strings.TrimSuffix(poddURL, "/") + "/v1/files"
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	targetPath := r.URL.Query().Get("path")
	if targetPath == "" {
		http.Error(w, "path parameter is required", http.StatusBadRequest)
		return
	}

	fullPath, err := s.resolveFilePath(rec, targetPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if r.URL.Query().Get("format") == "json" || r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"path":    targetPath,
			"content": string(data),
			"size":    len(data),
		})
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveSandboxFromRequest(r)
	if err != nil {
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
		target := strings.TrimSuffix(poddURL, "/") + "/v1/files"
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	targetPath := r.URL.Query().Get("path")
	if targetPath == "" {
		http.Error(w, "path parameter is required", http.StatusBadRequest)
		return
	}

	fullPath, err := s.resolveFilePath(rec, targetPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}

	if err := os.RemoveAll(fullPath); err != nil {
		http.Error(w, "failed to delete file: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"deleted": targetPath,
	})
}

type fileEntryInfo struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	Mode  string `json:"mode"`
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveSandboxFromRequest(r)
	if err != nil {
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
		target := strings.TrimSuffix(poddURL, "/") + "/v1/files/list"
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	targetPath := r.URL.Query().Get("path")
	if targetPath == "" {
		targetPath = "/workspace"
	}

	fullPath, err := s.resolveFilePath(rec, targetPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	fi, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "directory not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !fi.IsDir() {
		http.Error(w, "path is not a directory", http.StatusBadRequest)
		return
	}

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		http.Error(w, "failed to read directory: "+err.Error(), http.StatusInternalServerError)
		return
	}

	result := make([]fileEntryInfo, 0, len(entries))
	for _, entry := range entries {
		info, _ := entry.Info()
		var size int64
		var mode string
		if info != nil {
			size = info.Size()
			mode = info.Mode().String()
		}
		entryPath := path.Join(targetPath, entry.Name())
		result = append(result, fileEntryInfo{
			Name:  entry.Name(),
			Path:  entryPath,
			IsDir: entry.IsDir(),
			Size:  size,
			Mode:  mode,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"path":    targetPath,
		"entries": result,
	})
}

func (s *Server) handleArchiveSync(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("sandboxID")
	rec, ok := s.resolveSandbox(r.Context(), id)
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
		target := strings.TrimSuffix(poddURL, "/") + "/v1/files/archive-sync"
		s.forwardToPodd(w, r, target)
		return
	}

	// Local fallback
	dest := r.URL.Query().Get("dest")
	if dest == "" {
		dest = r.URL.Query().Get("destination")
	}
	if dest == "" {
		dest = r.URL.Query().Get("path")
	}
	baseDir := rec.WorkspaceDir
	if baseDir == "" {
		baseDir = s.workspaceDirForSandbox(rec.SandboxID, rec.VolumeKey)
		_ = os.MkdirAll(baseDir, 0755)
		rec.WorkspaceDir = baseDir
	}
	if dest != "" {
		resolvedDest, err := s.resolveFilePath(rec, dest)
		if err != nil {
			http.Error(w, "invalid destination: "+err.Error(), http.StatusBadRequest)
			return
		}
		baseDir = resolvedDest
	}
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var removeTargets []string
	for _, key := range []string{"remove_paths", "removePaths"} {
		for _, entry := range r.URL.Query()[key] {
			for _, part := range strings.Split(entry, ",") {
				trimmed := strings.TrimSpace(part)
				if trimmed == "" {
					continue
				}
				resolved, err := resolveArchiveRemovePath(baseDir, trimmed)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				removeTargets = append(removeTargets, resolved)
			}
		}
	}

	var bodyBytes []byte
	if r.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	if len(bodyBytes) == 0 {
		if len(removeTargets) == 0 {
			http.Error(w, "empty request body", http.StatusBadRequest)
			return
		}
		for _, target := range removeTargets {
			if err := os.RemoveAll(target); err != nil {
				http.Error(w, "failed to remove path: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":    "ok",
			"extracted": 0,
		})
		return
	}

	// Remove uninstalled paths under destination before extracting archive
	for _, target := range removeTargets {
		if err := os.RemoveAll(target); err != nil {
			http.Error(w, "failed to remove path: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	br := bufio.NewReader(bytes.NewReader(bodyBytes))
	magic, err := br.Peek(2)
	if err != nil && err != io.EOF {
		http.Error(w, "failed to read archive: "+err.Error(), http.StatusBadRequest)
		return
	}

	var tr *tar.Reader
	if len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gzr, err := gzip.NewReader(br)
		if err != nil {
			http.Error(w, "invalid gzip data: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer gzr.Close()
		tr = tar.NewReader(gzr)
	} else {
		tr = tar.NewReader(br)
	}

	var extractedCount int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "tar read error: "+err.Error(), http.StatusBadRequest)
			return
		}

		cleanName := path.Clean("/" + hdr.Name)
		var rel string
		if strings.HasPrefix(cleanName, "/workspace/") {
			rel = strings.TrimPrefix(cleanName, "/workspace/")
		} else {
			rel = strings.TrimPrefix(cleanName, "/")
		}
		if rel == "" || rel == "." {
			continue
		}

		target := filepath.Join(baseDir, filepath.FromSlash(rel))
		cleanTarget := filepath.Clean(target)
		cleanBase := filepath.Clean(baseDir)
		if cleanTarget != cleanBase && !strings.HasPrefix(cleanTarget, cleanBase+string(filepath.Separator)) {
			http.Error(w, "directory traversal in tar entry: "+hdr.Name, http.StatusBadRequest)
			return
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(cleanTarget, 0755); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(cleanTarget), 0755); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			mode := os.FileMode(hdr.Mode & 0777)
			if mode == 0 {
				mode = 0644
			}
			f, err := os.OpenFile(cleanTarget, os.O_CREATE|os.O_RDWR|os.O_TRUNC, mode)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = f.Close()
			extractedCount++
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":    "ok",
		"extracted": extractedCount,
	})
}

func resolveArchiveRemovePath(baseDir, rawPath string) (string, error) {
	if rawPath == "" || strings.HasPrefix(rawPath, "/") || strings.HasPrefix(rawPath, "\\") {
		return "", fmt.Errorf("invalid archive path %q", rawPath)
	}
	normalized := strings.ReplaceAll(rawPath, "\\", "/")
	for _, seg := range strings.Split(normalized, "/") {
		if seg == ".." {
			return "", fmt.Errorf("invalid archive path traversal %q", rawPath)
		}
	}
	rel := strings.TrimPrefix(normalized, "./")
	cleanRel := path.Clean(rel)
	if cleanRel == "" || cleanRel == "." || cleanRel == ".." || strings.HasPrefix(cleanRel, "../") {
		return "", fmt.Errorf("invalid archive path %q", rawPath)
	}
	cleanBase := filepath.Clean(baseDir)
	target := filepath.Clean(filepath.Join(cleanBase, filepath.FromSlash(cleanRel)))
	if target == cleanBase || !strings.HasPrefix(target, cleanBase+string(filepath.Separator)) {
		return "", fmt.Errorf("archive path %q escapes destination", rawPath)
	}
	return target, nil
}
