package egress

import (
	"net"
	"strings"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

// normalizeHost strips surrounding whitespace, converts to lowercase, strips trailing dots,
// and extracts the hostname if a port is attached.
func normalizeHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if strings.HasPrefix(h, "[") && strings.Contains(h, "]") {
		// Bracketed IPv6 e.g. [::1]:8080
		if host, _, err := net.SplitHostPort(h); err == nil {
			return host
		}
	} else if strings.Count(h, ":") == 1 {
		// IPv4 or hostname with port e.g. api.github.com:443
		if host, _, err := net.SplitHostPort(h); err == nil {
			return host
		}
	}
	return strings.TrimSuffix(h, ".")
}

// MatchHost evaluates whether a host matches an exact or wildcard pattern.
// Supported patterns:
//   - "*": matches any non-empty host
//   - Exact match: e.g. "api.github.com" matches "api.github.com"
//   - Wildcard: e.g. "*.domain.com" matches "api.domain.com", "sub.api.domain.com", and "domain.com",
//     with strict dot-boundary matching to prevent "evilgithub.com" matching "*.github.com".
func MatchHost(host, pattern string) bool {
	h := normalizeHost(host)
	p := normalizeHost(pattern)
	if h == "" || p == "" {
		return false
	}
	if p == "*" {
		return true
	}
	if h == p {
		return true
	}
	if strings.HasPrefix(p, "*.") {
		suffix := p[1:]     // e.g. ".domain.com"
		baseDomain := p[2:] // e.g. "domain.com"
		if h == baseDomain || strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

// CheckEgressAllowed evaluates a NetworkPolicySpec against a target host:
// 1. Ordered Egress rules are evaluated first (first MatchHost hit wins).
// 2. Legacy DenyHosts are evaluated next (matches are denied).
// 3. Legacy AllowHosts are evaluated next (matches are allowed).
// 4. Otherwise, falls back to DefaultAction ("Allow" -> true, else -> false).
func CheckEgressAllowed(policy v1alpha1.NetworkPolicySpec, host string) bool {
	h := normalizeHost(host)
	if h == "" {
		return false
	}

	// Ordered rules take precedence (first match wins)
	for _, rule := range policy.Egress {
		if !MatchHost(h, rule.Target) {
			continue
		}
		return strings.EqualFold(strings.TrimSpace(rule.Action), string(v1alpha1.NetworkDefaultAllow))
	}

	// Legacy deny rules
	for _, denyPattern := range policy.DenyHosts {
		if MatchHost(h, denyPattern) {
			return false
		}
	}

	// Legacy explicit allow rules
	for _, allowPattern := range policy.AllowHosts {
		if MatchHost(h, allowPattern) {
			return true
		}
	}

	// Default action fallback
	return strings.EqualFold(string(policy.DefaultAction), string(v1alpha1.NetworkDefaultAllow))
}
