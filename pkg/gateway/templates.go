package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

var hex64Regex = regexp.MustCompile(`^[0-9a-f]{64}$`)

const defaultTemplateCreatedAt = "2026-11-12T00:00:00Z"

// BuildStatusReason describes the error reason for a failed template build.
type BuildStatusReason struct {
	Message    string                     `json:"message"`
	Step       string                     `json:"step,omitempty"`
	LogEntries []controller.BuildLogEntry `json:"logEntries,omitempty"`
}

// E2BTemplateBuild represents a build entry in the template response.
type E2BTemplateBuild struct {
	BuildID     string             `json:"buildID"`
	TemplateID  string             `json:"templateID,omitempty"`
	Status      string             `json:"status"`
	CreatedAt   string             `json:"createdAt"`
	UpdatedAt   string             `json:"updatedAt"`
	CPUCount    int                `json:"cpuCount"`
	MemoryMB    int                `json:"memoryMB"`
	DiskSizeMB  int                `json:"diskSizeMB"`
	EnvdVersion string             `json:"envdVersion"`
	Reason      *BuildStatusReason `json:"reason,omitempty"`
}

// E2BTemplateResponse represents an E2B-compatible template object.
type E2BTemplateResponse struct {
	TemplateID    string             `json:"templateID"`
	BuildID       string             `json:"buildID"`
	CPUCount      int                `json:"cpuCount"`
	MemoryMB      int                `json:"memoryMB"`
	DiskSizeMB    int                `json:"diskSizeMB"`
	Public        bool               `json:"public"`
	Aliases       []string           `json:"aliases"`
	Names         []string           `json:"names"`
	CreatedAt     string             `json:"createdAt"`
	UpdatedAt     string             `json:"updatedAt"`
	CreatedBy     *string            `json:"createdBy"`
	LastSpawnedAt *string            `json:"lastSpawnedAt"`
	SpawnCount    int                `json:"spawnCount"`
	BuildCount    int                `json:"buildCount"`
	EnvdVersion   string             `json:"envdVersion"`
	BuildStatus   string             `json:"buildStatus"`
	Builds        []E2BTemplateBuild `json:"builds,omitempty"`
}

// TemplateTagInfo represents a single tag entry returned by GET /templates/{templateID}/tags.
type TemplateTagInfo struct {
	Tag     string `json:"tag"`
	BuildID string `json:"buildID"`
}

type buildStatusResponse struct {
	Logs       []string                   `json:"logs"`
	LogEntries []controller.BuildLogEntry `json:"logEntries"`
	TemplateID string                     `json:"templateID"`
	BuildID    string                     `json:"buildID"`
	Status     string                     `json:"status"`
	Reason     *BuildStatusReason         `json:"reason,omitempty"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    status,
		"message": msg,
	})
}

func (s *Server) syncTemplates(ctx context.Context) {
	if s.reconciler != nil && s.reconciler.KubeClient() != nil {
		_ = s.reconciler.SyncTemplatesFromKube(ctx)
	}
}

func (s *Server) gatewayHTTPClient() *http.Client {
	s.mu.RLock()
	c := s.httpClient
	s.mu.RUnlock()
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (s *Server) toE2BTemplateResponse(tpl v1alpha1.PodTemplate, includeBuilds bool) E2BTemplateResponse {
	templateID := tpl.Status.TemplateID
	if templateID == "" {
		templateID = "tpl_" + controller.Slugify(tpl.Metadata.Name)
	}

	buildID := tpl.Status.LatestBuildID
	if buildID == "" {
		buildID = tpl.Status.ReadyBuildID
	}
	if buildID == "" {
		buildID = "bld_" + controller.Slugify(tpl.Metadata.Name)
	}

	cpu := int(tpl.Spec.CPUCount)
	if cpu <= 0 {
		cpu = 2
	}
	mem := int(tpl.Spec.MemoryMB)
	if mem <= 0 {
		mem = 2048
	}
	disk := int(tpl.Spec.DiskSizeMB)
	if disk <= 0 {
		disk = 10240
	}

	names := make([]string, 0, len(tpl.Spec.Names))
	if len(tpl.Spec.Names) > 0 {
		names = append(names, tpl.Spec.Names...)
	} else {
		names = append(names, tpl.Metadata.Name)
	}

	var aliases []string
	for _, n := range names {
		if !strings.Contains(n, ":") {
			aliases = append(aliases, n)
		}
	}
	if len(aliases) == 0 {
		aliases = []string{tpl.Metadata.Name}
	}

	createdAt := tpl.Status.CreatedAt
	if createdAt == "" {
		createdAt = defaultTemplateCreatedAt
	}
	updatedAt := tpl.Status.UpdatedAt
	if updatedAt == "" {
		updatedAt = createdAt
	}

	var lastSpawnedAt *string
	if tpl.Status.LastSpawnedAt != "" {
		ts := tpl.Status.LastSpawnedAt
		lastSpawnedAt = &ts
	}

	buildCount := int(tpl.Status.BuildCount)
	if buildCount <= 0 {
		buildCount = max(1, len(tpl.Status.Builds))
	}

	crStatus := tpl.Status.BuildStatus
	if crStatus == "" {
		if tpl.Status.ReadyImage != "" {
			crStatus = v1alpha1.BuildStatusReady
		} else {
			crStatus = v1alpha1.BuildStatusWaiting
		}
	}
	wireStatus := controller.WireBuildStatus(crStatus)

	resp := E2BTemplateResponse{
		TemplateID:    templateID,
		BuildID:       buildID,
		CPUCount:      cpu,
		MemoryMB:      mem,
		DiskSizeMB:    disk,
		Public:        tpl.Spec.Public || tpl.Status.Public,
		Aliases:       aliases,
		Names:         names,
		CreatedAt:     createdAt,
		UpdatedAt:     updatedAt,
		CreatedBy:     nil,
		LastSpawnedAt: lastSpawnedAt,
		SpawnCount:    int(tpl.Status.SpawnCount),
		BuildCount:    buildCount,
		EnvdVersion:   "cubepod-podd",
		BuildStatus:   wireStatus,
	}

	if includeBuilds {
		if len(tpl.Status.Builds) > 0 {
			resp.Builds = make([]E2BTemplateBuild, 0, len(tpl.Status.Builds))
			for _, rec := range tpl.Status.Builds {
				recStatus := rec.Status
				if rec.BuildID == tpl.Status.LatestBuildID && tpl.Status.BuildStatus != "" {
					recStatus = tpl.Status.BuildStatus
				}
				if recStatus == "" {
					recStatus = crStatus
				}
				recWire := controller.WireBuildStatus(recStatus)
				recCreated := rec.CreatedAt
				if recCreated == "" {
					recCreated = createdAt
				}
				recUpdated := rec.UpdatedAt
				if recUpdated == "" {
					recUpdated = updatedAt
				}
				recCPU := int(rec.CPUCount)
				if recCPU <= 0 {
					recCPU = cpu
				}
				recMem := int(rec.MemoryMB)
				if recMem <= 0 {
					recMem = mem
				}
				var recReason *BuildStatusReason
				if recWire == v1alpha1.BuildStatusError {
					msg := rec.Reason
					if msg == "" {
						msg = tpl.Status.Reason
					}
					if msg == "" {
						msg = "build failed"
					}
					step := rec.FailedStep
					if step == "" {
						step = tpl.Status.FailedStep
					}
					recReason = &BuildStatusReason{
						Message: msg,
						Step:    step,
					}
				}
				resp.Builds = append(resp.Builds, E2BTemplateBuild{
					BuildID:     rec.BuildID,
					TemplateID:  templateID,
					Status:      recWire,
					CreatedAt:   recCreated,
					UpdatedAt:   recUpdated,
					CPUCount:    recCPU,
					MemoryMB:    recMem,
					DiskSizeMB:  disk,
					EnvdVersion: "cubepod-podd",
					Reason:      recReason,
				})
			}
		} else {
			var recReason *BuildStatusReason
			if wireStatus == v1alpha1.BuildStatusError {
				msg := tpl.Status.Reason
				if msg == "" {
					msg = "build failed"
				}
				recReason = &BuildStatusReason{
					Message: msg,
					Step:    tpl.Status.FailedStep,
				}
			}
			resp.Builds = []E2BTemplateBuild{
				{
					BuildID:     buildID,
					TemplateID:  templateID,
					Status:      wireStatus,
					CreatedAt:   createdAt,
					UpdatedAt:   updatedAt,
					CPUCount:    cpu,
					MemoryMB:    mem,
					DiskSizeMB:  disk,
					EnvdVersion: "cubepod-podd",
					Reason:      recReason,
				},
			}
		}
	}

	return resp
}

func (s *Server) listPodTemplatesSorted(ctx context.Context) ([]v1alpha1.PodTemplate, error) {
	if s.reconciler != nil {
		s.syncTemplates(ctx)
		_ = s.reconciler.EnsureSeededTemplates(ctx)
	}

	var list []v1alpha1.PodTemplate
	if s.reconciler != nil && s.reconciler.KubeClient() != nil {
		var err error
		list, err = s.reconciler.KubeClient().ListPodTemplates(ctx, "")
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].Metadata.Name < list[j].Metadata.Name
	})
	return list, nil
}

func (s *Server) handleE2BListTemplates(w http.ResponseWriter, r *http.Request) {
	_ = r.URL.Query().Get("teamID") // teamID is ignored

	list, err := s.listPodTemplatesSorted(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list templates: "+err.Error())
		return
	}

	if len(list) > 5000 {
		writeError(w, http.StatusBadRequest, "template count exceeds 5000 limit")
		return
	}

	items := make([]E2BTemplateResponse, 0, len(list))
	for _, tpl := range list {
		items = append(items, s.toE2BTemplateResponse(tpl, false))
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(items)
}

func (s *Server) handleE2BListTemplatesV2(w http.ResponseWriter, r *http.Request) {
	_ = r.URL.Query().Get("teamID") // teamID is ignored

	limit := 100
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if parsed > 100 {
			parsed = 100
		}
		limit = parsed
	}
	nextToken := r.URL.Query().Get("nextToken")

	list, err := s.listPodTemplatesSorted(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list templates: "+err.Error())
		return
	}

	filtered := list
	if nextToken != "" {
		filtered = make([]v1alpha1.PodTemplate, 0, len(list))
		for _, tpl := range list {
			if tpl.Metadata.Name > nextToken {
				filtered = append(filtered, tpl)
			}
		}
	}

	var nextCursor string
	if len(filtered) > limit {
		filtered = filtered[:limit]
		nextCursor = filtered[len(filtered)-1].Metadata.Name
	}

	items := make([]E2BTemplateResponse, 0, len(filtered))
	for _, tpl := range filtered {
		items = append(items, s.toE2BTemplateResponse(tpl, false))
	}

	if nextCursor != "" {
		w.Header().Set("X-Next-Token", nextCursor)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(items)
}

func (s *Server) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	templateID := r.PathValue("templateID")
	if templateID == "" {
		writeError(w, http.StatusBadRequest, "templateID is required")
		return
	}
	s.syncTemplates(r.Context())
	tpl, ok := s.reconciler.GetPodTemplate(templateID)
	if !ok {
		writeError(w, http.StatusNotFound, "template not found")
		return
	}
	resp := s.toE2BTemplateResponse(*tpl, true)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleGetTemplateByAlias(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	if alias == "" {
		writeError(w, http.StatusBadRequest, "alias is required")
		return
	}
	s.syncTemplates(r.Context())
	tpl, ok := s.reconciler.GetPodTemplate(alias)
	if !ok {
		writeError(w, http.StatusNotFound, "template not found")
		return
	}
	resp := s.toE2BTemplateResponse(*tpl, true)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	templateID := r.PathValue("templateID")
	if templateID == "" {
		writeError(w, http.StatusBadRequest, "templateID is required")
		return
	}
	s.syncTemplates(r.Context())
	force := r.URL.Query().Get("force") == "true"
	err := s.reconciler.DeleteTemplate(r.Context(), templateID, force)
	if err != nil {
		if errors.Is(err, controller.ErrTemplateNotFound) {
			writeError(w, http.StatusNotFound, "template not found")
			return
		}
		if errors.Is(err, controller.ErrTemplateConflict) || strings.Contains(err.Error(), "referenced") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type patchTemplateRequest struct {
	Public bool `json:"public"`
}

func (s *Server) handlePatchTemplate(w http.ResponseWriter, r *http.Request) {
	templateID := r.PathValue("templateID")
	var req patchTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	s.syncTemplates(r.Context())
	if err := s.reconciler.SetTemplatePublic(r.Context(), templateID, req.Public); err != nil {
		if errors.Is(err, controller.ErrTemplateNotFound) {
			writeError(w, http.StatusNotFound, "template not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tpl, _ := s.reconciler.GetPodTemplate(templateID)
	if tpl == nil {
		writeError(w, http.StatusNotFound, "template not found")
		return
	}
	resp := s.toE2BTemplateResponse(*tpl, true)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handlePatchTemplateV2(w http.ResponseWriter, r *http.Request) {
	templateID := r.PathValue("templateID")
	var req patchTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	s.syncTemplates(r.Context())
	if err := s.reconciler.SetTemplatePublic(r.Context(), templateID, req.Public); err != nil {
		if errors.Is(err, controller.ErrTemplateNotFound) {
			writeError(w, http.StatusNotFound, "template not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tpl, _ := s.reconciler.GetPodTemplate(templateID)
	if tpl == nil {
		writeError(w, http.StatusNotFound, "template not found")
		return
	}
	names := tpl.Spec.Names
	if names == nil {
		names = []string{tpl.Metadata.Name}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"names": names})
}

func (s *Server) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	var req controller.CreateTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	s.syncTemplates(r.Context())
	idempotencyKey := r.Header.Get("Idempotency-Key")
	resp, err := s.reconciler.CreateTemplate(r.Context(), req, idempotencyKey)
	if err != nil {
		if errors.Is(err, controller.ErrIdempotencyMismatch) {
			writeError(w, http.StatusConflict, "idempotency_request_mismatch")
			return
		}
		if errors.Is(err, controller.ErrTemplateConflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted) // 202
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleStartBuild(w http.ResponseWriter, r *http.Request) {
	templateID := r.PathValue("templateID")
	buildID := r.PathValue("buildID")
	if templateID == "" || buildID == "" {
		writeError(w, http.StatusBadRequest, "templateID and buildID are required")
		return
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body: "+err.Error())
		return
	}
	var req controller.StartBuildRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if ctrlURL := s.ControllerURL(); ctrlURL != "" {
		target := fmt.Sprintf("%s/internal/v1/template-builds/%s/%s/start",
			ctrlURL, url.PathEscape(templateID), url.PathEscape(buildID))
		fwdReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(bodyBytes))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to construct controller request: "+err.Error())
			return
		}
		fwdReq.Header.Set("Content-Type", "application/json")
		resp, err := s.gatewayHTTPClient().Do(fwdReq)
		if err != nil {
			writeError(w, http.StatusBadGateway, "controller build request failed: "+err.Error())
			return
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(resp.StatusCode)
		if len(respBody) > 0 {
			_, _ = w.Write(respBody)
		} else if resp.StatusCode == http.StatusAccepted {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "accepted"})
		}
		return
	}

	s.syncTemplates(r.Context())
	err = s.reconciler.StartBuildAsync(context.WithoutCancel(r.Context()), templateID, buildID, req)
	if err != nil {
		if errors.Is(err, controller.ErrTemplateBuildDisabled) || strings.HasPrefix(err.Error(), "501") {
			writeError(w, http.StatusNotImplemented, "template build is not enabled")
			return
		}
		if errors.Is(err, controller.ErrTemplateNotFound) || strings.HasPrefix(err.Error(), "404") {
			writeError(w, http.StatusNotFound, "template not found")
			return
		}
		if errors.Is(err, controller.ErrTemplateNotReady) || errors.Is(err, controller.ErrTemplateConflict) || strings.HasPrefix(err.Error(), "409") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if strings.HasPrefix(err.Error(), "400") || strings.Contains(err.Error(), "exactly one") || strings.Contains(err.Error(), "unknown step type") || strings.Contains(err.Error(), "ARG") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "accepted"})
}

func findBuildOnTemplate(tpl *v1alpha1.PodTemplate, buildID string) (status, reason, step string, found bool) {
	if tpl == nil || buildID == "" {
		return "", "", "", false
	}
	var matched *v1alpha1.PodTemplateBuildRecord
	for i := range tpl.Status.Builds {
		if tpl.Status.Builds[i].BuildID == buildID {
			matched = &tpl.Status.Builds[i]
			break
		}
	}

	if buildID == tpl.Status.LatestBuildID {
		st := tpl.Status.BuildStatus
		if st == "" && matched != nil {
			st = matched.Status
		}
		if st == "" {
			if tpl.Status.ReadyImage != "" {
				st = v1alpha1.BuildStatusReady
			} else {
				st = v1alpha1.BuildStatusWaiting
			}
		}
		r := tpl.Status.Reason
		s := tpl.Status.FailedStep
		if matched != nil {
			if matched.Reason != "" {
				r = matched.Reason
			}
			if matched.FailedStep != "" {
				s = matched.FailedStep
			}
		}
		return st, r, s, true
	}

	if matched != nil {
		st := matched.Status
		if st == "" {
			st = v1alpha1.BuildStatusWaiting
		}
		return st, matched.Reason, matched.FailedStep, true
	}

	if buildID == tpl.Status.ReadyBuildID && tpl.Status.ReadyBuildID != "" {
		return v1alpha1.BuildStatusReady, "", "", true
	}

	return "", "", "", false
}

func (s *Server) fetchBuildLogs(ctx context.Context, buildID string, offset, limit int, direction, level string) ([]controller.BuildLogEntry, error) {
	if ctrlURL := s.ControllerURL(); ctrlURL != "" {
		q := url.Values{}
		q.Set("cursor", strconv.Itoa(offset))
		q.Set("limit", strconv.Itoa(limit))
		if direction != "" {
			q.Set("direction", direction)
		}
		if level != "" {
			q.Set("level", level)
		}
		target := fmt.Sprintf("%s/internal/v1/template-builds/%s/logs?%s", ctrlURL, url.PathEscape(buildID), q.Encode())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		resp, err := s.gatewayHTTPClient().Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return []controller.BuildLogEntry{}, nil
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("controller logs returned %d: %s", resp.StatusCode, string(body))
		}
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		var wrapped struct {
			Logs []controller.BuildLogEntry `json:"logs"`
		}
		if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Logs != nil {
			return wrapped.Logs, nil
		}
		var arr []controller.BuildLogEntry
		if err := json.Unmarshal(raw, &arr); err == nil && arr != nil {
			return arr, nil
		}
		return []controller.BuildLogEntry{}, nil
	}

	if s.reconciler == nil || s.reconciler.ContextStore() == nil {
		return []controller.BuildLogEntry{}, nil
	}
	store := s.reconciler.ContextStore()
	if strings.EqualFold(direction, "backward") {
		all, err := store.ReadLogs(buildID, 0, 0, level)
		if err != nil {
			return nil, err
		}
		if len(all) == 0 {
			return []controller.BuildLogEntry{}, nil
		}
		end := len(all)
		if offset > 0 && offset < end {
			end = offset
		}
		start := 0
		if limit > 0 && end-limit > 0 {
			start = end - limit
		}
		out := make([]controller.BuildLogEntry, end-start)
		copy(out, all[start:end])
		return out, nil
	}

	entries, err := store.ReadLogs(buildID, offset, limit, level)
	if err != nil {
		return nil, err
	}
	if entries == nil {
		return []controller.BuildLogEntry{}, nil
	}
	return entries, nil
}

func (s *Server) handleGetBuildStatus(w http.ResponseWriter, r *http.Request) {
	templateID := r.PathValue("templateID")
	buildID := r.PathValue("buildID")
	if templateID == "" || buildID == "" {
		writeError(w, http.StatusBadRequest, "templateID and buildID are required")
		return
	}

	offset := 0
	if offsetStr := r.URL.Query().Get("logsOffset"); offsetStr != "" {
		parsed, err := strconv.Atoi(offsetStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "logsOffset must be an integer")
			return
		}
		if parsed > 0 {
			offset = parsed
		}
	}
	limit := 100
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if parsed > 100 {
			parsed = 100
		}
		limit = parsed
	}
	level := r.URL.Query().Get("level")

	s.syncTemplates(r.Context())
	tpl, ok := s.reconciler.GetPodTemplate(templateID)
	if !ok {
		writeError(w, http.StatusNotFound, "template not found")
		return
	}

	crStatus, recReason, recStep, found := findBuildOnTemplate(tpl, buildID)
	if !found {
		writeError(w, http.StatusNotFound, "build not found")
		return
	}
	wireStatus := controller.WireBuildStatus(crStatus)

	entries, err := s.fetchBuildLogs(r.Context(), buildID, offset, limit, "", level)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch build logs: "+err.Error())
		return
	}
	logs := make([]string, 0, len(entries))
	for _, entry := range entries {
		logs = append(logs, entry.Message)
	}

	tplTemplateID := tpl.Status.TemplateID
	if tplTemplateID == "" {
		tplTemplateID = templateID
	}

	var reason *BuildStatusReason
	if wireStatus == v1alpha1.BuildStatusError {
		if recReason == "" {
			recReason = "build failed"
		}
		reason = &BuildStatusReason{
			Message: recReason,
			Step:    recStep,
		}
	}

	resp := buildStatusResponse{
		Logs:       logs,
		LogEntries: entries,
		TemplateID: tplTemplateID,
		BuildID:    buildID,
		Status:     wireStatus,
		Reason:     reason,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func resolveExternalSchemeAndHost(r *http.Request) (string, string) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if xfp := r.Header.Get("X-Forwarded-Proto"); xfp != "" {
		first := strings.TrimSpace(strings.Split(xfp, ",")[0])
		if first != "" {
			scheme = strings.ToLower(first)
		}
	}

	host := r.Host
	if xfh := r.Header.Get("X-Forwarded-Host"); xfh != "" {
		first := strings.TrimSpace(strings.Split(xfh, ",")[0])
		if first != "" {
			host = first
		}
	}
	if host == "" {
		host = "localhost"
	}
	return scheme, host
}

func (s *Server) handleGetTemplateFiles(w http.ResponseWriter, r *http.Request) {
	if !s.TemplateBuildEnabled() {
		writeError(w, http.StatusNotImplemented, "template build is not enabled")
		return
	}
	templateID := r.PathValue("templateID")
	hash := r.PathValue("hash")
	if templateID == "" {
		writeError(w, http.StatusBadRequest, "templateID is required")
		return
	}
	if !hex64Regex.MatchString(hash) {
		writeError(w, http.StatusBadRequest, "hash must be 64 lowercase hex characters")
		return
	}

	s.syncTemplates(r.Context())
	if _, ok := s.reconciler.GetPodTemplate(templateID); !ok {
		writeError(w, http.StatusNotFound, "template not found")
		return
	}

	var present bool
	if ctrlURL := s.ControllerURL(); ctrlURL != "" {
		target := fmt.Sprintf("%s/internal/v1/template-builds/context/%s", ctrlURL, url.PathEscape(hash))
		req, err := http.NewRequestWithContext(r.Context(), http.MethodHead, target, nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create context check request: "+err.Error())
			return
		}
		resp, err := s.gatewayHTTPClient().Do(req)
		if err != nil {
			writeError(w, http.StatusBadGateway, "controller context check failed: "+err.Error())
			return
		}
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK, http.StatusNoContent:
			present = true
		case http.StatusNotFound:
			present = false
		default:
			writeError(w, http.StatusBadGateway, fmt.Sprintf("controller context check returned status %d", resp.StatusCode))
			return
		}
	} else if s.reconciler != nil && s.reconciler.ContextStore() != nil {
		present = s.reconciler.ContextStore().IsPresent(hash)
	}

	scheme, host := resolveExternalSchemeAndHost(r)
	uploadURL := fmt.Sprintf("%s://%s/templates/%s/files/%s", scheme, host, url.PathEscape(templateID), hash)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"present": present,
		"url":     uploadURL,
		"headers": map[string]string{},
	})
}

func (s *Server) handlePutTemplateFiles(w http.ResponseWriter, r *http.Request) {
	if !s.TemplateBuildEnabled() {
		writeError(w, http.StatusNotImplemented, "template build is not enabled")
		return
	}
	templateID := r.PathValue("templateID")
	hash := r.PathValue("hash")
	if templateID == "" {
		writeError(w, http.StatusBadRequest, "templateID is required")
		return
	}
	if !hex64Regex.MatchString(hash) {
		writeError(w, http.StatusBadRequest, "hash must be 64 lowercase hex characters")
		return
	}

	s.syncTemplates(r.Context())
	if _, ok := s.reconciler.GetPodTemplate(templateID); !ok {
		writeError(w, http.StatusNotFound, "template not found")
		return
	}

	if ctrlURL := s.ControllerURL(); ctrlURL != "" {
		target := fmt.Sprintf("%s/internal/v1/template-builds/context/%s", ctrlURL, url.PathEscape(hash))
		fwdReq, err := http.NewRequestWithContext(r.Context(), http.MethodPut, target, r.Body)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create context upload request: "+err.Error())
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "" {
			fwdReq.Header.Set("Content-Type", ct)
		}
		if ce := r.Header.Get("Content-Encoding"); ce != "" {
			fwdReq.Header.Set("Content-Encoding", ce)
		}
		uploadClient := &http.Client{Timeout: 10 * time.Minute}
		resp, err := uploadClient.Do(fwdReq)
		if err != nil {
			writeError(w, http.StatusBadGateway, "controller context upload failed: "+err.Error())
			return
		}
		defer resp.Body.Close()

		respBody, _ := io.ReadAll(resp.Body)
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		status := resp.StatusCode
		if status == http.StatusOK || status == http.StatusNoContent {
			status = http.StatusCreated
		}
		w.WriteHeader(status)
		if len(respBody) > 0 {
			_, _ = w.Write(respBody)
		}
		return
	}

	if s.reconciler == nil || s.reconciler.ContextStore() == nil {
		writeError(w, http.StatusInternalServerError, "context store is not configured")
		return
	}

	if err := s.reconciler.ContextStore().Store(hash, r.Body); err != nil {
		switch {
		case errors.Is(err, controller.ErrContextTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		case errors.Is(err, controller.ErrTarMismatch):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, controller.ErrInvalidContextTar), errors.Is(err, controller.ErrPathTraversal):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleGetBuildLogs(w http.ResponseWriter, r *http.Request) {
	if !s.TemplateBuildEnabled() {
		writeError(w, http.StatusNotImplemented, "template build is not enabled")
		return
	}
	templateID := r.PathValue("templateID")
	buildID := r.PathValue("buildID")
	if templateID == "" || buildID == "" {
		writeError(w, http.StatusBadRequest, "templateID and buildID are required")
		return
	}

	cursor := 0
	if cursorStr := r.URL.Query().Get("cursor"); cursorStr != "" {
		parsed, err := strconv.Atoi(cursorStr)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "cursor must be a non-negative integer")
			return
		}
		cursor = parsed
	}
	limit := 100
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if parsed > 100 {
			parsed = 100
		}
		limit = parsed
	}
	direction := r.URL.Query().Get("direction")
	level := r.URL.Query().Get("level")

	s.syncTemplates(r.Context())
	tpl, ok := s.reconciler.GetPodTemplate(templateID)
	if !ok {
		writeError(w, http.StatusNotFound, "template not found")
		return
	}
	if _, _, _, found := findBuildOnTemplate(tpl, buildID); !found {
		writeError(w, http.StatusNotFound, "build not found")
		return
	}

	entries, err := s.fetchBuildLogs(r.Context(), buildID, cursor, limit, direction, level)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to fetch build logs: "+err.Error())
		return
	}
	if entries == nil {
		entries = []controller.BuildLogEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"logs": entries,
	})
}

func extractTagsFromTemplate(tpl *v1alpha1.PodTemplate) []string {
	if tpl == nil {
		return []string{}
	}
	var tags []string
	seen := make(map[string]struct{})
	for _, n := range tpl.Spec.Names {
		if idx := strings.IndexByte(n, ':'); idx >= 0 && idx+1 < len(n) {
			tag := n[idx+1:]
			if _, ok := seen[tag]; !ok {
				seen[tag] = struct{}{}
				tags = append(tags, tag)
			}
		}
	}
	if tags == nil {
		return []string{}
	}
	return tags
}

func (s *Server) handleAssignTemplateTags(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Target string   `json:"target"`
		Tags   []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Target) == "" || len(req.Tags) == 0 {
		writeError(w, http.StatusBadRequest, "target and tags are required")
		return
	}

	s.syncTemplates(r.Context())
	tpl, err := s.reconciler.AssignTemplateTags(r.Context(), req.Target, req.Tags)
	if errors.Is(err, controller.ErrTemplateNotFound) && strings.HasSuffix(req.Target, ":default") {
		tpl, err = s.reconciler.AssignTemplateTags(r.Context(), strings.TrimSuffix(req.Target, ":default"), req.Tags)
	}
	if err != nil {
		if errors.Is(err, controller.ErrTemplateNotFound) {
			writeError(w, http.StatusNotFound, "template not found")
			return
		}
		if errors.Is(err, controller.ErrTemplateConflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tags":  extractTagsFromTemplate(tpl),
		"names": tpl.Spec.Names,
	})
}

func (s *Server) handleDeleteTemplateTags(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string   `json:"name"`
		Target string   `json:"target"`
		Tags   []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	nameOrID := strings.TrimSpace(req.Name)
	if nameOrID == "" {
		nameOrID = strings.TrimSpace(req.Target)
	}
	if nameOrID == "" || len(req.Tags) == 0 {
		writeError(w, http.StatusBadRequest, "name and tags are required")
		return
	}

	s.syncTemplates(r.Context())
	_, err := s.reconciler.DeleteTemplateTags(r.Context(), nameOrID, req.Tags)
	if errors.Is(err, controller.ErrTemplateNotFound) && strings.HasSuffix(nameOrID, ":default") {
		_, err = s.reconciler.DeleteTemplateTags(r.Context(), strings.TrimSuffix(nameOrID, ":default"), req.Tags)
	}
	if err != nil {
		if errors.Is(err, controller.ErrTemplateNotFound) {
			writeError(w, http.StatusNotFound, "template not found")
			return
		}
		if errors.Is(err, controller.ErrTemplateConflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetTemplateTags(w http.ResponseWriter, r *http.Request) {
	templateID := r.PathValue("templateID")
	if templateID == "" {
		writeError(w, http.StatusBadRequest, "templateID is required")
		return
	}
	s.syncTemplates(r.Context())
	tpl, ok := s.reconciler.GetPodTemplate(templateID)
	if !ok {
		writeError(w, http.StatusNotFound, "template not found")
		return
	}

	buildID := tpl.Status.ReadyBuildID
	if buildID == "" {
		buildID = tpl.Status.LatestBuildID
	}
	if buildID == "" {
		buildID = "bld_" + controller.Slugify(tpl.Metadata.Name)
	}

	tags := extractTagsFromTemplate(tpl)
	out := make([]TemplateTagInfo, 0, len(tags))
	for _, tag := range tags {
		out = append(out, TemplateTagInfo{
			Tag:     tag,
			BuildID: buildID,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}
