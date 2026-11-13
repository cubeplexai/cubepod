package controller

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMaxContextBytes limits uploaded build context archives to 512 MiB.
	DefaultMaxContextBytes int64 = 512 * 1024 * 1024
)

var (
	ErrInvalidContextTar = errors.New("400: body is neither valid gzip nor tar archive")
	ErrPathTraversal     = errors.New("400: tar member path escapes context prefix")
	ErrTarMismatch       = errors.New("409: context tar body mismatch for existing hash")
	ErrContextTooLarge   = errors.New("413: context tar exceeds maximum allowed size")
)

// BuildLogEntry represents a structured line of template build output.
type BuildLogEntry struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
	Level     string `json:"level"`
}

// ContextStore manages uploaded template context tar archives and build logs.
type ContextStore interface {
	Store(hash string, r io.Reader) error
	IsPresent(hash string) bool
	Unpack(hash, destDir string) error
	Get(hash string) ([]byte, error)
	AppendLog(buildID string, lines []string) error
	ReadLogs(buildID string, offset, limit int, level string) ([]BuildLogEntry, error)
	SweepExpired(now time.Time, contextTTL, logTTL time.Duration) error
}

// MemoryContextStore is an in-memory and optional disk-backed implementation of ContextStore.
type MemoryContextStore struct {
	mu       sync.RWMutex
	tars     map[string][]byte
	tarTimes map[string]time.Time
	logs     map[string][]BuildLogEntry
	logTimes map[string]time.Time
	baseDir  string
	maxBytes int64
}

// NewMemoryContextStore creates a new MemoryContextStore.
func NewMemoryContextStore(baseDir ...string) *MemoryContextStore {
	dir := ""
	if len(baseDir) > 0 && baseDir[0] != "" {
		dir = baseDir[0]
	} else if env := os.Getenv("CUBEPOD_STATE"); env != "" {
		dir = filepath.Join(env, "template-context")
	}
	return &MemoryContextStore{
		tars:     make(map[string][]byte),
		tarTimes: make(map[string]time.Time),
		logs:     make(map[string][]BuildLogEntry),
		logTimes: make(map[string]time.Time),
		baseDir:  dir,
		maxBytes: DefaultMaxContextBytes,
	}
}

// SetMaxBytes overrides the maximum accepted compressed and decompressed archive size.
func (s *MemoryContextStore) SetMaxBytes(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > 0 {
		s.maxBytes = n
	} else {
		s.maxBytes = DefaultMaxContextBytes
	}
}

func validateTarEntry(hdr *tar.Header) error {
	clean := filepath.Clean(hdr.Name)
	if filepath.IsAbs(hdr.Name) || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: member %q", ErrPathTraversal, hdr.Name)
	}

	switch hdr.Typeflag {
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return fmt.Errorf("%w: unsupported device/fifo tar member %q", ErrInvalidContextTar, hdr.Name)
	case tar.TypeSymlink:
		if hdr.Linkname == "" || filepath.IsAbs(hdr.Linkname) {
			return fmt.Errorf("%w: symlink target %q escapes context prefix", ErrPathTraversal, hdr.Linkname)
		}
		resolved := filepath.Clean(filepath.Join(filepath.Dir(clean), hdr.Linkname))
		if filepath.IsAbs(resolved) || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: symlink %q target %q escapes context prefix", ErrPathTraversal, hdr.Name, hdr.Linkname)
		}
	case tar.TypeLink:
		if hdr.Linkname == "" || filepath.IsAbs(hdr.Linkname) {
			return fmt.Errorf("%w: hardlink target %q escapes context prefix", ErrPathTraversal, hdr.Linkname)
		}
		resolved := filepath.Clean(hdr.Linkname)
		if filepath.IsAbs(resolved) || resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: hardlink %q target %q escapes context prefix", ErrPathTraversal, hdr.Name, hdr.Linkname)
		}
	}
	return nil
}

// Store reads the body, enforces maxBytes, decompresses if gzipped, validates tar headers, and saves it.
func (s *MemoryContextStore) Store(hash string, r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	limit := s.maxBytes
	if limit <= 0 {
		limit = DefaultMaxContextBytes
	}

	// Read bounded input (limit+1) so both compressed and raw tar uploads respect maxBytes.
	rawInput, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return fmt.Errorf("%w: failed to read stream: %v", ErrInvalidContextTar, err)
	}
	if int64(len(rawInput)) > limit {
		return ErrContextTooLarge
	}
	if len(rawInput) < 2 {
		return ErrInvalidContextTar
	}

	var tarBytes []byte
	if rawInput[0] == 0x1f && rawInput[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(rawInput))
		if err != nil {
			return fmt.Errorf("%w: invalid gzip: %v", ErrInvalidContextTar, err)
		}
		defer gz.Close()

		decompressed, err := io.ReadAll(io.LimitReader(gz, limit+1))
		if err != nil {
			return fmt.Errorf("%w: failed to decompress gzip stream: %v", ErrInvalidContextTar, err)
		}
		if int64(len(decompressed)) > limit {
			return ErrContextTooLarge
		}
		tarBytes = decompressed
	} else {
		tarBytes = rawInput
	}

	// Validate tar format and inspect entries for path traversal and disallowed member types.
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	hasEntries := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: invalid tar format: %v", ErrInvalidContextTar, err)
		}
		hasEntries = true
		if err := validateTarEntry(hdr); err != nil {
			return err
		}
	}

	if !hasEntries && len(tarBytes) < 512 {
		return ErrInvalidContextTar
	}

	now := time.Now().UTC()

	// Check if already stored
	if existing, exists := s.tars[hash]; exists {
		if bytes.Equal(existing, tarBytes) {
			s.tarTimes[hash] = now
			return nil // Same bytes: success
		}
		return fmt.Errorf("%w: hash %s", ErrTarMismatch, hash)
	}

	// Also check disk if baseDir is set
	if s.baseDir != "" {
		tarPath := filepath.Join(s.baseDir, hash+".tar")
		if data, err := os.ReadFile(tarPath); err == nil {
			if bytes.Equal(data, tarBytes) {
				s.tars[hash] = data
				s.tarTimes[hash] = now
				return nil
			}
			return fmt.Errorf("%w: hash %s", ErrTarMismatch, hash)
		}

		_ = os.MkdirAll(s.baseDir, 0755)
		partialPath := filepath.Join(s.baseDir, hash+".tar.partial")
		if err := os.WriteFile(partialPath, tarBytes, 0644); err != nil {
			return fmt.Errorf("failed to write partial tar: %w", err)
		}
		if err := os.Rename(partialPath, tarPath); err != nil {
			return fmt.Errorf("failed to finalize tar file: %w", err)
		}
	}

	s.tars[hash] = tarBytes
	s.tarTimes[hash] = now
	return nil
}

// IsPresent checks if the decompressed tar is stored. A *.partial file is not present.
func (s *MemoryContextStore) IsPresent(hash string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.tars[hash]; ok {
		return true
	}

	if s.baseDir != "" {
		tarPath := filepath.Join(s.baseDir, hash+".tar")
		if fi, err := os.Stat(tarPath); err == nil && !fi.IsDir() {
			return true
		}
	}

	return false
}

// Get returns the decompressed tar bytes.
func (s *MemoryContextStore) Get(hash string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if data, ok := s.tars[hash]; ok {
		return data, nil
	}

	if s.baseDir != "" {
		tarPath := filepath.Join(s.baseDir, hash+".tar")
		data, err := os.ReadFile(tarPath)
		if err == nil {
			return data, nil
		}
	}

	return nil, fmt.Errorf("context tar %s not found", hash)
}

// Unpack extracts the tar under destDir/<hash>/...
func (s *MemoryContextStore) Unpack(hash, destDir string) error {
	data, err := s.Get(hash)
	if err != nil {
		return err
	}

	targetDir := filepath.Join(destDir, hash)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", targetDir, err)
	}

	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("error reading tar member: %w", err)
		}

		if err := validateTarEntry(hdr); err != nil {
			return err
		}

		clean := filepath.Clean(hdr.Name)
		if clean == "." {
			continue
		}

		destPath := filepath.Join(targetDir, clean)
		rel, err := filepath.Rel(targetDir, destPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: member %s escapes target directory", ErrPathTraversal, hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destPath, 0755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return err
			}
			_ = os.Remove(destPath)
			if err := os.Symlink(hdr.Linkname, destPath); err != nil {
				return err
			}
		case tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return err
			}
			linkTarget := filepath.Join(targetDir, filepath.Clean(hdr.Linkname))
			_ = os.Remove(destPath)
			if err := os.Link(linkTarget, destPath); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return err
			}
			mode := hdr.FileInfo().Mode()
			if mode == 0 {
				mode = 0644
			}
			f, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}

	return nil
}

// AppendLog appends structured build log lines to memory and, when baseDir is configured, to logs/<buildID>.log.
func (s *MemoryContextStore) AppendLog(buildID string, lines []string) error {
	if buildID == "" || len(lines) == 0 {
		return nil
	}
	now := time.Now().UTC()
	entries := make([]BuildLogEntry, 0, len(lines))
	var encodedLines []byte
	for _, line := range lines {
		entry := BuildLogEntry{
			Timestamp: now.Format(time.RFC3339Nano),
			Message:   line,
			Level:     inferLogLevel(line),
		}
		entries = append(entries, entry)
		b, _ := json.Marshal(entry)
		encodedLines = append(encodedLines, b...)
		encodedLines = append(encodedLines, '\n')
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs[buildID] = append(s.logs[buildID], entries...)
	s.logTimes[buildID] = now

	if s.baseDir != "" {
		logsDir := filepath.Join(s.baseDir, "logs")
		if err := os.MkdirAll(logsDir, 0755); err != nil {
			return err
		}
		logPath := filepath.Join(logsDir, buildID+".log")
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		_, writeErr := f.Write(encodedLines)
		closeErr := f.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// ReadLogs returns paginated build log entries filtered by optional level.
func (s *MemoryContextStore) ReadLogs(buildID string, offset, limit int, level string) ([]BuildLogEntry, error) {
	if buildID == "" {
		return nil, nil
	}

	s.mu.RLock()
	inMem := make([]BuildLogEntry, len(s.logs[buildID]))
	copy(inMem, s.logs[buildID])
	baseDir := s.baseDir
	s.mu.RUnlock()

	all := inMem
	if len(all) == 0 && baseDir != "" {
		logPath := filepath.Join(baseDir, "logs", buildID+".log")
		if f, err := os.Open(logPath); err == nil {
			defer f.Close()
			var fallbackTS string
			if fi, statErr := f.Stat(); statErr == nil {
				fallbackTS = fi.ModTime().UTC().Format(time.RFC3339Nano)
			} else {
				fallbackTS = time.Now().UTC().Format(time.RFC3339Nano)
			}
			scanner := bufio.NewScanner(f)
			// Allow longer build log lines up to 1 MiB.
			scanner.Buffer(make([]byte, 64*1024), 1024*1024)
			for scanner.Scan() {
				text := scanner.Text()
				if text == "" {
					continue
				}
				var parsed BuildLogEntry
				if err := json.Unmarshal([]byte(text), &parsed); err == nil && (parsed.Message != "" || parsed.Timestamp != "") {
					if parsed.Level == "" {
						parsed.Level = inferLogLevel(parsed.Message)
					}
					if parsed.Timestamp == "" {
						parsed.Timestamp = fallbackTS
					}
					all = append(all, parsed)
					continue
				}
				all = append(all, BuildLogEntry{
					Timestamp: fallbackTS,
					Message:   text,
					Level:     inferLogLevel(text),
				})
			}
			if err := scanner.Err(); err != nil {
				return nil, err
			}
		}
	}

	var filtered []BuildLogEntry
	for _, entry := range all {
		if level != "" && !strings.EqualFold(entry.Level, level) {
			continue
		}
		filtered = append(filtered, entry)
	}

	if offset < 0 {
		offset = 0
	}
	if offset >= len(filtered) {
		return []BuildLogEntry{}, nil
	}
	filtered = filtered[offset:]
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

// SweepExpired removes expired context tar archives and build logs older than their TTLs.
func (s *MemoryContextStore) SweepExpired(now time.Time, contextTTL, logTTL time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if contextTTL > 0 {
		cutoff := now.Add(-contextTTL)
		for hash, ts := range s.tarTimes {
			if ts.Before(cutoff) {
				delete(s.tarTimes, hash)
				delete(s.tars, hash)
			}
		}
		if s.baseDir != "" {
			if entries, err := os.ReadDir(s.baseDir); err == nil {
				for _, entry := range entries {
					if entry.IsDir() {
						continue
					}
					name := entry.Name()
					if !strings.HasSuffix(name, ".tar") && !strings.HasSuffix(name, ".tar.partial") {
						continue
					}
					if info, infoErr := entry.Info(); infoErr == nil && info.ModTime().Before(cutoff) {
						_ = os.Remove(filepath.Join(s.baseDir, name))
						hash := strings.TrimSuffix(strings.TrimSuffix(name, ".partial"), ".tar")
						delete(s.tars, hash)
						delete(s.tarTimes, hash)
					}
				}
			}
		}
	}

	if logTTL > 0 {
		cutoff := now.Add(-logTTL)
		for buildID, ts := range s.logTimes {
			if ts.Before(cutoff) {
				delete(s.logTimes, buildID)
				delete(s.logs, buildID)
			}
		}
		if s.baseDir != "" {
			logsDir := filepath.Join(s.baseDir, "logs")
			if entries, err := os.ReadDir(logsDir); err == nil {
				for _, entry := range entries {
					if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
						continue
					}
					if info, infoErr := entry.Info(); infoErr == nil && info.ModTime().Before(cutoff) {
						_ = os.Remove(filepath.Join(logsDir, entry.Name()))
						buildID := strings.TrimSuffix(entry.Name(), ".log")
						delete(s.logs, buildID)
						delete(s.logTimes, buildID)
					}
				}
			}
		}
	}

	return nil
}

func inferLogLevel(line string) string {
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "fatal"):
		return "error"
	case strings.Contains(lower, "warn"):
		return "warn"
	default:
		return "info"
	}
}
