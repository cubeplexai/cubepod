package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/gateway"
)

func TestCEP0019_GatewayTemplateAPI(t *testing.T) {
	// 1. gate off GET /templates stays wrapped
	t.Run("gate off GET /templates stays wrapped", func(t *testing.T) {
		r := controller.NewReconciler()
		s := gateway.NewServer(r)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		resp, err := http.Get(ts.URL + "/templates")
		if err != nil {
			t.Fatalf("GET /templates failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		body, _ := io.ReadAll(resp.Body)
		var wrapped map[string]any
		if err := json.Unmarshal(body, &wrapped); err != nil {
			t.Fatalf("expected wrapped JSON object, got err: %v, body: %s", err, string(body))
		}
		tpls, ok := wrapped["templates"]
		if !ok || tpls == nil {
			t.Fatalf("expected 'templates' key in wrapped response, got: %s", string(body))
		}

		// Verify new routes are NOT registered when gate is off
		postResp, err := http.Post(ts.URL+"/templates", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("POST /templates failed: %v", err)
		}
		defer postResp.Body.Close()
		if postResp.StatusCode != http.StatusNotFound && postResp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("expected 404/405 for POST /templates with gate off, got %d", postResp.StatusCode)
		}

		aliasResp, err := http.Get(ts.URL + "/templates/aliases/base")
		if err != nil {
			t.Fatalf("GET /templates/aliases/base failed: %v", err)
		}
		defer aliasResp.Body.Close()
		if aliasResp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 for alias route with gate off, got %d", aliasResp.StatusCode)
		}
	})

	// 2. list get alias delete mark public
	t.Run("list get alias delete mark public", func(t *testing.T) {
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		s := gateway.NewServer(r)
		s.SetE2BTemplateAPI(true)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		// A. GET /templates returns JSON array
		resp, err := http.Get(ts.URL + "/templates?teamID=some-team")
		if err != nil {
			t.Fatalf("GET /templates failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var list []gateway.E2BTemplateResponse
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatalf("failed to decode template array: %v", err)
		}
		if len(list) < 3 {
			t.Fatalf("expected at least 3 seeded templates, got %d", len(list))
		}

		// B. POST /v3/templates create
		createBody := controller.CreateTemplateRequest{
			Name:          "custom-app:v1",
			Alias:         "custom-alias",
			CPUCount:      2,
			MemoryMB:      2048,
			MinFreeDiskMB: 10240,
		}
		bodyBytes, _ := json.Marshal(createBody)

		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v3/templates", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "idem-test-key-1")
		createResp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("create template failed: %v", err)
		}
		defer createResp.Body.Close()
		if createResp.StatusCode != http.StatusAccepted {
			t.Fatalf("expected 202 Accepted, got %d", createResp.StatusCode)
		}

		var created controller.CreateTemplateResponse
		if err := json.NewDecoder(createResp.Body).Decode(&created); err != nil {
			t.Fatalf("failed to decode create response: %v", err)
		}
		if !strings.HasPrefix(created.TemplateID, "tpl_") {
			t.Fatalf("expected tpl_ prefix, got %s", created.TemplateID)
		}
		if !strings.HasPrefix(created.BuildID, "bld_") {
			t.Fatalf("expected bld_ prefix, got %s", created.BuildID)
		}

		// Idempotency replay with same body returns identical response
		reqReplay, _ := http.NewRequest(http.MethodPost, ts.URL+"/v3/templates", bytes.NewReader(bodyBytes))
		reqReplay.Header.Set("Content-Type", "application/json")
		reqReplay.Header.Set("Idempotency-Key", "idem-test-key-1")
		replayResp, err := http.DefaultClient.Do(reqReplay)
		if err != nil {
			t.Fatalf("replay failed: %v", err)
		}
		defer replayResp.Body.Close()
		if replayResp.StatusCode != http.StatusAccepted {
			t.Fatalf("expected 202 for idempotency replay, got %d", replayResp.StatusCode)
		}
		var replayed controller.CreateTemplateResponse
		_ = json.NewDecoder(replayResp.Body).Decode(&replayed)
		if replayed.TemplateID != created.TemplateID || replayed.BuildID != created.BuildID {
			t.Fatalf("idempotency mismatch: got %v, want %v", replayed, created)
		}

		// Idempotency mismatch with different body returns 409
		diffBodyBytes, _ := json.Marshal(controller.CreateTemplateRequest{Name: "different-name"})
		reqDiff, _ := http.NewRequest(http.MethodPost, ts.URL+"/v3/templates", bytes.NewReader(diffBodyBytes))
		reqDiff.Header.Set("Content-Type", "application/json")
		reqDiff.Header.Set("Idempotency-Key", "idem-test-key-1")
		diffResp, err := http.DefaultClient.Do(reqDiff)
		if err != nil {
			t.Fatalf("diff idempotency request failed: %v", err)
		}
		defer diffResp.Body.Close()
		if diffResp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 for idempotency mismatch, got %d", diffResp.StatusCode)
		}

		// Fast-path build to make it ready
		startReq := controller.StartBuildRequest{
			FromImage: "busybox:latest",
		}
		startBytes, _ := json.Marshal(startReq)
		buildURL := fmt.Sprintf("%s/v2/templates/%s/builds/%s", ts.URL, created.TemplateID, created.BuildID)
		bResp, err := http.Post(buildURL, "application/json", bytes.NewReader(startBytes))
		if err != nil {
			t.Fatalf("start build failed: %v", err)
		}
		defer bResp.Body.Close()
		if bResp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 for start build, got %d", bResp.StatusCode)
		}

		// C. GET /templates/{templateID}
		getResp, err := http.Get(ts.URL + "/templates/" + created.TemplateID)
		if err != nil {
			t.Fatalf("get template failed: %v", err)
		}
		defer getResp.Body.Close()
		if getResp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", getResp.StatusCode)
		}
		var gotTpl gateway.E2BTemplateResponse
		if err := json.NewDecoder(getResp.Body).Decode(&gotTpl); err != nil {
			t.Fatalf("failed to decode template: %v", err)
		}
		if gotTpl.TemplateID != created.TemplateID {
			t.Fatalf("expected templateID %s, got %s", created.TemplateID, gotTpl.TemplateID)
		}
		if len(gotTpl.Builds) == 0 || gotTpl.Builds[0].BuildID != created.BuildID {
			t.Fatalf("expected build in builds, got %v", gotTpl.Builds)
		}
		if gotTpl.BuildStatus != "ready" {
			t.Fatalf("expected buildStatus ready, got %s", gotTpl.BuildStatus)
		}

		// Unknown templateID is 404
		notFoundResp, err := http.Get(ts.URL + "/templates/tpl_unknown999")
		if err != nil {
			t.Fatalf("get unknown failed: %v", err)
		}
		defer notFoundResp.Body.Close()
		if notFoundResp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", notFoundResp.StatusCode)
		}

		// D. GET /templates/aliases/{alias}
		for _, alias := range []string{"custom-app", "custom-app:v1", "custom-alias"} {
			aResp, err := http.Get(ts.URL + "/templates/aliases/" + alias)
			if err != nil {
				t.Fatalf("get alias %q failed: %v", alias, err)
			}
			if aResp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200 for alias %q, got %d", alias, aResp.StatusCode)
			}
			var aliasTpl gateway.E2BTemplateResponse
			_ = json.NewDecoder(aResp.Body).Decode(&aliasTpl)
			if aliasTpl.TemplateID != created.TemplateID {
				t.Fatalf("expected templateID %s, got %s", created.TemplateID, aliasTpl.TemplateID)
			}
			aResp.Body.Close()
		}

		// Unknown alias is 404
		noAliasResp, err := http.Get(ts.URL + "/templates/aliases/nonexistent-alias")
		if err != nil {
			t.Fatalf("get nonexistent alias failed: %v", err)
		}
		defer noAliasResp.Body.Close()
		if noAliasResp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 for nonexistent alias, got %d", noAliasResp.StatusCode)
		}

		// E. PATCH /templates/{templateID} with {"public":true}
		patchBody := strings.NewReader(`{"public":true}`)
		pReq, _ := http.NewRequest(http.MethodPatch, ts.URL+"/templates/"+created.TemplateID, patchBody)
		pReq.Header.Set("Content-Type", "application/json")
		pResp, err := http.DefaultClient.Do(pReq)
		if err != nil {
			t.Fatalf("patch v1 failed: %v", err)
		}
		defer pResp.Body.Close()
		if pResp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 for patch v1, got %d", pResp.StatusCode)
		}

		// PATCH /v2/templates/{templateID} with {"public":true} returns {"names":[...]}
		pReqV2, _ := http.NewRequest(http.MethodPatch, ts.URL+"/v2/templates/"+created.TemplateID, strings.NewReader(`{"public":true}`))
		pReqV2.Header.Set("Content-Type", "application/json")
		pV2Resp, err := http.DefaultClient.Do(pReqV2)
		if err != nil {
			t.Fatalf("patch v2 failed: %v", err)
		}
		defer pV2Resp.Body.Close()
		if pV2Resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 for patch v2, got %d", pV2Resp.StatusCode)
		}
		var v2Data struct {
			Names []string `json:"names"`
		}
		if err := json.NewDecoder(pV2Resp.Body).Decode(&v2Data); err != nil {
			t.Fatalf("failed to decode v2 patch response: %v", err)
		}
		if len(v2Data.Names) == 0 {
			t.Fatalf("expected non-empty names in v2 patch response")
		}

		// F. DELETE /templates/{templateID}
		delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/templates/"+created.TemplateID, nil)
		delResp, err := http.DefaultClient.Do(delReq)
		if err != nil {
			t.Fatalf("delete failed: %v", err)
		}
		defer delResp.Body.Close()
		if delResp.StatusCode != http.StatusOK && delResp.StatusCode != http.StatusNoContent {
			t.Fatalf("expected 200 or 204 for delete, got %d", delResp.StatusCode)
		}

		// Subsequent GET returns 404
		afterDelResp, err := http.Get(ts.URL + "/templates/" + created.TemplateID)
		if err != nil {
			t.Fatalf("get after delete failed: %v", err)
		}
		defer afterDelResp.Body.Close()
		if afterDelResp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 after delete, got %d", afterDelResp.StatusCode)
		}
	})

	// 3. fromImage empty steps ready does not start a builder
	t.Run("fromImage empty steps ready does not start a builder", func(t *testing.T) {
		fakeBuilder := controller.NewFakeImageBuilder()
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
			controller.WithImageBuilder(fakeBuilder),
		)
		s := gateway.NewServer(r)
		s.SetE2BTemplateAPI(true)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		createReq, _ := http.Post(ts.URL+"/templates", "application/json", strings.NewReader(`{"name":"nobuild-tpl"}`))
		var created controller.CreateTemplateResponse
		_ = json.NewDecoder(createReq.Body).Decode(&created)
		createReq.Body.Close()

		buildBody := controller.StartBuildRequest{
			FromImage: "python:3.13",
			Steps:     nil, // empty steps!
		}
		bData, _ := json.Marshal(buildBody)
		startResp, err := http.Post(fmt.Sprintf("%s/v2/templates/%s/builds/%s", ts.URL, created.TemplateID, created.BuildID), "application/json", bytes.NewReader(bData))
		if err != nil {
			t.Fatalf("start build failed: %v", err)
		}
		defer startResp.Body.Close()
		if startResp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", startResp.StatusCode)
		}

		if len(fakeBuilder.BuildCalls) != 0 {
			t.Fatalf("expected builder not to be started, but got %d calls", len(fakeBuilder.BuildCalls))
		}

		getResp, err := http.Get(ts.URL + "/templates/" + created.TemplateID)
		if err != nil {
			t.Fatalf("get template failed: %v", err)
		}
		defer getResp.Body.Close()
		var tpl gateway.E2BTemplateResponse
		_ = json.NewDecoder(getResp.Body).Decode(&tpl)
		if tpl.BuildStatus != "ready" {
			t.Fatalf("expected buildStatus ready, got %s", tpl.BuildStatus)
		}
	})

	// 4. sandbox create uses cpuCount memoryMB diskSizeMB
	t.Run("sandbox create uses cpuCount memoryMB diskSizeMB", func(t *testing.T) {
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		s := gateway.NewServer(r)
		s.SetE2BTemplateAPI(true)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		// Create template with custom resources
		cBody := controller.CreateTemplateRequest{
			Name:          "custom-sized",
			CPUCount:      4,
			MemoryMB:      8192,
			MinFreeDiskMB: 20480,
		}
		cBytes, _ := json.Marshal(cBody)
		cResp, err := http.Post(ts.URL+"/v3/templates", "application/json", bytes.NewReader(cBytes))
		if err != nil {
			t.Fatalf("create template failed: %v", err)
		}
		var created controller.CreateTemplateResponse
		_ = json.NewDecoder(cResp.Body).Decode(&created)
		cResp.Body.Close()

		// Fast path start build
		bBody := controller.StartBuildRequest{FromImage: "custom-image:1.0"}
		bBytes, _ := json.Marshal(bBody)
		bResp, err := http.Post(fmt.Sprintf("%s/v2/templates/%s/builds/%s", ts.URL, created.TemplateID, created.BuildID), "application/json", bytes.NewReader(bBytes))
		if err != nil {
			t.Fatalf("start build failed: %v", err)
		}
		bResp.Body.Close()

		// Create sandbox using templateID
		sbBody := strings.NewReader(fmt.Sprintf(`{"templateID":"%s"}`, created.TemplateID))
		sbResp, err := http.Post(ts.URL+"/sandboxes", "application/json", sbBody)
		if err != nil {
			t.Fatalf("sandbox create failed: %v", err)
		}
		defer sbResp.Body.Close()
		if sbResp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 Created, got %d", sbResp.StatusCode)
		}

		var sb gateway.SandboxRecord
		if err := json.NewDecoder(sbResp.Body).Decode(&sb); err != nil {
			t.Fatalf("decode sandbox record failed: %v", err)
		}
		if sb.CPUCount != 4 {
			t.Fatalf("expected CPUCount 4, got %d", sb.CPUCount)
		}
		if sb.MemoryMB != 8192 {
			t.Fatalf("expected MemoryMB 8192, got %d", sb.MemoryMB)
		}
		if sb.DiskSizeMB != 20480 {
			t.Fatalf("expected DiskSizeMB 20480, got %d", sb.DiskSizeMB)
		}
	})

	// 5. aliases base code-interpreter code-interpreter-v1 python
	t.Run("aliases base code-interpreter code-interpreter-v1 python", func(t *testing.T) {
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		s := gateway.NewServer(r)
		s.SetE2BTemplateAPI(true)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		aliases := []string{"", "base", "code-interpreter", "code-interpreter-v1", "python"}
		for _, alias := range aliases {
			sbBody := strings.NewReader(fmt.Sprintf(`{"templateID":"%s"}`, alias))
			sbResp, err := http.Post(ts.URL+"/sandboxes", "application/json", sbBody)
			if err != nil {
				t.Fatalf("create sandbox for alias %q failed: %v", alias, err)
			}
			if sbResp.StatusCode != http.StatusCreated {
				t.Fatalf("expected 201 Created for alias %q, got %d", alias, sbResp.StatusCode)
			}
			var sb gateway.SandboxRecord
			_ = json.NewDecoder(sbResp.Body).Decode(&sb)
			sbResp.Body.Close()

			if sb.CPUCount != 2 || sb.MemoryMB != 2048 || sb.DiskSizeMB != 10240 {
				t.Fatalf("expected 2/2048/10240 for alias %q, got %d/%d/%d", alias, sb.CPUCount, sb.MemoryMB, sb.DiskSizeMB)
			}
		}
	})

	// 6. gate off pre-CEP resolution
	t.Run("gate off pre-CEP resolution", func(t *testing.T) {
		r := controller.NewReconciler()
		s := gateway.NewServer(r)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		aliases := []string{"", "base", "code-interpreter", "code-interpreter-v1", "python", "alpine:3.19"}
		for _, alias := range aliases {
			sbBody := strings.NewReader(fmt.Sprintf(`{"templateID":"%s"}`, alias))
			sbResp, err := http.Post(ts.URL+"/sandboxes", "application/json", sbBody)
			if err != nil {
				t.Fatalf("create sandbox with gate off for %q failed: %v", alias, err)
			}
			if sbResp.StatusCode != http.StatusCreated {
				t.Fatalf("expected 201 Created with gate off for %q, got %d", alias, sbResp.StatusCode)
			}
			var sb gateway.SandboxRecord
			_ = json.NewDecoder(sbResp.Body).Decode(&sb)
			sbResp.Body.Close()

			if sb.CPUCount != 2 || sb.MemoryMB != 2048 || sb.DiskSizeMB != 10240 {
				t.Fatalf("expected 2/2048/10240 with gate off, got %d/%d/%d", sb.CPUCount, sb.MemoryMB, sb.DiskSizeMB)
			}
		}
	})

	// 7. delete referenced by PodBox or WarmPool returns 409
	t.Run("delete referenced by PodBox or WarmPool returns 409", func(t *testing.T) {
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		s := gateway.NewServer(r)
		s.SetE2BTemplateAPI(true)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		// Create template and make ready
		cResp, _ := http.Post(ts.URL+"/templates", "application/json", strings.NewReader(`{"name":"ref-test"}`))
		var created controller.CreateTemplateResponse
		_ = json.NewDecoder(cResp.Body).Decode(&created)
		cResp.Body.Close()

		bResp, _ := http.Post(fmt.Sprintf("%s/v2/templates/%s/builds/%s", ts.URL, created.TemplateID, created.BuildID), "application/json", strings.NewReader(`{"fromImage":"busybox:latest"}`))
		bResp.Body.Close()

		// Reference by PodBox
		sbResp, _ := http.Post(ts.URL+"/sandboxes", "application/json", strings.NewReader(fmt.Sprintf(`{"templateID":"%s"}`, created.TemplateID)))
		if sbResp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201, got %d", sbResp.StatusCode)
		}
		sbResp.Body.Close()

		// Delete without force should return 409
		delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/templates/"+created.TemplateID, nil)
		delResp, err := http.DefaultClient.Do(delReq)
		if err != nil {
			t.Fatalf("delete request failed: %v", err)
		}
		defer delResp.Body.Close()
		if delResp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 for referenced delete, got %d", delResp.StatusCode)
		}

		// Template must still exist
		getResp, err := http.Get(ts.URL + "/templates/" + created.TemplateID)
		if err != nil {
			t.Fatalf("get template failed: %v", err)
		}
		defer getResp.Body.Close()
		if getResp.StatusCode != http.StatusOK {
			t.Fatalf("expected template to remain, got %d", getResp.StatusCode)
		}

		// Reference by WarmPool
		cResp2, _ := http.Post(ts.URL+"/templates", "application/json", strings.NewReader(`{"name":"ref-wp-test"}`))
		var created2 controller.CreateTemplateResponse
		_ = json.NewDecoder(cResp2.Body).Decode(&created2)
		cResp2.Body.Close()

		bResp2, _ := http.Post(fmt.Sprintf("%s/v2/templates/%s/builds/%s", ts.URL, created2.TemplateID, created2.BuildID), "application/json", strings.NewReader(`{"fromImage":"busybox:latest"}`))
		bResp2.Body.Close()

		wpResp, _ := http.Post(ts.URL+"/warmpools", "application/json", strings.NewReader(fmt.Sprintf(`{"poolName":"test-wp","templateRef":"%s","replicas":1}`, created2.TemplateID)))
		if wpResp.StatusCode != http.StatusOK && wpResp.StatusCode != http.StatusCreated {
			t.Fatalf("create warmpool failed: %d", wpResp.StatusCode)
		}
		wpResp.Body.Close()

		delReq2, _ := http.NewRequest(http.MethodDelete, ts.URL+"/templates/"+created2.TemplateID, nil)
		delResp2, err := http.DefaultClient.Do(delReq2)
		if err != nil {
			t.Fatalf("delete wp-referenced failed: %v", err)
		}
		defer delResp2.Body.Close()
		if delResp2.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 for warmpool-referenced delete, got %d", delResp2.StatusCode)
		}
	})

	// 8. force delete removes the template and leaves volumes
	t.Run("force delete removes the template and leaves volumes", func(t *testing.T) {
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		s := gateway.NewServer(r)
		s.SetE2BTemplateAPI(true)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		cResp, _ := http.Post(ts.URL+"/templates", "application/json", strings.NewReader(`{"name":"force-del-test"}`))
		var created controller.CreateTemplateResponse
		_ = json.NewDecoder(cResp.Body).Decode(&created)
		cResp.Body.Close()

		bResp, _ := http.Post(fmt.Sprintf("%s/v2/templates/%s/builds/%s", ts.URL, created.TemplateID, created.BuildID), "application/json", strings.NewReader(`{"fromImage":"busybox:latest"}`))
		bResp.Body.Close()

		sbResp, _ := http.Post(ts.URL+"/sandboxes", "application/json", strings.NewReader(fmt.Sprintf(`{"templateID":"%s"}`, created.TemplateID)))
		var sb gateway.SandboxRecord
		_ = json.NewDecoder(sbResp.Body).Decode(&sb)
		sbResp.Body.Close()

		// Force delete
		delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/templates/"+created.TemplateID+"?force=true", nil)
		delResp, err := http.DefaultClient.Do(delReq)
		if err != nil {
			t.Fatalf("force delete failed: %v", err)
		}
		defer delResp.Body.Close()
		if delResp.StatusCode != http.StatusOK && delResp.StatusCode != http.StatusNoContent {
			t.Fatalf("expected 200/204 for force delete, got %d", delResp.StatusCode)
		}

		// Template is removed
		getResp, err := http.Get(ts.URL + "/templates/" + created.TemplateID)
		if err != nil {
			t.Fatalf("get after delete failed: %v", err)
		}
		defer getResp.Body.Close()
		if getResp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 after force delete, got %d", getResp.StatusCode)
		}

		// Volumes must remain intact
		memClient := r.KubeClient().(*controller.MemoryKubeClient)
		if len(memClient.PVCs) == 0 {
			t.Fatalf("expected PVC volumes to remain after force delete of template")
		}
	})

	// 9. uploaded reported as building
	t.Run("uploaded reported as building", func(t *testing.T) {
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		s := gateway.NewServer(r)
		s.SetE2BTemplateAPI(true)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		cResp, _ := http.Post(ts.URL+"/templates", "application/json", strings.NewReader(`{"name":"uploaded-status-tpl"}`))
		var created controller.CreateTemplateResponse
		_ = json.NewDecoder(cResp.Body).Decode(&created)
		cResp.Body.Close()

		// Manually set status to uploaded in CR
		tpl, ok := r.GetPodTemplate(created.TemplateID)
		if !ok {
			t.Fatalf("template not found in reconciler")
		}
		tpl.Status.BuildStatus = v1alpha1.BuildStatusUploaded
		_ = r.KubeClient().UpdatePodTemplateStatus(context.Background(), *tpl)

		// A. GET /templates list reports building
		listResp, err := http.Get(ts.URL + "/templates")
		if err != nil {
			t.Fatalf("GET /templates failed: %v", err)
		}
		defer listResp.Body.Close()
		var list []gateway.E2BTemplateResponse
		_ = json.NewDecoder(listResp.Body).Decode(&list)
		var found bool
		for _, item := range list {
			if item.TemplateID == created.TemplateID {
				found = true
				if item.BuildStatus != "building" {
					t.Fatalf("expected list buildStatus 'building' when CR is uploaded, got %q", item.BuildStatus)
				}
			}
		}
		if !found {
			t.Fatalf("created template not found in list")
		}

		// B. GET /templates/{templateID} reports building
		getResp, err := http.Get(ts.URL + "/templates/" + created.TemplateID)
		if err != nil {
			t.Fatalf("GET /templates/{id} failed: %v", err)
		}
		defer getResp.Body.Close()
		var single gateway.E2BTemplateResponse
		_ = json.NewDecoder(getResp.Body).Decode(&single)
		if single.BuildStatus != "building" {
			t.Fatalf("expected get buildStatus 'building', got %q", single.BuildStatus)
		}
		if len(single.Builds) == 0 || single.Builds[0].Status != "building" {
			t.Fatalf("expected builds[0].status 'building', got %v", single.Builds)
		}

		// C. GET /templates/{templateID}/builds/{buildID}/status reports building
		stResp, err := http.Get(fmt.Sprintf("%s/templates/%s/builds/%s/status", ts.URL, created.TemplateID, created.BuildID))
		if err != nil {
			t.Fatalf("get build status failed: %v", err)
		}
		defer stResp.Body.Close()
		var stData struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(stResp.Body).Decode(&stData)
		if stData.Status != "building" {
			t.Fatalf("expected status 'building', got %q", stData.Status)
		}
	})

	// 10. placeholder cubepod.io/unbuilt:waiting is 409
	t.Run("placeholder cubepod.io/unbuilt:waiting is 409", func(t *testing.T) {
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		s := gateway.NewServer(r)
		s.SetE2BTemplateAPI(true)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		// Literal placeholder
		resp, err := http.Post(ts.URL+"/sandboxes", "application/json", strings.NewReader(`{"templateID":"cubepod.io/unbuilt:waiting"}`))
		if err != nil {
			t.Fatalf("post sandboxes failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 for placeholder, got %d", resp.StatusCode)
		}

		// Known template that is unbuilt (waiting)
		cResp, _ := http.Post(ts.URL+"/templates", "application/json", strings.NewReader(`{"name":"waiting-tpl"}`))
		var created controller.CreateTemplateResponse
		_ = json.NewDecoder(cResp.Body).Decode(&created)
		cResp.Body.Close()

		resp2, err := http.Post(ts.URL+"/sandboxes", "application/json", strings.NewReader(fmt.Sprintf(`{"templateID":"%s"}`, created.TemplateID)))
		if err != nil {
			t.Fatalf("post sandboxes failed: %v", err)
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 for unbuilt template, got %d", resp2.StatusCode)
		}
	})

	// 11. ghcr.io/org/app:1 still falls back when the gate is on
	t.Run("ghcr.io/org/app:1 still falls back when the gate is on", func(t *testing.T) {
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		s := gateway.NewServer(r)
		s.SetE2BTemplateAPI(true)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		resp, err := http.Post(ts.URL+"/sandboxes", "application/json", strings.NewReader(`{"templateID":"ghcr.io/org/app:1"}`))
		if err != nil {
			t.Fatalf("post sandboxes failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 for OCI fallback, got %d", resp.StatusCode)
		}
		var sb gateway.SandboxRecord
		_ = json.NewDecoder(resp.Body).Decode(&sb)
		if sb.TemplateID != "ghcr.io/org/app:1" {
			t.Fatalf("expected templateID ghcr.io/org/app:1, got %s", sb.TemplateID)
		}
	})

	// Additional edge case tests
	t.Run("build validation and beta stubs", func(t *testing.T) {
		// Test builds disabled -> 501 for non-empty steps
		rDisabled := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(false),
		)
		sDisabled := gateway.NewServer(rDisabled)
		sDisabled.SetE2BTemplateAPI(true)
		tsDisabled := httptest.NewServer(sDisabled.Handler())
		defer tsDisabled.Close()

		cResp, _ := http.Post(tsDisabled.URL+"/templates", "application/json", strings.NewReader(`{"name":"disabled-tpl"}`))
		var createdDisabled controller.CreateTemplateResponse
		_ = json.NewDecoder(cResp.Body).Decode(&createdDisabled)
		cResp.Body.Close()

		buildURLDisabled := fmt.Sprintf("%s/v2/templates/%s/builds/%s", tsDisabled.URL, createdDisabled.TemplateID, createdDisabled.BuildID)

		// Non-empty steps while builds disabled -> 501
		r5, _ := http.Post(buildURLDisabled, "application/json", strings.NewReader(`{"fromImage":"img","steps":[{"type":"RUN","args":["echo hi"]}]}`))
		if r5.StatusCode != http.StatusNotImplemented {
			t.Fatalf("expected 501 for builds disabled, got %d", r5.StatusCode)
		}
		r5.Body.Close()

		// Test builds enabled -> 400 validation for ARG, unknown step, etc.
		rEnabled := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
		)
		sEnabled := gateway.NewServer(rEnabled)
		sEnabled.SetE2BTemplateAPI(true)
		tsEnabled := httptest.NewServer(sEnabled.Handler())
		defer tsEnabled.Close()

		cResp2, _ := http.Post(tsEnabled.URL+"/templates", "application/json", strings.NewReader(`{"name":"enabled-tpl"}`))
		var createdEnabled controller.CreateTemplateResponse
		_ = json.NewDecoder(cResp2.Body).Decode(&createdEnabled)
		cResp2.Body.Close()

		buildURLEnabled := fmt.Sprintf("%s/v2/templates/%s/builds/%s", tsEnabled.URL, createdEnabled.TemplateID, createdEnabled.BuildID)

		// Both fromImage and fromTemplate -> 400
		r1, _ := http.Post(buildURLEnabled, "application/json", strings.NewReader(`{"fromImage":"img","fromTemplate":"tpl"}`))
		if r1.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for both fromImage and fromTemplate, got %d", r1.StatusCode)
		}
		r1.Body.Close()

		// Neither -> 400
		r2, _ := http.Post(buildURLEnabled, "application/json", strings.NewReader(`{"startCmd":"echo"}`))
		if r2.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for neither fromImage nor fromTemplate, got %d", r2.StatusCode)
		}
		r2.Body.Close()

		// Unknown step type -> 400
		r3, _ := http.Post(buildURLEnabled, "application/json", strings.NewReader(`{"fromImage":"img","steps":[{"type":"UNKNOWN","args":["foo"]}]}`))
		if r3.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for unknown step type, got %d", r3.StatusCode)
		}
		r3.Body.Close()

		// ARG step -> 400
		r4, _ := http.Post(buildURLEnabled, "application/json", strings.NewReader(`{"fromImage":"img","steps":[{"type":"ARG","args":["FOO=BAR"]}]}`))
		if r4.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for ARG step, got %d", r4.StatusCode)
		}
		r4.Body.Close()

		// Beta file route: invalid hash is 400
		f1, _ := http.Get(fmt.Sprintf("%s/templates/%s/files/invalid-hash", tsEnabled.URL, createdEnabled.TemplateID))
		if f1.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for non-hex hash, got %d", f1.StatusCode)
		}
		f1.Body.Close()

		// Beta file route: 64 lowercase hex returns 501 stub
		validHash := strings.Repeat("a", 64)
		f2, _ := http.Get(fmt.Sprintf("%s/templates/%s/files/%s", tsEnabled.URL, createdEnabled.TemplateID, validHash))
		if f2.StatusCode != http.StatusNotImplemented {
			t.Fatalf("expected 501 for beta file route stub, got %d", f2.StatusCode)
		}
		f2.Body.Close()

		// Beta log route stub -> 501
		lResp, _ := http.Get(fmt.Sprintf("%s/templates/%s/builds/%s/logs", tsEnabled.URL, createdEnabled.TemplateID, createdEnabled.BuildID))
		if lResp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("expected 501 for beta logs route, got %d", lResp.StatusCode)
		}
		lResp.Body.Close()
	})

	t.Run("more than 5000 rows returns 400", func(t *testing.T) {
		r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
		memClient := r.KubeClient().(*controller.MemoryKubeClient)
		for i := 0; i < 5001; i++ {
			name := fmt.Sprintf("tpl-%d", i)
			memClient.Templates[name] = v1alpha1.PodTemplate{
				Metadata: v1alpha1.ObjectMeta{Name: name},
				Spec:     v1alpha1.PodTemplateSpec{Image: "img:1"},
			}
		}
		s := gateway.NewServer(r)
		s.SetE2BTemplateAPI(true)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		resp, err := http.Get(ts.URL + "/templates")
		if err != nil {
			t.Fatalf("GET /templates failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request for >5000 rows, got %d", resp.StatusCode)
		}
	})
}
