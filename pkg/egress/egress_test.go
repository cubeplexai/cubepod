package egress_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/egress"
)

func TestCertMinterAndVerifier(t *testing.T) {
	ca, err := egress.NewCA()
	if err != nil {
		t.Fatalf("failed to create CA: %v", err)
	}

	sandboxID := "cpod-test-sandbox-42"
	certPEM, keyPEM, err := ca.MintSandboxCert(sandboxID, 1*time.Hour)
	if err != nil {
		t.Fatalf("MintSandboxCert failed: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatalf("expected non-empty cert and key PEM")
	}

	// Verify certificate extraction
	extractedID, err := ca.VerifySandboxCert(certPEM)
	if err != nil {
		t.Fatalf("VerifySandboxCert failed: %v", err)
	}
	if extractedID != sandboxID {
		t.Fatalf("expected sandboxID %q, got %q", sandboxID, extractedID)
	}

	// Package-level verifier
	pkgExtractedID, err := egress.VerifySandboxCert(ca, certPEM)
	if err != nil {
		t.Fatalf("package-level VerifySandboxCert failed: %v", err)
	}
	if pkgExtractedID != sandboxID {
		t.Fatalf("expected pkgExtractedID %q, got %q", sandboxID, pkgExtractedID)
	}

	// Reject empty sandbox ID
	_, _, err = ca.MintSandboxCert("", time.Hour)
	if err == nil {
		t.Fatalf("expected error minting cert with empty sandbox ID")
	}

	// Reject invalid PEM
	_, err = ca.VerifySandboxCert([]byte("invalid pem data"))
	if err == nil {
		t.Fatalf("expected error verifying invalid PEM")
	}

	// Untrusted CA cert
	otherCA, err := egress.NewCA()
	if err != nil {
		t.Fatalf("failed to create other CA: %v", err)
	}
	otherCertPEM, _, err := otherCA.MintSandboxCert("cpod-other", time.Hour)
	if err != nil {
		t.Fatalf("otherCA.MintSandboxCert failed: %v", err)
	}
	_, err = ca.VerifySandboxCert(otherCertPEM)
	if err == nil {
		t.Fatalf("expected verification failure for certificate signed by other CA")
	}
}

func TestHostPolicyMatching(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		pattern  string
		expected bool
	}{
		{"exact match", "api.github.com", "api.github.com", true},
		{"exact match uppercase", "API.GITHUB.COM", "api.github.com", true},
		{"exact match with port", "api.github.com:443", "api.github.com", true},
		{"wildcard match subdomain", "api.github.com", "*.github.com", true},
		{"wildcard match nested subdomain", "foo.api.github.com", "*.github.com", true},
		{"wildcard match apex domain", "github.com", "*.github.com", true},
		{"wildcard dot boundary prevents evil suffix", "evilgithub.com", "*.github.com", false},
		{"wildcard dot boundary prevents prefix", "not-github.com", "*.github.com", false},
		{"wildcard mismatch domain", "gitlab.com", "*.github.com", false},
		{"catch-all wildcard", "anything.internal.service", "*", true},
		{"empty host", "", "*.github.com", false},
		{"empty pattern", "api.github.com", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := egress.MatchHost(tc.host, tc.pattern)
			if got != tc.expected {
				t.Errorf("MatchHost(%q, %q) = %v; want %v", tc.host, tc.pattern, got, tc.expected)
			}
		})
	}
}

func TestCheckEgressAllowed(t *testing.T) {
	// Policy 1: Default Deny with Allow list
	denyPolicy := v1alpha1.NetworkPolicySpec{
		DefaultAction: v1alpha1.NetworkDefaultDeny,
		AllowHosts:    []string{"*.github.com", "pypi.org"},
		DenyHosts:     []string{"evil.github.com"},
	}

	if !egress.CheckEgressAllowed(denyPolicy, "api.github.com") {
		t.Errorf("expected api.github.com to be allowed under default deny + allow rule")
	}
	if !egress.CheckEgressAllowed(denyPolicy, "pypi.org") {
		t.Errorf("expected pypi.org to be allowed")
	}
	if egress.CheckEgressAllowed(denyPolicy, "evil.github.com") {
		t.Errorf("expected evil.github.com to be denied by explicit deny list")
	}
	if egress.CheckEgressAllowed(denyPolicy, "google.com") {
		t.Errorf("expected google.com to be denied by default deny")
	}

	// Policy 2: Default Allow with Deny list
	allowPolicy := v1alpha1.NetworkPolicySpec{
		DefaultAction: v1alpha1.NetworkDefaultAllow,
		DenyHosts:     []string{"*.malicious.org", "blocked.com"},
	}

	if !egress.CheckEgressAllowed(allowPolicy, "example.com") {
		t.Errorf("expected example.com to be allowed under default allow")
	}
	if egress.CheckEgressAllowed(allowPolicy, "sub.malicious.org") {
		t.Errorf("expected sub.malicious.org to be denied by deny rule")
	}
	if egress.CheckEgressAllowed(allowPolicy, "blocked.com") {
		t.Errorf("expected blocked.com to be denied by deny rule")
	}
}

func TestVaultSecretExchange(t *testing.T) {
	v := egress.NewVault()

	sandboxID := "cpod-sandbox-01"
	rawToken := "ghp_secureToken1234567890"

	placeholder, err := v.BindSecret(
		sandboxID,
		"GITHUB_TOKEN",
		rawToken,
		[]string{"*.github.com", "api.github.com"},
		[]string{"Authorization", "X-Api-Key"},
	)
	if err != nil {
		t.Fatalf("BindSecret failed: %v", err)
	}

	// Assert placeholder structure: cbxref_<32-chars>
	if !strings.HasPrefix(placeholder, "cbxref_") {
		t.Fatalf("expected placeholder prefix cbxref_, got %s", placeholder)
	}
	if len(placeholder) != 7+32 {
		t.Fatalf("expected placeholder length %d (cbxref_ + 32 chars), got %d: %s", 7+32, len(placeholder), placeholder)
	}

	// 1. Successful exchange with exact header case
	secret, err := v.Exchange(sandboxID, placeholder, "api.github.com", "Authorization")
	if err != nil {
		t.Fatalf("expected successful exchange, got: %v", err)
	}
	if secret != rawToken {
		t.Fatalf("expected secret %q, got %q", rawToken, secret)
	}

	// 2. Successful exchange with case-insensitive header
	secret, err = v.Exchange(sandboxID, placeholder, "api.github.com", "authorization")
	if err != nil {
		t.Fatalf("expected successful exchange with lowercase header, got: %v", err)
	}
	if secret != rawToken {
		t.Fatalf("expected secret %q, got %q", rawToken, secret)
	}

	// 3. Successful exchange with second allowed header
	secret, err = v.Exchange(sandboxID, placeholder, "api.github.com", "x-api-key")
	if err != nil {
		t.Fatalf("expected successful exchange with X-Api-Key, got: %v", err)
	}
	if secret != rawToken {
		t.Fatalf("expected secret %q, got %q", rawToken, secret)
	}

	// 4. Fail closed on wrong sandbox ID
	_, err = v.Exchange("cpod-imposter", placeholder, "api.github.com", "Authorization")
	if !errors.Is(err, egress.ErrSandboxMismatch) {
		t.Fatalf("expected ErrSandboxMismatch, got: %v", err)
	}

	// 5. Fail closed on disallowed host
	_, err = v.Exchange(sandboxID, placeholder, "evilgithub.com", "Authorization")
	if !errors.Is(err, egress.ErrHostNotAllowed) {
		t.Fatalf("expected ErrHostNotAllowed, got: %v", err)
	}

	_, err = v.Exchange(sandboxID, placeholder, "google.com", "Authorization")
	if !errors.Is(err, egress.ErrHostNotAllowed) {
		t.Fatalf("expected ErrHostNotAllowed, got: %v", err)
	}

	// 6. Fail closed on disallowed header
	_, err = v.Exchange(sandboxID, placeholder, "api.github.com", "Cookie")
	if !errors.Is(err, egress.ErrHeaderNotAllowed) {
		t.Fatalf("expected ErrHeaderNotAllowed, got: %v", err)
	}

	_, err = v.Exchange(sandboxID, placeholder, "api.github.com", "")
	if !errors.Is(err, egress.ErrHeaderNotAllowed) {
		t.Fatalf("expected ErrHeaderNotAllowed for empty header, got: %v", err)
	}

	// 7. Fail closed on unknown placeholder
	_, err = v.Exchange(sandboxID, "cbxref_unknown0000000000000000000000000", "api.github.com", "Authorization")
	if !errors.Is(err, egress.ErrUnknownPlaceholder) {
		t.Fatalf("expected ErrUnknownPlaceholder, got: %v", err)
	}

	// 8. Revoke sandbox bindings
	v.RevokeSandbox(sandboxID)
	_, err = v.Exchange(sandboxID, placeholder, "api.github.com", "Authorization")
	if !errors.Is(err, egress.ErrUnknownPlaceholder) {
		t.Fatalf("expected ErrUnknownPlaceholder after RevokeSandbox, got: %v", err)
	}
	if v.Count() != 0 {
		t.Fatalf("expected vault count 0 after revocation, got %d", v.Count())
	}
}

func TestConcurrentVaultAccess(t *testing.T) {
	v := egress.NewVault()
	const workers = 8
	const iterations = 50

	var wg sync.WaitGroup
	wg.Add(workers)

	for w := 0; w < workers; w++ {
		go func(workerID int) {
			defer wg.Done()
			sbID := "cpod-sandbox-worker"
			for i := 0; i < iterations; i++ {
				ph, err := v.BindSecret(
					sbID,
					"API_KEY",
					"secret-value",
					[]string{"*.example.com"},
					[]string{"Authorization"},
				)
				if err != nil {
					t.Errorf("BindSecret failed: %v", err)
					return
				}
				s, err := v.Exchange(sbID, ph, "api.example.com", "Authorization")
				if err != nil || s != "secret-value" {
					t.Errorf("Exchange failed: %v, got %s", err, s)
					return
				}
				v.RevokeSecret(ph)
			}
		}(w)
	}

	wg.Wait()
}
