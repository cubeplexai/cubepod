package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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

var envKeyRegexp = regexp.MustCompile("^[A-Za-z_][A-Za-z0-9_]*$")

// RenderedMeta contains metadata extracted from build steps.
type RenderedMeta struct {
	RunAsUser     *int64
	RunAsGroup    *int64
	RunAsUsername string
	HasUserStep   bool
	FilesHashes   []string
}

func posixSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// RenderDockerfile translates E2B build steps into a Dockerfile in memory.
func RenderDockerfile(fromImage, buildID string, req StartBuildRequest) (string, *RenderedMeta, error) {
	if fromImage == "" {
		return "", nil, fmt.Errorf("400: fromImage cannot be empty")
	}

	var srcPassword string
	if req.FromImageRegistry != nil {
		srcPassword = req.FromImageRegistry.Password
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("FROM %s", fromImage))

	meta := &RenderedMeta{}
	var envOrder []string
	envMap := make(map[string]string)
	var lastUserStep string

	for _, step := range req.Steps {
		for _, arg := range step.Args {
			if strings.Contains(arg, "cpodref_") {
				return "", nil, fmt.Errorf("400: build step arguments must not contain cpodref_ secret references")
			}
			if srcPassword != "" && strings.Contains(arg, srcPassword) {
				return "", nil, fmt.Errorf("400: build step arguments must not contain registry credentials")
			}
		}

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
				k := step.Args[i]
				v := step.Args[i+1]
				if !envKeyRegexp.MatchString(k) {
					return "", nil, fmt.Errorf("400: invalid environment variable key %q", k)
				}
				if _, exists := envMap[k]; !exists {
					envOrder = append(envOrder, k)
				}
				envMap[k] = v
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
			meta.HasUserStep = true

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

			var envPrefix strings.Builder
			for _, k := range envOrder {
				envPrefix.WriteString("export ")
				envPrefix.WriteString(k)
				envPrefix.WriteString("=")
				envPrefix.WriteString(posixSingleQuote(envMap[k]))
				envPrefix.WriteString("; ")
			}
			lines = append(lines, fmt.Sprintf("RUN %s%s", envPrefix.String(), cmd))

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

// TemplateBuildMetricsSnapshot holds Prometheus-ready counters and duration histogram buckets.
type TemplateBuildMetricsSnapshot struct {
	TemplateBuildsTotal          map[string]int64
	TemplateBuildDurationBuckets map[string]int64
	TemplateBuildDurationSum     float64
	TemplateBuildDurationCount   int64
}

type templateBuildMetricsCollector struct {
	mu       sync.Mutex
	total    map[string]int64
	buckets  map[string]int64
	sumSecs  float64
	countObs int64
}

func (m *templateBuildMetricsCollector) observe(result string, dur time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.total == nil {
		m.total = make(map[string]int64)
	}
	if m.buckets == nil {
		m.buckets = map[string]int64{
			"le_5":   0,
			"le_15":  0,
			"le_60":  0,
			"le_300": 0,
			"le_inf": 0,
		}
	}
	m.total[result]++
	secs := dur.Seconds()
	if secs < 0 {
		secs = 0
	}
	m.sumSecs += secs
	m.countObs++
	if secs <= 5 {
		m.buckets["le_5"]++
	}
	if secs <= 15 {
		m.buckets["le_15"]++
	}
	if secs <= 60 {
		m.buckets["le_60"]++
	}
	if secs <= 300 {
		m.buckets["le_300"]++
	}
	m.buckets["le_inf"]++
}

func (m *templateBuildMetricsCollector) snapshot() TemplateBuildMetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	tot := make(map[string]int64, len(m.total))
	for k, v := range m.total {
		tot[k] = v
	}
	bkt := make(map[string]int64, len(m.buckets))
	for k, v := range m.buckets {
		bkt[k] = v
	}
	return TemplateBuildMetricsSnapshot{
		TemplateBuildsTotal:          tot,
		TemplateBuildDurationBuckets: bkt,
		TemplateBuildDurationSum:     m.sumSecs,
		TemplateBuildDurationCount:   m.countObs,
	}
}

// TemplateBuildMetrics returns a snapshot of template build counters and duration histogram buckets.
func (r *Reconciler) TemplateBuildMetrics() TemplateBuildMetricsSnapshot {
	return r.buildMetrics.snapshot()
}

func computeResourcesForTemplate(cpu, mem int32) v1alpha1.ResourceRequirements {
	cpuReq := "1"
	if cpu < 1 {
		cpuReq = fmt.Sprintf("%d", cpu)
	}
	memReq := "2Gi"
	if mem < 2048 {
		memReq = fmt.Sprintf("%dMi", mem)
	}
	return v1alpha1.ResourceRequirements{
		CPURequest:    cpuReq,
		CPULimit:      fmt.Sprintf("%d", cpu),
		MemoryRequest: memReq,
		MemoryLimit:   fmt.Sprintf("%dMi", mem),
	}
}

func upsertBuildRecord(builds []v1alpha1.PodTemplateBuildRecord, rec v1alpha1.PodTemplateBuildRecord) []v1alpha1.PodTemplateBuildRecord {
	for i := range builds {
		if builds[i].BuildID == rec.BuildID {
			if builds[i].CreatedAt != "" && rec.CreatedAt == "" {
				rec.CreatedAt = builds[i].CreatedAt
			}
			if builds[i].CPUCount != 0 && rec.CPUCount == 0 {
				rec.CPUCount = builds[i].CPUCount
			}
			if builds[i].MemoryMB != 0 && rec.MemoryMB == 0 {
				rec.MemoryMB = builds[i].MemoryMB
			}
			builds[i] = rec
			return builds
		}
	}
	return append(builds, rec)
}

func primaryNameOfTemplate(tpl *v1alpha1.PodTemplate) string {
	if tpl == nil {
		return ""
	}
	if tpl.Metadata.Annotations != nil {
		if pn := tpl.Metadata.Annotations["cubepod.io/primary-name"]; pn != "" {
			return pn
		}
	}
	if len(tpl.Spec.Names) > 0 && tpl.Spec.Names[0] != "" {
		return tpl.Spec.Names[0]
	}
	return tpl.Metadata.Name
}

func (r *Reconciler) persistLocalTemplateLocked(tpl *v1alpha1.PodTemplate) {
	stateDir := r.stateDir
	if stateDir == "" {
		stateDir = os.Getenv("CUBEPOD_STATE")
	}
	if stateDir == "" || tpl == nil || tpl.Status.TemplateID == "" {
		return
	}
	dir := filepath.Join(stateDir, "templates")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	data, err := json.MarshalIndent(tpl, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, tpl.Status.TemplateID+".json"), data, 0644)
}

func (r *Reconciler) removeLocalTemplateLocked(templateID string) {
	stateDir := r.stateDir
	if stateDir == "" {
		stateDir = os.Getenv("CUBEPOD_STATE")
	}
	if stateDir == "" || templateID == "" {
		return
	}
	_ = os.Remove(filepath.Join(stateDir, "templates", templateID+".json"))
}

func (r *Reconciler) loadLocalTemplates(ctx context.Context) error {
	if r.stateDir == "" {
		return nil
	}
	dir := filepath.Join(r.stateDir, "templates")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			continue
		}
		var tpl v1alpha1.PodTemplate
		if jsonErr := json.Unmarshal(data, &tpl); jsonErr != nil || tpl.Metadata.Name == "" {
			continue
		}
		if r.kubeClient != nil {
			if err := r.kubeClient.CreatePodTemplate(ctx, tpl); err != nil {
				_ = r.kubeClient.UpdatePodTemplate(ctx, tpl)
				_ = r.kubeClient.UpdatePodTemplateStatus(ctx, tpl)
			}
		}
	}
	return nil
}

// SyncTemplatesFromKube synchronizes PodTemplate CRs from Kubernetes into memory.
func (r *Reconciler) SyncTemplatesFromKube(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if r.kubeClient == nil {
		return nil
	}
	list, err := r.kubeClient.ListPodTemplates(ctx, r.namespace)
	if err != nil {
		if errors.Is(err, ErrCRDNotInstalled) {
			return nil
		}
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, tpl := range list {
		if tpl.Metadata.Name == "" {
			continue
		}
		cp := tpl
		r.templateObjects[tpl.Metadata.Name] = &cp
		_ = r.indexTemplateLocked(&cp)
		r.templates[tpl.Metadata.Name] = cp.Spec
	}
	return nil
}

func (r *Reconciler) targetImageTagLocked(slug, buildID string) string {
	if r.templateRegistry != "" {
		prefix := strings.Trim(r.templateRepoPrefix, "/")
		if prefix != "" {
			return fmt.Sprintf("%s/%s/%s:%s", r.templateRegistry, prefix, slug, buildID)
		}
		return fmt.Sprintf("%s/%s:%s", r.templateRegistry, slug, buildID)
	}
	return fmt.Sprintf("cubepod-templates/%s:%s", slug, buildID)
}

// CreateTemplate handles POST /templates creation of a new template or same-name rebuild.
func (r *Reconciler) CreateTemplate(ctx context.Context, req CreateTemplateRequest, idempotencyKey string) (*CreateTemplateResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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

	for _, n := range names {
		if existingObjName, exists := r.templateIndex[n]; exists && existingObjName != slug {
			return nil, fmt.Errorf("%w: name %q already claimed by %q", ErrTemplateConflict, n, existingObjName)
		}
	}

	cpu := req.CPUCount
	if cpu <= 0 {
		cpu = 2
	}
	mem := req.MemoryMB
	if mem <= 0 {
		mem = 2048
	}
	disk := max(req.MinFreeDiskMB, int32(10240))
	resources := computeResourcesForTemplate(cpu, mem)
	now := time.Now().UTC().Format(time.RFC3339)

	if existingObj, ok := r.templateObjects[slug]; ok {
		if primaryNameOfTemplate(existingObj) != primaryName {
			return nil, fmt.Errorf("%w: slug %q collision with template %s", ErrTemplateConflict, slug, existingObj.Status.TemplateID)
		}
		if existingObj.Status.BuildStatus == v1alpha1.BuildStatusBuilding || existingObj.Status.BuildStatus == v1alpha1.BuildStatusUploaded {
			return nil, fmt.Errorf("%w: build %s already in progress", ErrTemplateConflict, existingObj.Status.LatestBuildID)
		}

		// Same-name rebuild: reuse TemplateID, allocate new buildID, keep ReadyImage and ReadyBuildID unchanged.
		templateID := existingObj.Status.TemplateID
		if templateID == "" {
			templateID = fmt.Sprintf("tpl_%s", randomHex(8))
			existingObj.Status.TemplateID = templateID
		}
		buildID := fmt.Sprintf("bld_%s", randomHex(8))

		for _, oldName := range existingObj.Spec.Names {
			if mapped, mappedOk := r.templateIndex[oldName]; mappedOk && mapped == slug {
				delete(r.templateIndex, oldName)
			}
		}

		if existingObj.Metadata.Annotations == nil {
			existingObj.Metadata.Annotations = make(map[string]string)
		}
		existingObj.Metadata.Annotations["cubepod.io/primary-name"] = primaryName
		existingObj.Spec.Names = names
		existingObj.Spec.CPUCount = cpu
		existingObj.Spec.MemoryMB = mem
		existingObj.Spec.DiskSizeMB = disk
		existingObj.Spec.Resources = resources
		existingObj.Spec.Storage.StorageClassName = r.defaultStorageClass
		existingObj.Spec.Storage.RootfsSize = fmt.Sprintf("%dMi", disk)
		existingObj.Spec.Storage.WorkspaceSize = "20Gi"

		if existingObj.Status.CreatedAt == "" {
			existingObj.Status.CreatedAt = now
		}
		existingObj.Status.UpdatedAt = now
		existingObj.Status.LatestBuildID = buildID
		existingObj.Status.BuildStatus = v1alpha1.BuildStatusWaiting
		existingObj.Status.Reason = ""
		existingObj.Status.FailedStep = ""
		existingObj.Status.BuildCount++
		existingObj.Status.Builds = upsertBuildRecord(existingObj.Status.Builds, v1alpha1.PodTemplateBuildRecord{
			BuildID:   buildID,
			Status:    v1alpha1.BuildStatusWaiting,
			CreatedAt: now,
			UpdatedAt: now,
			CPUCount:  cpu,
			MemoryMB:  mem,
		})

		if err := r.kubeClient.UpdatePodTemplate(ctx, *existingObj); err != nil {
			return nil, err
		}
		if err := r.kubeClient.UpdatePodTemplateStatus(ctx, *existingObj); err != nil {
			return nil, err
		}
		_ = r.indexTemplateLocked(existingObj)
		r.templates[slug] = existingObj.Spec
		r.persistLocalTemplateLocked(existingObj)

		resp := CreateTemplateResponse{
			TemplateID: templateID,
			BuildID:    buildID,
			Public:     existingObj.Spec.Public,
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

	templateID := fmt.Sprintf("tpl_%s", randomHex(8))
	buildID := fmt.Sprintf("bld_%s", randomHex(8))

	tpl := v1alpha1.PodTemplate{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodTemplate",
		Metadata: v1alpha1.ObjectMeta{
			Name:      slug,
			Namespace: r.namespace,
			Annotations: map[string]string{
				"cubepod.io/primary-name": primaryName,
			},
		},
		Spec: v1alpha1.PodTemplateSpec{
			RuntimeClassName: r.defaultRuntimeClass,
			Image:            "cubepod.io/unbuilt:waiting",
			PoddImage:        r.poddInjectorImage,
			Names:            names,
			CPUCount:         cpu,
			MemoryMB:         mem,
			DiskSizeMB:       disk,
			Resources:        resources,
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
			BuildCount:    1,
			CreatedAt:     now,
			UpdatedAt:     now,
			Builds: []v1alpha1.PodTemplateBuildRecord{
				{
					BuildID:   buildID,
					Status:    v1alpha1.BuildStatusWaiting,
					CreatedAt: now,
					UpdatedAt: now,
					CPUCount:  cpu,
					MemoryMB:  mem,
				},
			},
		},
	}

	if err := r.kubeClient.CreatePodTemplate(ctx, tpl); err != nil {
		return nil, err
	}

	r.templateObjects[slug] = &tpl
	_ = r.indexTemplateLocked(&tpl)
	r.templates[slug] = tpl.Spec
	r.persistLocalTemplateLocked(&tpl)

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

type preparedBuildPlan struct {
	slug              string
	templateID        string
	buildID           string
	baseImage         string
	targetTag         string
	rendered          string
	filesHashes       []string
	effectiveStartCmd string
	effectiveReadyCmd string
	runAsUser         *int64
	runAsGroup        *int64
	runAsUsername     string
	fromImageRegistry *RegistryAuth
	noCache           bool
	skipBuild         bool
	builder           ImageBuilder
	pullSecretName    string
	pushSecretName    string
	templateRegistry  string
	repoPrefix        string
	cpuCount          int32
	memoryMB          int32
}

func (r *Reconciler) prepareBuild(ctx context.Context, templateID, buildID string, req StartBuildRequest) (*preparedBuildPlan, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	// 1. Validate fromImage vs fromTemplate
	if (req.FromImage == "" && req.FromTemplate == "") || (req.FromImage != "" && req.FromTemplate != "") {
		return nil, false, fmt.Errorf("400: exactly one of fromImage or fromTemplate must be set")
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
		return nil, false, fmt.Errorf("%w: %s", ErrTemplateNotFound, templateID)
	}

	// 3. Second build while latest is waiting or building: 409
	if tpl.Status.LatestBuildID != "" && tpl.Status.LatestBuildID != buildID {
		if tpl.Status.BuildStatus == v1alpha1.BuildStatusWaiting || tpl.Status.BuildStatus == v1alpha1.BuildStatusBuilding || tpl.Status.BuildStatus == v1alpha1.BuildStatusUploaded {
			return nil, false, fmt.Errorf("%w: build %s already in progress", ErrTemplateConflict, tpl.Status.LatestBuildID)
		}
	}
	if tpl.Status.LatestBuildID == buildID && (tpl.Status.BuildStatus == v1alpha1.BuildStatusBuilding || tpl.Status.BuildStatus == v1alpha1.BuildStatusUploaded) {
		return nil, false, fmt.Errorf("%w: build %s already in progress", ErrTemplateConflict, buildID)
	}

	// 4. Resolve base image and inherited runtime metadata
	baseImage := req.FromImage
	effectiveStartCmd := req.StartCmd
	effectiveReadyCmd := req.ReadyCmd
	var inheritedUser *int64
	var inheritedGroup *int64
	var inheritedUsername string

	if req.FromTemplate != "" {
		fromObjName, ok := r.templateIndex[req.FromTemplate]
		if !ok {
			return nil, false, fmt.Errorf("%w: fromTemplate %q not found", ErrTemplateNotFound, req.FromTemplate)
		}
		fromObj := r.templateObjects[fromObjName]
		if fromObj == nil {
			return nil, false, fmt.Errorf("%w: fromTemplate %q not found", ErrTemplateNotFound, req.FromTemplate)
		}
		if fromObj.Status.ReadyImage == "" {
			return nil, false, fmt.Errorf("%w: fromTemplate %q is not ready", ErrTemplateNotReady, req.FromTemplate)
		}
		baseImage = fromObj.Status.ReadyImage
		if effectiveStartCmd == "" {
			effectiveStartCmd = fromObj.Spec.StartCmd
		}
		if effectiveReadyCmd == "" {
			effectiveReadyCmd = fromObj.Spec.ReadyCmd
		}
		inheritedUser = fromObj.Spec.RunAsUser
		inheritedGroup = fromObj.Spec.RunAsGroup
		inheritedUsername = fromObj.Spec.RunAsUsername
	}

	// 5. Render and validate Dockerfile/steps synchronously
	now := time.Now().UTC().Format(time.RFC3339)
	rendered, meta, err := RenderDockerfile(baseImage, buildID, req)
	if err != nil {
		tpl.Status.BuildStatus = v1alpha1.BuildStatusError
		tpl.Status.LatestBuildID = buildID
		tpl.Status.Reason = err.Error()
		tpl.Status.UpdatedAt = now
		tpl.Status.Builds = upsertBuildRecord(tpl.Status.Builds, v1alpha1.PodTemplateBuildRecord{
			BuildID:   buildID,
			Status:    v1alpha1.BuildStatusError,
			Reason:    err.Error(),
			UpdatedAt: now,
			CPUCount:  tpl.Spec.CPUCount,
			MemoryMB:  tpl.Spec.MemoryMB,
		})
		_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
		r.persistLocalTemplateLocked(tpl)
		return nil, false, err
	}

	runAsUser := inheritedUser
	runAsGroup := inheritedGroup
	runAsUsername := inheritedUsername
	if meta.HasUserStep {
		runAsUser = meta.RunAsUser
		runAsGroup = meta.RunAsGroup
		runAsUsername = meta.RunAsUsername
	}

	// 6. Validate context tars synchronously
	for _, hash := range meta.FilesHashes {
		if !r.contextStore.IsPresent(hash) {
			reason := fmt.Sprintf("missing context tar for hash %s", hash)
			tpl.Status.BuildStatus = v1alpha1.BuildStatusError
			tpl.Status.LatestBuildID = buildID
			tpl.Status.Reason = reason
			tpl.Status.UpdatedAt = now
			tpl.Status.Builds = upsertBuildRecord(tpl.Status.Builds, v1alpha1.PodTemplateBuildRecord{
				BuildID:   buildID,
				Status:    v1alpha1.BuildStatusError,
				Reason:    reason,
				UpdatedAt: now,
				CPUCount:  tpl.Spec.CPUCount,
				MemoryMB:  tpl.Spec.MemoryMB,
			})
			_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
			r.persistLocalTemplateLocked(tpl)
			return nil, false, fmt.Errorf("400: %s", reason)
		}
	}

	// 7. Fast-path to ready ONLY when len(req.Steps) == 0 && effectiveReadyCmd == ""
	if len(req.Steps) == 0 && effectiveReadyCmd == "" {
		tpl.Status.BuildStatus = v1alpha1.BuildStatusReady
		tpl.Status.ReadyBuildID = buildID
		tpl.Status.ReadyImage = baseImage
		tpl.Status.LatestBuildID = buildID
		tpl.Status.Reason = ""
		tpl.Status.FailedStep = ""
		tpl.Status.UpdatedAt = now
		tpl.Status.Builds = upsertBuildRecord(tpl.Status.Builds, v1alpha1.PodTemplateBuildRecord{
			BuildID:   buildID,
			Status:    v1alpha1.BuildStatusReady,
			CreatedAt: now,
			UpdatedAt: now,
			CPUCount:  tpl.Spec.CPUCount,
			MemoryMB:  tpl.Spec.MemoryMB,
		})
		tpl.Spec.Image = baseImage
		tpl.Spec.StartCmd = effectiveStartCmd
		tpl.Spec.ReadyCmd = effectiveReadyCmd
		tpl.Spec.RunAsUser = runAsUser
		tpl.Spec.RunAsGroup = runAsGroup
		tpl.Spec.RunAsUsername = runAsUsername

		_ = r.kubeClient.UpdatePodTemplate(ctx, *tpl)
		_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
		r.templates[tpl.Metadata.Name] = tpl.Spec
		r.persistLocalTemplateLocked(tpl)
		r.retireUnclaimedWarmEntriesForTemplateLocked(tpl.Metadata.Name, baseImage)

		r.buildMetrics.observe(v1alpha1.BuildStatusReady, 0)
		slog.Info("template build finished",
			"templateID", tpl.Status.TemplateID,
			"buildID", buildID,
			"result", v1alpha1.BuildStatusReady,
		)
		return nil, true, nil
	}

	// 8. Otherwise require templateBuildEnabled && imageBuilder != nil
	if !r.templateBuildEnabled || r.imageBuilder == nil {
		return nil, false, ErrTemplateBuildDisabled
	}

	skipBuild := len(req.Steps) == 0
	targetTag := r.targetImageTagLocked(tpl.Metadata.Name, buildID)
	if skipBuild {
		targetTag = baseImage
	}

	// Transition to building without touching ReadyImage / ReadyBuildID
	tpl.Status.BuildStatus = v1alpha1.BuildStatusBuilding
	tpl.Status.LatestBuildID = buildID
	tpl.Status.Reason = ""
	tpl.Status.FailedStep = ""
	tpl.Status.UpdatedAt = now
	tpl.Status.Builds = upsertBuildRecord(tpl.Status.Builds, v1alpha1.PodTemplateBuildRecord{
		BuildID:   buildID,
		Status:    v1alpha1.BuildStatusBuilding,
		CreatedAt: now,
		UpdatedAt: now,
		CPUCount:  tpl.Spec.CPUCount,
		MemoryMB:  tpl.Spec.MemoryMB,
	})
	_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
	r.persistLocalTemplateLocked(tpl)

	return &preparedBuildPlan{
		slug:              tpl.Metadata.Name,
		templateID:        tpl.Status.TemplateID,
		buildID:           buildID,
		baseImage:         baseImage,
		targetTag:         targetTag,
		rendered:          rendered,
		filesHashes:       meta.FilesHashes,
		effectiveStartCmd: effectiveStartCmd,
		effectiveReadyCmd: effectiveReadyCmd,
		runAsUser:         runAsUser,
		runAsGroup:        runAsGroup,
		runAsUsername:     runAsUsername,
		fromImageRegistry: req.FromImageRegistry,
		noCache:           req.Force,
		skipBuild:         skipBuild,
		builder:           r.imageBuilder,
		pullSecretName:    r.templatePullSecret,
		pushSecretName:    r.templatePushSecret,
		templateRegistry:  r.templateRegistry,
		repoPrefix:        r.templateRepoPrefix,
		cpuCount:          tpl.Spec.CPUCount,
		memoryMB:          tpl.Spec.MemoryMB,
	}, false, nil
}

// StartBuild validates and executes a template build or fast-path registration synchronously.
func (r *Reconciler) StartBuild(ctx context.Context, templateID, buildID string, req StartBuildRequest) error {
	plan, fastPath, err := r.prepareBuild(ctx, templateID, buildID, req)
	if err != nil || fastPath {
		return err
	}
	return r.executeBuild(ctx, plan)
}

// StartBuildAsync validates synchronously, sets status to "building", and launches the build in a background goroutine.
func (r *Reconciler) StartBuildAsync(ctx context.Context, templateID, buildID string, req StartBuildRequest) error {
	plan, fastPath, err := r.prepareBuild(ctx, templateID, buildID, req)
	if err != nil || fastPath {
		return err
	}
	go func() {
		_ = r.executeBuild(context.Background(), plan)
	}()
	return nil
}

func (r *Reconciler) createEphemeralSourceSecret(ctx context.Context, buildID, baseImage string, auth *RegistryAuth) (string, error) {
	if auth == nil || (auth.Username == "" && auth.Password == "") || r.kubeClient == nil {
		return "", nil
	}
	host := extractImageRegistryHost(baseImage)
	encoded := base64.StdEncoding.EncodeToString([]byte(auth.Username + ":" + auth.Password))
	cfg := dockerConfigJSON{
		Auths: map[string]dockerConfigAuthEntry{
			host: {
				Username: auth.Username,
				Password: auth.Password,
				Auth:     encoded,
			},
		},
	}
	rawCfg, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	secretName := fmt.Sprintf("cubepod-template-src-%s", buildID)
	sec := KubeSecret{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: v1alpha1.ObjectMeta{
			Name:      secretName,
			Namespace: r.namespace,
			Labels: map[string]string{
				"cubepod.io/ephemeral-build-secret": "true",
				"cubepod.io/build-id":               buildID,
			},
		},
		Type: "kubernetes.io/dockerconfigjson",
		Data: map[string][]byte{
			".dockerconfigjson": rawCfg,
		},
	}
	if err := r.kubeClient.CreateSecret(ctx, sec); err != nil {
		return "", err
	}
	return secretName, nil
}

func (r *Reconciler) executeBuild(ctx context.Context, plan *preparedBuildPlan) error {
	if ctx == nil {
		ctx = context.Background()
	}
	startTime := time.Now()
	result := v1alpha1.BuildStatusError
	defer func() {
		r.buildMetrics.observe(result, time.Since(startTime))
		slog.Info("template build finished",
			"templateID", plan.templateID,
			"buildID", plan.buildID,
			"result", result,
		)
	}()

	var passwords []string
	if plan.fromImageRegistry != nil && plan.fromImageRegistry.Password != "" {
		passwords = append(passwords, plan.fromImageRegistry.Password)
	}

	srcSecretName, secErr := r.createEphemeralSourceSecret(ctx, plan.buildID, plan.baseImage, plan.fromImageRegistry)
	if secErr != nil {
		r.markBuildError(ctx, plan, ScrubString(secErr.Error(), passwords))
		return secErr
	}
	if srcSecretName != "" {
		defer func() {
			delCtx, delCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer delCancel()
			_ = r.kubeClient.DeleteSecret(delCtx, r.namespace, srcSecretName)
		}()
	}

	workDir := filepath.Join(r.contextDir, "work", plan.buildID)
	_ = os.MkdirAll(workDir, 0755)
	defer func() {
		_ = os.RemoveAll(workDir)
	}()

	for _, hash := range plan.filesHashes {
		if unpackErr := r.contextStore.Unpack(hash, workDir); unpackErr != nil {
			r.markBuildError(ctx, plan, ScrubString(unpackErr.Error(), passwords))
			return unpackErr
		}
	}

	if !plan.skipBuild {
		buildOpts := BuildOptions{
			Tag:            plan.targetTag,
			ContextDir:     workDir,
			LogDir:         r.contextDir,
			Dockerfile:     plan.rendered,
			BuildID:        plan.buildID,
			Passwords:      passwords,
			NoCache:        plan.noCache,
			Registry:       plan.templateRegistry,
			RepoPrefix:     plan.repoPrefix,
			Slug:           plan.slug,
			SourceImage:    plan.baseImage,
			SourceAuth:     plan.fromImageRegistry,
			PushSecretName: plan.pushSecretName,
			Namespace:      r.namespace,
			KubeClient:     r.kubeClient,
		}

		if buildErr := plan.builder.Build(ctx, buildOpts); buildErr != nil {
			r.markBuildError(ctx, plan, ScrubString(buildErr.Error(), passwords))
			return buildErr
		}

		r.mu.Lock()
		if tpl, ok := r.templateObjects[plan.slug]; ok {
			now := time.Now().UTC().Format(time.RFC3339)
			tpl.Status.BuildStatus = v1alpha1.BuildStatusUploaded
			tpl.Status.UpdatedAt = now
			tpl.Status.Builds = upsertBuildRecord(tpl.Status.Builds, v1alpha1.PodTemplateBuildRecord{
				BuildID:   plan.buildID,
				Status:    v1alpha1.BuildStatusUploaded,
				UpdatedAt: now,
				CPUCount:  plan.cpuCount,
				MemoryMB:  plan.memoryMB,
			})
			_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
			r.persistLocalTemplateLocked(tpl)
		}
		r.mu.Unlock()
	}

	if plan.effectiveReadyCmd != "" {
		probeSrcSecret := ""
		if plan.skipBuild {
			probeSrcSecret = srcSecretName
		}
		timeout := r.templateProbeTimeout
		if timeout <= 0 {
			timeout = 120 * time.Second
		}
		probeErr := plan.builder.Probe(ctx, plan.targetTag, ProbeOptions{
			BuildID:          plan.buildID,
			Namespace:        r.namespace,
			StartCmd:         plan.effectiveStartCmd,
			ReadyCmd:         plan.effectiveReadyCmd,
			RunAsUser:        plan.runAsUser,
			RunAsGroup:       plan.runAsGroup,
			RunAsUsername:    plan.runAsUsername,
			Timeout:          timeout,
			PullSecretName:   plan.pullSecretName,
			SourceSecretName: probeSrcSecret,
		})
		if probeErr != nil {
			r.markBuildError(ctx, plan, ScrubString(probeErr.Error(), passwords))
			return probeErr
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	tpl, ok := r.templateObjects[plan.slug]
	if !ok {
		return fmt.Errorf("%w: %s", ErrTemplateNotFound, plan.templateID)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tpl.Status.BuildStatus = v1alpha1.BuildStatusReady
	tpl.Status.ReadyBuildID = plan.buildID
	tpl.Status.ReadyImage = plan.targetTag
	tpl.Status.Reason = ""
	tpl.Status.FailedStep = ""
	tpl.Status.UpdatedAt = now
	tpl.Status.Builds = upsertBuildRecord(tpl.Status.Builds, v1alpha1.PodTemplateBuildRecord{
		BuildID:   plan.buildID,
		Status:    v1alpha1.BuildStatusReady,
		UpdatedAt: now,
		CPUCount:  plan.cpuCount,
		MemoryMB:  plan.memoryMB,
	})
	tpl.Spec.Image = plan.targetTag
	tpl.Spec.StartCmd = plan.effectiveStartCmd
	tpl.Spec.ReadyCmd = plan.effectiveReadyCmd
	tpl.Spec.RunAsUser = plan.runAsUser
	tpl.Spec.RunAsGroup = plan.runAsGroup
	tpl.Spec.RunAsUsername = plan.runAsUsername

	_ = r.kubeClient.UpdatePodTemplate(ctx, *tpl)
	_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
	r.templates[tpl.Metadata.Name] = tpl.Spec
	r.persistLocalTemplateLocked(tpl)
	r.retireUnclaimedWarmEntriesForTemplateLocked(tpl.Metadata.Name, plan.targetTag)
	result = v1alpha1.BuildStatusReady
	return nil
}

func (r *Reconciler) markBuildError(ctx context.Context, plan *preparedBuildPlan, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tpl, ok := r.templateObjects[plan.slug]
	if !ok {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tpl.Status.BuildStatus = v1alpha1.BuildStatusError
	tpl.Status.Reason = reason
	tpl.Status.UpdatedAt = now
	tpl.Status.Builds = upsertBuildRecord(tpl.Status.Builds, v1alpha1.PodTemplateBuildRecord{
		BuildID:   plan.buildID,
		Status:    v1alpha1.BuildStatusError,
		Reason:    reason,
		UpdatedAt: now,
		CPUCount:  plan.cpuCount,
		MemoryMB:  plan.memoryMB,
	})
	_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
	r.persistLocalTemplateLocked(tpl)
}

// RecordTemplateSpawn increments SpawnCount and updates LastSpawnedAt when a sandbox is created from a template.
func (r *Reconciler) RecordTemplateSpawn(ctx context.Context, idOrAlias string) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	var tpl *v1alpha1.PodTemplate
	for _, obj := range r.templateObjects {
		if obj.Status.TemplateID == idOrAlias || obj.Metadata.Name == idOrAlias || slices.Contains(obj.Spec.Names, idOrAlias) {
			tpl = obj
			break
		}
	}
	if tpl == nil {
		if objName, ok := r.templateIndex[idOrAlias]; ok {
			tpl = r.templateObjects[objName]
		}
	}
	if tpl == nil {
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	tpl.Status.SpawnCount++
	tpl.Status.LastSpawnedAt = now
	_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl)
	r.persistLocalTemplateLocked(tpl)
}

// DeleteTemplate deletes a PodTemplate if not currently building/uploaded and not referenced by active PodBoxes or WarmPools.
func (r *Reconciler) DeleteTemplate(ctx context.Context, templateID string, force bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
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

	if tpl.Status.BuildStatus == v1alpha1.BuildStatusBuilding || tpl.Status.BuildStatus == v1alpha1.BuildStatusUploaded {
		return fmt.Errorf("%w: build %s is currently %s", ErrTemplateConflict, tpl.Status.LatestBuildID, tpl.Status.BuildStatus)
	}

	if !force {
		for boxName, spec := range r.specs {
			if st := r.boxes[boxName]; st != nil && st.Phase == v1alpha1.PodBoxPhaseTerminated {
				continue
			}
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
	r.removeLocalTemplateLocked(tpl.Status.TemplateID)

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
	now := time.Now().UTC().Format(time.RFC3339)
	tpl.Status.UpdatedAt = now

	if err := r.kubeClient.UpdatePodTemplate(ctx, *tpl); err != nil {
		return err
	}
	if err := r.kubeClient.UpdatePodTemplateStatus(ctx, *tpl); err != nil {
		return err
	}

	r.templates[tpl.Metadata.Name] = tpl.Spec
	r.persistLocalTemplateLocked(tpl)
	return nil
}

func (r *Reconciler) findTemplateLocked(nameOrID string) *v1alpha1.PodTemplate {
	for _, obj := range r.templateObjects {
		if obj.Status.TemplateID == nameOrID || obj.Metadata.Name == nameOrID || slices.Contains(obj.Spec.Names, nameOrID) {
			return obj
		}
	}
	if objName, ok := r.templateIndex[nameOrID]; ok {
		return r.templateObjects[objName]
	}
	return nil
}

func normalizeTemplateTagEntry(primaryName, rawTag string) string {
	t := strings.TrimSpace(rawTag)
	if t == "" {
		return ""
	}
	if strings.Contains(t, ":") {
		return t
	}
	return fmt.Sprintf("%s:%s", primaryName, t)
}

// AssignTemplateTags adds tagged names (accepting "name:tag" or bare "tag") to a template's Spec.Names.
func (r *Reconciler) AssignTemplateTags(ctx context.Context, nameOrID string, tags []string) (*v1alpha1.PodTemplate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	tpl := r.findTemplateLocked(nameOrID)
	if tpl == nil {
		return nil, fmt.Errorf("%w: %s", ErrTemplateNotFound, nameOrID)
	}

	primary := primaryNameOfTemplate(tpl)
	var normalized []string
	for _, raw := range tags {
		entry := normalizeTemplateTagEntry(primary, raw)
		if entry == "" {
			continue
		}
		if existing, exists := r.templateIndex[entry]; exists && existing != tpl.Metadata.Name {
			return nil, fmt.Errorf("%w: tag %q already claimed by %q", ErrTemplateConflict, entry, existing)
		}
		if !slices.Contains(normalized, entry) {
			normalized = append(normalized, entry)
		}
	}

	for _, entry := range normalized {
		if !slices.Contains(tpl.Spec.Names, entry) {
			tpl.Spec.Names = append(tpl.Spec.Names, entry)
		}
	}
	tpl.Status.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	if err := r.kubeClient.UpdatePodTemplate(ctx, *tpl); err != nil {
		return nil, err
	}
	_ = r.indexTemplateLocked(tpl)
	r.templates[tpl.Metadata.Name] = tpl.Spec
	r.persistLocalTemplateLocked(tpl)

	cp := *tpl
	return &cp, nil
}

// DeleteTemplateTags removes tagged names from a template's Spec.Names while preserving the primary name.
func (r *Reconciler) DeleteTemplateTags(ctx context.Context, nameOrID string, tags []string) (*v1alpha1.PodTemplate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	tpl := r.findTemplateLocked(nameOrID)
	if tpl == nil {
		return nil, fmt.Errorf("%w: %s", ErrTemplateNotFound, nameOrID)
	}

	primary := primaryNameOfTemplate(tpl)
	toRemove := make(map[string]struct{}, len(tags))
	for _, raw := range tags {
		entry := normalizeTemplateTagEntry(primary, raw)
		if entry == "" || entry == primary {
			continue
		}
		toRemove[entry] = struct{}{}
	}

	kept := make([]string, 0, len(tpl.Spec.Names))
	for _, existing := range tpl.Spec.Names {
		if existing == primary {
			kept = append(kept, existing)
			continue
		}
		if _, rm := toRemove[existing]; rm {
			if mapped, ok := r.templateIndex[existing]; ok && mapped == tpl.Metadata.Name {
				delete(r.templateIndex, existing)
			}
			continue
		}
		kept = append(kept, existing)
	}
	tpl.Spec.Names = kept
	tpl.Status.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

	if err := r.kubeClient.UpdatePodTemplate(ctx, *tpl); err != nil {
		return nil, err
	}
	_ = r.indexTemplateLocked(tpl)
	r.templates[tpl.Metadata.Name] = tpl.Spec
	r.persistLocalTemplateLocked(tpl)

	cp := *tpl
	return &cp, nil
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
	for poolName, pool := range r.warmPools {
		if pool.spec.TemplateRef == tplName || r.matchesTemplateNameLocked(pool.spec.TemplateRef, tplName) {
			var kept []*WarmPodEntry
			var avail int32
			retired := false
			for _, entry := range pool.entries {
				if !entry.Claimed && entry.Image != "" && entry.Image != newReadyImage {
					retired = true
					_ = r.kubeClient.DeletePod(context.Background(), r.namespace, entry.PodName)
					_ = r.kubeClient.DeletePVC(context.Background(), r.namespace, entry.RootfsPVCName)
					_ = r.kubeClient.DeletePVC(context.Background(), r.namespace, entry.WorkspacePVCName)
					continue
				}
				if !entry.Claimed {
					avail++
				}
				kept = append(kept, entry)
			}
			pool.entries = kept
			if retired && r.kubeClient != nil {
				_ = r.kubeClient.UpdatePodWarmPoolStatus(context.Background(), r.namespace, poolName, v1alpha1.PodWarmPoolStatus{
					ReadyReplicas:     avail,
					AvailableReplicas: avail,
				})
			}
		}
	}
}

func (r *Reconciler) isBuildPIDAliveLocked(buildID string) bool {
	if pid, hasPid := r.buildingPids[buildID]; hasPid && pid > 0 && syscall.Kill(pid, 0) == nil {
		return true
	}
	var candidateDirs []string
	if r.contextDir != "" {
		candidateDirs = append(candidateDirs, r.contextDir)
	}
	if r.stateDir != "" {
		candidateDirs = append(candidateDirs, r.stateDir, filepath.Join(r.stateDir, "template-context"))
	}
	if env := os.Getenv("CUBEPOD_STATE"); env != "" {
		candidateDirs = append(candidateDirs, env, filepath.Join(env, "template-context"))
	}
	for _, dir := range candidateDirs {
		pidPath := filepath.Join(dir, "pids", buildID+".pid")
		data, err := os.ReadFile(pidPath)
		if err != nil {
			continue
		}
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
		if parseErr == nil && pid > 0 && syscall.Kill(pid, 0) == nil {
			return true
		}
	}
	return false
}

// RecoverInFlightBuilds checks all templates in building or uploaded state after a controller restart.
func (r *Reconciler) RecoverInFlightBuilds(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	var inFlight []*v1alpha1.PodTemplate
	for _, tpl := range r.templateObjects {
		if tpl.Status.BuildStatus == v1alpha1.BuildStatusBuilding || tpl.Status.BuildStatus == v1alpha1.BuildStatusUploaded {
			cp := *tpl
			inFlight = append(inFlight, &cp)
		}
	}
	builder := r.imageBuilder
	probeTimeout := r.templateProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = 120 * time.Second
	}
	r.mu.Unlock()

	for _, tpl := range inFlight {
		buildID := tpl.Status.LatestBuildID
		slug := tpl.Metadata.Name

		r.mu.RLock()
		targetTag := r.targetImageTagLocked(slug, buildID)
		r.mu.RUnlock()

		tagExists, _ := builder.TagExists(ctx, targetTag)
		if tagExists {
			// Tag exists: move to uploaded
			r.mu.Lock()
			current, ok := r.templateObjects[slug]
			if !ok {
				r.mu.Unlock()
				continue
			}
			now := time.Now().UTC().Format(time.RFC3339)
			current.Status.BuildStatus = v1alpha1.BuildStatusUploaded
			current.Status.UpdatedAt = now
			current.Status.Builds = upsertBuildRecord(current.Status.Builds, v1alpha1.PodTemplateBuildRecord{
				BuildID:   buildID,
				Status:    v1alpha1.BuildStatusUploaded,
				UpdatedAt: now,
				CPUCount:  current.Spec.CPUCount,
				MemoryMB:  current.Spec.MemoryMB,
			})
			_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *current)
			r.persistLocalTemplateLocked(current)
			probeOpts := ProbeOptions{
				BuildID:        buildID,
				Namespace:      r.namespace,
				StartCmd:       current.Spec.StartCmd,
				ReadyCmd:       current.Spec.ReadyCmd,
				RunAsUser:      current.Spec.RunAsUser,
				RunAsGroup:     current.Spec.RunAsGroup,
				RunAsUsername:  current.Spec.RunAsUsername,
				Timeout:        probeTimeout,
				PullSecretName: r.templatePullSecret,
			}
			r.mu.Unlock()

			// Run probe outside r.mu lock
			var probeErr error
			if probeOpts.ReadyCmd != "" {
				probeErr = builder.Probe(ctx, targetTag, probeOpts)
			}

			r.mu.Lock()
			current, ok = r.templateObjects[slug]
			if !ok {
				r.mu.Unlock()
				continue
			}
			now = time.Now().UTC().Format(time.RFC3339)
			if probeErr != nil {
				// Failed probe sets error and leaves previous readyImage
				current.Status.BuildStatus = v1alpha1.BuildStatusError
				current.Status.Reason = probeErr.Error()
				current.Status.UpdatedAt = now
				current.Status.Builds = upsertBuildRecord(current.Status.Builds, v1alpha1.PodTemplateBuildRecord{
					BuildID:   buildID,
					Status:    v1alpha1.BuildStatusError,
					Reason:    probeErr.Error(),
					UpdatedAt: now,
					CPUCount:  current.Spec.CPUCount,
					MemoryMB:  current.Spec.MemoryMB,
				})
				_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *current)
				r.persistLocalTemplateLocked(current)
			} else {
				// Probe succeeds: set ready, readyImage, and spec.image
				current.Status.BuildStatus = v1alpha1.BuildStatusReady
				current.Status.ReadyBuildID = buildID
				current.Status.ReadyImage = targetTag
				current.Status.Reason = ""
				current.Status.FailedStep = ""
				current.Status.UpdatedAt = now
				current.Status.Builds = upsertBuildRecord(current.Status.Builds, v1alpha1.PodTemplateBuildRecord{
					BuildID:   buildID,
					Status:    v1alpha1.BuildStatusReady,
					UpdatedAt: now,
					CPUCount:  current.Spec.CPUCount,
					MemoryMB:  current.Spec.MemoryMB,
				})
				current.Spec.Image = targetTag

				_ = r.kubeClient.UpdatePodTemplate(ctx, *current)
				_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *current)
				r.templates[current.Metadata.Name] = current.Spec
				r.persistLocalTemplateLocked(current)
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
			if r.isBuildPIDAliveLocked(buildID) {
				// Local mode: pid is still running, keep watching
				r.mu.Unlock()
				continue
			}

			now := time.Now().UTC().Format(time.RFC3339)
			current.Status.BuildStatus = v1alpha1.BuildStatusError
			current.Status.Reason = "builder disappeared"
			current.Status.UpdatedAt = now
			current.Status.Builds = upsertBuildRecord(current.Status.Builds, v1alpha1.PodTemplateBuildRecord{
				BuildID:   buildID,
				Status:    v1alpha1.BuildStatusError,
				Reason:    "builder disappeared",
				UpdatedAt: now,
				CPUCount:  current.Spec.CPUCount,
				MemoryMB:  current.Spec.MemoryMB,
			})
			_ = r.kubeClient.UpdatePodTemplateStatus(ctx, *current)
			r.persistLocalTemplateLocked(current)
			r.mu.Unlock()
		}
	}
}
