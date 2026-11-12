package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/gateway"
)

// Upstream E2B Python SDK (v2.63.0) Model Representations for Conformance Verification

type E2BCreateResponse struct {
	TemplateID  string `json:"templateID"`
	SandboxID   string `json:"sandboxID"`
	ClientID    string `json:"clientID"`
	EnvdVersion string `json:"envdVersion"`
}

type E2BListedSandbox struct {
	TemplateID  string    `json:"templateID"`
	SandboxID   string    `json:"sandboxID"`
	ClientID    string    `json:"clientID"`
	StartedAt   time.Time `json:"startedAt"`
	EndAt       time.Time `json:"endAt"`
	CPUCount    int       `json:"cpuCount"`
	MemoryMB    int       `json:"memoryMB"`
	DiskSizeMB  int       `json:"diskSizeMB"`
	State       string    `json:"state"`
	EnvdVersion string    `json:"envdVersion"`
}

type E2BSandboxDetail struct {
	TemplateID  string    `json:"templateID"`
	SandboxID   string    `json:"sandboxID"`
	ClientID    string    `json:"clientID"`
	StartedAt   time.Time `json:"startedAt"`
	EndAt       time.Time `json:"endAt"`
	EnvdVersion string    `json:"envdVersion"`
	CPUCount    int       `json:"cpuCount"`
	MemoryMB    int       `json:"memoryMB"`
	DiskSizeMB  int       `json:"diskSizeMB"`
	State       string    `json:"state"`
}

func TestE2BWireConformance_LifecycleAndSchemas(t *testing.T) {
	reconciler := controller.NewReconciler()
	gw := gateway.NewServer(reconciler)
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	client := srv.Client()

	// 1. Verify POST /sandboxes with upstream E2B shape (templateID: "base", envVars, metadata, timeout)
	createPayload := []byte(`{
		"templateID": "base",
		"timeout": 300,
		"metadata": {"project": "test-eval", "user_id": "u-123"},
		"envVars": {"TEST_ENV": "1"}
	}`)
	resp, err := client.Post(srv.URL+"/sandboxes", "application/json", bytes.NewReader(createPayload))
	if err != nil {
		t.Fatalf("POST /sandboxes failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", resp.StatusCode)
	}

	var rawMap map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rawMap); err != nil {
		t.Fatalf("failed to decode create response json: %v", err)
	}

	// Verify required E2B SDK fields exist
	requiredCreateFields := []string{"templateID", "sandboxID", "clientID", "envdVersion"}
	for _, f := range requiredCreateFields {
		val, ok := rawMap[f]
		if !ok || val == "" {
			t.Fatalf("missing required E2B SDK field %q in POST /sandboxes response", f)
		}
	}

	rawBytes, _ := json.Marshal(rawMap)
	var e2bCreated E2BCreateResponse
	if err := json.Unmarshal(rawBytes, &e2bCreated); err != nil {
		t.Fatalf("failed to parse as upstream E2BCreateResponse: %v", err)
	}
	if e2bCreated.ClientID == "" || e2bCreated.EnvdVersion == "" {
		t.Fatalf("expected non-empty clientID and envdVersion, got %+v", e2bCreated)
	}

	sandboxID := e2bCreated.SandboxID

	// 2. Verify POST /sandboxes/{id}/refreshes with {"duration": 600}
	refreshPayload := []byte(`{"duration": 600}`)
	refreshResp, err := client.Post(srv.URL+"/sandboxes/"+sandboxID+"/refreshes", "application/json", bytes.NewReader(refreshPayload))
	if err != nil {
		t.Fatalf("POST /sandboxes/{id}/refreshes failed: %v", err)
	}
	_ = refreshResp.Body.Close()
	if refreshResp.StatusCode != http.StatusNoContent && refreshResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 204 or 200 from /refreshes, got %d", refreshResp.StatusCode)
	}

	// 3. Verify GET /sandboxes/{id} conforms to E2BSandboxDetail
	detailResp, err := client.Get(srv.URL + "/sandboxes/" + sandboxID)
	if err != nil {
		t.Fatalf("GET /sandboxes/{id} failed: %v", err)
	}
	defer detailResp.Body.Close()
	if detailResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from GET /sandboxes/{id}, got %d", detailResp.StatusCode)
	}

	var detailMap map[string]any
	if err := json.NewDecoder(detailResp.Body).Decode(&detailMap); err != nil {
		t.Fatalf("failed to decode detail map: %v", err)
	}
	requiredDetailFields := []string{"templateID", "sandboxID", "clientID", "startedAt", "endAt", "envdVersion", "cpuCount", "memoryMB", "diskSizeMB"}
	for _, f := range requiredDetailFields {
		if _, ok := detailMap[f]; !ok {
			t.Fatalf("missing required E2B SandboxDetail field %q in GET response", f)
		}
	}

	// 4. Verify GET /sandboxes conforms to []E2BListedSandbox
	listResp, err := client.Get(srv.URL + "/sandboxes")
	if err != nil {
		t.Fatalf("GET /sandboxes failed: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from GET /sandboxes, got %d", listResp.StatusCode)
	}

	var listItems []map[string]any
	if err := json.NewDecoder(listResp.Body).Decode(&listItems); err != nil {
		t.Fatalf("failed to decode list items: %v", err)
	}
	if len(listItems) == 0 {
		t.Fatalf("expected at least 1 sandbox in list response")
	}
	for _, f := range requiredDetailFields {
		if _, ok := listItems[0][f]; !ok {
			t.Fatalf("missing required E2B ListedSandbox field %q in list response", f)
		}
	}
}
