package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cubeplexai/cubepod/pkg/events"
)

// E2BEventsConfig configures the lifecycle events and signed webhook manager.
type E2BEventsConfig struct {
	Dir             string
	EncryptionKey   []byte
	AllowPrivateIPs bool
}

// ConfigureEvents initializes (or reconfigures) the gateway events and webhook manager.
func (s *Server) ConfigureEvents(cfg E2BEventsConfig) error {
	mgr, err := events.Open(cfg.Dir, events.Config{
		EncryptionKey:   cfg.EncryptionKey,
		AllowPrivateIPs: cfg.AllowPrivateIPs,
	})
	if err != nil {
		return err
	}
	prev := s.events.Swap(mgr)
	if prev != nil {
		prev.Close()
	}
	return nil
}

// CloseEvents flushes and stops the background webhook delivery worker.
func (s *Server) CloseEvents() {
	if mgr := s.events.Swap(nil); mgr != nil {
		mgr.Close()
	}
}

func (s *Server) requireEventsManager(w http.ResponseWriter) (*events.Manager, bool) {
	mgr := s.events.Load()
	if mgr == nil {
		writeError(w, http.StatusNotImplemented, "events and webhooks are not configured")
		return nil, false
	}
	return mgr, true
}

func (s *Server) emitSandboxEvent(tenantHash, eventType string, rec *SandboxRecord, data *events.EventData) {
	mgr := s.events.Load()
	if mgr == nil || rec == nil || tenantHash == "" {
		return
	}
	mgr.Emit(tenantHash, eventType, rec.SandboxID, rec.TemplateID, "", "", data)
}

func executionDataForRecord(rec *SandboxRecord) *events.ExecutionData {
	if rec == nil {
		return nil
	}
	durMs := time.Since(rec.StartedAt).Milliseconds()
	if durMs < 0 {
		durMs = 0
	}
	return &events.ExecutionData{
		StartedAt:     rec.StartedAt.UTC().Format(time.RFC3339),
		VCPUCount:     rec.CPUCount,
		MemoryMB:      rec.MemoryMB,
		ExecutionTime: durMs,
	}
}

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	mgr, ok := s.requireEventsManager(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req events.CreateWebhookInput
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	res, err := mgr.CreateWebhook(key, req)
	if err != nil {
		writeEventsError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(res)
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	mgr, ok := s.requireEventsManager(w)
	if !ok {
		return
	}
	items, err := mgr.ListWebhooks(key)
	if err != nil {
		writeEventsError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

func (s *Server) handleGetWebhook(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	mgr, ok := s.requireEventsManager(w)
	if !ok {
		return
	}
	item, err := mgr.GetWebhook(key, r.PathValue("webhookID"))
	if err != nil {
		writeEventsError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(item)
}

func (s *Server) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	mgr, ok := s.requireEventsManager(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req events.UpdateWebhookInput
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	res, err := mgr.UpdateWebhook(key, r.PathValue("webhookID"), req)
	if err != nil {
		writeEventsError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	mgr, ok := s.requireEventsManager(w)
	if !ok {
		return
	}
	if err := mgr.DeleteWebhook(key, r.PathValue("webhookID")); err != nil {
		writeEventsError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	s.listEventsCommon(w, r, strings.TrimSpace(r.URL.Query().Get("sandboxID")))
}

func (s *Server) handleListSandboxEvents(w http.ResponseWriter, r *http.Request) {
	s.listEventsCommon(w, r, r.PathValue("sandboxID"))
}

func (s *Server) listEventsCommon(w http.ResponseWriter, r *http.Request, sandboxID string) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	mgr, ok := s.requireEventsManager(w)
	if !ok {
		return
	}
	limit := events.DefaultListLimit
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed <= 0 || parsed > events.DefaultListLimit {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = parsed
	}
	eventType := strings.TrimSpace(r.URL.Query().Get("eventType"))
	nextToken := strings.TrimSpace(r.URL.Query().Get("nextToken"))
	items, next, err := mgr.ListEvents(key, sandboxID, eventType, limit, nextToken)
	if err != nil {
		writeEventsError(w, err)
		return
	}
	if next != "" {
		w.Header().Set("X-Next-Token", next)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

func writeEventsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, events.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "API key is required")
	case errors.Is(err, events.ErrInvalidWebhook):
		writeError(w, http.StatusBadRequest, "invalid webhook configuration")
	case errors.Is(err, events.ErrInvalidCursor):
		writeError(w, http.StatusBadRequest, "invalid pagination cursor")
	case errors.Is(err, events.ErrNotFound):
		writeError(w, http.StatusNotFound, "webhook not found")
	case errors.Is(err, events.ErrLimitExceeded):
		writeError(w, http.StatusTooManyRequests, "tenant webhook limit exceeded")
	default:
		writeError(w, http.StatusInternalServerError, "events operation failed")
	}
}
