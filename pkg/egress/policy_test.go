package egress

import (
	"testing"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

func TestCheckEgressAllowed_OrderedEgressRules(t *testing.T) {
	tests := []struct {
		name   string
		policy v1alpha1.NetworkPolicySpec
		host   string
		want   bool
	}{
		{
			name: "allow before wildcard deny",
			policy: v1alpha1.NetworkPolicySpec{
				DefaultAction: v1alpha1.NetworkDefaultDeny,
				Egress: []v1alpha1.NetworkEgressRule{
					{Action: "allow", Target: "api.github.com"},
					{Action: "deny", Target: "*"},
				},
			},
			host: "API.GITHUB.COM:443",
			want: true,
		},
		{
			name: "wildcard deny blocks non-matching allow",
			policy: v1alpha1.NetworkPolicySpec{
				DefaultAction: v1alpha1.NetworkDefaultAllow,
				Egress: []v1alpha1.NetworkEgressRule{
					{Action: "allow", Target: "api.github.com"},
					{Action: "deny", Target: "*"},
				},
			},
			host: "evil.example.com",
			want: false,
		},
		{
			name: "deny before wildcard allow blocks specific subdomain",
			policy: v1alpha1.NetworkPolicySpec{
				DefaultAction: v1alpha1.NetworkDefaultDeny,
				Egress: []v1alpha1.NetworkEgressRule{
					{Action: "deny", Target: "secret.corp.example"},
					{Action: "allow", Target: "*.corp.example"},
				},
			},
			host: "secret.corp.example",
			want: false,
		},
		{
			name: "deny before wildcard allow permits sibling subdomain",
			policy: v1alpha1.NetworkPolicySpec{
				DefaultAction: v1alpha1.NetworkDefaultDeny,
				Egress: []v1alpha1.NetworkEgressRule{
					{Action: "deny", Target: "secret.corp.example"},
					{Action: "allow", Target: "*.corp.example"},
				},
			},
			host: "docs.corp.example",
			want: true,
		},
		{
			name: "ordered rules override legacy lists on match",
			policy: v1alpha1.NetworkPolicySpec{
				DefaultAction: v1alpha1.NetworkDefaultDeny,
				DenyHosts:     []string{"api.openai.com"},
				AllowHosts:    []string{"pypi.org"},
				Egress: []v1alpha1.NetworkEgressRule{
					{Action: "ALLOW", Target: "api.openai.com"},
					{Action: "deny", Target: "pypi.org"},
				},
			},
			host: "api.openai.com",
			want: true,
		},
		{
			name: "ordered deny beats legacy allow",
			policy: v1alpha1.NetworkPolicySpec{
				DefaultAction: v1alpha1.NetworkDefaultAllow,
				AllowHosts:    []string{"pypi.org"},
				Egress: []v1alpha1.NetworkEgressRule{
					{Action: "deny", Target: "pypi.org"},
				},
			},
			host: "pypi.org",
			want: false,
		},
		{
			name: "falls through to legacy DenyHosts and AllowHosts",
			policy: v1alpha1.NetworkPolicySpec{
				DefaultAction: v1alpha1.NetworkDefaultDeny,
				DenyHosts:     []string{"blocked.example"},
				AllowHosts:    []string{"allowed.example"},
				Egress: []v1alpha1.NetworkEgressRule{
					{Action: "allow", Target: "specific.example"},
				},
			},
			host: "allowed.example",
			want: true,
		},
		{
			name: "falls through to DefaultAction Allow when unmatched",
			policy: v1alpha1.NetworkPolicySpec{
				DefaultAction: v1alpha1.NetworkDefaultAllow,
				Egress: []v1alpha1.NetworkEgressRule{
					{Action: "deny", Target: "blocked.example"},
				},
			},
			host: "unmatched.example",
			want: true,
		},
		{
			name: "unknown rule action fails closed",
			policy: v1alpha1.NetworkPolicySpec{
				DefaultAction: v1alpha1.NetworkDefaultAllow,
				Egress: []v1alpha1.NetworkEgressRule{
					{Action: "bogus", Target: "example.com"},
				},
			},
			host: "example.com",
			want: false,
		},
		{
			name: "empty host denied",
			policy: v1alpha1.NetworkPolicySpec{
				DefaultAction: v1alpha1.NetworkDefaultAllow,
				Egress: []v1alpha1.NetworkEgressRule{
					{Action: "allow", Target: "*"},
				},
			},
			host: "   ",
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckEgressAllowed(tc.policy, tc.host); got != tc.want {
				t.Fatalf("CheckEgressAllowed(%+v, %q) = %v, want %v", tc.policy, tc.host, got, tc.want)
			}
		})
	}
}
