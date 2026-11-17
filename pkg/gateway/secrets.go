package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/egress"
	"github.com/cubeplexai/cubepod/pkg/secret"
)

// E2BSecretConfig configures the tenant-isolated E2B Secrets store.
type E2BSecretConfig struct {
	// Backend selects "file" or "k8s". Defaults to "k8s" when Dir is empty and KubeClient is set.
	Backend string
	// Dir is the durable secret store root (for "file" backend).
	Dir string
	// EncryptionKey is an optional 32-byte AES-256 key (for "file" backend).
	EncryptionKey []byte
	// KubeClient is the Kubernetes client (for "k8s" backend).
	KubeClient controller.KubeClient
	// Namespace is the Kubernetes namespace for "k8s" backend Secrets.
	Namespace string
	// MaxSecretsPerTenant caps secrets per API key tenant (default 100).
	MaxSecretsPerTenant int
}

var (
	secretPlaceholderRe = regexp.MustCompile(`\$\{e2b\.secrets\.([a-zA-Z0-9_.-]{1,128})\}`)

	forbiddenTransformHeaders = map[string]bool{
		"host":                true,
		"content-length":      true,
		"transfer-encoding":   true,
		"connection":          true,
		"proxy-authorization": true,
		"proxy-connection":    true,
		"te":                  true,
		"trailer":             true,
		"upgrade":             true,
	}
)

// ConfigureE2BSecrets opens (or reopens) the durable E2B secret store.
func (s *Server) ConfigureE2BSecrets(cfg E2BSecretConfig) error {
	secretCfg := secret.Config{
		EncryptionKey:       cfg.EncryptionKey,
		MaxSecretsPerTenant: cfg.MaxSecretsPerTenant,
		OnUpdate: func(tenantHash, secretID string, value secret.SecretValue) {
			if s.vault != nil {
				s.vault.UpdateSecretByRef(tenantHash, secretID, value.Raw())
			}
		},
		OnRevoke: func(tenantHash, secretID string) {
			if s.vault != nil {
				s.vault.RevokeSecretByRef(tenantHash, secretID)
			}
		},
	}

	backendMode := strings.ToLower(strings.TrimSpace(cfg.Backend))
	var (
		st  *secret.Store
		err error
	)
	if backendMode == "k8s" || (backendMode == "" && strings.TrimSpace(cfg.Dir) == "" && cfg.KubeClient != nil) {
		kb, kbErr := secret.NewKubeSecretBackend(cfg.KubeClient, cfg.Namespace)
		if kbErr != nil {
			return kbErr
		}
		st, err = secret.OpenWithBackend(kb, secretCfg)
	} else {
		st, err = secret.Open(cfg.Dir, secretCfg)
	}
	if err != nil {
		return err
	}
	s.secrets.Store(st)
	return nil
}

// SecretStore returns the configured E2B secret store, or nil if disabled.
func (s *Server) SecretStore() *secret.Store {
	return s.secrets.Load()
}

// StartSecretsSync runs periodic secret store synchronization in the background until ctx is cancelled.
func (s *Server) StartSecretsSync(ctx context.Context, interval time.Duration) {
	if st := s.secrets.Load(); st != nil {
		st.StartSync(ctx, interval)
	}
}

func (s *Server) rehydrateVaultBinding(ctx context.Context, sandboxID, placeholder string) error {
	if s.vault == nil || s.reconciler == nil || s.reconciler.KubeClient() == nil {
		return egress.ErrUnknownPlaceholder
	}
	st := s.secrets.Load()
	if st == nil {
		return egress.ErrUnknownPlaceholder
	}
	box, err := s.reconciler.KubeClient().GetPodBox(ctx, s.reconciler.Namespace(), sandboxID)
	if err != nil || box == nil || box.Spec.NetworkPolicy == nil {
		return egress.ErrUnknownPlaceholder
	}

	var matched *v1alpha1.SecretRefBinding
	for i := range box.Spec.NetworkPolicy.SecretRefBindings {
		if box.Spec.NetworkPolicy.SecretRefBindings[i].Placeholder == placeholder {
			matched = &box.Spec.NetworkPolicy.SecretRefBindings[i]
			break
		}
	}
	if matched == nil {
		return egress.ErrUnknownPlaceholder
	}

	_, val, err := st.ResolveByTenantID(matched.TenantHash, matched.SecretID)
	if errors.Is(err, secret.ErrNotFound) {
		if syncErr := st.Sync(ctx); syncErr != nil {
			return syncErr
		}
		_, val, err = st.ResolveByTenantID(matched.TenantHash, matched.SecretID)
	}
	if err != nil {
		return err
	}
	raw := val.Raw()
	if !isValidHeaderValue(raw) {
		return errors.New("secret value contains invalid control characters")
	}
	return s.vault.RestoreManagedSecret(
		matched.Placeholder,
		sandboxID,
		matched.TenantHash,
		matched.SecretID,
		"",
		raw,
		matched.AllowedHosts,
		matched.AllowedHeaders,
	)
}

func (s *Server) requireSecretStore(w http.ResponseWriter) (*secret.Store, bool) {
	st := s.secrets.Load()
	if st == nil {
		writeError(w, http.StatusNotImplemented, "E2B secrets store is not configured")
		return nil, false
	}
	return st, true
}

func (s *Server) handleCreateSecret(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	st, ok := s.requireSecretStore(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	info, err := st.Create(key, req.Name, req.Value)
	if err != nil {
		writeSecretMgmtError(w, err, req.Name)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(info)
}

func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	st, ok := s.requireSecretStore(w)
	if !ok {
		return
	}
	limit := secret.DefaultListLimit
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		parsed, err := strconv.Atoi(rawLimit)
		if err != nil || parsed <= 0 || parsed > secret.DefaultListLimit {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = parsed
	}
	nextToken := strings.TrimSpace(r.URL.Query().Get("nextToken"))
	items, next, err := st.List(key, limit, nextToken)
	if err != nil {
		writeSecretMgmtError(w, err, "")
		return
	}
	if next != "" {
		w.Header().Set("X-Next-Token", next)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(items)
}

func (s *Server) handleGetSecret(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	st, ok := s.requireSecretStore(w)
	if !ok {
		return
	}
	info, err := st.Get(key, r.PathValue("secretID"))
	if err != nil {
		writeSecretMgmtError(w, err, "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func (s *Server) handleUpdateSecret(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	st, ok := s.requireSecretStore(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	info, err := st.Update(key, r.PathValue("secretID"), req.Value)
	if err != nil {
		writeSecretMgmtError(w, err, "")
		return
	}
	if s.vault != nil {
		s.vault.UpdateSecretByRef(secret.TenantHash(key), info.ID, req.Value)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	key, ok := requireManagementKey(w, r)
	if !ok {
		return
	}
	st, ok := s.requireSecretStore(w)
	if !ok {
		return
	}
	info, err := st.Delete(key, r.PathValue("secretID"))
	if err != nil {
		writeSecretMgmtError(w, err, "")
		return
	}
	if s.vault != nil {
		s.vault.RevokeSecretByRef(secret.TenantHash(key), info.ID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeSecretMgmtError(w http.ResponseWriter, err error, name string) {
	switch {
	case errors.Is(err, secret.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "API key is required")
	case errors.Is(err, secret.ErrInvalidName):
		writeError(w, http.StatusBadRequest, "invalid secret name")
	case errors.Is(err, secret.ErrInvalidValue):
		writeError(w, http.StatusBadRequest, "invalid secret value")
	case errors.Is(err, secret.ErrInvalidCursor):
		writeError(w, http.StatusBadRequest, "invalid pagination cursor")
	case errors.Is(err, secret.ErrExists):
		writeError(w, http.StatusConflict, "Secret with name '"+name+"' already exists")
	case errors.Is(err, secret.ErrNotFound):
		writeError(w, http.StatusNotFound, "secret not found")
	case errors.Is(err, secret.ErrLimitExceeded):
		writeError(w, http.StatusTooManyRequests, "tenant secret limit exceeded")
	default:
		writeError(w, http.StatusInternalServerError, "secret operation failed")
	}
}

func isValidHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		isAlphaNum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if isAlphaNum {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}

func isForbiddenTransformHeader(name string) bool {
	return forbiddenTransformHeaders[strings.ToLower(strings.TrimSpace(name))]
}

func isValidHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		b := v[i]
		if b == '\t' {
			continue
		}
		if b < 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

type e2bNetworkSpec struct {
	AllowOut []string                        `json:"allowOut,omitempty"`
	DenyOut  []string                        `json:"denyOut,omitempty"`
	Rules    map[string][]e2bNetworkRuleSpec `json:"rules,omitempty"`
}

type e2bNetworkRuleSpec struct {
	Transform *e2bTransformSpec `json:"transform,omitempty"`
}

type e2bTransformSpec struct {
	Headers map[string]string `json:"headers,omitempty"`
}

// resolveSandboxSecretsAndNetwork translates E2B network rules and extension secretBindings
// into a NetworkPolicySpec containing only cpodref_ placeholders, binding raw secrets in Vault.
func (s *Server) resolveSandboxSecretsAndNetwork(apiKey, sandboxID string, req *createSandboxReq, basePolicy *v1alpha1.NetworkPolicySpec) (*v1alpha1.NetworkPolicySpec, map[string]string, error) {
	policy := cloneNetworkPolicy(basePolicy)
	if req.Network != nil {
		if policy == nil {
			policy = &v1alpha1.NetworkPolicySpec{DefaultAction: v1alpha1.NetworkDefaultAllow}
		}
		for _, target := range req.Network.DenyOut {
			t := strings.TrimSpace(target)
			if t == "" {
				continue
			}
			if t == "0.0.0.0/0" || t == "*" || strings.EqualFold(t, "all") {
				policy.DefaultAction = v1alpha1.NetworkDefaultDeny
				continue
			}
			policy.DenyHosts = append(policy.DenyHosts, t)
		}
		for _, target := range req.Network.AllowOut {
			t := strings.TrimSpace(target)
			if t == "" {
				continue
			}
			if t == "0.0.0.0/0" || t == "*" || strings.EqualFold(t, "all") {
				policy.DefaultAction = v1alpha1.NetworkDefaultAllow
				continue
			}
			policy.AllowHosts = append(policy.AllowHosts, t)
		}

		for rawHost, rules := range req.Network.Rules {
			host := strings.TrimSpace(rawHost)
			if host == "" {
				return nil, nil, errors.New("network.rules host cannot be empty")
			}
			policy.AllowHosts = append(policy.AllowHosts, host)
			mergedHeaders := make(map[string]string)
			for _, rule := range rules {
				if rule.Transform == nil {
					continue
				}
				for hdrName, hdrTemplate := range rule.Transform.Headers {
					if !isValidHeaderName(hdrName) || isForbiddenTransformHeader(hdrName) {
						return nil, nil, errors.New("invalid or forbidden transform header: " + hdrName)
					}
					canonHdr := http.CanonicalHeaderKey(hdrName)
					if !isValidHeaderValue(hdrTemplate) {
						return nil, nil, errors.New("transform header template contains control characters")
					}
					resolvedTemplate, refBindings, err := s.bindSecretTokens(apiKey, sandboxID, host, canonHdr, hdrTemplate)
					if err != nil {
						return nil, nil, err
					}
					mergedHeaders[canonHdr] = resolvedTemplate
					policy.SecretRefBindings = append(policy.SecretRefBindings, refBindings...)
				}
			}
			if len(mergedHeaders) > 0 {
				policy.HeaderTransforms = append(policy.HeaderTransforms, v1alpha1.HeaderTransformRule{
					Host:    host,
					Headers: mergedHeaders,
				})
			}
		}
	}

	var extraEnv map[string]string
	for _, sb := range req.Extension.SecretBindings {
		rawSecret := sb.RawSecret
		tenantHash := ""
		secretID := ""
		if rawSecret == "" && (sb.SecretName != "" || sb.SecretID != "") {
			st := s.secrets.Load()
			if st == nil {
				return nil, nil, errors.New("E2B secrets store is not configured")
			}
			if apiKey == "" {
				return nil, nil, secret.ErrUnauthorized
			}
			var info secret.SecretInfo
			var val secret.SecretValue
			var err error
			if sb.SecretID != "" {
				info, val, err = st.ResolveByID(apiKey, sb.SecretID)
			} else {
				info, val, err = st.ResolveByName(apiKey, sb.SecretName)
			}
			if err != nil {
				return nil, nil, err
			}
			rawSecret = val.Raw()
			tenantHash = secret.TenantHash(apiKey)
			secretID = info.ID
		}
		if rawSecret == "" {
			continue
		}
		if !isValidHeaderValue(rawSecret) {
			return nil, nil, errors.New("secret value contains invalid control characters")
		}
		if s.vault == nil {
			return nil, nil, errors.New("egress vault is not initialized")
		}
		ph, err := s.vault.BindManagedSecret(sandboxID, tenantHash, secretID, sb.EnvName, rawSecret, sb.AllowedHosts, sb.AllowedHeaders)
		if err != nil {
			return nil, nil, err
		}
		if sb.EnvName != "" {
			if extraEnv == nil {
				extraEnv = make(map[string]string)
			}
			extraEnv[sb.EnvName] = ph
		}
		if policy == nil {
			policy = &v1alpha1.NetworkPolicySpec{DefaultAction: v1alpha1.NetworkDefaultDeny}
		}
		policy.SecretBindings = append(policy.SecretBindings, v1alpha1.SecretBinding{
			EnvName:        sb.EnvName,
			Placeholder:    ph,
			AllowedHosts:   append([]string(nil), sb.AllowedHosts...),
			AllowedHeaders: append([]string(nil), sb.AllowedHeaders...),
		})
		if secretID != "" {
			policy.SecretRefBindings = append(policy.SecretRefBindings, v1alpha1.SecretRefBinding{
				Placeholder:    ph,
				TenantHash:     tenantHash,
				SecretID:       secretID,
				AllowedHosts:   append([]string(nil), sb.AllowedHosts...),
				AllowedHeaders: append([]string(nil), sb.AllowedHeaders...),
			})
		}
	}

	return policy, extraEnv, nil
}

func (s *Server) bindSecretTokens(apiKey, sandboxID, host, hdrName, tmpl string) (string, []v1alpha1.SecretRefBinding, error) {
	matches := secretPlaceholderRe.FindAllStringSubmatchIndex(tmpl, -1)
	if len(matches) == 0 {
		return tmpl, nil, nil
	}
	st := s.secrets.Load()
	if st == nil {
		return "", nil, errors.New("E2B secrets store is not configured")
	}
	if apiKey == "" {
		return "", nil, secret.ErrUnauthorized
	}

	tenantHash := secret.TenantHash(apiKey)
	var out strings.Builder
	var refs []v1alpha1.SecretRefBinding
	last := 0
	for _, m := range matches {
		out.WriteString(tmpl[last:m[0]])
		secName := tmpl[m[2]:m[3]]
		info, val, err := st.ResolveByName(apiKey, secName)
		if err != nil {
			return "", nil, err
		}
		rawVal := val.Raw()
		if !isValidHeaderValue(rawVal) {
			return "", nil, errors.New("secret value contains invalid control characters")
		}
		if s.vault == nil {
			return "", nil, errors.New("egress vault is not initialized")
		}
		ph, err := s.vault.BindManagedSecret(sandboxID, tenantHash, info.ID, "", rawVal, []string{host}, []string{hdrName})
		if err != nil {
			return "", nil, err
		}
		out.WriteString(ph)
		refs = append(refs, v1alpha1.SecretRefBinding{
			Placeholder:    ph,
			TenantHash:     tenantHash,
			SecretID:       info.ID,
			AllowedHosts:   []string{host},
			AllowedHeaders: []string{hdrName},
		})
		last = m[1]
	}
	out.WriteString(tmpl[last:])
	return out.String(), refs, nil
}
