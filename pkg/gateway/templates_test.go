package gateway_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
		if bResp.StatusCode != http.StatusAccepted {
			t.Fatalf("expected 202 for start build, got %d", bResp.StatusCode)
		}

		// Wait for async fast-path build to complete
		waitForBuildStatus := func(target string) {
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				stResp, err := http.Get(fmt.Sprintf("%s/templates/%s/builds/%s/status", ts.URL, created.TemplateID, created.BuildID))
				if err == nil {
					var st struct {
						Status string `json:"status"`
					}
					_ = json.NewDecoder(stResp.Body).Decode(&st)
					stResp.Body.Close()
					if st.Status == target {
						return
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatalf("timed out waiting for build %s to reach status %s", created.BuildID, target)
		}
		waitForBuildStatus("ready")

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
		if startResp.StatusCode != http.StatusAccepted {
			t.Fatalf("expected 202, got %d", startResp.StatusCode)
		}

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			stResp, err := http.Get(fmt.Sprintf("%s/templates/%s/builds/%s/status", ts.URL, created.TemplateID, created.BuildID))
			if err == nil {
				var st struct {
					Status string `json:"status"`
				}
				_ = json.NewDecoder(stResp.Body).Decode(&st)
				stResp.Body.Close()
				if st.Status == "ready" {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
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

		// Beta file route: 64 lowercase hex returns 501 when build disabled, 201 when enabled
		validHash := strings.Repeat("a", 64)
		fDisabled, _ := http.Get(fmt.Sprintf("%s/templates/%s/files/%s", tsDisabled.URL, createdDisabled.TemplateID, validHash))
		if fDisabled.StatusCode != http.StatusNotImplemented {
			t.Fatalf("expected 501 for beta file route when build disabled, got %d", fDisabled.StatusCode)
		}
		fDisabled.Body.Close()

		lDisabled, _ := http.Get(fmt.Sprintf("%s/templates/%s/builds/%s/logs", tsDisabled.URL, createdDisabled.TemplateID, createdDisabled.BuildID))
		if lDisabled.StatusCode != http.StatusNotImplemented {
			t.Fatalf("expected 501 for beta logs route when build disabled, got %d", lDisabled.StatusCode)
		}
		lDisabled.Body.Close()

		f2, _ := http.Get(fmt.Sprintf("%s/templates/%s/files/%s", tsEnabled.URL, createdEnabled.TemplateID, validHash))
		if f2.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 for beta file route when build enabled, got %d", f2.StatusCode)
		}
		f2.Body.Close()

		// Beta log route -> 200 when build enabled
		lResp, _ := http.Get(fmt.Sprintf("%s/templates/%s/builds/%s/logs", tsEnabled.URL, createdEnabled.TemplateID, createdEnabled.BuildID))
		if lResp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 for beta logs route when build enabled, got %d", lResp.StatusCode)
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

func makeValidTarGz(t *testing.T, name string, content []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{
		Name: name,
		Mode: 0644,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

type mockPoddKubeClient struct {
	*controller.MemoryKubeClient
	podIP string
}

func (m *mockPoddKubeClient) EnsurePod(ctx context.Context, pod controller.PodManifest) (string, error) {
	if _, err := m.MemoryKubeClient.EnsurePod(ctx, pod); err != nil {
		return "", err
	}
	return m.podIP, nil
}

func TestCEP0019_GatewayConfigAndMetrics(t *testing.T) {
	r := controller.NewReconciler(controller.WithE2BTemplateAPI(true))
	memClient := r.KubeClient().(*controller.MemoryKubeClient)
	memClient.Templates["tpl-ready"] = v1alpha1.PodTemplate{
		Metadata: v1alpha1.ObjectMeta{Name: "tpl-ready"},
		Spec:     v1alpha1.PodTemplateSpec{Image: "img:1"},
		Status:   v1alpha1.PodTemplateStatus{BuildStatus: v1alpha1.BuildStatusReady},
	}
	memClient.Templates["tpl-uploaded"] = v1alpha1.PodTemplate{
		Metadata: v1alpha1.ObjectMeta{Name: "tpl-uploaded"},
		Spec:     v1alpha1.PodTemplateSpec{Image: "img:2"},
		Status:   v1alpha1.PodTemplateStatus{BuildStatus: v1alpha1.BuildStatusUploaded},
	}
	memClient.Templates["tpl-building"] = v1alpha1.PodTemplate{
		Metadata: v1alpha1.ObjectMeta{Name: "tpl-building"},
		Spec:     v1alpha1.PodTemplateSpec{Image: "img:3"},
		Status:   v1alpha1.PodTemplateStatus{BuildStatus: v1alpha1.BuildStatusBuilding},
	}
	memClient.Templates["tpl-waiting"] = v1alpha1.PodTemplate{
		Metadata: v1alpha1.ObjectMeta{Name: "tpl-waiting"},
		Spec:     v1alpha1.PodTemplateSpec{Image: "img:4"},
		Status:   v1alpha1.PodTemplateStatus{BuildStatus: v1alpha1.BuildStatusWaiting},
	}
	memClient.Templates["tpl-error"] = v1alpha1.PodTemplate{
		Metadata: v1alpha1.ObjectMeta{Name: "tpl-error"},
		Spec:     v1alpha1.PodTemplateSpec{Image: "img:5"},
		Status:   v1alpha1.PodTemplateStatus{BuildStatus: v1alpha1.BuildStatusError},
	}

	s := gateway.NewServer(r)
	if s.ProbeTimeout() != 120*time.Second {
		t.Fatalf("expected default ProbeTimeout 120s, got %v", s.ProbeTimeout())
	}

	gateway.ApplyProcess(s, true)
	if !s.E2BTemplateAPIEnabled() {
		t.Fatal("expected E2BTemplateAPIEnabled after ApplyProcess")
	}

	gateway.ApplyTemplateConfig(s, true, true, "http://controller.internal:8080/", 45*time.Second)
	if !s.TemplateBuildEnabled() {
		t.Fatal("expected TemplateBuildEnabled true")
	}
	if !r.TemplateBuildEnabled() {
		t.Fatal("expected reconciler TemplateBuildEnabled true via SetTemplateBuild propagation")
	}
	if s.ControllerURL() != "http://controller.internal:8080" {
		t.Fatalf("expected trimmed controller URL, got %q", s.ControllerURL())
	}
	if s.ProbeTimeout() != 45*time.Second {
		t.Fatalf("expected ProbeTimeout 45s, got %v", s.ProbeTimeout())
	}

	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(bodyBytes)

	for _, want := range []string{
		`cubepod_templates{status="ready"} 4`,
		`cubepod_templates{status="building"} 2`,
		`cubepod_templates{status="waiting"} 1`,
		`cubepod_templates{status="error"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected metric %q in /metrics output:\n%s", want, body)
		}
	}
	if strings.Contains(body, "cubepod_gateway_templates_total 3") {
		t.Fatalf("did not expect hardcoded cubepod_gateway_templates_total 3 when E2B template API enabled")
	}

	// Gate off preserves cubepod_gateway_templates_total
	s.SetE2BTemplateAPI(false)
	respOff, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics (gate off) failed: %v", err)
	}
	bodyOffBytes, _ := io.ReadAll(respOff.Body)
	respOff.Body.Close()
	if !strings.Contains(string(bodyOffBytes), "cubepod_gateway_templates_total") {
		t.Fatalf("expected cubepod_gateway_templates_total when gate off, got:\n%s", string(bodyOffBytes))
	}
}

func TestCEP0019_MultiReplicaKubeSync(t *testing.T) {
	sharedKC := controller.NewMemoryKubeClient()

	r1 := controller.NewReconciler(
		controller.WithE2BTemplateAPI(true),
		controller.WithTemplateBuildEnabled(true),
		controller.WithKubeClient(sharedKC),
	)
	r2 := controller.NewReconciler(
		controller.WithE2BTemplateAPI(true),
		controller.WithTemplateBuildEnabled(true),
		controller.WithKubeClient(sharedKC),
	)

	s1 := gateway.NewServer(r1)
	s2 := gateway.NewServer(r2)
	ts1 := httptest.NewServer(s1.Handler())
	defer ts1.Close()
	ts2 := httptest.NewServer(s2.Handler())
	defer ts2.Close()

	// Create template on replica 1
	resp, err := http.Post(ts1.URL+"/templates", "application/json", strings.NewReader(`{"name":"shared-replica-tpl","cpuCount":4,"memoryMB":4096}`))
	if err != nil {
		t.Fatalf("create on replica 1 failed: %v", err)
	}
	var created controller.CreateTemplateResponse
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	// Build fast-path on replica 1
	bResp, err := http.Post(
		fmt.Sprintf("%s/v2/templates/%s/builds/%s", ts1.URL, created.TemplateID, created.BuildID),
		"application/json",
		strings.NewReader(`{"fromImage":"alpine:latest"}`),
	)
	if err != nil {
		t.Fatalf("build on replica 1 failed: %v", err)
	}
	bResp.Body.Close()
	if bResp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 on replica 1, got %d", bResp.StatusCode)
	}

	// Poll status on replica 2 (different Reconciler memory map, synced via shared KubeClient)
	deadline := time.Now().Add(3 * time.Second)
	reachedReady := false
	for time.Now().Before(deadline) {
		stResp, err := http.Get(fmt.Sprintf("%s/templates/%s/builds/%s/status", ts2.URL, created.TemplateID, created.BuildID))
		if err == nil && stResp.StatusCode == http.StatusOK {
			var st struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(stResp.Body).Decode(&st)
			stResp.Body.Close()
			if st.Status == "ready" {
				reachedReady = true
				break
			}
		} else if stResp != nil {
			stResp.Body.Close()
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !reachedReady {
		t.Fatal("replica 2 did not observe ready status synced via KubeClient")
	}

	// Lookup on replica 2 by ID, alias, v2 list, and spawn sandbox
	for _, path := range []string{
		"/templates/" + created.TemplateID,
		"/templates/aliases/shared-replica-tpl",
		"/v2/templates",
	} {
		gResp, err := http.Get(ts2.URL + path)
		if err != nil {
			t.Fatalf("GET %s on replica 2 failed: %v", path, err)
		}
		gResp.Body.Close()
		if gResp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 for GET %s on replica 2, got %d", path, gResp.StatusCode)
		}
	}

	sbxResp, err := http.Post(ts2.URL+"/sandboxes", "application/json", strings.NewReader(`{"templateID":"shared-replica-tpl"}`))
	if err != nil {
		t.Fatalf("POST /sandboxes on replica 2 failed: %v", err)
	}
	defer sbxResp.Body.Close()
	if sbxResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 spawning synced template on replica 2, got %d", sbxResp.StatusCode)
	}
}

func TestCEP0019_WireContract_StatusReasonPaginationAndDelete(t *testing.T) {
	fakeBuilder := controller.NewFakeImageBuilder()
	r := controller.NewReconciler(
		controller.WithE2BTemplateAPI(true),
		controller.WithTemplateBuildEnabled(true),
		controller.WithImageBuilder(fakeBuilder),
	)
	s := gateway.NewServer(r)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// Create 3 templates to test v2 cursor pagination & X-Next-Token
	var ids []string
	for i := 0; i < 3; i++ {
		cResp, _ := http.Post(ts.URL+"/templates", "application/json", strings.NewReader(fmt.Sprintf(`{"name":"page-tpl-%d"}`, i)))
		var c controller.CreateTemplateResponse
		_ = json.NewDecoder(cResp.Body).Decode(&c)
		cResp.Body.Close()
		ids = append(ids, c.TemplateID)

		// Unknown buildID on existing template -> 404
		unknownBuildResp, _ := http.Get(fmt.Sprintf("%s/templates/%s/builds/bld_does_not_exist/status", ts.URL, c.TemplateID))
		if unknownBuildResp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 for unknown buildID, got %d", unknownBuildResp.StatusCode)
		}
		unknownBuildResp.Body.Close()

		// While waiting, "reason" MUST be omitted completely from JSON
		waitResp, _ := http.Get(fmt.Sprintf("%s/templates/%s/builds/%s/status", ts.URL, c.TemplateID, c.BuildID))
		if waitResp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 for waiting status, got %d", waitResp.StatusCode)
		}
		var rawStatus map[string]any
		_ = json.NewDecoder(waitResp.Body).Decode(&rawStatus)
		waitResp.Body.Close()
		if _, hasReason := rawStatus["reason"]; hasReason {
			t.Fatalf("expected reason field to be omitted when waiting, got %#v", rawStatus["reason"])
		}
	}

	// GET /v2/templates?limit=2 -> paginated items + X-Next-Token header
	seenIDs := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		u := ts.URL + "/v2/templates?limit=2"
		if cursor != "" {
			u += "&nextToken=" + cursor
		}
		v2Page, err := http.Get(u)
		if err != nil {
			t.Fatalf("GET /v2/templates failed: %v", err)
		}
		if v2Page.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 for GET /v2/templates, got %d", v2Page.StatusCode)
		}
		var pageItems []gateway.E2BTemplateResponse
		_ = json.NewDecoder(v2Page.Body).Decode(&pageItems)
		nextToken := v2Page.Header.Get("X-Next-Token")
		v2Page.Body.Close()

		if len(pageItems) == 0 || len(pageItems) > 2 {
			t.Fatalf("expected 1..2 items on page %d, got %d", pages, len(pageItems))
		}
		for _, item := range pageItems {
			if seenIDs[item.TemplateID] {
				t.Fatalf("duplicate templateID %s across paginated pages", item.TemplateID)
			}
			seenIDs[item.TemplateID] = true
		}
		pages++
		if pages == 1 && nextToken == "" {
			t.Fatal("expected non-empty X-Next-Token header on first page")
		}
		if nextToken == "" {
			break
		}
		cursor = nextToken
	}
	if pages < 2 || len(seenIDs) < 6 {
		t.Fatalf("expected >=2 pages and >=6 templates (3 defaults + 3 created), got pages=%d total=%d", pages, len(seenIDs))
	}

	// Trigger failing build to verify error status returns object reason{message,step} + logsOffset/limit/level
	fakeBuilder.BuildErr = fmt.Errorf("apt-get failed at exit code 100")
	errCreateResp, _ := http.Post(ts.URL+"/templates", "application/json", strings.NewReader(`{"name":"failing-tpl"}`))
	var failCreated controller.CreateTemplateResponse
	_ = json.NewDecoder(errCreateResp.Body).Decode(&failCreated)
	errCreateResp.Body.Close()

	_ = r.ContextStore().AppendLog(failCreated.BuildID, []string{
		"debug step 0",
		"info step 1",
		"info step 2",
		"error: apt-get failed",
	})

	startFail, _ := http.Post(
		fmt.Sprintf("%s/v2/templates/%s/builds/%s", ts.URL, failCreated.TemplateID, failCreated.BuildID),
		"application/json",
		strings.NewReader(`{"fromImage":"ubuntu:24.04","steps":[{"type":"RUN","args":["apt-get update"]}]}`),
	)
	startFail.Body.Close()
	if startFail.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 starting failing build, got %d", startFail.StatusCode)
	}

	var errorStatus map[string]any
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		stResp, _ := http.Get(fmt.Sprintf("%s/templates/%s/builds/%s/status?logsOffset=0&limit=5&level=info", ts.URL, failCreated.TemplateID, failCreated.BuildID))
		var parsed map[string]any
		_ = json.NewDecoder(stResp.Body).Decode(&parsed)
		stResp.Body.Close()
		if parsed["status"] == "error" {
			errorStatus = parsed
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if errorStatus == nil {
		t.Fatal("timed out waiting for error build status")
	}
	reasonObj, ok := errorStatus["reason"].(map[string]any)
	if !ok || reasonObj["message"] == "" {
		t.Fatalf("expected reason object with message on error status, got %#v", errorStatus["reason"])
	}
	logsSlice, _ := errorStatus["logs"].([]any)
	if len(logsSlice) == 0 {
		t.Fatalf("expected filtered logs in error status, got %#v", errorStatus["logs"])
	}

	// DELETE /templates/{id} returns 204 No Content
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/templates/"+ids[0], nil)
	delResp, _ := http.DefaultClient.Do(delReq)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content on DELETE /templates/{id}, got %d", delResp.StatusCode)
	}
}

func TestCEP0019_BetaFileUploadLogsTagsAndControllerProxy(t *testing.T) {
	t.Run("local context upload logs and tags", func(t *testing.T) {
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
		)
		s := gateway.NewServer(r)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		cResp, _ := http.Post(ts.URL+"/templates", "application/json", strings.NewReader(`{"name":"beta-local"}`))
		var created controller.CreateTemplateResponse
		_ = json.NewDecoder(cResp.Body).Decode(&created)
		cResp.Body.Close()

		tarBytes, hash := makeValidTarGz(t, "hello.txt", []byte("world"))

		// 1. GET /files/{hash} with X-Forwarded-* headers -> 201 present=false & absolute https URL
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/templates/%s/files/%s", ts.URL, created.TemplateID, hash), nil)
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", "api.cubepod.example")
		fResp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET files failed: %v", err)
		}
		var fileInfo struct {
			Present bool              `json:"present"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		}
		_ = json.NewDecoder(fResp.Body).Decode(&fileInfo)
		fResp.Body.Close()
		if fResp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201, got %d", fResp.StatusCode)
		}
		if fileInfo.Present {
			t.Fatal("expected present=false before upload")
		}
		wantPrefix := "https://api.cubepod.example/templates/"
		if !strings.HasPrefix(fileInfo.URL, wantPrefix) {
			t.Fatalf("expected absolute upload URL starting with %q, got %q", wantPrefix, fileInfo.URL)
		}

		// 2. PUT invalid tar -> 400 Bad Request
		invalidReq, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/templates/%s/files/%s", ts.URL, created.TemplateID, hash), bytes.NewReader([]byte("not-a-tar-gz")))
		invalidResp, _ := http.DefaultClient.Do(invalidReq)
		invalidResp.Body.Close()
		if invalidResp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid tar.gz, got %d", invalidResp.StatusCode)
		}

		// 3. PUT valid tar.gz -> 201 Created, duplicate same-bytes PUT -> 201 Created, mismatched bytes -> 409 Conflict
		for attempt := 0; attempt < 2; attempt++ {
			putReq, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/templates/%s/files/%s", ts.URL, created.TemplateID, hash), bytes.NewReader(tarBytes))
			putResp, err := http.DefaultClient.Do(putReq)
			if err != nil {
				t.Fatalf("PUT files attempt %d failed: %v", attempt, err)
			}
			putResp.Body.Close()
			if putResp.StatusCode != http.StatusCreated {
				t.Fatalf("attempt %d: expected 201, got %d", attempt, putResp.StatusCode)
			}
		}

		wrongTarBytes, _ := makeValidTarGz(t, "other.txt", []byte("different"))
		badReq, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/templates/%s/files/%s", ts.URL, created.TemplateID, hash), bytes.NewReader(wrongTarBytes))
		badResp, _ := http.DefaultClient.Do(badReq)
		badResp.Body.Close()
		if badResp.StatusCode != http.StatusConflict {
			t.Fatalf("expected 409 for hash mismatch, got %d", badResp.StatusCode)
		}

		// Oversized upload -> 413 Payload Too Large
		if memCS, ok := r.ContextStore().(*controller.MemoryContextStore); ok {
			memCS.SetMaxBytes(16)
			largeReq, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/templates/%s/files/%s", ts.URL, created.TemplateID, strings.Repeat("c", 64)), bytes.NewReader(wrongTarBytes))
			largeResp, _ := http.DefaultClient.Do(largeReq)
			largeResp.Body.Close()
			if largeResp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("expected 413 for oversized context upload, got %d", largeResp.StatusCode)
			}
			memCS.SetMaxBytes(controller.DefaultMaxContextBytes)
		}

		fResp2, _ := http.Get(fmt.Sprintf("%s/templates/%s/files/%s", ts.URL, created.TemplateID, hash))
		var fileInfo2 struct {
			Present bool   `json:"present"`
			URL     string `json:"url"`
		}
		_ = json.NewDecoder(fResp2.Body).Decode(&fileInfo2)
		fResp2.Body.Close()
		if !fileInfo2.Present || !strings.HasPrefix(fileInfo2.URL, "http://") {
			t.Fatalf("expected present=true and http:// URL, got %+v", fileInfo2)
		}

		// 4. GET /builds/{buildID}/logs -> 200
		_ = r.ContextStore().AppendLog(created.BuildID, []string{"copying layer"})
		logsResp, _ := http.Get(fmt.Sprintf("%s/templates/%s/builds/%s/logs?limit=10", ts.URL, created.TemplateID, created.BuildID))
		var logsOut struct {
			Logs []controller.BuildLogEntry `json:"logs"`
		}
		_ = json.NewDecoder(logsResp.Body).Decode(&logsOut)
		logsResp.Body.Close()
		if logsResp.StatusCode != http.StatusOK || len(logsOut.Logs) != 1 {
			t.Fatalf("expected 200 and 1 log entry, got status=%d logs=%v", logsResp.StatusCode, logsOut.Logs)
		}

		// 5. Tags: POST 201, GET 200, DELETE 204
		tagResp, _ := http.Post(ts.URL+"/templates/tags", "application/json", strings.NewReader(`{"target":"beta-local:default","tags":["prod","v1"]}`))
		tagResp.Body.Close()
		if tagResp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 for POST /templates/tags, got %d", tagResp.StatusCode)
		}

		getTagsResp, _ := http.Get(fmt.Sprintf("%s/templates/%s/tags", ts.URL, created.TemplateID))
		var tagsList []struct {
			Tag string `json:"tag"`
		}
		_ = json.NewDecoder(getTagsResp.Body).Decode(&tagsList)
		getTagsResp.Body.Close()
		if getTagsResp.StatusCode != http.StatusOK || len(tagsList) < 2 {
			t.Fatalf("expected 200 and >=2 tags, got status=%d tags=%v", getTagsResp.StatusCode, tagsList)
		}

		delTagReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/templates/tags", strings.NewReader(`{"name":"beta-local","tags":["prod"]}`))
		delTagReq.Header.Set("Content-Type", "application/json")
		delTagResp, _ := http.DefaultClient.Do(delTagReq)
		delTagResp.Body.Close()
		if delTagResp.StatusCode != http.StatusNoContent {
			t.Fatalf("expected 204 for DELETE /templates/tags, got %d", delTagResp.StatusCode)
		}
	})

	t.Run("controllerURL forwarding mode", func(t *testing.T) {
		var sawStart, sawLogs, sawHead, sawPut atomic.Int32
		mockCtrl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/internal/v1/template-builds/") && strings.HasSuffix(r.URL.Path, "/start"):
				sawStart.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"status":"building"}`))
			case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/internal/v1/template-builds/") && strings.HasSuffix(r.URL.Path, "/logs"):
				sawLogs.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"logs":[{"timestamp":"2026-11-13T00:00:00Z","level":"info","message":"from-controller"}]}`))
			case r.Method == http.MethodHead && strings.HasPrefix(r.URL.Path, "/internal/v1/template-builds/context/"):
				sawHead.Add(1)
				w.WriteHeader(http.StatusNotFound)
			case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/internal/v1/template-builds/context/"):
				sawPut.Add(1)
				w.WriteHeader(http.StatusCreated)
			default:
				http.NotFound(w, r)
			}
		}))
		defer mockCtrl.Close()

		localBuilder := controller.NewFakeImageBuilder()
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
			controller.WithImageBuilder(localBuilder),
		)
		s := gateway.NewServer(r)
		s.SetControllerURL(mockCtrl.URL + "/")
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		cResp, _ := http.Post(ts.URL+"/templates", "application/json", strings.NewReader(`{"name":"proxy-tpl"}`))
		var created controller.CreateTemplateResponse
		_ = json.NewDecoder(cResp.Body).Decode(&created)
		cResp.Body.Close()

		// Start build should forward to controllerURL and NOT call localBuilder
		bResp, _ := http.Post(
			fmt.Sprintf("%s/v2/templates/%s/builds/%s", ts.URL, created.TemplateID, created.BuildID),
			"application/json",
			strings.NewReader(`{"fromImage":"alpine:latest","steps":[{"type":"RUN","args":["echo ok"]}]}`),
		)
		bResp.Body.Close()
		if bResp.StatusCode != http.StatusAccepted {
			t.Fatalf("expected 202 from proxied start build, got %d", bResp.StatusCode)
		}
		if len(localBuilder.BuildCalls) != 0 {
			t.Fatalf("expected 0 localBuilder calls when controllerURL set, got %d", len(localBuilder.BuildCalls))
		}

		validHash := strings.Repeat("b", 64)
		fGet, _ := http.Get(fmt.Sprintf("%s/templates/%s/files/%s", ts.URL, created.TemplateID, validHash))
		fGet.Body.Close()
		if fGet.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 from proxied HEAD file check, got %d", fGet.StatusCode)
		}

		fPutReq, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/templates/%s/files/%s", ts.URL, created.TemplateID, validHash), strings.NewReader("payload"))
		fPutResp, _ := http.DefaultClient.Do(fPutReq)
		fPutResp.Body.Close()
		if fPutResp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 from proxied PUT file upload, got %d", fPutResp.StatusCode)
		}

		stResp, _ := http.Get(fmt.Sprintf("%s/templates/%s/builds/%s/status", ts.URL, created.TemplateID, created.BuildID))
		var st struct {
			Logs []string `json:"logs"`
		}
		_ = json.NewDecoder(stResp.Body).Decode(&st)
		stResp.Body.Close()
		if len(st.Logs) != 1 || st.Logs[0] != "from-controller" {
			t.Fatalf("expected proxied controller log, got %v", st.Logs)
		}

		lResp, _ := http.Get(fmt.Sprintf("%s/templates/%s/builds/%s/logs", ts.URL, created.TemplateID, created.BuildID))
		lResp.Body.Close()
		if lResp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 from proxied logs, got %d", lResp.StatusCode)
		}

		if sawStart.Load() != 1 || sawHead.Load() != 1 || sawPut.Load() != 1 || sawLogs.Load() < 2 {
			t.Fatalf("unexpected proxy call counts: start=%d head=%d put=%d logs=%d", sawStart.Load(), sawHead.Load(), sawPut.Load(), sawLogs.Load())
		}
	})
}

func TestCEP0019_SandboxSpawnCountReadyCmdAndFork(t *testing.T) {
	t.Run("readyCmd polling succeeds and increments spawnCount", func(t *testing.T) {
		var probeCalls atomic.Int32
		mockPodd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/healthz":
				w.WriteHeader(http.StatusOK)
			case "/v1/commands/run":
				n := probeCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if n < 3 {
					_, _ = w.Write([]byte(`{"exit_code":1}`))
					return
				}
				_, _ = w.Write([]byte(`{"exit_code":0}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer mockPodd.Close()

		kc := &mockPoddKubeClient{
			MemoryKubeClient: controller.NewMemoryKubeClient(),
			podIP:            strings.TrimPrefix(mockPodd.URL, "http://"),
		}
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
			controller.WithKubeClient(kc),
		)
		s := gateway.NewServer(r)
		s.SetLivePodRouting(true)
		s.SetProbeTimeout(2 * time.Second)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		cResp, _ := http.Post(ts.URL+"/templates", "application/json", strings.NewReader(`{"name":"probe-ok","cpuCount":4,"memoryMB":4096}`))
		var created controller.CreateTemplateResponse
		_ = json.NewDecoder(cResp.Body).Decode(&created)
		cResp.Body.Close()

		bResp, _ := http.Post(
			fmt.Sprintf("%s/v2/templates/%s/builds/%s", ts.URL, created.TemplateID, created.BuildID),
			"application/json",
			strings.NewReader(`{"fromImage":"python:3.13","readyCmd":"curl -sf localhost:8080/health"}`),
		)
		bResp.Body.Close()

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			stResp, _ := http.Get(fmt.Sprintf("%s/templates/%s/builds/%s/status", ts.URL, created.TemplateID, created.BuildID))
			var st struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(stResp.Body).Decode(&st)
			stResp.Body.Close()
			if st.Status == "ready" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		sbxResp, err := http.Post(ts.URL+"/v2/sandboxes", "application/json", strings.NewReader(`{"templateID":"probe-ok"}`))
		if err != nil {
			t.Fatalf("POST /v2/sandboxes failed: %v", err)
		}
		var sbx gateway.SandboxRecord
		_ = json.NewDecoder(sbxResp.Body).Decode(&sbx)
		sbxResp.Body.Close()
		if sbxResp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 after readyCmd succeeds, got %d", sbxResp.StatusCode)
		}
		if probeCalls.Load() < 3 {
			t.Fatalf("expected >=3 readyCmd polls, got %d", probeCalls.Load())
		}

		// SpawnCount and LastSpawnedAt updated on template
		tplResp, _ := http.Get(ts.URL + "/templates/" + created.TemplateID)
		var gotTpl gateway.E2BTemplateResponse
		_ = json.NewDecoder(tplResp.Body).Decode(&gotTpl)
		tplResp.Body.Close()
		if gotTpl.SpawnCount != 1 || gotTpl.LastSpawnedAt == nil {
			t.Fatalf("expected spawnCount=1 and non-nil lastSpawnedAt, got count=%d lastSpawnedAt=%v", gotTpl.SpawnCount, gotTpl.LastSpawnedAt)
		}

		// Fork inherits parent CPU/Memory/Disk and cubepod.io/resolved-image
		forkResp, err := http.Post(fmt.Sprintf("%s/sandboxes/%s/fork", ts.URL, sbx.SandboxID), "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("POST /sandboxes/{id}/fork failed: %v", err)
		}
		var child gateway.SandboxRecord
		_ = json.NewDecoder(forkResp.Body).Decode(&child)
		forkResp.Body.Close()
		if forkResp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 for fork, got %d", forkResp.StatusCode)
		}
		if child.CPUCount != sbx.CPUCount || child.MemoryMB != sbx.MemoryMB || child.DiskSizeMB != sbx.DiskSizeMB {
			t.Fatalf("fork did not inherit resources: parent=(%d,%d,%d) child=(%d,%d,%d)",
				sbx.CPUCount, sbx.MemoryMB, sbx.DiskSizeMB, child.CPUCount, child.MemoryMB, child.DiskSizeMB)
		}
		boxes, _ := kc.ListPodBoxes(context.Background(), "")
		var parentImg, childImg string
		for _, b := range boxes {
			if b.Metadata.Name == sbx.SandboxID {
				parentImg = b.Metadata.Annotations["cubepod.io/resolved-image"]
			}
			if b.Metadata.Name == child.SandboxID {
				childImg = b.Metadata.Annotations["cubepod.io/resolved-image"]
			}
		}
		if parentImg == "" || childImg != parentImg {
			t.Fatalf("expected child resolved-image annotation %q, got %q", parentImg, childImg)
		}
	})

	t.Run("readyCmd timeout returns 503 and preserves sandbox for start-cmd.log", func(t *testing.T) {
		mockPodd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/healthz":
				w.WriteHeader(http.StatusOK)
			case "/v1/commands/run":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"exit_code":1}`))
			case "/v1/files":
				_, _ = w.Write([]byte("start-cmd failed: port 8080 refused connection"))
			default:
				http.NotFound(w, r)
			}
		}))
		defer mockPodd.Close()

		kc := &mockPoddKubeClient{
			MemoryKubeClient: controller.NewMemoryKubeClient(),
			podIP:            strings.TrimPrefix(mockPodd.URL, "http://"),
		}
		r := controller.NewReconciler(
			controller.WithE2BTemplateAPI(true),
			controller.WithTemplateBuildEnabled(true),
			controller.WithKubeClient(kc),
		)
		s := gateway.NewServer(r)
		s.SetLivePodRouting(true)
		s.SetProbeTimeout(150 * time.Millisecond)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		cResp, _ := http.Post(ts.URL+"/templates", "application/json", strings.NewReader(`{"name":"probe-timeout"}`))
		var created controller.CreateTemplateResponse
		_ = json.NewDecoder(cResp.Body).Decode(&created)
		cResp.Body.Close()

		bResp, _ := http.Post(
			fmt.Sprintf("%s/v2/templates/%s/builds/%s", ts.URL, created.TemplateID, created.BuildID),
			"application/json",
			strings.NewReader(`{"fromImage":"python:3.13","readyCmd":"false"}`),
		)
		bResp.Body.Close()

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			stResp, _ := http.Get(fmt.Sprintf("%s/templates/%s/builds/%s/status", ts.URL, created.TemplateID, created.BuildID))
			var st struct {
				Status string `json:"status"`
			}
			_ = json.NewDecoder(stResp.Body).Decode(&st)
			stResp.Body.Close()
			if st.Status == "ready" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		sbxResp, err := http.Post(ts.URL+"/sandboxes", "application/json", strings.NewReader(`{"templateID":"probe-timeout"}`))
		if err != nil {
			t.Fatalf("POST /sandboxes failed: %v", err)
		}
		var errBody struct {
			Code      int    `json:"code"`
			Message   string `json:"message"`
			SandboxID string `json:"sandboxID"`
			LogPath   string `json:"logPath"`
		}
		_ = json.NewDecoder(sbxResp.Body).Decode(&errBody)
		sbxResp.Body.Close()
		if sbxResp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 on readyCmd timeout, got %d", sbxResp.StatusCode)
		}
		if errBody.Code != 503 || errBody.SandboxID == "" || errBody.LogPath != "/var/log/cubepod/start-cmd.log" {
			t.Fatalf("unexpected 503 body: %+v", errBody)
		}

		// PodBox should be marked Failed and preserved
		boxes, _ := kc.ListPodBoxes(context.Background(), "")
		foundFailed := false
		for _, b := range boxes {
			if b.Metadata.Name == errBody.SandboxID && b.Status.Phase == v1alpha1.PodBoxPhaseFailed {
				foundFailed = true
			}
		}
		if !foundFailed {
			t.Fatalf("expected PodBox %s to remain with Failed phase", errBody.SandboxID)
		}

		// Sandbox record stays accessible so /files?path=/var/log/cubepod/start-cmd.log works
		logResp, err := http.Get(fmt.Sprintf("%s/sandboxes/%s/files?path=%s", ts.URL, errBody.SandboxID, errBody.LogPath))
		if err != nil {
			t.Fatalf("GET start-cmd.log failed: %v", err)
		}
		logData, _ := io.ReadAll(logResp.Body)
		logResp.Body.Close()
		if logResp.StatusCode != http.StatusOK || !strings.Contains(string(logData), "port 8080 refused") {
			t.Fatalf("expected readable start-cmd.log after 503 timeout, got status=%d body=%q", logResp.StatusCode, string(logData))
		}
	})
}
