package secret

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreCRUDPaginationAndEncryption(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}

	st, err := Open(dir, Config{EncryptionKey: key, MaxSecretsPerTenant: 3})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	// Create
	info1, err := st.Create("key-tenant-a", "stripe_api_key", "sk_live_super_secret_123")
	if err != nil {
		t.Fatalf("Create 1 failed: %v", err)
	}
	if !strings.HasPrefix(info1.ID, "sec_") || info1.Version != 1 || info1.Name != "stripe_api_key" {
		t.Fatalf("unexpected info1: %+v", info1)
	}

	// Duplicate name rejected
	if _, err := st.Create("key-tenant-a", "stripe_api_key", "other"); !errors.Is(err, ErrExists) {
		t.Fatalf("expected ErrExists, got %v", err)
	}

	// Plaintext must never appear on disk
	rawDisk, err := os.ReadFile(filepath.Join(dir, secretsFileName))
	if err != nil {
		t.Fatalf("read secrets file: %v", err)
	}
	if strings.Contains(string(rawDisk), "sk_live_super_secret_123") || strings.Contains(string(rawDisk), "stripe_api_key") {
		t.Fatalf("secrets file on disk contains unencrypted plaintext!")
	}

	// Quota enforcement
	if _, err := st.Create("key-tenant-a", "sec_2", "val2"); err != nil {
		t.Fatalf("Create 2 failed: %v", err)
	}
	if _, err := st.Create("key-tenant-a", "sec_3", "val3"); err != nil {
		t.Fatalf("Create 3 failed: %v", err)
	}
	if _, err := st.Create("key-tenant-a", "sec_4", "val4"); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("expected ErrLimitExceeded, got %v", err)
	}

	// Tenant isolation
	if _, err := st.Get("key-tenant-b", info1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-tenant Get, got %v", err)
	}
	if _, err := st.Create("key-tenant-b", "stripe_api_key", "sk_tenant_b"); err != nil {
		t.Fatalf("tenant B create same name failed: %v", err)
	}

	// Pagination
	page1, next, err := st.List("key-tenant-a", 2, "")
	if err != nil || len(page1) != 2 || next == "" {
		t.Fatalf("List page 1 unexpected: len=%d next=%q err=%v", len(page1), next, err)
	}
	page2, next2, err := st.List("key-tenant-a", 2, next)
	if err != nil || len(page2) != 1 || next2 != "" {
		t.Fatalf("List page 2 unexpected: len=%d next=%q err=%v", len(page2), next2, err)
	}

	// Update / Rotate
	upd, err := st.Update("key-tenant-a", info1.ID, "sk_live_rotated_456")
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if upd.Version != 2 || upd.UpdatedAt == nil {
		t.Fatalf("expected version 2 and updatedAt, got %+v", upd)
	}

	// Reopen from disk and verify persistence
	st2, err := Open(dir, Config{EncryptionKey: key})
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	_, val, err := st2.ResolveByName("key-tenant-a", "stripe_api_key")
	if err != nil || val.Raw() != "sk_live_rotated_456" {
		t.Fatalf("ResolveByName after reopen failed: val=%q err=%v", val.Raw(), err)
	}
	if fmt.Sprintf("%s", val) != "[REDACTED]" {
		t.Fatalf("SecretValue.String() leaked secret!")
	}

	// Delete
	if _, err := st2.Delete("key-tenant-a", info1.ID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := st2.Get("key-tenant-a", info1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestStoreValidationAndAutoKEK(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir, Config{})
	if err != nil {
		t.Fatalf("Open with auto KEK failed: %v", err)
	}
	if _, err := st.Create("", "a", "b"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if _, err := st.Create("k", "bad name!", "b"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("expected ErrInvalidName, got %v", err)
	}
	if _, err := st.Create("k", "good", ""); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("expected ErrInvalidValue for empty, got %v", err)
	}
	if _, err := st.Create("k", "good", strings.Repeat("x", MaxSecretValueBytes+1)); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("expected ErrInvalidValue for oversized, got %v", err)
	}
}
