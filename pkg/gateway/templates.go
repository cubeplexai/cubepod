package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
)

var hex64Regex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// E2BTemplateBuild represents a build entry in the template response.
type E2BTemplateBuild struct {
	BuildID    string `json:"buildID"`
	Status     string `json:"status"`
	TemplateID string `json:"templateID,omitempty"`
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

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    status,
		"message": msg,
	})
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

	createdAt := "2026-11-12T00:00:00Z"

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
		UpdatedAt:     createdAt,
		CreatedBy:     nil,
		LastSpawnedAt: nil,
		SpawnCount:    0,
		BuildCount:    1,
		EnvdVersion:   "cubepod-podd",
		BuildStatus:   wireStatus,
	}

	if includeBuilds {
		resp.Builds = []E2BTemplateBuild{
			{
				BuildID:    buildID,
				Status:     wireStatus,
				TemplateID: templateID,
			},
		}
	}

	return resp
}

func (s *Server) handleE2BListTemplates(w http.ResponseWriter, r *http.Request) {
	_ = r.URL.Query().Get("teamID") // teamID is ignored

	if s.reconciler != nil {
		_ = s.reconciler.EnsureSeededTemplates(r.Context())
	}

	var list []v1alpha1.PodTemplate
	if s.reconciler != nil && s.reconciler.KubeClient() != nil {
		var err error
		list, err = s.reconciler.KubeClient().ListPodTemplates(r.Context(), "")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list templates: "+err.Error())
			return
		}
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

func (s *Server) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	templateID := r.PathValue("templateID")
	if templateID == "" {
		writeError(w, http.StatusBadRequest, "templateID is required")
		return
	}
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "deleted"})
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
	var req controller.StartBuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	err := s.reconciler.StartBuild(r.Context(), templateID, buildID, req)
	if err != nil {
		if errors.Is(err, controller.ErrTemplateBuildDisabled) || strings.HasPrefix(err.Error(), "501") {
			writeError(w, http.StatusNotImplemented, "template build is not enabled")
			return
		}
		if errors.Is(err, controller.ErrTemplateNotFound) || strings.HasPrefix(err.Error(), "404") {
			writeError(w, http.StatusNotFound, "template not found")
			return
		}
		if errors.Is(err, controller.ErrTemplateConflict) || strings.HasPrefix(err.Error(), "409") {
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
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
}

func (s *Server) handleGetBuildStatus(w http.ResponseWriter, r *http.Request) {
	templateID := r.PathValue("templateID")
	buildID := r.PathValue("buildID")
	if templateID == "" || buildID == "" {
		writeError(w, http.StatusBadRequest, "templateID and buildID are required")
		return
	}
	tpl, ok := s.reconciler.GetPodTemplate(templateID)
	if !ok {
		writeError(w, http.StatusNotFound, "template not found")
		return
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

	tplTemplateID := tpl.Status.TemplateID
	if tplTemplateID == "" {
		tplTemplateID = templateID
	}

	resp := map[string]any{
		"logs":       []string{},
		"logEntries": []any{},
		"templateID": tplTemplateID,
		"buildID":    buildID,
		"status":     wireStatus,
		"reason":     tpl.Status.Reason,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleTemplateFiles(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if !hex64Regex.MatchString(hash) {
		writeError(w, http.StatusBadRequest, "hash must be 64 lowercase hex characters")
		return
	}
	writeError(w, http.StatusNotImplemented, "template build is not enabled")
}

func (s *Server) handleBetaStub(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotImplemented, "template build is not enabled")
}
