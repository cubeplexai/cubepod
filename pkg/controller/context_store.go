package controller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

var (
	ErrInvalidContextTar = errors.New("400: body is neither valid gzip nor tar archive")
	ErrPathTraversal     = errors.New("400: tar member path escapes context prefix")
	ErrTarMismatch       = errors.New("409: context tar body mismatch for existing hash")
)

// ContextStore manages uploaded template context tar archives.
type ContextStore interface {
	Store(hash string, r io.Reader) error
	IsPresent(hash string) bool
	Unpack(hash, destDir string) error
	Get(hash string) ([]byte, error)
}

// MemoryContextStore is an in-memory implementation of ContextStore.
type MemoryContextStore struct {
	mu      sync.RWMutex
	tars    map[string][]byte
	baseDir string
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
		tars:    make(map[string][]byte),
		baseDir: dir,
	}
}

// Store reads the body, decompresses if gzipped, validates tar headers, and saves it.
func (s *MemoryContextStore) Store(hash string, r io.Reader) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Peek at the first 2 bytes to check for gzip magic 1f 8b
	header := make([]byte, 2)
	n, err := io.ReadFull(r, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: failed to read stream: %v", ErrInvalidContextTar, err)
	}
	if n < 2 {
		return ErrInvalidContextTar
	}

	combinedReader := io.MultiReader(bytes.NewReader(header[:n]), r)
	var tarBytes []byte

	if header[0] == 0x1f && header[1] == 0x8b {
		// Gzip stream: decompress
		gz, err := gzip.NewReader(combinedReader)
		if err != nil {
			return fmt.Errorf("%w: invalid gzip: %v", ErrInvalidContextTar, err)
		}
		defer gz.Close()

		decompressed, err := io.ReadAll(gz)
		if err != nil {
			return fmt.Errorf("%w: failed to decompress gzip stream: %v", ErrInvalidContextTar, err)
		}
		tarBytes = decompressed
	} else {
		// Raw tar stream
		raw, err := io.ReadAll(combinedReader)
		if err != nil {
			return fmt.Errorf("%w: failed to read stream: %v", ErrInvalidContextTar, err)
		}
		tarBytes = raw
	}

	// Validate tar format and inspect entries for path traversal
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

		// Check path traversal
		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(hdr.Name) || filepath.IsAbs(clean) || clean == ".." || filepath.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: member %q", ErrPathTraversal, hdr.Name)
		}
	}

	if !hasEntries && len(tarBytes) < 512 {
		return ErrInvalidContextTar
	}

	// Check if already stored
	if existing, exists := s.tars[hash]; exists {
		if bytes.Equal(existing, tarBytes) {
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

		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) || clean == ".." || filepath.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: %s", ErrPathTraversal, hdr.Name)
		}

		destPath := filepath.Join(targetDir, clean)
		rel, err := filepath.Rel(targetDir, destPath)
		if err != nil || rel == ".." || filepath.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: member %s escapes target directory", ErrPathTraversal, hdr.Name)
		}

		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(destPath, 0755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}

		f, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, hdr.FileInfo().Mode())
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}

	return nil
}
