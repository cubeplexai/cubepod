package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

var (
	ErrTemplateNotReady      = errors.New("409: template is not ready")
	ErrTemplateNotFound      = errors.New("404: template not found")
	ErrTemplateConflict      = errors.New("409: template conflict")
	ErrTemplateBuildDisabled = errors.New("501: template build is not enabled")
	ErrIdempotencyMismatch   = errors.New("409: idempotency_request_mismatch")
)

// WireBuildStatus maps the internal CR build status to the wire status reported to E2B clients.
// "building" and "uploaded" both return "building".
// "ready" and "error" pass through.
// "waiting" passes through.
func WireBuildStatus(crStatus string) string {
	switch crStatus {
	case v1alpha1.BuildStatusBuilding, v1alpha1.BuildStatusUploaded:
		return v1alpha1.BuildStatusBuilding
	case v1alpha1.BuildStatusReady:
		return v1alpha1.BuildStatusReady
	case v1alpha1.BuildStatusError:
		return v1alpha1.BuildStatusError
	case v1alpha1.BuildStatusWaiting:
		return v1alpha1.BuildStatusWaiting
	default:
		return crStatus
	}
}

// Slugify generates a DNS-1123 compliant slug from a name (max 63 characters).
func Slugify(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, ":", "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ".", "-")
	s = strings.ReplaceAll(s, "_", "-")

	reg := regexp.MustCompile("[^a-z0-9-]+")
	s = reg.ReplaceAllString(s, "-")

	dashReg := regexp.MustCompile("-+")
	s = dashReg.ReplaceAllString(s, "-")

	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = s[:63]
		s = strings.TrimRight(s, "-")
	}
	if s == "" {
		s = "template"
	}
	return s
}

func randomHex(n int) string {
	bytes := make([]byte, n/2)
	if _, err := rand.Read(bytes); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano())[:n]
	}
	return hex.EncodeToString(bytes)
}

// CreateTemplateRequest represents the payload for creating a template.
type CreateTemplateRequest struct {
	Name          string   `json:"name"`
	Tags          []string `json:"tags,omitempty"`
	Alias         string   `json:"alias,omitempty"`
	CPUCount      int32    `json:"cpuCount,omitempty"`
	MemoryMB      int32    `json:"memoryMB,omitempty"`
	MinFreeDiskMB int32    `json:"minFreeDiskMb,omitempty"`
}

// CreateTemplateResponse represents the 202 Accepted response.
type CreateTemplateResponse struct {
	TemplateID string   `json:"templateID"`
	BuildID    string   `json:"buildID"`
	Public     bool     `json:"public"`
	Aliases    []string `json:"aliases"`
	Names      []string `json:"names"`
	Tags       []string `json:"tags"`
}

// RegistryAuth contains credentials for pulling a base image.
type RegistryAuth struct {
	Type     string `json:"type,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// BuildStep represents a single build instruction from the E2B build API.
type BuildStep struct {
	Type      string   `json:"type"`
	Args      []string `json:"args"`
	FilesHash string   `json:"filesHash,omitempty"`
	Force     bool     `json:"force,omitempty"`
}

// StartBuildRequest represents the E2B TemplateBuildStartV2 payload.
type StartBuildRequest struct {
	FromImage         string        `json:"fromImage,omitempty"`
	FromTemplate      string        `json:"fromTemplate,omitempty"`
	FromImageRegistry *RegistryAuth `json:"fromImageRegistry,omitempty"`
	Force             bool          `json:"force,omitempty"`
	StartCmd          string        `json:"startCmd,omitempty"`
	ReadyCmd          string        `json:"readyCmd,omitempty"`
	Steps             []BuildStep   `json:"steps,omitempty"`
}

// ResolvedTemplate represents the resolved runtime parameters for a template.
type ResolvedTemplate struct {
	Image         string
	CPUCount      int32
	MemoryMB      int32
	DiskSizeMB    int32
	StartCmd      string
	ReadyCmd      string
	RunAsUser     *int64
	RunAsGroup    *int64
	RunAsUsername string
	TemplateSpec  v1alpha1.PodTemplateSpec
}

// RenderedMeta contains metadata extracted from build steps.
type RenderedMeta struct {
	RunAsUser     *int64
	RunAsGroup    *int64
	RunAsUsername string
	FilesHashes   []string
}

// RenderDockerfile translates E2B build steps into a Dockerfile in memory.
func RenderDockerfile(fromImage, buildID string, req StartBuildRequest) (string, *RenderedMeta, error) {
	if fromImage == "" {
		return "", nil, fmt.Errorf("400: fromImage cannot be empty")
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("FROM %s", fromImage))

	meta := &RenderedMeta{}
	var accumulatedEnv []string
	var lastUserStep string

	for _, step := range req.Steps {
		stepType := strings.ToUpper(strings.TrimSpace(step.Type))
		if stepType == "ARG" {
			return "", nil, fmt.Errorf("400: incoming step type ARG is not allowed")
		}

		if step.Force {
			lines = append(lines, fmt.Sprintf("ARG CUBEPOD_CACHEBUST=%s", buildID))
		}

		switch stepType {
		case "ENV":
			if len(step.Args)%2 != 0 {
				return "", nil, fmt.Errorf("400: ENV args length must be even, got %d", len(step.Args))
			}
			for i := 0; i < len(step.Args); i += 2 {
				accumulatedEnv = append(accumulatedEnv, fmt.Sprintf("%s=%s", step.Args[i], step.Args[i+1]))
			}
			// ENV instruction is NOT emitted into Dockerfile.

		case "WORKDIR":
			if len(step.Args) == 0 {
				return "", nil, fmt.Errorf("400: WORKDIR missing directory argument")
			}
			dir := step.Args[0]
			clean := filepath.Clean(dir)
			if clean == "/workspace" || strings.HasPrefix(clean, "/workspace/") {
				return "", nil, fmt.Errorf("400: WORKDIR cannot be /workspace or under it: %s", dir)
			}
			lines = append(lines, fmt.Sprintf("WORKDIR %s", dir))

		case "USER":
			if len(step.Args) == 0 {
				return "", nil, fmt.Errorf("400: USER missing argument")
			}
			u := step.Args[0]
			lines = append(lines, fmt.Sprintf("USER %s", u))
			lastUserStep = u

			// Parse user for spec fields:
			if strings.Contains(u, ":") {
				parts := strings.SplitN(u, ":", 2)
				uid, err1 := strconv.ParseInt(parts[0], 10, 64)
				gid, err2 := strconv.ParseInt(parts[1], 10, 64)
				if err1 == nil && err2 == nil {
					meta.RunAsUser = &uid
					meta.RunAsGroup = &gid
					meta.RunAsUsername = ""
				} else {
					meta.RunAsUsername = u
					meta.RunAsUser = nil
					meta.RunAsGroup = nil
				}
			} else {
				uid, err := strconv.ParseInt(u, 10, 64)
				if err == nil {
					meta.RunAsUser = &uid
					meta.RunAsGroup = nil
					meta.RunAsUsername = ""
				} else {
					meta.RunAsUsername = u
					meta.RunAsUser = nil
					meta.RunAsGroup = nil
				}
			}

		case "COPY", "ADD":
			if len(step.Args) < 2 {
				return "", nil, fmt.Errorf("400: COPY/ADD requires at least 2 arguments")
			}
			src := step.Args[0]
			dest := step.Args[1]

			srcBase := filepath.Base(filepath.Clean(src))
			if srcBase == "podd" || srcBase == "podd-init" {
				return "", nil, fmt.Errorf("400: COPY/ADD cannot copy podd or podd-init: %s", src)
			}

			if filepath.IsAbs(src) || strings.Contains(src, "..") || strings.HasPrefix(filepath.Clean(src), "..") {
				return "", nil, fmt.Errorf("400: COPY src cannot be absolute or contain ..: %s", src)
			}
			cleanDest := filepath.Clean(dest)
			if cleanDest == "/workspace" || strings.HasPrefix(cleanDest, "/workspace/") {
				return "", nil, fmt.Errorf("400: COPY dest cannot be /workspace or under it: %s", dest)
			}

			if step.FilesHash != "" {
				matched, _ := regexp.MatchString("^[0-9a-f]{64}$", step.FilesHash)
				if !matched {
					return "", nil, fmt.Errorf("400: filesHash must be 64 lowercase hex characters: %s", step.FilesHash)
				}
				meta.FilesHashes = append(meta.FilesHashes, step.FilesHash)
				src = fmt.Sprintf("%s/%s", step.FilesHash, src)
			}

			var flags []string
			if len(step.Args) >= 3 && step.Args[2] != "" {
				flags = append(flags, fmt.Sprintf("--chown=%s", step.Args[2]))
			}
			if len(step.Args) >= 4 && step.Args[3] != "" {
				mode := step.Args[3]
				matched, _ := regexp.MatchString("^[0-7]{3,4}$", mode)
				if !matched {
					return "", nil, fmt.Errorf("400: invalid mode %s", mode)
				}
				flags = append(flags, fmt.Sprintf("--chmod=%s", mode))
			}

			flagStr := ""
			if len(flags) > 0 {
				flagStr = strings.Join(flags, " ") + " "
			}
			lines = append(lines, fmt.Sprintf("COPY %s%s %s", flagStr, src, dest))

		case "RUN":
			if len(step.Args) == 0 {
				return "", nil, fmt.Errorf("400: RUN missing argument")
			}
			cmd := step.Args[0]
			runUser := ""
			if len(step.Args) > 1 && step.Args[1] != "" {
				runUser = step.Args[1]
			}

			if runUser != "" {
				lines = append(lines, fmt.Sprintf("USER %s", runUser))
			}

			fullCmd := cmd
			if len(accumulatedEnv) > 0 {
				fullCmd = strings.Join(accumulatedEnv, " ") + " " + cmd
			}
			lines = append(lines, fmt.Sprintf("RUN %s", fullCmd))

			if runUser != "" {
				if lastUserStep != "" {
					lines = append(lines, fmt.Sprintf("USER %s", lastUserStep))
				}
			}

		default:
			return "", nil, fmt.Errorf("400: unknown step type: %s", step.Type)
		}
	}

	rendered := strings.Join(lines, "\n") + "\n"

	// Safety check: rendered text must not contain podd-init
	if strings.Contains(rendered, "podd-init") {
		return "", nil, fmt.Errorf("400: rendered Dockerfile must not contain podd-init")
	}

	return rendered, meta, nil
}

type idempotencyRecord struct {
	requestHash string
	response    CreateTemplateResponse
}

func computeRequestHash(req CreateTemplateRequest) string {
	data, _ := json.Marshal(req)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

type seededDef struct {
	name       string
	names      []string
	image      string
	cpu        int32
	mem        int32
	rootfsSize string
	wsSize     string
}

var seededTemplates = []seededDef{
	{
		name:       "cubeplex-ubuntu24-desktop",
		names:      []string{"cubeplex-ubuntu24-desktop", "base", "code-interpreter", "code-interpreter-v1", "python"},
		image:      "cubeplex-ubuntu24-desktop:latest",
		cpu:        2,
		mem:        2048,
		rootfsSize: "10Gi",
		wsSize:     "20Gi",
	},
	{
		name:       "ubuntu-24-04",
		names:      []string{"ubuntu:24.04"},
		image:      "ubuntu:24.04",
		cpu:        2,
		mem:        2048,
		rootfsSize: "10Gi",
		wsSize:     "20Gi",
	},
	{
		name:       "debian-12",
		names:      []string{"debian:12"},
		image:      "debian:12",
		cpu:        2,
		mem:        2048,
		rootfsSize: "10Gi",
		wsSize:     "20Gi",
	},
}

// EnsureSeededTemplates ensures the three default PodTemplates are present if absent.
// It indexes them and does not overwrite existing objects.
func (r *Reconciler) EnsureSeededTemplates(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.kubeClient != nil {
		if list, err := r.kubeClient.ListPodTemplates(ctx, r.namespace); err == nil {
			for _, tpl := range list {
				cp := tpl
				r.templateObjects[tpl.Metadata.Name] = &cp
				_ = r.indexTemplateLocked(&cp)
				r.templates[tpl.Metadata.Name] = tpl.Spec
			}
		}
	}

	for _, s := range seededTemplates {
		if _, exists := r.templateObjects[s.name]; !exists {
			tpl := v1alpha1.PodTemplate{
				APIVersion: "cubepod.io/v1alpha1",
				Kind:       "PodTemplate",
				Metadata: v1alpha1.ObjectMeta{
					Name:      s.name,
					Namespace: r.namespace,
				},
				Spec: v1alpha1.PodTemplateSpec{
					RuntimeClassName: r.defaultRuntimeClass,
					Image:            s.image,
					PoddImage:        r.poddInjectorImage,
					Names:            s.names,
					CPUCount:         s.cpu,
					MemoryMB:         s.mem,
					DiskSizeMB:       10240,
					Resources: v1alpha1.ResourceRequirements{
						CPURequest:    "1",
						CPULimit:      fmt.Sprintf("%d", s.cpu),
						MemoryRequest: "2Gi",
						MemoryLimit:   fmt.Sprintf("%dMi", s.mem),
					},
					Storage: v1alpha1.StorageSpec{
						StorageClassName: r.defaultStorageClass,
						RootfsSize:       s.rootfsSize,
						WorkspaceSize:    s.wsSize,
					},
				},
				Status: v1alpha1.PodTemplateStatus{
					TemplateID:    "tpl_" + Slugify(s.name),
					ReadyBuildID:  "bld_seeded",
					ReadyImage:    s.image,
					LatestBuildID: "bld_seeded",
					BuildStatus:   v1alpha1.BuildStatusReady,
				},
			}

			if r.kubeClient != nil {
				_ = r.kubeClient.CreatePodTemplate(ctx, tpl)
			}
			r.templateObjects[s.name] = &tpl
			_ = r.indexTemplateLocked(&tpl)
			r.templates[s.name] = tpl.Spec
		}
	}
	return nil
}

func (r *Reconciler) indexTemplateLocked(tpl *v1alpha1.PodTemplate) error {
	keys := []string{tpl.Metadata.Name}
	if tpl.Status.TemplateID != "" {
		keys = append(keys, tpl.Status.TemplateID)
	}
	for _, n := range tpl.Spec.Names {
		if !slices.Contains(keys, n) {
			keys = append(keys, n)
		}
	}

	for _, k := range keys {
		if existing, exists := r.templateIndex[k]; exists && existing != tpl.Metadata.Name {
			return fmt.Errorf("%w: name %q already claimed by template %q", ErrTemplateConflict, k, existing)
		}
	}

	for _, k := range keys {
		r.templateIndex[k] = tpl.Metadata.Name
	}
	return nil
}

// ResolveTemplate resolves a template reference for sandbox creation.
func (r *Reconciler) ResolveTemplate(templateID string) (*ResolvedTemplate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resolveTemplateLocked(templateID)
}

func (r *Reconciler) resolveTemplateLocked(templateID string) (*ResolvedTemplate, error) {
	// If gate is OFF: pre-CEP resolution behavior
	if !r.e2bTemplateAPI {
		effectiveID := templateID
		if effectiveID == "" || effectiveID == "base" || effectiveID == "code-interpreter" || effectiveID == "code-interpreter-v1" || effectiveID == "python" {
			effectiveID = "cubeplex-ubuntu24-desktop"
		}

		tpl, ok := r.templates[effectiveID]
		if !ok {
			if obj, exists := r.templateObjects[effectiveID]; exists {
				tpl = obj.Spec
			} else {
				tpl = r.defaultTemplate(effectiveID)
			}
		}

		img := tpl.Image
		if img == "" {
			img = r.defaultSandboxImage
		}
		if img == "" {
			img = "ubuntu:24.04"
		}

		return &ResolvedTemplate{
			Image:         img,
			CPUCount:      2,
			MemoryMB:      2048,
			DiskSizeMB:    10240,
			StartCmd:      tpl.StartCmd,
			ReadyCmd:      tpl.ReadyCmd,
			RunAsUser:     tpl.RunAsUser,
			RunAsGroup:    tpl.RunAsGroup,
			RunAsUsername: tpl.RunAsUsername,
			TemplateSpec:  tpl,
		}, nil
	}

	// Gate is ON: CEP resolution order
	// 0. Empty templateID becomes base
	target := templateID
	if target == "" {
		target = "base"
	}

	// 1 & 2. Alias index hit
	if objName, hit := r.templateIndex[target]; hit {
		obj := r.templateObjects[objName]
		if obj != nil {
			isReady := obj.Status.BuildStatus == v1alpha1.BuildStatusReady || obj.Status.ReadyImage != ""
			if !isReady {
				return nil, fmt.Errorf("%w: %s", ErrTemplateNotReady, target)
			}

			img := obj.Status.ReadyImage
			if img == "" {
				img = obj.Spec.Image
			}
			if img == "" || img == "cubepod.io/unbuilt:waiting" {
				return nil, fmt.Errorf("%w: %s", ErrTemplateNotReady, target)
			}

			cpu := obj.Spec.CPUCount
			if cpu <= 0 {
				cpu = 2
			}
			mem := obj.Spec.MemoryMB
			if mem <= 0 {
				mem = 2048
			}
			disk := obj.Spec.DiskSizeMB
			if disk <= 0 {
				disk = 10240
			}

			tplSpec := obj.Spec
			if tplSpec.RuntimeClassName == "" && r.runtimeClassSet {
				tplSpec.RuntimeClassName = r.defaultRuntimeClass
			}
			if tplSpec.Storage.StorageClassName == "" && r.defaultStorageClass != "" {
				tplSpec.Storage.StorageClassName = r.defaultStorageClass
			}
			if tplSpec.Storage.RootfsSize == "" {
				tplSpec.Storage.RootfsSize = fmt.Sprintf("%dMi", disk)
			}
			if tplSpec.Storage.WorkspaceSize == "" {
				tplSpec.Storage.WorkspaceSize = "20Gi"
			}
			if tplSpec.PoddImage == "" && r.poddInjectorImage != "" {
				tplSpec.PoddImage = r.poddInjectorImage
			}

			return &ResolvedTemplate{
				Image:         img,
				CPUCount:      cpu,
				MemoryMB:      mem,
				DiskSizeMB:    disk,
				StartCmd:      obj.Spec.StartCmd,
				ReadyCmd:      obj.Spec.ReadyCmd,
				RunAsUser:     obj.Spec.RunAsUser,
				RunAsGroup:    obj.Spec.RunAsGroup,
				RunAsUsername: obj.Spec.RunAsUsername,
				TemplateSpec:  tplSpec,
			}, nil
		}
	}

	// 3. No hit and string is base, code-interpreter, code-interpreter-v1, or python:
	// use seeded cubeplex-ubuntu24-desktop
	if target == "base" || target == "code-interpreter" || target == "code-interpreter-v1" || target == "python" {
		if obj, exists := r.templateObjects["cubeplex-ubuntu24-desktop"]; exists {
			img := obj.Status.ReadyImage
			if img == "" {
				img = obj.Spec.Image
			}
			return &ResolvedTemplate{
				Image:         img,
				CPUCount:      2,
				MemoryMB:      2048,
				DiskSizeMB:    10240,
				StartCmd:      obj.Spec.StartCmd,
				ReadyCmd:      obj.Spec.ReadyCmd,
				RunAsUser:     obj.Spec.RunAsUser,
				RunAsGroup:    obj.Spec.RunAsGroup,
				RunAsUsername: obj.Spec.RunAsUsername,
				TemplateSpec:  obj.Spec,
			}, nil
		}
	}

	// 4. No hit and string contains / or : -> OCI fallback like today,
	// except literal cubepod.io/unbuilt:waiting which is 409 template is not ready
	if strings.Contains(target, "/") || strings.Contains(target, ":") {
		if target == "cubepod.io/unbuilt:waiting" {
			return nil, fmt.Errorf("%w: placeholder waiting", ErrTemplateNotReady)
		}
		tpl := r.defaultTemplate(target)
		return &ResolvedTemplate{
			Image:        target,
			CPUCount:     2,
			MemoryMB:     2048,
			DiskSizeMB:   10240,
			TemplateSpec: tpl,
		}, nil
	}

	// 5. Otherwise 404 template not found
	return nil, fmt.Errorf("%w: %s", ErrTemplateNotFound, target)
}

// CreateTemplate handles POST /templates creation of a new template.
func (r *Reconciler) CreateTemplate(ctx context.Context, req CreateTemplateRequest, idempotencyKey string) (*CreateTemplateResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	reqHash := computeRequestHash(req)
	if idempotencyKey != "" {
		if rec, exists := r.idempotency[idempotencyKey]; exists {
			if rec.requestHash == reqHash {
				respCopy := rec.response
				return &respCopy, nil
			}
			return nil, ErrIdempotencyMismatch
		}
	}

	primaryName := req.Name
	if primaryName == "" {
		primaryName = req.Alias
	}
	if primaryName == "" {
		primaryName = "template"
	}

	var tags []string
	tags = append(tags, req.Tags...)
	if strings.Contains(primaryName, ":") {
		parts := strings.SplitN(primaryName, ":", 2)
		primaryName = parts[0]
		tags = append(tags, parts[1])
	}

	var names []string
	names = append(names, primaryName)
	for _, t := range tags {
		taggedName := fmt.Sprintf("%s:%s", primaryName, t)
		if !slices.Contains(names, taggedName) {
			names = append(names, taggedName)
		}
	}
	if req.Alias != "" && !slices.Contains(names, req.Alias) {
		names = append(names, req.Alias)
	}

	slug := Slugify(primaryName)

	if existingObj, ok := r.templateObjects[slug]; ok {
		return nil, fmt.Errorf("%w: slug %q collision with template %s", ErrTemplateConflict, slug, existingObj.Status.TemplateID)
	}

	for _, n := range names {
		if existingObjName, exists := r.templateIndex[n]; exists && existingObjName != slug {
			return nil, fmt.Errorf("%w: name %q already claimed by %q", ErrTemplateConflict, n, existingObjName)
		}
	}

	templateID := fmt.Sprintf("tpl_%s", randomHex(8))
	buildID := fmt.Sprintf("bld_%s", randomHex(8))

	cpu := req.CPUCount
	if cpu <= 0 {
		cpu = 2
	}
	mem := req.MemoryMB
	if mem <= 0 {
		mem = 2048
	}
	disk := req.MinFreeDiskMB
	if disk <= 0 {
		disk = 10240
	}

	tpl := v1alpha1.PodTemplate{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodTemplate",
		Metadata: v1alpha1.ObjectMeta{
			Name:      slug,
			Namespace: r.namespace,
		},
		Spec: v1alpha1.PodTemplateSpec{
			RuntimeClassName: r.defaultRuntimeClass,
			Image:            "cubepod.io/unbuilt:waiting",
			PoddImage:        r.poddInjectorImage,
			Names:            names,
			CPUCount:         cpu,
			MemoryMB:         mem,
			DiskSizeMB:       disk,
			Storage: v1alpha1.StorageSpec{
				StorageClassName: r.defaultStorageClass,
				RootfsSize:       fmt.Sprintf("%dMi", disk),
				WorkspaceSize:    "20Gi",
			},
		},
		Status: v1alpha1.PodTemplateStatus{
			TemplateID:    templateID,
			LatestBuildID: buildID,
			BuildStatus:   v1alpha1.BuildStatusWaiting,
		},
	}

	if err := r.kubeClient.CreatePodTemplate(ctx, tpl); err != nil {
		return nil, err
	}

	r.templateObjects[slug] = &tpl
	_ = r.indexTemplateLocked(&tpl)
	r.templates[slug] = tpl.Spec

	resp := CreateTemplateResponse{
		TemplateID: templateID,
		BuildID:    buildID,
		Public:     false,
		Aliases:    []string{primaryName},
		Names:      names,
		Tags:       tags,
	}

	if idempotencyKey != "" {
		r.idempotency[idempotencyKey] = idempotencyRecord{
			requestHash: reqHash,
			response:    resp,
		}
	}

	return &resp, nil
}

// StartBuild initiates a template build or fast-path image registration.
func (r *Reconciler) StartBuild(ctx context.Context, templateID, buildID string, req StartBuildRequest) error {
	r.mu.Lock()

	// 1. Validate fromImage vs fromTemplate
	if (req.FromImage == "" && req.FromTemplate == "") || (req.FromImage != "" && req.FromTemplate != "") {
		r.mu.Unlock()
		return fmt.Errorf("400: exactly one of fromImage or fromTemplate must be set")
	}

	// 2. Find template object by templateID
	var tpl *v1alpha1.PodTemplate
	for _, obj := range r.templateObjects {
		if obj.Status.TemplateID == templateID || obj.Metadata.Name == templateID {
			tpl = obj
			break
		}
	}
	if tpl == nil {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrTemplateNotFound, templateID)
	}

	// 3. Second build while latest is waiting or building: 409
	if tpl.Status.LatestBuildID != "" && tpl.Status.LatestBuildID != buildID {
		if tpl.Status.BuildStatus == v1alpha1.BuildStatusWaiting || tpl.Status.BuildStatus == v1alpha1.BuildStatusBuilding || tpl.Status.BuildStatus == v1alpha1.BuildStatusUploaded {
			r.mu.Unlock()
			return fmt.Errorf("%w: build %s already in progress", ErrTemplateConflict, tpl.Status.LatestBuildID)
		}
	}

	// 4. Resolve base image
	baseImage := req.FromImage
	if req.FromTemplate != "" {
		fromObjName, ok := r.templateIndex[req.FromTemplate]
		if !ok {
			r.mu.Unlock()
			return fmt.Errorf("400: fromTemplate %q not found", req.FromTemplate)
		}
		fromObj := r.templateObjects[fromObjName]
		if fromObj == nil || (fromObj.Status.BuildStatus != v1alpha1.BuildStatusReady && fromObj.Status.ReadyImage == "") {
			r.mu.Unlock()
			return fmt.Errorf("400: fromTemplate %q is not ready", req.FromTemplate)
		}
		baseImage = fromObj.Status.ReadyImage
		if baseImage == "" {
			baseImage = fromObj.Spec.Image
		}
	}

	// 5. If steps are empty: fromImage or fromTemplate with empty steps -> ready immediately without builder!
	if len(req.Steps) == 0 {
		tpl.Status.BuildStatus = v1alpha1.BuildStatusReady
		tpl.Status.ReadyBuildID = buildID
		tpl.Status.ReadyImage = baseImage
		tpl.Status.LatestBuildID = buildID
		tpl.Status.Reason = ""
		tpl.Spec.Image = baseImage
		tpl.Spec.StartCmd = req.StartCmd
		tpl.Spec.ReadyCmd = req.ReadyCmd

		_ = r.kubeClient.UpdatePodTemplate(ctx, *tpl)
		_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
		r.templates[tpl.Metadata.Name] = tpl.Spec
		r.retireUnclaimedWarmEntriesForTemplateLocked(tpl.Metadata.Name, baseImage)
		r.mu.Unlock()
		return nil
	}

	// 6. Non-empty steps while template builds are disabled: return ErrTemplateBuildDisabled
	if !r.templateBuildEnabled {
		r.mu.Unlock()
		return ErrTemplateBuildDisabled
	}

	// 7. Transition to building: does NOT clear readyImage or readyBuildID
	tpl.Status.BuildStatus = v1alpha1.BuildStatusBuilding
	tpl.Status.LatestBuildID = buildID
	_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)

	// Render Dockerfile
	rendered, meta, err := RenderDockerfile(baseImage, buildID, req)
	if err != nil {
		tpl.Status.BuildStatus = v1alpha1.BuildStatusError
		tpl.Status.Reason = err.Error()
		_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
		r.mu.Unlock()
		return err
	}

	// Check missing tars in context store
	for _, hash := range meta.FilesHashes {
		if !r.contextStore.IsPresent(hash) {
			tpl.Status.BuildStatus = v1alpha1.BuildStatusError
			tpl.Status.Reason = fmt.Sprintf("missing context tar for hash %s", hash)
			_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
			r.mu.Unlock()
			return fmt.Errorf("400: missing context tar for hash %s", hash)
		}
	}

	builder := r.imageBuilder
	targetTag := fmt.Sprintf("cubepod-templates/%s:%s", tpl.Metadata.Name, buildID)
	contextDir := filepath.Join(r.contextDir, buildID)
	_ = os.MkdirAll(contextDir, 0755)

	for _, hash := range meta.FilesHashes {
		_ = r.contextStore.Unpack(hash, contextDir)
	}

	var passwords []string
	if req.FromImageRegistry != nil && req.FromImageRegistry.Password != "" {
		passwords = append(passwords, req.FromImageRegistry.Password)
	}

	buildOpts := BuildOptions{
		Tag:        targetTag,
		ContextDir: contextDir,
		Dockerfile: rendered,
		BuildID:    buildID,
		Passwords:  passwords,
		NoCache:    req.Force,
	}

	r.mu.Unlock()

	buildErr := builder.Build(ctx, buildOpts)

	r.mu.Lock()
	if buildErr != nil {
		tpl.Status.BuildStatus = v1alpha1.BuildStatusError
		tpl.Status.Reason = buildErr.Error()
		_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
		r.mu.Unlock()
		return buildErr
	}

	// Success: uploaded
	tpl.Status.BuildStatus = v1alpha1.BuildStatusUploaded
	_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
	r.mu.Unlock()

	// Probe outside r.mu lock
	var probeErr error
	if req.ReadyCmd != "" {
		probeErr = builder.Probe(ctx, targetTag, req.ReadyCmd, 120*time.Second)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if probeErr != nil {
		tpl.Status.BuildStatus = v1alpha1.BuildStatusError
		tpl.Status.Reason = probeErr.Error()
		_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
		return probeErr
	}

	// Ready: and only then set spec.image = readyImage
	tpl.Status.BuildStatus = v1alpha1.BuildStatusReady
	tpl.Status.ReadyBuildID = buildID
	tpl.Status.ReadyImage = targetTag
	tpl.Status.Reason = ""
	tpl.Spec.Image = targetTag
	tpl.Spec.StartCmd = req.StartCmd
	tpl.Spec.ReadyCmd = req.ReadyCmd
	tpl.Spec.RunAsUser = meta.RunAsUser
	tpl.Spec.RunAsGroup = meta.RunAsGroup
	tpl.Spec.RunAsUsername = meta.RunAsUsername

	_ = r.kubeClient.UpdatePodTemplate(ctx, *tpl)
	_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
	r.templates[tpl.Metadata.Name] = tpl.Spec
	r.retireUnclaimedWarmEntriesForTemplateLocked(tpl.Metadata.Name, targetTag)

	return nil
}

// DeleteTemplate deletes a PodTemplate if not referenced by any PodBox or WarmPool, or if force is true.
func (r *Reconciler) DeleteTemplate(ctx context.Context, templateID string, force bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var tpl *v1alpha1.PodTemplate
	for _, obj := range r.templateObjects {
		if obj.Status.TemplateID == templateID || obj.Metadata.Name == templateID {
			tpl = obj
			break
		}
	}
	if tpl == nil {
		return fmt.Errorf("%w: %s", ErrTemplateNotFound, templateID)
	}

	if !force {
		for boxName, spec := range r.specs {
			if r.templateMatchesRef(tpl, spec.TemplateRef) {
				return fmt.Errorf("%w: referenced by PodBox %s", ErrTemplateConflict, boxName)
			}
		}
		for poolName, pool := range r.warmPools {
			if r.templateMatchesRef(tpl, pool.spec.TemplateRef) {
				return fmt.Errorf("%w: referenced by WarmPool %s", ErrTemplateConflict, poolName)
			}
		}
	}

	_ = r.kubeClient.DeletePodTemplate(ctx, r.namespace, tpl.Metadata.Name)

	delete(r.templateIndex, tpl.Metadata.Name)
	if tpl.Status.TemplateID != "" {
		delete(r.templateIndex, tpl.Status.TemplateID)
	}
	for _, n := range tpl.Spec.Names {
		delete(r.templateIndex, n)
	}
	delete(r.templateObjects, tpl.Metadata.Name)
	delete(r.templates, tpl.Metadata.Name)

	return nil
}

// GetPodTemplate retrieves a PodTemplate by name, templateID, or alias in spec.Names.
func (r *Reconciler) GetPodTemplate(nameOrID string) (*v1alpha1.PodTemplate, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, obj := range r.templateObjects {
		if obj.Status.TemplateID == nameOrID || obj.Metadata.Name == nameOrID || slices.Contains(obj.Spec.Names, nameOrID) {
			cp := *obj
			return &cp, true
		}
	}
	if objName, ok := r.templateIndex[nameOrID]; ok {
		if obj, exists := r.templateObjects[objName]; exists {
			cp := *obj
			return &cp, true
		}
	}
	return nil, false
}

// SetTemplatePublic marks a template public or private, updating spec and status.
func (r *Reconciler) SetTemplatePublic(ctx context.Context, templateID string, public bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var tpl *v1alpha1.PodTemplate
	for _, obj := range r.templateObjects {
		if obj.Status.TemplateID == templateID || obj.Metadata.Name == templateID || slices.Contains(obj.Spec.Names, templateID) {
			tpl = obj
			break
		}
	}
	if tpl == nil {
		if objName, ok := r.templateIndex[templateID]; ok {
			tpl = r.templateObjects[objName]
		}
	}
	if tpl == nil {
		return fmt.Errorf("%w: %s", ErrTemplateNotFound, templateID)
	}

	tpl.Spec.Public = public
	tpl.Status.Public = public

	if err := r.kubeClient.UpdatePodTemplate(ctx, *tpl); err != nil {
		return err
	}
	if err := r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl); err != nil {
		return err
	}

	r.templates[tpl.Metadata.Name] = tpl.Spec
	return nil
}

func (r *Reconciler) templateMatchesRef(tpl *v1alpha1.PodTemplate, ref string) bool {
	if ref == tpl.Metadata.Name || ref == tpl.Status.TemplateID {
		return true
	}
	for _, n := range tpl.Spec.Names {
		if n == ref {
			return true
		}
	}
	return false
}

func (r *Reconciler) matchesTemplateNameLocked(ref, tplName string) bool {
	if ref == tplName {
		return true
	}
	if objName, ok := r.templateIndex[ref]; ok && objName == tplName {
		return true
	}
	return false
}

func (r *Reconciler) retireUnclaimedWarmEntriesForTemplateLocked(tplName, newReadyImage string) {
	for _, pool := range r.warmPools {
		if pool.spec.TemplateRef == tplName || r.matchesTemplateNameLocked(pool.spec.TemplateRef, tplName) {
			var kept []*WarmPodEntry
			for _, entry := range pool.entries {
				if !entry.Claimed && entry.Image != "" && entry.Image != newReadyImage {
					_ = r.kubeClient.DeletePod(context.Background(), r.namespace, entry.PodName)
					_ = r.kubeClient.DeletePVC(context.Background(), r.namespace, entry.RootfsPVCName)
					_ = r.kubeClient.DeletePVC(context.Background(), r.namespace, entry.WorkspacePVCName)
					continue
				}
				kept = append(kept, entry)
			}
			pool.entries = kept
		}
	}
}

// RecoverInFlightBuilds checks all templates in building state after a controller restart.
func (r *Reconciler) RecoverInFlightBuilds(ctx context.Context) {
	r.mu.Lock()
	var inFlight []*v1alpha1.PodTemplate
	for _, tpl := range r.templateObjects {
		if tpl.Status.BuildStatus == v1alpha1.BuildStatusBuilding {
			cp := *tpl
			inFlight = append(inFlight, &cp)
		}
	}
	builder := r.imageBuilder
	r.mu.Unlock()

	for _, tpl := range inFlight {
		buildID := tpl.Status.LatestBuildID
		slug := tpl.Metadata.Name
		targetTag := fmt.Sprintf("cubepod-templates/%s:%s", slug, buildID)

		tagExists, _ := builder.TagExists(ctx, targetTag)
		if tagExists {
			// Tag exists: move to uploaded
			r.mu.Lock()
			current, ok := r.templateObjects[slug]
			if !ok {
				r.mu.Unlock()
				continue
			}
			current.Status.BuildStatus = v1alpha1.BuildStatusUploaded
			_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *current)
			readyCmd := current.Spec.ReadyCmd
			r.mu.Unlock()

			// Run probe outside r.mu lock
			var probeErr error
			if readyCmd != "" {
				probeErr = builder.Probe(ctx, targetTag, readyCmd, 120*time.Second)
			}

			r.mu.Lock()
			current, ok = r.templateObjects[slug]
			if !ok {
				r.mu.Unlock()
				continue
			}
			if probeErr != nil {
				// Failed probe sets error and leaves previous readyImage
				current.Status.BuildStatus = v1alpha1.BuildStatusError
				current.Status.Reason = probeErr.Error()
				_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *current)
			} else {
				// Probe succeeds: set ready, readyImage, and spec.image
				current.Status.BuildStatus = v1alpha1.BuildStatusReady
				current.Status.ReadyBuildID = buildID
				current.Status.ReadyImage = targetTag
				current.Status.Reason = ""
				current.Spec.Image = targetTag

				_ = r.kubeClient.UpdatePodTemplate(ctx, *current)
				_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *current)
				r.templates[current.Metadata.Name] = current.Spec
				r.retireUnclaimedWarmEntriesForTemplateLocked(current.Metadata.Name, targetTag)
			}
			r.mu.Unlock()
		} else {
			// Tag does NOT exist
			r.mu.Lock()
			current, ok := r.templateObjects[slug]
			if !ok {
				r.mu.Unlock()
				continue
			}
			pid, hasPid := r.buildingPids[buildID]
			if hasPid && pid > 0 && syscall.Kill(pid, 0) == nil {
				// Local mode: pid is still running, keep watching
				r.mu.Unlock()
				continue
			}

			current.Status.BuildStatus = v1alpha1.BuildStatusError
			current.Status.Reason = "builder disappeared"
			_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *current)
			r.mu.Unlock()
		}
	}
}
