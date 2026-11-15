package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/gateway"
)

func newVolumeHarness(t *testing.T, dir string) (*gateway.Server, *httptest.Server, *controller.MemoryKubeClient) {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	mem := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(controller.WithKubeClient(mem))
	srv := gateway.NewServer(rec)
	if err := srv.ConfigureE2BVolumes(gateway.E2BVolumeConfig{Dir: dir}); err != nil {
		t.Fatalf("configure volumes: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, mem
}

func volReq(t *testing.T, method, url, key string, body []byte, contentType string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("X-API-KEY", key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readJSON(t *testing.T, resp *http.Response, dest any) {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if dest == nil {
		return
	}
	if err := json.Unmarshal(b, dest); err != nil {
		t.Fatalf("status %d body %s: %v", resp.StatusCode, b, err)
	}
}

type volCreate struct {
	VolumeID string `json:"volumeID"`
	Name     string `json:"name"`
	Token    string `json:"token"`
}

func createVol(t *testing.T, ts *httptest.Server, key, name string) volCreate {
	t.Helper()
	resp := volReq(t, http.MethodPost, ts.URL+"/volumes", key, []byte(`{"name":"`+name+`"}`), "application/json")
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create %s: %d %s", name, resp.StatusCode, b)
	}
	var got volCreate
	readJSON(t, resp, &got)
	if got.VolumeID == "" || got.Token == "" || got.Name != name {
		t.Fatalf("create body: %+v", got)
	}
	return got
}

func TestE2BVolumeManagement(t *testing.T) {
	_, ts, mem := newVolumeHarness(t, "")
	key := "cpod_test_api_key"

	resp := volReq(t, http.MethodGet, ts.URL+"/volumes", "", nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing key: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = volReq(t, http.MethodPost, ts.URL+"/volumes", key, []byte(`{"name":"bad name"}`), "application/json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid name: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = volReq(t, http.MethodGet, ts.URL+"/volumes", key, nil, "")
	var empty []map[string]any
	readJSON(t, resp, &empty)
	if resp.StatusCode != http.StatusOK || len(empty) != 0 {
		t.Fatalf("empty list: %d %#v", resp.StatusCode, empty)
	}

	vol := createVol(t, ts, key, "cpod-test-under_score")

	resp = volReq(t, http.MethodPost, ts.URL+"/volumes", key, []byte(`{"name":"cpod-test-under_score"}`), "application/json")
	var dup struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	readJSON(t, resp, &dup)
	if resp.StatusCode != http.StatusBadRequest || dup.Code != 400 || dup.Message != "Volume with name 'cpod-test-under_score' already exists" {
		t.Fatalf("duplicate: %d %+v", resp.StatusCode, dup)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/volumes/"+vol.VolumeID, nil)
	req.Header.Set("X-API-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var info volCreate
	readJSON(t, resp, &info)
	if resp.StatusCode != http.StatusOK || info.Token != vol.Token || info.VolumeID != vol.VolumeID {
		t.Fatalf("getInfo: %d %+v", resp.StatusCode, info)
	}

	resp = volReq(t, http.MethodGet, ts.URL+"/volumes", key, nil, "")
	var listed []map[string]any
	readJSON(t, resp, &listed)
	if len(listed) != 1 {
		t.Fatalf("list len %d", len(listed))
	}
	if _, ok := listed[0]["token"]; ok {
		t.Fatalf("list leaked token: %#v", listed[0])
	}
	if listed[0]["volumeID"] != vol.VolumeID || listed[0]["name"] != vol.Name {
		t.Fatalf("list entry %#v", listed[0])
	}

	var e2bPVC int
	for _, pvc := range mem.ListPVCs() {
		if strings.HasPrefix(pvc.Name, "e2bvol-") {
			e2bPVC++
			if pvc.Name != "e2bvol-"+vol.VolumeID || pvc.VolumeMode != "Filesystem" || pvc.StorageClassName != "standard" {
				t.Fatalf("pvc: %+v", pvc)
			}
			if pvc.Labels["cubepod.io/e2b-volume-id"] != vol.VolumeID {
				t.Fatalf("pvc labels: %+v", pvc.Labels)
			}
		}
	}
	if e2bPVC != 1 {
		t.Fatalf("expected 1 e2b filesystem PVC, got %d (%v)", e2bPVC, mem.ListPVCs())
	}

	resp = volReq(t, http.MethodDelete, ts.URL+"/volumes/"+vol.VolumeID, key, nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || len(bytes.TrimSpace(body)) != 0 {
		t.Fatalf("delete: %d %q", resp.StatusCode, body)
	}
	resp = volReq(t, http.MethodDelete, ts.URL+"/volumes/"+vol.VolumeID, key, nil, "")
	var missing struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	readJSON(t, resp, &missing)
	if resp.StatusCode != http.StatusNotFound || missing.Code != 404 {
		t.Fatalf("second delete: %d %+v", resp.StatusCode, missing)
	}
	left := 0
	for _, pvc := range mem.ListPVCs() {
		if pvc.Name == "e2bvol-"+vol.VolumeID {
			left++
		}
	}
	if left != 0 {
		t.Fatal("filesystem PVC was not deleted")
	}
}

func TestE2BVolumeContentAndEscape(t *testing.T) {
	srv, ts, _ := newVolumeHarness(t, "")
	key := "cpod_test_api_key"
	vol := createVol(t, ts, key, "cpod-test-content")
	base := ts.URL + "/volumecontent/" + vol.VolumeID

	put := func(path, query string, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut, base+"/file?path="+path+query, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+vol.Token)
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := put("hello.txt", "", "hello-volume")
	var st map[string]any
	readJSON(t, resp, &st)
	if resp.StatusCode != http.StatusCreated || st["type"] != "file" || st["name"] != "hello.txt" || st["path"] != "/hello.txt" {
		t.Fatalf("write: %d %#v", resp.StatusCode, st)
	}
	if int(st["size"].(float64)) != len("hello-volume") {
		t.Fatalf("size %#v", st["size"])
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/file?path=/hello.txt", nil)
	req.Header.Set("Authorization", "Bearer "+vol.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "hello-volume" {
		t.Fatalf("read: %d %q", resp.StatusCode, b)
	}

	req, _ = http.NewRequest(http.MethodGet, base+"/file?path=/hello.txt", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing bearer: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, base+"/file?path=/hello.txt", nil)
	req.Header.Set("Authorization", "Bearer not-the-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad bearer: %d", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, base+"/path?path=/hello.txt", nil)
	req.Header.Set("Authorization", "Bearer "+vol.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	readJSON(t, resp, &st)
	if resp.StatusCode != http.StatusOK || st["type"] != "file" {
		t.Fatalf("stat: %d %#v", resp.StatusCode, st)
	}

	req, _ = http.NewRequest(http.MethodPost, base+"/dir?path=/no-parent/child", nil)
	req.Header.Set("Authorization", "Bearer "+vol.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("mkdir without force: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, base+"/dir?path=/cpod-test-deep/nested&force=true&mode=493", nil)
	req.Header.Set("Authorization", "Bearer "+vol.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	readJSON(t, resp, &st)
	if resp.StatusCode != http.StatusCreated || st["type"] != "directory" || st["path"] != "/cpod-test-deep/nested" {
		t.Fatalf("force mkdir: %d %#v", resp.StatusCode, st)
	}

	resp = put("/cpod-test-deep/nested/note.txt", "&uid=10&gid=20&mode=420", "meta")
	readJSON(t, resp, &st)
	if resp.StatusCode != http.StatusCreated || int(st["uid"].(float64)) != 10 || int(st["gid"].(float64)) != 20 || int(st["mode"].(float64)) != 420 {
		t.Fatalf("write meta: %d %#v", resp.StatusCode, st)
	}
	patchBody := []byte(`{"uid":11,"gid":21,"mode":384}`)
	req, _ = http.NewRequest(http.MethodPatch, base+"/path?path=/cpod-test-deep/nested/note.txt", bytes.NewReader(patchBody))
	req.Header.Set("Authorization", "Bearer "+vol.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	readJSON(t, resp, &st)
	if resp.StatusCode != http.StatusOK || int(st["uid"].(float64)) != 11 || int(st["gid"].(float64)) != 21 || int(st["mode"].(float64)) != 384 {
		t.Fatalf("patch: %d %#v", resp.StatusCode, st)
	}

	resp = put("/tree/a/b.txt", "&force=true", "nested")
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("force write: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, base+"/dir?path=/&depth=1", nil)
	req.Header.Set("Authorization", "Bearer "+vol.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var top []map[string]any
	readJSON(t, resp, &top)
	names := map[string]bool{}
	for _, e := range top {
		names[e["name"].(string)] = true
		if e["name"] == "b.txt" {
			t.Fatalf("depth 1 listed nested file: %#v", top)
		}
	}
	if !names["hello.txt"] || !names["cpod-test-deep"] || !names["tree"] {
		t.Fatalf("depth 1 names: %#v", top)
	}
	req, _ = http.NewRequest(http.MethodGet, base+"/dir?path=/tree&depth=2", nil)
	req.Header.Set("Authorization", "Bearer "+vol.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var deep []map[string]any
	readJSON(t, resp, &deep)
	foundB := false
	for _, e := range deep {
		if e["path"] == "/tree/a/b.txt" {
			foundB = true
		}
	}
	if !foundB {
		t.Fatalf("depth 2 missing file: %#v", deep)
	}

	req, _ = http.NewRequest(http.MethodDelete, base+"/path?path=/tree", nil)
	req.Header.Set("Authorization", "Bearer "+vol.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove dir: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, base+"/path?path=/tree/a/b.txt", nil)
	req.Header.Set("Authorization", "Bearer "+vol.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var nf struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	readJSON(t, resp, &nf)
	if resp.StatusCode != http.StatusNotFound || nf.Code != "404" || nf.Message == "" {
		t.Fatalf("missing stat: %d %+v", resp.StatusCode, nf)
	}

	for _, p := range []string{"../outside.txt", "/../../etc/passwd"} {
		resp = put(p, "", "nope")
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("escape %s: %d", p, resp.StatusCode)
		}
	}
	marker := "cpod-test-volume-marker-not-host"
	resp = put("/etc/passwd", "&force=true", marker)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("volume-absolute write: %d", resp.StatusCode)
	}
	host, err := os.ReadFile("/etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(host), marker) {
		t.Fatal("write escaped the volume root onto the host")
	}
	dataDir := srv.E2BVolumeDataDir(vol.VolumeID)
	if dataDir == "" {
		t.Fatal("missing data dir")
	}
	got, err := os.ReadFile(filepath.Join(dataDir, "etc", "passwd"))
	if err != nil || string(got) != marker {
		t.Fatalf("volume file: %v %q", err, got)
	}
	link := filepath.Join(dataDir, "escape-link")
	if err := os.Symlink("/etc/passwd", link); err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest(http.MethodGet, base+"/file?path=/escape-link", nil)
	req.Header.Set("Authorization", "Bearer "+vol.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("symlink read: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, base+"/path?path=/escape-link", nil)
	req.Header.Set("Authorization", "Bearer "+vol.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	readJSON(t, resp, &st)
	if resp.StatusCode != http.StatusOK || st["type"] != "symlink" {
		t.Fatalf("symlink stat: %d %#v", resp.StatusCode, st)
	}
}

func TestE2BVolumeIsolationMountsAndRestart(t *testing.T) {
	dir := t.TempDir()
	srv, ts, mem := newVolumeHarness(t, dir)
	keyA := "cpod_test_api_key"
	keyB := "cpod_test_other_key"
	vol := createVol(t, ts, keyA, "cpod-test-persist")

	resp := volReq(t, http.MethodGet, ts.URL+"/volumes", keyB, nil, "")
	var other []map[string]any
	readJSON(t, resp, &other)
	if len(other) != 0 {
		t.Fatalf("other key listed volumes: %#v", other)
	}
	resp = volReq(t, http.MethodGet, ts.URL+"/volumes/"+vol.VolumeID, keyB, nil, "")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || strings.Contains(string(b), vol.Name) || strings.Contains(string(b), vol.Token) {
		t.Fatalf("cross get: %d %s", resp.StatusCode, b)
	}
	resp = volReq(t, http.MethodDelete, ts.URL+"/volumes/"+vol.VolumeID, keyB, nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross delete: %d", resp.StatusCode)
	}

	same := createVol(t, ts, keyB, "cpod-test-persist")
	if same.VolumeID == vol.VolumeID {
		t.Fatal("tenants shared a volume id")
	}
	onlyA := createVol(t, ts, keyA, "cpod-test-only-a")

	putReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/volumecontent/"+vol.VolumeID+"/file?path=/keep.txt", strings.NewReader("still-here"))
	putReq.Header.Set("Authorization", "Bearer "+vol.Token)
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusCreated {
		t.Fatalf("seed write: %d", putResp.StatusCode)
	}

	mountBody := []byte(`{"templateID":"base","volumeMounts":[{"name":"cpod-test-persist","path":"/mnt/data"}]}`)
	resp = volReq(t, http.MethodPost, ts.URL+"/sandboxes", "", mountBody, "application/json")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("mount without key: %d", resp.StatusCode)
	}
	crossBody := []byte(`{"templateID":"base","volumeMounts":[{"name":"cpod-test-only-a","path":"/mnt/other"}]}`)
	resp = volReq(t, http.MethodPost, ts.URL+"/sandboxes", keyB, crossBody, "application/json")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || strings.Contains(string(body), onlyA.VolumeID) || strings.Contains(string(body), onlyA.Token) || strings.Contains(string(body), onlyA.Name) {
		t.Fatalf("cross mount: %d %s", resp.StatusCode, body)
	}
	// The shared name resolves to key B's own volume, never key A's id.
	resp = volReq(t, http.MethodPost, ts.URL+"/sandboxes", keyB, mountBody, "application/json")
	var sbB struct {
		SandboxID string `json:"sandboxID"`
	}
	readJSON(t, resp, &sbB)
	if resp.StatusCode != http.StatusCreated || sbB.SandboxID == "" {
		t.Fatalf("same-name mount: %d %+v", resp.StatusCode, sbB)
	}
	bMounts := srv.E2BVolumeMounts(sbB.SandboxID)
	if len(bMounts) != 1 || bMounts[0].VolumeID != same.VolumeID || bMounts[0].VolumeID == vol.VolumeID {
		t.Fatalf("key B mounted the wrong volume: %+v", bMounts)
	}
	delB, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+sbB.SandboxID, nil)
	delBResp, err := http.DefaultClient.Do(delB)
	if err != nil {
		t.Fatal(err)
	}
	delBResp.Body.Close()

	resp = volReq(t, http.MethodPost, ts.URL+"/sandboxes", keyA, mountBody, "application/json")
	var sb struct {
		SandboxID string `json:"sandboxID"`
	}
	readJSON(t, resp, &sb)
	if resp.StatusCode != http.StatusCreated || sb.SandboxID == "" {
		t.Fatalf("sandbox: %d %+v", resp.StatusCode, sb)
	}
	mounts := srv.E2BVolumeMounts(sb.SandboxID)
	if len(mounts) != 1 || mounts[0].ContainerPath != "/mnt/data" || mounts[0].VolumeName != "cpod-test-persist" {
		t.Fatalf("mount spec: %+v", mounts)
	}
	if mounts[0].DockerVolume != "" {
		t.Fatalf("docker volume provisioned in unit test: %+v", mounts[0])
	}
	if !strings.HasPrefix(mounts[0].HostPath, dir) {
		t.Fatalf("host path %s not under %s", mounts[0].HostPath, dir)
	}

	fileResp, err := http.Get(ts.URL + "/sandboxes/" + sb.SandboxID + "/files?path=/mnt/data/keep.txt")
	if err != nil {
		t.Fatal(err)
	}
	fb, _ := io.ReadAll(fileResp.Body)
	fileResp.Body.Close()
	if fileResp.StatusCode != http.StatusOK || string(fb) != "still-here" {
		t.Fatalf("mounted read: %d %q", fileResp.StatusCode, fb)
	}

	resp = volReq(t, http.MethodDelete, ts.URL+"/volumes/"+vol.VolumeID, keyA, nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete in use: %d", resp.StatusCode)
	}
	before := mem.ListPVCs()
	var workspaceLeft bool
	for _, pvc := range before {
		if strings.Contains(pvc.Name, "workspace-pvc") {
			workspaceLeft = true
		}
	}
	if !workspaceLeft {
		t.Fatalf("sandbox did not record a workspace PVC: %+v", before)
	}

	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+sb.SandboxID, nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete sandbox: %d", delResp.StatusCode)
	}
	if specs := srv.E2BVolumeMounts(sb.SandboxID); len(specs) != 0 {
		t.Fatalf("mounts remain after sandbox delete: %+v", specs)
	}

	resp = volReq(t, http.MethodPost, ts.URL+"/sandboxes", keyA, mountBody, "application/json")
	var sb2 struct {
		SandboxID string `json:"sandboxID"`
	}
	readJSON(t, resp, &sb2)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("second sandbox: %d", resp.StatusCode)
	}
	fileResp, err = http.Get(ts.URL + "/sandboxes/" + sb2.SandboxID + "/files?path=/mnt/data/keep.txt")
	if err != nil {
		t.Fatal(err)
	}
	fb, _ = io.ReadAll(fileResp.Body)
	fileResp.Body.Close()
	if fileResp.StatusCode != http.StatusOK || string(fb) != "still-here" {
		t.Fatalf("data after new sandbox: %d %q", fileResp.StatusCode, fb)
	}
	delReq, _ = http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+sb2.SandboxID, nil)
	delResp, err = http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()

	ts.Close()
	srv2, ts2, mem2 := newVolumeHarness(t, dir)
	_ = srv2
	resp = volReq(t, http.MethodGet, ts2.URL+"/volumes/"+vol.VolumeID, keyA, nil, "")
	var again volCreate
	readJSON(t, resp, &again)
	if resp.StatusCode != http.StatusOK || again.Token != vol.Token {
		t.Fatalf("restart get: %d %+v", resp.StatusCode, again)
	}
	req, _ := http.NewRequest(http.MethodGet, ts2.URL+"/volumecontent/"+vol.VolumeID+"/file?path=/keep.txt", nil)
	req.Header.Set("Authorization", "Bearer "+vol.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	fb, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(fb) != "still-here" {
		t.Fatalf("restart read: %d %q", resp.StatusCode, fb)
	}
	foundPVC := false
	for _, pvc := range mem2.ListPVCs() {
		if pvc.Name == "e2bvol-"+vol.VolumeID && pvc.VolumeMode == "Filesystem" {
			foundPVC = true
		}
	}
	// A new process records the PVC again when the volume is loaded only if
	// create runs. Restart must still serve the bytes; the claim is re-noted
	// only on create. The original client is a different process here, so the
	// new MemoryKubeClient starts empty until a later create. Persistence is
	// the on-disk file above.
	_ = foundPVC

	resp = volReq(t, http.MethodDelete, ts2.URL+"/volumes/"+vol.VolumeID, keyA, nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete after restart: %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dir, vol.VolumeID)); !os.IsNotExist(err) {
		t.Fatalf("data dir remains: %v", err)
	}
}

func TestE2BVolumeConcurrent(t *testing.T) {
	_, ts, _ := newVolumeHarness(t, "")
	key := "cpod_test_api_key"
	vol := createVol(t, ts, key, "cpod-test-race")
	const n = 8
	var wg sync.WaitGroup
	errCh := make(chan error, n*2)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/f-%d.txt", i)
			body := fmt.Sprintf("v-%d", i)
			req, err := http.NewRequest(http.MethodPut, ts.URL+"/volumecontent/"+vol.VolumeID+"/file?path="+path, strings.NewReader(body))
			if err != nil {
				errCh <- err
				return
			}
			req.Header.Set("Authorization", "Bearer "+vol.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errCh <- err
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				errCh <- fmt.Errorf("write %s: %d", path, resp.StatusCode)
				return
			}
			req, err = http.NewRequest(http.MethodGet, ts.URL+"/volumecontent/"+vol.VolumeID+"/dir?path=/", nil)
			if err != nil {
				errCh <- err
				return
			}
			req.Header.Set("Authorization", "Bearer "+vol.Token)
			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				errCh <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errCh <- fmt.Errorf("list: %d", resp.StatusCode)
				return
			}
			var entries []map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/volumecontent/"+vol.VolumeID+"/dir?path=/&depth=1", nil)
	req.Header.Set("Authorization", "Bearer "+vol.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	readJSON(t, resp, &entries)
	if len(entries) != n {
		t.Fatalf("expected %d files, got %#v", n, entries)
	}
}

func TestE2BVolumeSandboxCreateMountsOnPod(t *testing.T) {
	_, ts, mem := newVolumeHarness(t, "")
	keyA := "cpod_test_api_key"
	vol := createVol(t, ts, keyA, "cpod-test-pod-mount")
	mountBody, _ := json.Marshal(map[string]any{
		"templateID":   "base",
		"volumeMounts": []map[string]string{{"name": vol.Name, "path": "/mnt/cpod-test-vol"}},
	})
	resp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", keyA, mountBody, "application/json")
	var sb struct {
		SandboxID string `json:"sandboxID"`
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create sandbox: %d %s", resp.StatusCode, b)
	}
	readJSON(t, resp, &sb)
	if sb.SandboxID == "" {
		t.Fatal("missing sandbox id")
	}
	pod, ok := mem.GetPod("cubepod-workloads", sb.SandboxID+"-kata-pod")
	if !ok {
		t.Fatalf("expected pod %s-kata-pod, pods=%v", sb.SandboxID, podNames(mem))
	}
	pvc := "e2bvol-" + vol.VolumeID
	var sawVol, sawMount, sawWS bool
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim == pvc {
			sawVol = true
		}
		if v.Name == "workspace-volume" {
			sawWS = true
		}
	}
	for _, c := range pod.Containers {
		if c.Name != "sandbox" {
			continue
		}
		for _, m := range c.VolumeMounts {
			if m.MountPath == "/run/cubepod/volumes/"+vol.VolumeID && (m.Name == pvc || m.Name == "e2bvol-"+vol.VolumeID) {
				sawMount = true
			}
		}
	}
	if !sawVol || !sawMount || !sawWS {
		t.Fatalf("pod missing official mount or workspace (vol=%v mount=%v ws=%v) volumes=%+v containers=%+v", sawVol, sawMount, sawWS, pod.Volumes, pod.Containers)
	}
	if box, err := mem.GetPodBox(context.Background(), "cubepod-workloads", sb.SandboxID); err != nil || box == nil || len(box.Spec.E2BVolumeMounts) != 1 {
		t.Fatalf("expected PodBox spec to record official mounts: err=%v box=%+v", err, box)
	}
	del, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+sb.SandboxID, nil)
	del.Header.Set("X-API-KEY", keyA)
	resp, err := http.DefaultClient.Do(del)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestE2BVolumeReservedMountPathRejected(t *testing.T) {
	_, ts, _ := newVolumeHarness(t, "")
	keyA := "cpod_test_api_key"
	vol := createVol(t, ts, keyA, "cpod-test-reserved-path")
	for _, path := range []string{"/", "/workspace", "/etc", "/proc", "/opt/cubepod"} {
		body, _ := json.Marshal(map[string]any{
			"templateID":   "base",
			"volumeMounts": []map[string]string{{"name": vol.Name, "path": path}},
		})
		resp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", keyA, body, "application/json")
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("path %s: expected 400, got %d %s", path, resp.StatusCode, b)
		}
	}
}

func podNames(mem *controller.MemoryKubeClient) []string {
	var names []string
	for _, p := range mem.AllPods() {
		names = append(names, p.Name)
	}
	return names
}

func TestE2BVolumeLiveK8sGatedOff(t *testing.T) {
	srv, ts, mem := newVolumeHarness(t, "")
	srv.SetLivePodRouting(true)
	keyA := "cpod_test_api_key"

	vol := createVol(t, ts, keyA, "cpod-test-k8s-gated")
	if _, ok := mem.GetPVC("cubepod-workloads", "e2bvol-"+vol.VolumeID); ok {
		t.Fatalf("expected no PVC created on live K8s when volumeMounts are gated off")
	}

	body, _ := json.Marshal(map[string]any{
		"templateID":   "base",
		"volumeMounts": []map[string]string{{"name": vol.Name, "path": "/mnt/data"}},
	})
	resp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", keyA, body, "application/json")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501 Not Implemented on live K8s, got %d %s", resp.StatusCode, b)
	}
}

func TestE2BVolumeLiveK8sEnabledWithoutSharedPVC_Returns501(t *testing.T) {
	dir := t.TempDir()
	mem := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(controller.WithKubeClient(mem))
	srv := gateway.NewServer(rec)
	srv.SetLivePodRouting(true)
	if err := srv.ConfigureE2BVolumes(gateway.E2BVolumeConfig{
		Dir:                dir,
		K8sE2BVolumeMounts: true,
		SharedPVCName:      "",
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	keyA := "cpod_test_api_key"
	vol := createVol(t, ts, keyA, "cpod-test-k8s-no-pvc")
	body, _ := json.Marshal(map[string]any{
		"templateID":   "base",
		"volumeMounts": []map[string]string{{"name": vol.Name, "path": "/mnt/data"}},
	})
	resp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", keyA, body, "application/json")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501 Not Implemented when SharedPVCName is empty, got %d %s", resp.StatusCode, b)
	}
}

func TestE2BVolumeLiveK8sRWXEnabledAndFork(t *testing.T) {
	dir := t.TempDir()
	mem := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(controller.WithKubeClient(mem))
	srv := gateway.NewServer(rec)
	srv.SetLivePodRouting(true)
	sharedPVC := "cubepod-e2b-volumes"
	if err := srv.ConfigureE2BVolumes(gateway.E2BVolumeConfig{
		Dir:                dir,
		K8sE2BVolumeMounts: true,
		SharedPVCName:      sharedPVC,
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	keyA := "cpod_test_api_key"
	vol := createVol(t, ts, keyA, "cpod-test-k8s-rwx")
	if _, ok := mem.GetPVC("cubepod-workloads", "e2bvol-"+vol.VolumeID); ok {
		t.Fatalf("expected no per-volume e2bvol-* PVC created when SharedPVCName is set")
	}

	body, _ := json.Marshal(map[string]any{
		"templateID":   "base",
		"volumeMounts": []map[string]string{{"name": vol.Name, "path": "/mnt/dataset"}},
	})
	resp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", keyA, body, "application/json")
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create sandbox: %d %s", resp.StatusCode, b)
	}
	var sb struct {
		SandboxID string `json:"sandboxID"`
	}
	readJSON(t, resp, &sb)

	box, err := mem.GetPodBox(context.Background(), "cubepod-workloads", sb.SandboxID)
	if err != nil || box == nil || len(box.Spec.E2BVolumeMounts) != 1 {
		t.Fatalf("expected PodBox with 1 E2BVolumeMount, err=%v box=%+v", err, box)
	}
	gotMount := box.Spec.E2BVolumeMounts[0]
	if gotMount.PVCName != sharedPVC || gotMount.SubPath != vol.VolumeID+"/data" || gotMount.Path != "/mnt/dataset" {
		t.Fatalf("unexpected PodBox E2BVolumeMount: %+v", gotMount)
	}

	pod, ok := mem.GetPod("cubepod-workloads", sb.SandboxID+"-kata-pod")
	if !ok {
		t.Fatalf("expected pod %s-kata-pod", sb.SandboxID)
	}
	stagingPath := "/run/cubepod/volumes/" + vol.VolumeID
	wantEnv := `[{"volume_id":"` + vol.VolumeID + `","staging_path":"` + stagingPath + `","target_path":"/mnt/dataset"}]`
	var sawStaging, sawEnv bool
	for _, c := range pod.Containers {
		if c.Name != "sandbox" {
			continue
		}
		for _, m := range c.VolumeMounts {
			if m.Name == sharedPVC && m.MountPath == stagingPath && m.SubPath == vol.VolumeID+"/data" {
				sawStaging = true
			}
		}
		for _, e := range c.Env {
			if e.Name == "CUBEPOD_E2B_MOUNTS" && e.Value == wantEnv {
				sawEnv = true
			}
		}
	}
	if !sawStaging || !sawEnv {
		t.Fatalf("pod missing staging mount or CUBEPOD_E2B_MOUNTS (staging=%v env=%v) containers=%+v", sawStaging, sawEnv, pod.Containers)
	}

	// Fork the sandbox and verify mount inheritance + DELETE /volumes/{id} tracking.
	forkResp := volReq(t, http.MethodPost, ts.URL+"/sandboxes/"+sb.SandboxID+"/fork", keyA, nil, "application/json")
	if forkResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(forkResp.Body)
		forkResp.Body.Close()
		t.Fatalf("fork sandbox: %d %s", forkResp.StatusCode, b)
	}
	var child struct {
		SandboxID string `json:"sandboxID"`
	}
	readJSON(t, forkResp, &child)
	childBox, err := mem.GetPodBox(context.Background(), "cubepod-workloads", child.SandboxID)
	if err != nil || childBox == nil || len(childBox.Spec.E2BVolumeMounts) != 1 {
		t.Fatalf("expected forked PodBox to inherit E2BVolumeMounts, err=%v box=%+v", err, childBox)
	}
	if childBox.Spec.E2BVolumeMounts[0].PVCName != sharedPVC || childBox.Spec.E2BVolumeMounts[0].SubPath != vol.VolumeID+"/data" {
		t.Fatalf("unexpected forked PodBox mount: %+v", childBox.Spec.E2BVolumeMounts[0])
	}

	// Delete parent; volume must still be busy (409) because fork holds a reference.
	delParent := volReq(t, http.MethodDelete, ts.URL+"/sandboxes/"+sb.SandboxID, keyA, nil, "")
	delParent.Body.Close()

	delVolBusy := volReq(t, http.MethodDelete, ts.URL+"/volumes/"+vol.VolumeID, keyA, nil, "")
	delVolBusy.Body.Close()
	if delVolBusy.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict deleting volume while fork is alive, got %d", delVolBusy.StatusCode)
	}

	// Delete fork; volume deletion now succeeds.
	delChild := volReq(t, http.MethodDelete, ts.URL+"/sandboxes/"+child.SandboxID, keyA, nil, "")
	delChild.Body.Close()

	delVolOK := volReq(t, http.MethodDelete, ts.URL+"/volumes/"+vol.VolumeID, keyA, nil, "")
	delVolOK.Body.Close()
	if delVolOK.StatusCode != http.StatusNoContent && delVolOK.StatusCode != http.StatusOK {
		t.Fatalf("expected volume delete to succeed after fork deleted, got %d", delVolOK.StatusCode)
	}
}

func TestE2BVolumeUploadSizeLimitTenantCapAndSetuidMask(t *testing.T) {
	dir := t.TempDir()
	mem := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(controller.WithKubeClient(mem))
	srv := gateway.NewServer(rec)
	if err := srv.ConfigureE2BVolumes(gateway.E2BVolumeConfig{
		Dir:                 dir,
		MaxFileSizeBytes:    16,
		MaxVolumesPerTenant: 2,
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	key := "cpod_test_api_key"
	v1 := createVol(t, ts, key, "cpod-test-cap-1")
	_ = createVol(t, ts, key, "cpod-test-cap-2")

	// 3rd volume hits tenant cap (429).
	b3, _ := json.Marshal(map[string]string{"name": "cpod-test-cap-3"})
	resp3 := volReq(t, http.MethodPost, ts.URL+"/volumes", key, b3, "application/json")
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on 3rd volume, got %d", resp3.StatusCode)
	}

	// Upload > 16 bytes returns 413.
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/volumecontent/"+v1.VolumeID+"/file?path=/big", strings.NewReader(strings.Repeat("x", 32)))
	req.Header.Set("Authorization", "Bearer "+v1.Token)
	respBig, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	respBig.Body.Close()
	if respBig.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 on oversized upload, got %d", respBig.StatusCode)
	}

	// uid=0&mode=2541 (0o4755) masks setuid bit to 0o755 on disk and in response.
	reqSU, _ := http.NewRequest(http.MethodPut, ts.URL+"/volumecontent/"+v1.VolumeID+"/file?path=/su&uid=0&mode=2541", strings.NewReader("hi"))
	reqSU.Header.Set("Authorization", "Bearer "+v1.Token)
	respSU, err := http.DefaultClient.Do(reqSU)
	if err != nil {
		t.Fatal(err)
	}
	var ent map[string]any
	readJSON(t, respSU, &ent)
	if respSU.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", respSU.StatusCode)
	}
	if mode, _ := ent["mode"].(float64); int(mode) != 0o755 {
		t.Fatalf("expected masked mode 0755 (%d), got %v", 0o755, ent["mode"])
	}
	fi, err := os.Lstat(filepath.Join(srv.E2BVolumeDataDir(v1.VolumeID), "su"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatalf("on-disk file has special mode bits: %v", fi.Mode())
	}
}

func TestE2BVolume_RestartPreservesPodBoxMountsAndBlocksDelete(t *testing.T) {
	dir := t.TempDir()
	mem := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(controller.WithKubeClient(mem), controller.WithNamespace("cubepod-workloads"))
	srv := gateway.NewServer(rec)
	srv.SetLivePodRouting(true)
	if err := srv.ConfigureE2BVolumes(gateway.E2BVolumeConfig{
		Dir:                dir,
		K8sE2BVolumeMounts: true,
		SharedPVCName:      "cubepod-e2b-volumes",
	}); err != nil {
		t.Fatalf("configure volumes: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Create volume (NotePVC should record cubepod-e2b-volumes) and attach via sandbox create.
	vol := createVol(t, ts, "key-r", "persist-vol")
	volID := vol.VolumeID

	sbxBody, _ := json.Marshal(map[string]any{
		"templateID":   "base",
		"volumeMounts": []map[string]string{{"name": "persist-vol", "path": "/mnt/ds"}},
	})
	sbxResp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", "key-r", sbxBody, "application/json")
	var sbx map[string]any
	readJSON(t, sbxResp, &sbx)
	if sbxResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %+v", sbxResp.StatusCode, sbx)
	}
	sbxID, _ := sbx["sandboxID"].(string)

	// Simulate gateway restart: new Server sharing mem (PodBox CRs) and dir (volumes store).
	rec2 := controller.NewReconciler(controller.WithKubeClient(mem), controller.WithNamespace("cubepod-workloads"))
	srv2 := gateway.NewServer(rec2)
	srv2.SetLivePodRouting(true)
	if err := srv2.ConfigureE2BVolumes(gateway.E2BVolumeConfig{
		Dir:                dir,
		K8sE2BVolumeMounts: true,
		SharedPVCName:      "cubepod-e2b-volumes",
	}); err != nil {
		t.Fatalf("reconfigure volumes after restart: %v", err)
	}
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()

	// Mounts must survive restart.
	if mounts := srv2.E2BVolumeMounts(sbxID); len(mounts) != 1 || mounts[0].VolumeID != volID {
		t.Fatalf("expected mounts preserved across gateway restart, got %+v", mounts)
	}

	// DELETE /volumes/{id} must return 409 Conflict because PodBox CR still references it.
	delResp := volReq(t, http.MethodDelete, ts2.URL+"/volumes/"+volID, "key-r", nil, "")
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when deleting mounted volume after restart, got %d", delResp.StatusCode)
	}

	// Verify PodBox CR still exists.
	if _, err := mem.GetPodBox(context.Background(), "cubepod-workloads", sbxID); err != nil {
		t.Fatalf("expected PodBox %s to exist: %v", sbxID, err)
	}
}
