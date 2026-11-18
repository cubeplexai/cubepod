package events

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	webhooksFileName    = "webhooks.enc.json"
	eventsFileName      = "events.json"
	deadLettersFileName = "dead_letters.json"
	kekFileName         = ".kek"
)

type encryptedEnvelope struct {
	Version    int    `json:"version"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

type webhookDiskState struct {
	Webhooks []StoredWebhook `json:"webhooks"`
}

type eventDiskState struct {
	Events []StoredEvent `json:"events"`
}

type deadLetterDiskState struct {
	DeadLetters []StoredDeadLetter `json:"deadLetters"`
}

// FileBackend persists webhooks (AES-256-GCM encrypted), lifecycle events, and dead letters
// as JSON files under a node-local directory. It is used for local Docker development and
// single-replica gateways.
type FileBackend struct {
	dir         string
	key         []byte
	mu          sync.Mutex
	eventsMu    sync.Mutex
	webhooks    map[string]StoredWebhook
	deadLetters map[string]StoredDeadLetter
}

// NewFileBackend initializes a file backend under dir. When encryptionKey is empty a local
// 32-byte KEK is generated and stored at <dir>/.kek (mode 0600).
func NewFileBackend(dir string, encryptionKey []byte) (*FileBackend, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("events dir is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create events dir: %w", err)
	}
	key, err := resolveKEK(dir, encryptionKey)
	if err != nil {
		return nil, err
	}
	return &FileBackend{
		dir:         dir,
		key:         key,
		webhooks:    make(map[string]StoredWebhook),
		deadLetters: make(map[string]StoredDeadLetter),
	}, nil
}

// Name implements Backend.
func (b *FileBackend) Name() string { return "file" }

// Shared implements Backend; local files are node-local.
func (b *FileBackend) Shared() bool { return false }

// LoadState reads webhooks.enc.json, events.json, and dead_letters.json.
func (b *FileBackend) LoadState(_ context.Context) (State, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var st State
	whPath := filepath.Join(b.dir, webhooksFileName)
	if raw, err := os.ReadFile(whPath); err == nil {
		var env encryptedEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return State{}, err
		}
		nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
		if err != nil {
			return State{}, err
		}
		ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
		if err != nil {
			return State{}, err
		}
		gcm, err := newGCM(b.key)
		if err != nil {
			return State{}, err
		}
		plain, err := gcm.Open(nil, nonce, ct, nil)
		if err != nil {
			return State{}, fmt.Errorf("decrypt webhooks store: %w", err)
		}
		var disk webhookDiskState
		if err := json.Unmarshal(plain, &disk); err != nil {
			return State{}, err
		}
		b.webhooks = make(map[string]StoredWebhook, len(disk.Webhooks))
		for _, w := range disk.Webhooks {
			b.webhooks[w.ID] = w
		}
		st.Webhooks = disk.Webhooks
	} else if !os.IsNotExist(err) {
		return State{}, err
	}

	evPath := filepath.Join(b.dir, eventsFileName)
	if raw, err := os.ReadFile(evPath); err == nil {
		var disk eventDiskState
		if err := json.Unmarshal(raw, &disk); err != nil {
			return State{}, err
		}
		st.Events = disk.Events
	} else if !os.IsNotExist(err) {
		return State{}, err
	}

	dlPath := filepath.Join(b.dir, deadLettersFileName)
	if raw, err := os.ReadFile(dlPath); err == nil {
		var disk deadLetterDiskState
		if err := json.Unmarshal(raw, &disk); err != nil {
			return State{}, err
		}
		b.deadLetters = make(map[string]StoredDeadLetter, len(disk.DeadLetters))
		for _, d := range disk.DeadLetters {
			b.deadLetters[d.ID] = d
		}
		st.DeadLetters = disk.DeadLetters
	} else if !os.IsNotExist(err) {
		return State{}, err
	}
	return st, nil
}

// PutWebhook upserts a webhook and rewrites the encrypted webhooks file.
func (b *FileBackend) PutWebhook(_ context.Context, rec StoredWebhook) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	prev, had := b.webhooks[rec.ID]
	b.webhooks[rec.ID] = rec
	if err := b.saveWebhooksLocked(); err != nil {
		if had {
			b.webhooks[rec.ID] = prev
		} else {
			delete(b.webhooks, rec.ID)
		}
		return err
	}
	return nil
}

// DeleteWebhook removes a webhook and its dead letters.
func (b *FileBackend) DeleteWebhook(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	prev, had := b.webhooks[id]
	if !had {
		return nil
	}
	delete(b.webhooks, id)
	if err := b.saveWebhooksLocked(); err != nil {
		b.webhooks[id] = prev
		return err
	}
	removed := false
	for dlID, d := range b.deadLetters {
		if d.WebhookID == id {
			delete(b.deadLetters, dlID)
			removed = true
		}
	}
	if removed {
		if err := b.saveDeadLettersLocked(); err != nil {
			slog.Warn("failed to persist dead letters after webhook deletion", "webhookID", id, "err", err)
		}
	}
	return nil
}

// UpdateWebhookStatus records LastDeliveryTime and ConsecutiveFailures for a webhook.
func (b *FileBackend) UpdateWebhookStatus(_ context.Context, id string, status WebhookStatus) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec, ok := b.webhooks[id]
	if !ok {
		return nil
	}
	rec.LastDeliveryTime = status.LastDeliveryTime
	rec.ConsecutiveFailures = status.ConsecutiveFailures
	b.webhooks[id] = rec
	return b.saveWebhooksLocked()
}

// RecordEvent is a no-op: the file backend persists the local ring via SaveEvents.
func (b *FileBackend) RecordEvent(context.Context, StoredEvent) error { return nil }

// SaveEvents atomically rewrites events.json with the local bounded ring.
func (b *FileBackend) SaveEvents(_ context.Context, events []StoredEvent) error {
	b.eventsMu.Lock()
	defer b.eventsMu.Unlock()
	data, err := json.Marshal(eventDiskState{Events: events})
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(b.dir, eventsFileName), data, 0o600)
}

// QueryEvents returns nil: file state is not shared across replicas.
func (b *FileBackend) QueryEvents(context.Context, string, string) ([]StoredEvent, error) {
	return nil, nil
}

// PutDeadLetter upserts a dead letter and rewrites dead_letters.json.
func (b *FileBackend) PutDeadLetter(_ context.Context, rec StoredDeadLetter) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.webhooks[rec.WebhookID]; !ok {
		return nil
	}
	b.deadLetters[rec.ID] = rec
	return b.saveDeadLettersLocked()
}

// DeleteDeadLetter removes a dead letter and rewrites dead_letters.json.
func (b *FileBackend) DeleteDeadLetter(_ context.Context, rec StoredDeadLetter) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.deadLetters[rec.ID]; !ok {
		return nil
	}
	delete(b.deadLetters, rec.ID)
	return b.saveDeadLettersLocked()
}

func (b *FileBackend) saveWebhooksLocked() error {
	disk := webhookDiskState{Webhooks: make([]StoredWebhook, 0, len(b.webhooks))}
	for _, w := range b.webhooks {
		disk.Webhooks = append(disk.Webhooks, w)
	}
	sort.Slice(disk.Webhooks, func(i, j int) bool { return disk.Webhooks[i].ID < disk.Webhooks[j].ID })
	plain, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	gcm, err := newGCM(b.key)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := gcm.Seal(nil, nonce, plain, nil)
	data, err := json.Marshal(encryptedEnvelope{
		Version:    1,
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	})
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(b.dir, webhooksFileName), data, 0o600)
}

func (b *FileBackend) saveDeadLettersLocked() error {
	disk := deadLetterDiskState{DeadLetters: make([]StoredDeadLetter, 0, len(b.deadLetters))}
	for _, d := range b.deadLetters {
		disk.DeadLetters = append(disk.DeadLetters, d)
	}
	sortDeadLetters(disk.DeadLetters)
	data, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(b.dir, deadLettersFileName), data, 0o600)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func resolveKEK(dir string, explicit []byte) ([]byte, error) {
	if len(explicit) > 0 {
		if len(explicit) != 32 {
			return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(explicit))
		}
		cp := make([]byte, 32)
		copy(cp, explicit)
		return cp, nil
	}
	kekPath := filepath.Join(dir, kekFileName)
	raw, err := os.ReadFile(kekPath)
	if err == nil {
		decoded, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil || len(decoded) != 32 {
			return nil, fmt.Errorf("corrupt local KEK at %s", kekPath)
		}
		return decoded, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	tmp := kekPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(encoded+"\n"), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, kekPath); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	slog.Warn("CUBEPOD_EVENTS_KEY unset; generated local webhook encryption key", "path", kekPath)
	return key, nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
