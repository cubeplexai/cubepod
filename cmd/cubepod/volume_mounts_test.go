package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/client"
)

func TestParseVolumeMount(t *testing.T) {
	tests := []struct {
		spec    string
		want    client.SandboxVolumeMount
		wantErr string
	}{
		{spec: "datasets:/data", want: client.SandboxVolumeMount{Name: "datasets", Path: "/data"}},
		{spec: "ws:/workspace", want: client.SandboxVolumeMount{Name: "ws", Path: "/workspace"}},
		{spec: "a:/mnt/with:colon", want: client.SandboxVolumeMount{Name: "a", Path: "/mnt/with:colon"}},
		{spec: " datasets : /data ", want: client.SandboxVolumeMount{Name: "datasets", Path: "/data"}},
		{spec: "foo", wantErr: "expected NAME:PATH"},
		{spec: ":/data", wantErr: "volume name must not be empty"},
		{spec: "foo:", wantErr: "mount path must not be empty"},
		{spec: "foo:relative", wantErr: "must be absolute"},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			got, err := parseVolumeMount(tt.spec)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseVolumeMount(%q) error = %v, want containing %q", tt.spec, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseVolumeMount(%q) unexpected error: %v", tt.spec, err)
			}
			if got != tt.want {
				t.Errorf("parseVolumeMount(%q) = %+v, want %+v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestParseSandboxCreateArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    client.CreateSandboxRequest
		wantErr bool
	}{
		{
			name: "defaults",
			want: client.CreateSandboxRequest{TemplateID: "cubeplex-ubuntu24-desktop"},
		},
		{
			name: "repeated volume mounts",
			args: []string{"--template", "base", "--volume-mount", "datasets:/data", "--volume-mount=assets:/mnt/assets"},
			want: client.CreateSandboxRequest{
				TemplateID: "base",
				VolumeMounts: []client.SandboxVolumeMount{
					{Name: "datasets", Path: "/data"},
					{Name: "assets", Path: "/mnt/assets"},
				},
			},
		},
		{
			name: "legacy volume key",
			args: []string{"--volume-key", "legacy"},
			want: client.CreateSandboxRequest{TemplateID: "cubeplex-ubuntu24-desktop", VolumeKey: "legacy"},
		},
		{
			name:    "invalid volume mount",
			args:    []string{"--volume-mount", "datasets:data"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSandboxCreateArgs(tt.args, io.Discard)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got request %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("request = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestMCP_CreateSandboxVolumeMounts(t *testing.T) {
	type capturedRequest struct {
		body   map[string]any
		apiKey string
	}
	newGateway := func(t *testing.T, captured *[]capturedRequest) *httptest.Server {
		t.Helper()
		mux := http.NewServeMux()
		mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create request: %v", err)
			}
			*captured = append(*captured, capturedRequest{body: body, apiKey: r.Header.Get("X-API-KEY")})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"sandboxID":"cpod-mount-1","templateID":"base","state":"Running"}`)
		})
		return httptest.NewServer(mux)
	}
	call := func(args string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"cubepod_create_sandbox","arguments":` + args + `}}`
	}
	decodeResult := func(t *testing.T, resp jsonRPCResponse) CallToolResult {
		t.Helper()
		buf, _ := json.Marshal(resp.Result)
		var res CallToolResult
		if err := json.Unmarshal(buf, &res); err != nil {
			t.Fatalf("decode tool result: %v", err)
		}
		return res
	}

	t.Run("forwards volume mounts with API key", func(t *testing.T) {
		var captured []capturedRequest
		server := newGateway(t, &captured)
		defer server.Close()
		c := client.New(server.URL, client.WithAPIKey("test-key"), client.WithTimeout(5*time.Second))

		responses := runMCPExchange(t, c, call(`{"templateID":"base","volumeKey":"legacy","volumeMounts":[{"name":" datasets","path":"/data "}]}`))
		if len(responses) != 1 {
			t.Fatalf("expected 1 response, got %d", len(responses))
		}
		if res := decodeResult(t, responses[0]); res.IsError {
			t.Fatalf("unexpected tool error: %+v", res)
		}
		if len(captured) != 1 {
			t.Fatalf("expected 1 create request, got %d", len(captured))
		}
		if captured[0].apiKey != "test-key" {
			t.Errorf("X-API-KEY = %q, want test-key", captured[0].apiKey)
		}
		wantMounts := []any{map[string]any{"name": "datasets", "path": "/data"}}
		if got := captured[0].body["volumeMounts"]; !reflect.DeepEqual(got, wantMounts) {
			t.Errorf("volumeMounts = %#v, want %#v", got, wantMounts)
		}
		wantExt := map[string]any{"volumeKey": "legacy"}
		if got := captured[0].body["extension"]; !reflect.DeepEqual(got, wantExt) {
			t.Errorf("extension = %#v, want %#v", got, wantExt)
		}
	})

	t.Run("omits volume mounts when empty", func(t *testing.T) {
		var captured []capturedRequest
		server := newGateway(t, &captured)
		defer server.Close()
		c := client.New(server.URL, client.WithTimeout(5*time.Second))

		responses := runMCPExchange(t, c, call(`{"templateID":"base","volumeMounts":[]}`))
		if res := decodeResult(t, responses[0]); res.IsError {
			t.Fatalf("unexpected tool error: %+v", res)
		}
		if len(captured) != 1 {
			t.Fatalf("expected 1 create request, got %d", len(captured))
		}
		if _, ok := captured[0].body["volumeMounts"]; ok {
			t.Errorf("volumeMounts should be omitted, got body %#v", captured[0].body)
		}
	})

	errorCases := []struct {
		name    string
		apiKey  string
		args    string
		wantSub string
	}{
		{name: "requires API key", args: `{"volumeMounts":[{"name":"datasets","path":"/data"}]}`, wantSub: "require an API key"},
		{name: "rejects relative path", apiKey: "k", args: `{"volumeMounts":[{"name":"datasets","path":"data"}]}`, wantSub: "must be absolute"},
		{name: "rejects empty name", apiKey: "k", args: `{"volumeMounts":[{"name":"","path":"/data"}]}`, wantSub: "volume name must not be empty"},
	}
	for _, tt := range errorCases {
		t.Run(tt.name, func(t *testing.T) {
			var captured []capturedRequest
			server := newGateway(t, &captured)
			defer server.Close()
			opts := []client.Option{client.WithTimeout(5 * time.Second)}
			if tt.apiKey != "" {
				opts = append(opts, client.WithAPIKey(tt.apiKey))
			}
			c := client.New(server.URL, opts...)

			responses := runMCPExchange(t, c, call(tt.args))
			res := decodeResult(t, responses[0])
			if !res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, tt.wantSub) {
				t.Fatalf("expected tool error containing %q, got %+v", tt.wantSub, res)
			}
			if len(captured) != 0 {
				t.Errorf("gateway should not be called, got %d requests", len(captured))
			}
		})
	}
}

func TestMCP_CreateSandboxSchemaAdvertisesVolumeMounts(t *testing.T) {
	var create *MCPTool
	for i := range mcpTools {
		if mcpTools[i].Name == "cubepod_create_sandbox" {
			create = &mcpTools[i]
		}
	}
	if create == nil {
		t.Fatal("cubepod_create_sandbox tool not found")
	}
	prop, ok := create.InputSchema.Properties["volumeMounts"]
	if !ok {
		t.Fatal("volumeMounts property missing")
	}
	if prop.Type != "array" || prop.Items == nil || prop.Items.Type != "object" {
		t.Fatalf("unexpected volumeMounts schema: %+v", prop)
	}
	if !reflect.DeepEqual(prop.Items.Required, []string{"name", "path"}) {
		t.Errorf("items.required = %v, want [name path]", prop.Items.Required)
	}
	if _, ok := create.InputSchema.Properties["volumeKey"]; !ok {
		t.Error("volumeKey property missing")
	}
}
