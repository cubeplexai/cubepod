package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cubeplexai/cubepod/pkg/client"
)

func TestCLI_TemplateCommands(t *testing.T) {
	mux := http.NewServeMux()
	var (
		deletedPaths []string
		patchBodies  []map[string]bool
		aliasEscaped []string
	)

	// List templates
	mux.HandleFunc("GET /templates", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"templateID":  "tpl_test_1",
				"buildID":     "bld_test_1",
				"cpuCount":    2,
				"memoryMB":    2048,
				"diskSizeMB":  10240,
				"public":      false,
				"aliases":     []string{"test-alias"},
				"names":       []string{"test-alias"},
				"buildStatus": "ready",
			},
		})
	})

	// Get template by ID
	mux.HandleFunc("GET /templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "not-found-id" || id == "alias-target" || id == "org/tpl:v1" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"message":"template not found"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"templateID":  id,
			"buildID":     "bld_1",
			"cpuCount":    2,
			"memoryMB":    2048,
			"diskSizeMB":  10240,
			"public":      true,
			"aliases":     []string{"alias-1"},
			"names":       []string{"alias-1"},
			"buildStatus": "ready",
		})
	})

	// Get template by alias
	mux.HandleFunc("GET /templates/aliases/{alias}", func(w http.ResponseWriter, r *http.Request) {
		alias := r.PathValue("alias")
		aliasEscaped = append(aliasEscaped, r.RequestURI)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"templateID":  "tpl_resolved_by_alias",
			"buildID":     "bld_alias_1",
			"cpuCount":    4,
			"memoryMB":    4096,
			"diskSizeMB":  20480,
			"public":      false,
			"aliases":     []string{alias},
			"names":       []string{alias},
			"buildStatus": "ready",
		})
	})

	// Create template v3
	mux.HandleFunc("POST /v3/templates", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"templateID": "tpl_cli_created",
			"buildID":    "bld_cli_created",
			"aliases":    []string{"my-py"},
			"names":      []string{"my-py"},
		})
	})

	// Start build v2 (202 Accepted)
	mux.HandleFunc("POST /v2/templates/{templateID}/builds/{buildID}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	// Build status
	mux.HandleFunc("GET /templates/{templateID}/builds/{buildID}/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"templateID": r.PathValue("templateID"),
			"buildID":    r.PathValue("buildID"),
			"status":     "ready",
			"logs":       []string{},
			"logEntries": []interface{}{},
		})
	})

	// Update template visibility
	mux.HandleFunc("PATCH /templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]bool
		_ = json.NewDecoder(r.Body).Decode(&body)
		patchBodies = append(patchBodies, body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"names":["my-py"]}`))
	})

	// Delete template (204 No Content)
	mux.HandleFunc("DELETE /templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		deletedPaths = append(deletedPaths, r.URL.Path)
		force := r.URL.Query().Get("force")
		if r.PathValue("id") == "tpl-force-test" && force != "true" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	c := client.New(server.URL)
	ctx := context.Background()

	// 1. Template list (text)
	var out bytes.Buffer
	if err := runTemplate(ctx, c, []string{"list"}, false, &out); err != nil {
		t.Fatalf("runTemplate list failed: %v", err)
	}
	if !strings.Contains(out.String(), "tpl_test_1") || !strings.Contains(out.String(), "test-alias") {
		t.Fatalf("expected list output to contain template and alias, got: %s", out.String())
	}

	// 2. Template list (JSON)
	out.Reset()
	if err := runTemplate(ctx, c, []string{"list"}, true, &out); err != nil {
		t.Fatalf("runTemplate list json failed: %v", err)
	}
	if !strings.Contains(out.String(), `"templateID": "tpl_test_1"`) {
		t.Fatalf("expected JSON output for list, got: %s", out.String())
	}

	// 3. Template get by ID
	out.Reset()
	if err := runTemplate(ctx, c, []string{"get", "tpl_direct"}, false, &out); err != nil {
		t.Fatalf("runTemplate get direct failed: %v", err)
	}
	if !strings.Contains(out.String(), "Template ID: tpl_direct") {
		t.Fatalf("expected get output to contain Template ID, got: %s", out.String())
	}

	// 4. Template get fallback to alias on 404 (both simple and org/tpl:v1)
	out.Reset()
	if err := runTemplate(ctx, c, []string{"get", "alias-target"}, false, &out); err != nil {
		t.Fatalf("runTemplate get alias fallback failed: %v", err)
	}
	if !strings.Contains(out.String(), "Template ID: tpl_resolved_by_alias") {
		t.Fatalf("expected alias fallback resolution, got: %s", out.String())
	}

	out.Reset()
	if err := runTemplate(ctx, c, []string{"get", "org/tpl:v1"}, false, &out); err != nil {
		t.Fatalf("runTemplate get org/tpl:v1 fallback failed: %v", err)
	}
	if !strings.Contains(out.String(), "Template ID: tpl_resolved_by_alias") {
		t.Fatalf("expected alias fallback resolution for org/tpl:v1, got: %s", out.String())
	}
	if len(aliasEscaped) == 0 || aliasEscaped[len(aliasEscaped)-1] != "/templates/aliases/org%2Ftpl:v1" {
		t.Fatalf("expected URL-escaped alias path, got: %v", aliasEscaped)
	}

	// 5. Template get with --alias flag & unknown flag rejection
	out.Reset()
	if err := runTemplate(ctx, c, []string{"get", "--alias", "explicit-alias"}, false, &out); err != nil {
		t.Fatalf("runTemplate get --alias failed: %v", err)
	}
	if !strings.Contains(out.String(), "Template ID: tpl_resolved_by_alias") {
		t.Fatalf("expected --alias resolution, got: %s", out.String())
	}

	if err := runTemplate(ctx, c, []string{"get", "tpl_1", "--bogus"}, false, &out); err == nil {
		t.Fatal("expected error for unknown flag in template get, got nil")
	}

	// 6. Template create --from-image --alias
	out.Reset()
	createArgs := []string{
		"create",
		"--from-image", "python:3.13",
		"--alias", "my-py",
		"--cpu", "2",
		"--memory-mb", "2048",
		"--disk-mb", "10240",
	}
	if err := runTemplate(ctx, c, createArgs, false, &out); err != nil {
		t.Fatalf("runTemplate create failed: %v", err)
	}
	if !strings.Contains(out.String(), "Created template: tpl_cli_created") {
		t.Fatalf("expected create confirmation, got: %s", out.String())
	}

	// 7. Template create missing required flag & unknown flag
	if err := runTemplate(ctx, c, []string{"create", "--from-image", "python:3.13"}, false, &out); err == nil {
		t.Fatal("expected error on missing alias, got nil")
	}
	if err := runTemplate(ctx, c, []string{"create", "--from-image", "python:3.13", "--alias", "a", "--unknown"}, false, &out); err == nil {
		t.Fatal("expected error on unknown flag in create, got nil")
	}

	// 8. Template update --public=false and --public=true
	out.Reset()
	if err := runTemplate(ctx, c, []string{"update", "tpl_upd_1", "--public=false"}, false, &out); err != nil {
		t.Fatalf("runTemplate update --public=false failed: %v", err)
	}
	if !strings.Contains(out.String(), "Updated template tpl_upd_1 (public: false)") {
		t.Fatalf("unexpected update output: %s", out.String())
	}
	if len(patchBodies) != 1 || patchBodies[0]["public"] != false {
		t.Fatalf("expected PATCH public=false, got: %+v", patchBodies)
	}

	out.Reset()
	if err := runTemplate(ctx, c, []string{"update", "--public=true", "tpl_upd_1"}, true, &out); err != nil {
		t.Fatalf("runTemplate update --public=true json failed: %v", err)
	}
	if len(patchBodies) != 2 || patchBodies[1]["public"] != true {
		t.Fatalf("expected PATCH public=true, got: %+v", patchBodies)
	}
	if !strings.Contains(out.String(), `"public": true`) {
		t.Fatalf("expected JSON update output, got: %s", out.String())
	}

	if err := runTemplate(ctx, c, []string{"update", "tpl_upd_1"}, false, &out); err == nil {
		t.Fatal("expected error when --public is omitted, got nil")
	}
	if err := runTemplate(ctx, c, []string{"update", "tpl_upd_1", "--public=false", "--extra"}, false, &out); err == nil {
		t.Fatal("expected error on unknown flag in update, got nil")
	}

	// 9. Template delete unknown flag MUST NOT execute destructive DELETE
	if err := runTemplate(ctx, c, []string{"delete", "tpl-do-not-delete", "--dry-run"}, false, &out); err == nil {
		t.Fatal("expected error for unknown flag --dry-run in template delete, got nil")
	}
	if err := runTemplate(ctx, c, []string{"delete", "--dry-run", "tpl-do-not-delete"}, false, &out); err == nil {
		t.Fatal("expected error for leading unknown flag --dry-run in template delete, got nil")
	}
	if len(deletedPaths) != 0 {
		t.Fatalf("expected zero DELETE requests on unknown flag, got: %v", deletedPaths)
	}

	// 10. Template delete with --force (204 No Content)
	out.Reset()
	if err := runTemplate(ctx, c, []string{"delete", "tpl-force-test", "--force"}, false, &out); err != nil {
		t.Fatalf("runTemplate delete failed: %v", err)
	}
	if !strings.Contains(out.String(), "Deleted template: tpl-force-test") {
		t.Fatalf("expected delete confirmation, got: %s", out.String())
	}
}
