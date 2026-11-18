package main

import (
	"testing"
)

func TestResolveE2BTemplateAPIFlag(t *testing.T) {
	cases := []struct {
		name     string
		explicit bool
		flagVal  bool
		gates    map[string]bool
		want     bool
	}{
		{name: "default enabled", explicit: false, flagVal: true, gates: nil, want: true},
		{name: "explicit --e2b-template-api=false disables", explicit: true, flagVal: false, gates: nil, want: false},
		{name: "rollback via --feature-gate=E2BTemplateAPI=false", explicit: false, flagVal: true, gates: map[string]bool{"E2BTemplateAPI": false}, want: false},
		{name: "explicit flag false overrides gate true", explicit: true, flagVal: false, gates: map[string]bool{"E2BTemplateAPI": true}, want: false},
		{name: "explicit flag true overridden by gate false", explicit: true, flagVal: true, gates: map[string]bool{"E2BTemplateAPI": false}, want: false},
		{name: "both true", explicit: true, flagVal: true, gates: map[string]bool{"E2BTemplateAPI": true}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveE2BTemplateAPIFlag(tc.explicit, tc.flagVal, tc.gates); got != tc.want {
				t.Fatalf("resolveE2BTemplateAPIFlag(%v, %v, %v) = %v, want %v", tc.explicit, tc.flagVal, tc.gates, got, tc.want)
			}
		})
	}
}
