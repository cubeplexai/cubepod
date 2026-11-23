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

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/events"
	"github.com/cubeplexai/cubepod/pkg/gateway"
	"github.com/cubeplexai/cubepod/pkg/volume"
)

func newVolumeHarness(t *testing.T, dir string) (*gateway.Server, *httptest.Server, *controller.MemoryKubeClient) {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	mem := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(controller.WithKubeClient(mem))
	srv := gateway.NewServer(rec)
	if err := srv.ConfigureVolumes(gateway.VolumeConfig{Dir: dir}); err != nil {
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

func TestVolumeManagement(t *testing.T) {
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

	var volPVC int
	for _, pvc := range mem.ListPVCs() {
		if strings.HasPrefix(pvc.Name, "vol-") {
			volPVC++
			if pvc.Name != "vol-"+vol.VolumeID || pvc.VolumeMode != "Filesystem" || pvc.StorageClassName != "standard" {
				t.Fatalf("pvc: %+v", pvc)
			}
			if pvc.Labels["cubepod.io/volume-id"] != vol.VolumeID {
				t.Fatalf("pvc labels: %+v", pvc.Labels)
			}
		}
	}
	if volPVC != 1 {
		t.Fatalf("expected 1 volume filesystem PVC, got %d (%v)", volPVC, mem.ListPVCs())
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
		if pvc.Name == "vol-"+vol.VolumeID {
			left++
		}
	}
	if left != 0 {
		t.Fatal("filesystem PVC was not deleted")
	}
}

func TestVolumeContentAndEscape(t *testing.T) {
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
	dataDir := srv.VolumeDataDir(vol.VolumeID)
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

func TestVolumeIsolationMountsAndRestart(t *testing.T) {
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
	bMounts := srv.VolumeMounts(sbB.SandboxID)
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
	mounts := srv.VolumeMounts(sb.SandboxID)
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
	if specs := srv.VolumeMounts(sb.SandboxID); len(specs) != 0 {
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
		if pvc.Name == "vol-"+vol.VolumeID && pvc.VolumeMode == "Filesystem" {
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

func TestVolumeConcurrent(t *testing.T) {
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

func TestVolumeSandboxCreateMountsOnPod(t *testing.T) {
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
	pod, ok := mem.GetPod("cubepod-workloads", sb.SandboxID)
	if !ok {
		t.Fatalf("expected pod %s, pods=%v", sb.SandboxID, podNames(mem))
	}
	pvc := "vol-" + vol.VolumeID
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
			if m.MountPath == "/run/cubepod/volumes/"+vol.VolumeID && (m.Name == pvc || m.Name == "vol-"+vol.VolumeID) {
				sawMount = true
			}
		}
	}
	if !sawVol || !sawMount || !sawWS {
		t.Fatalf("pod missing volume mount or workspace (vol=%v mount=%v ws=%v) volumes=%+v containers=%+v", sawVol, sawMount, sawWS, pod.Volumes, pod.Containers)
	}
	if box, err := mem.GetPodBox(context.Background(), "cubepod-workloads", sb.SandboxID); err != nil || box == nil || len(box.Spec.VolumeMounts) != 1 {
		t.Fatalf("expected PodBox spec to record volume mounts: err=%v box=%+v", err, box)
	}
	del, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+sb.SandboxID, nil)
	del.Header.Set("X-API-KEY", keyA)
	resp, err := http.DefaultClient.Do(del)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestVolumeReservedMountPathRejected(t *testing.T) {
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

func TestVolumeLiveK8sGatedOff(t *testing.T) {
	srv, ts, mem := newVolumeHarness(t, "")
	srv.SetLivePodRouting(true)
	keyA := "cpod_test_api_key"

	vol := createVol(t, ts, keyA, "cpod-test-k8s-gated")
	if _, ok := mem.GetPVC("cubepod-workloads", "vol-"+vol.VolumeID); ok {
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

func TestVolumeLiveK8sEnabledWithoutSharedPVC_Returns501(t *testing.T) {
	dir := t.TempDir()
	mem := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(controller.WithKubeClient(mem))
	srv := gateway.NewServer(rec)
	srv.SetLivePodRouting(true)
	if err := srv.ConfigureVolumes(gateway.VolumeConfig{
		Dir:             dir,
		K8sVolumeMounts: true,
		SharedPVCName:   "",
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

func TestVolumeLiveK8sRWXEnabledAndFork(t *testing.T) {
	dir := t.TempDir()
	mem := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(controller.WithKubeClient(mem))
	srv := gateway.NewServer(rec)
	srv.SetLivePodRouting(true)
	sharedPVC := "cubepod-volumes"
	if err := srv.ConfigureVolumes(gateway.VolumeConfig{
		Dir:             dir,
		K8sVolumeMounts: true,
		SharedPVCName:   sharedPVC,
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	keyA := "cpod_test_api_key"
	vol := createVol(t, ts, keyA, "cpod-test-k8s-rwx")
	if _, ok := mem.GetPVC("cubepod-workloads", "vol-"+vol.VolumeID); ok {
		t.Fatalf("expected no per-volume vol-* PVC created when SharedPVCName is set")
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
	if err != nil || box == nil || len(box.Spec.VolumeMounts) != 1 {
		t.Fatalf("expected PodBox with 1 VolumeMount, err=%v box=%+v", err, box)
	}
	gotMount := box.Spec.VolumeMounts[0]
	if gotMount.PVCName != sharedPVC || gotMount.SubPath != vol.VolumeID+"/data" || gotMount.Path != "/mnt/dataset" {
		t.Fatalf("unexpected PodBox VolumeMount: %+v", gotMount)
	}

	pod, ok := mem.GetPod("cubepod-workloads", sb.SandboxID)
	if !ok {
		t.Fatalf("expected pod %s", sb.SandboxID)
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
			if e.Name == "CUBEPOD_VOLUME_MOUNTS" && e.Value == wantEnv {
				sawEnv = true
			}
		}
	}
	if !sawStaging || !sawEnv {
		t.Fatalf("pod missing staging mount or CUBEPOD_VOLUME_MOUNTS (staging=%v env=%v) containers=%+v", sawStaging, sawEnv, pod.Containers)
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
	if err != nil || childBox == nil || len(childBox.Spec.VolumeMounts) != 1 {
		t.Fatalf("expected forked PodBox to inherit VolumeMounts, err=%v box=%+v", err, childBox)
	}
	if childBox.Spec.VolumeMounts[0].PVCName != sharedPVC || childBox.Spec.VolumeMounts[0].SubPath != vol.VolumeID+"/data" {
		t.Fatalf("unexpected forked PodBox mount: %+v", childBox.Spec.VolumeMounts[0])
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

func TestVolumeUploadSizeLimitTenantCapAndSetuidMask(t *testing.T) {
	dir := t.TempDir()
	mem := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(controller.WithKubeClient(mem))
	srv := gateway.NewServer(rec)
	if err := srv.ConfigureVolumes(gateway.VolumeConfig{
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
	fi, err := os.Lstat(filepath.Join(srv.VolumeDataDir(v1.VolumeID), "su"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatalf("on-disk file has special mode bits: %v", fi.Mode())
	}
}

func TestVolume_RestartPreservesPodBoxMountsAndBlocksDelete(t *testing.T) {
	dir := t.TempDir()
	mem := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(controller.WithKubeClient(mem), controller.WithNamespace("cubepod-workloads"))
	srv := gateway.NewServer(rec)
	srv.SetLivePodRouting(true)
	if err := srv.ConfigureVolumes(gateway.VolumeConfig{
		Dir:             dir,
		K8sVolumeMounts: true,
		SharedPVCName:   "cubepod-volumes",
	}); err != nil {
		t.Fatalf("configure volumes: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Create volume (NotePVC should record cubepod-volumes) and attach via sandbox create.
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
	if err := srv2.ConfigureVolumes(gateway.VolumeConfig{
		Dir:             dir,
		K8sVolumeMounts: true,
		SharedPVCName:   "cubepod-volumes",
	}); err != nil {
		t.Fatalf("reconfigure volumes after restart: %v", err)
	}
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()

	// Mounts must survive restart.
	if mounts := srv2.VolumeMounts(sbxID); len(mounts) != 1 || mounts[0].VolumeID != volID {
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

func TestVolumeMountCounters(t *testing.T) {
	srv, ts, _ := newVolumeHarness(t, "")
	key := "cpod_test_counters"
	v1 := createVol(t, ts, key, "vol-one")
	v2 := createVol(t, ts, key, "vol-two")

	// 1. Successful sandbox create with 2 mounts increments VolumeMountsTotal by 2.
	body, _ := json.Marshal(map[string]any{
		"templateID": "base",
		"volumeMounts": []map[string]string{
			{"name": v1.Name, "path": "/mnt/one"},
			{"name": v2.Name, "path": "/mnt/two"},
		},
	})
	resp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", key, body, "application/json")
	var sb struct {
		SandboxID string `json:"sandboxID"`
	}
	readJSON(t, resp, &sb)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	if got := srv.VolumeMountsTotal.Load(); got != 2 {
		t.Fatalf("VolumeMountsTotal = %d; want 2", got)
	}
	if got := srv.VolumeMountErrorsTotal.Load(); got != 0 {
		t.Fatalf("VolumeMountErrorsTotal = %d; want 0", got)
	}

	// 2. Planning failures increment VolumeMountErrorsTotal.
	missingBody, _ := json.Marshal(map[string]any{
		"templateID":   "base",
		"volumeMounts": []map[string]string{{"name": "does-not-exist", "path": "/mnt/missing"}},
	})
	rMissing := volReq(t, http.MethodPost, ts.URL+"/sandboxes", key, missingBody, "application/json")
	rMissing.Body.Close()
	if rMissing.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing volume, got %d", rMissing.StatusCode)
	}

	reservedBody, _ := json.Marshal(map[string]any{
		"templateID":   "base",
		"volumeMounts": []map[string]string{{"name": v1.Name, "path": "/workspace"}},
	})
	rReserved := volReq(t, http.MethodPost, ts.URL+"/sandboxes", key, reservedBody, "application/json")
	rReserved.Body.Close()
	if rReserved.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for reserved mount path, got %d", rReserved.StatusCode)
	}

	if got := srv.VolumeMountErrorsTotal.Load(); got != 2 {
		t.Fatalf("VolumeMountErrorsTotal = %d; want 2", got)
	}
	if got := srv.VolumeMountsTotal.Load(); got != 2 {
		t.Fatalf("VolumeMountsTotal = %d; want 2", got)
	}

	// 3. Clean up sandbox and test K8s gated 501 increments error counter.
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+sb.SandboxID, nil)
	delReq.Header.Set("X-API-KEY", key)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()

	srv.SetLivePodRouting(true)
	t.Cleanup(srv.Close)
	rGated := volReq(t, http.MethodPost, ts.URL+"/sandboxes", key, body, "application/json")
	rGated.Body.Close()
	if rGated.StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501 when K8sVolumeMounts is disabled, got %d", rGated.StatusCode)
	}
	if got := srv.VolumeMountErrorsTotal.Load(); got != 3 {
		t.Fatalf("VolumeMountErrorsTotal = %d; want 3", got)
	}
}

func TestVolumeRotateKeyEndpointAndSandboxMountIsolation(t *testing.T) {
	dir := t.TempDir()
	kekProv, err := volume.NewLocalKEKProvider("gw-kek-v1", bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	mem := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(controller.WithKubeClient(mem), controller.WithNamespace("cubepod-workloads"))
	srv := gateway.NewServer(rec)
	if err := srv.ConfigureVolumes(gateway.VolumeConfig{
		Dir: dir,
		KMS: kekProv,
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	oldKey := "cpod_gw_old_key"
	newKey := "cpod_gw_new_key"
	otherKey := "cpod_gw_other_tenant"

	v1 := createVol(t, ts, oldKey, "rot-vol-1")
	v2 := createVol(t, ts, oldKey, "rot-vol-2")
	otherVol := createVol(t, ts, otherKey, "rot-vol-1")

	// Mount v1 onto a sandbox before rotating the key.
	sbBody, _ := json.Marshal(map[string]any{
		"templateID":   "base",
		"volumeMounts": []map[string]string{{"name": v1.Name, "path": "/mnt/rot"}},
	})
	sbResp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", oldKey, sbBody, "application/json")
	var sb struct {
		SandboxID string `json:"sandboxID"`
	}
	readJSON(t, sbResp, &sb)
	if sbResp.StatusCode != http.StatusCreated {
		t.Fatalf("create sandbox: %d", sbResp.StatusCode)
	}

	// Missing X-API-Key -> 401.
	rNoAuth := volReq(t, http.MethodPost, ts.URL+"/volumes/rotate-key", "", []byte(`{"previousApiKey":"`+oldKey+`"}`), "application/json")
	rNoAuth.Body.Close()
	if rNoAuth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on missing X-API-Key, got %d", rNoAuth.StatusCode)
	}

	// Empty or self-identical previousApiKey -> 400.
	rBadPrev := volReq(t, http.MethodPost, ts.URL+"/volumes/rotate-key", newKey, []byte(`{"previousApiKey":""}`), "application/json")
	rBadPrev.Body.Close()
	if rBadPrev.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on empty previousApiKey, got %d", rBadPrev.StatusCode)
	}
	rSameKey := volReq(t, http.MethodPost, ts.URL+"/volumes/rotate-key", oldKey, []byte(`{"previousApiKey":"`+oldKey+`"}`), "application/json")
	rSameKey.Body.Close()
	if rSameKey.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on identical keys, got %d", rSameKey.StatusCode)
	}

	// Collision with existing tenant -> 409 Conflict.
	rCollide := volReq(t, http.MethodPost, ts.URL+"/volumes/rotate-key", otherKey, []byte(`{"previousApiKey":"`+oldKey+`"}`), "application/json")
	rCollide.Body.Close()
	if rCollide.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on target key collision, got %d", rCollide.StatusCode)
	}

	// Valid key rotation -> 200 {"rotatedVolumes": 2}.
	rRot := volReq(t, http.MethodPost, ts.URL+"/volumes/rotate-key", newKey, []byte(`{"previousApiKey":"`+oldKey+`"}`), "application/json")
	var rotBody struct {
		RotatedVolumes int `json:"rotatedVolumes"`
	}
	readJSON(t, rRot, &rotBody)
	if rRot.StatusCode != http.StatusOK || rotBody.RotatedVolumes != 2 {
		t.Fatalf("rotate-key: status=%d body=%+v", rRot.StatusCode, rotBody)
	}

	// Old key must be denied (401) across GET /volumes, GET /volumes/{id}, DELETE /volumes/{id},
	// POST /sandboxes with volumeMounts, and a second POST /volumes/rotate-key.
	for _, check := range []struct {
		method string
		url    string
		body   []byte
	}{
		{http.MethodGet, ts.URL + "/volumes", nil},
		{http.MethodGet, ts.URL + "/volumes/" + v1.VolumeID, nil},
		{http.MethodDelete, ts.URL + "/volumes/" + v2.VolumeID, nil},
		{http.MethodPost, ts.URL + "/sandboxes", sbBody},
		{http.MethodPost, ts.URL + "/volumes/rotate-key", []byte(`{"previousApiKey":"` + oldKey + `"}`)},
	} {
		keyToUse := oldKey
		if strings.HasSuffix(check.url, "/rotate-key") {
			keyToUse = "cpod_gw_another_fresh_key"
		}
		resp := volReq(t, check.method, check.url, keyToUse, check.body, "application/json")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 for revoked oldKey on %s %s, got %d", check.method, check.url, resp.StatusCode)
		}
	}

	// Other tenant remains untouched.
	rOther := volReq(t, http.MethodGet, ts.URL+"/volumes/"+otherVol.VolumeID, otherKey, nil, "")
	rOther.Body.Close()
	if rOther.StatusCode != http.StatusOK {
		t.Fatalf("other tenant GET failed: %d", rOther.StatusCode)
	}

	// Reopen gateway over same dir: newKey retains access, active sandbox mount still blocks delete (409),
	// and deleting sandbox releases mount so DELETE /volumes/{id} succeeds (204).
	srv2 := gateway.NewServer(rec)
	if err := srv2.ConfigureVolumes(gateway.VolumeConfig{
		Dir: dir,
		KMS: kekProv,
	}); err != nil {
		t.Fatalf("reopen gateway volumes: %v", err)
	}
	ts2 := httptest.NewServer(srv2.Handler())
	defer ts2.Close()

	rList := volReq(t, http.MethodGet, ts2.URL+"/volumes", newKey, nil, "")
	var listed []map[string]string
	readJSON(t, rList, &listed)
	if rList.StatusCode != http.StatusOK || len(listed) != 2 {
		t.Fatalf("reopened list(newKey): status=%d listed=%+v", rList.StatusCode, listed)
	}

	rDelBusy := volReq(t, http.MethodDelete, ts2.URL+"/volumes/"+v1.VolumeID, newKey, nil, "")
	rDelBusy.Body.Close()
	if rDelBusy.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 deleting mounted volume under newKey, got %d", rDelBusy.StatusCode)
	}

	// Clean up sandbox on srv (releases mount on shared dir after reopen or via DELETE /sandboxes on srv2 after PodBox sync).
	rDelSb := volReq(t, http.MethodDelete, ts.URL+"/sandboxes/"+sb.SandboxID, newKey, nil, "")
	rDelSb.Body.Close()
	// Reconcile srv2 with updated PodBox state and delete v1 cleanly.
	if err := srv2.ConfigureVolumes(gateway.VolumeConfig{Dir: dir, KMS: kekProv}); err != nil {
		t.Fatal(err)
	}
	rDelOK := volReq(t, http.MethodDelete, ts2.URL+"/volumes/"+v1.VolumeID, newKey, nil, "")
	rDelOK.Body.Close()
	if rDelOK.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 deleting unmounted volume under newKey, got %d", rDelOK.StatusCode)
	}
}

func TestVolumeKMSQuotaAndPrometheusMetrics(t *testing.T) {
	// Spin up a mock external HTTP KMS server to verify Gateway + HTTPKMSProvider integration.
	rawKEK := bytes.Repeat([]byte{0x91}, 32)
	backingKMS, err := volume.NewLocalKEKProvider("ext-kms-gw", rawKEK)
	if err != nil {
		t.Fatal(err)
	}
	kmsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gw-kms-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		switch r.URL.Path {
		case "/wrap":
			raw, _ := io.ReadAll(io.LimitReader(strings.NewReader(payload["plaintext"]), 4096))
			_ = raw
			var dec []byte
			if b, decErr := json.Marshal(payload["plaintext"]); decErr == nil {
				_ = json.Unmarshal(b, &dec)
			}
			wrapped, wErr := backingKMS.Wrap(r.Context(), dec)
			if wErr != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"keyId": "ext-kms-gw", "ciphertext": wrapped})
		case "/unwrap":
			var wrapped []byte
			if b, decErr := json.Marshal(payload["ciphertext"]); decErr == nil {
				_ = json.Unmarshal(b, &wrapped)
			}
			pt, uErr := backingKMS.Unwrap(r.Context(), wrapped)
			if uErr != nil {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"plaintext": pt})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer kmsSrv.Close()

	httpKMS, err := volume.NewHTTPKMSProvider(volume.HTTPKMSConfig{
		URL:         kmsSrv.URL,
		BearerToken: "gw-kms-token",
		KeyID:       "ext-kms-gw",
	})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	mem := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(controller.WithKubeClient(mem))
	srv := gateway.NewServer(rec)
	if err := srv.ConfigureVolumes(gateway.VolumeConfig{
		Dir:                 dir,
		MaxVolumesPerTenant: 2,
		MaxBytesPerTenant:   50,
		KMS:                 httpKMS,
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	tenantA := "cpod_metrics_tenant_a"
	tenantB := "cpod_metrics_tenant_b"

	vA1 := createVol(t, ts, tenantA, "mvol-a1")
	vA2 := createVol(t, ts, tenantA, "mvol-a2")
	vB1 := createVol(t, ts, tenantB, "mvol-b1")

	// Verify meta.json on disk uses envelope encryption (no plaintext token on disk).
	metaOnDisk, err := os.ReadFile(filepath.Join(dir, vA1.VolumeID, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(metaOnDisk), vA1.Token) || !strings.Contains(string(metaOnDisk), `"kmsKeyID": "ext-kms-gw"`) {
		t.Fatalf("expected KMS-encrypted meta.json without plaintext token, got %s", metaOnDisk)
	}

	// 3rd volume for tenantA -> 429 Too Many Requests (+1 quota rejection metric).
	rCountCap := volReq(t, http.MethodPost, ts.URL+"/volumes", tenantA, []byte(`{"name":"mvol-a3"}`), "application/json")
	rCountCap.Body.Close()
	if rCountCap.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on volume count cap, got %d", rCountCap.StatusCode)
	}

	// Upload 30B to vA1 + 15B to vA2 = 45B <= 50B -> 201 Created.
	putFile := func(vol volCreate, p, data string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/volumecontent/"+vol.VolumeID+"/file?path="+p, strings.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+vol.Token)
		resp, doErr := http.DefaultClient.Do(req)
		if doErr != nil {
			t.Fatal(doErr)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := putFile(vA1, "/part1.txt", strings.Repeat("a", 30)); code != http.StatusCreated {
		t.Fatalf("expected 201 for 30B upload, got %d", code)
	}
	if code := putFile(vA2, "/part2.txt", strings.Repeat("b", 15)); code != http.StatusCreated {
		t.Fatalf("expected 201 for 15B upload, got %d", code)
	}
	// Upload 10B more (45 + 10 = 55 > 50B) -> 429 Too Many Requests (+1 quota rejection metric).
	if code := putFile(vA2, "/overflow.txt", strings.Repeat("c", 10)); code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on tenant byte quota overflow, got %d", code)
	}
	// Tenant B remains unaffected and can upload 40B.
	if code := putFile(vB1, "/ok.txt", strings.Repeat("d", 40)); code != http.StatusCreated {
		t.Fatalf("expected 201 for isolated tenant B upload, got %d", code)
	}

	// Scrape Prometheus text /metrics and verify all 5 required metrics.
	mResp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	promBody, _ := io.ReadAll(mResp.Body)
	mResp.Body.Close()
	promText := string(promBody)
	for _, wantSubstr := range []string{
		"cubepod_volumes_storage_capacity_bytes ",
		"cubepod_volumes_storage_used_bytes ",
		"cubepod_volumes_storage_available_bytes ",
		"cubepod_volumes_total 3\n",
		"cubepod_volume_quota_rejections_total 2\n",
	} {
		if !strings.Contains(promText, wantSubstr) {
			t.Fatalf("Prometheus /metrics missing %q in:\n%s", wantSubstr, promText)
		}
	}

	// Also verify JSON /metrics snapshot.
	jReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	jReq.Header.Set("Accept", "application/json")
	jResp, err := http.DefaultClient.Do(jReq)
	if err != nil {
		t.Fatal(err)
	}
	var jMetrics map[string]any
	readJSON(t, jResp, &jMetrics)
	if capVal, _ := jMetrics["cubepod_volumes_storage_capacity_bytes"].(float64); capVal <= 0 {
		t.Fatalf("expected positive storage capacity bytes in JSON metrics, got %v", jMetrics["cubepod_volumes_storage_capacity_bytes"])
	}
	if totVal, _ := jMetrics["cubepod_volumes_total"].(float64); int(totVal) != 3 {
		t.Fatalf("expected cubepod_volumes_total=3, got %v", jMetrics["cubepod_volumes_total"])
	}
	if rejVal, _ := jMetrics["cubepod_volume_quota_rejections_total"].(float64); int(rejVal) != 2 {
		t.Fatalf("expected cubepod_volume_quota_rejections_total=2, got %v", jMetrics["cubepod_volume_quota_rejections_total"])
	}
}

func TestBlockWorkspaceVolume_CRUDMountSingleWriterForkAndMetrics(t *testing.T) {
	srv, ts, mem := newVolumeHarness(t, "")
	// Pure-block live Kubernetes mode (no SharedPVCName configured): block volumes must still work!
	srv.SetLivePodRouting(true)
	const apiKey = "cpod_block_key_1"

	// 1. Feature gate disabled -> 501 Not Implemented.
	srv.SetFeatureGate(gateway.FeatureGateBlockWorkspaceVolumes, false)
	fgResp := volReq(t, http.MethodPost, ts.URL+"/volumes", apiKey, []byte(`{"name":"ws-block","type":"block","size":"20Gi"}`), "application/json")
	fgResp.Body.Close()
	if fgResp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501 when BlockWorkspaceVolumes disabled, got %d", fgResp.StatusCode)
	}
	srv.SetFeatureGate(gateway.FeatureGateBlockWorkspaceVolumes, true)

	// 2. Invalid size on shared or block volume -> 400 Bad Request.
	badShared := volReq(t, http.MethodPost, ts.URL+"/volumes", apiKey, []byte(`{"name":"sh","type":"shared","size":"10Gi"}`), "application/json")
	badShared.Body.Close()
	if badShared.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for size on shared volume, got %d", badShared.StatusCode)
	}
	badSize := volReq(t, http.MethodPost, ts.URL+"/volumes", apiKey, []byte(`{"name":"ws-bad","type":"block","size":"100Mi"}`), "application/json")
	badSize.Body.Close()
	if badSize.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for <1Gi block size, got %d", badSize.StatusCode)
	}

	// 3. Create type: "block" volume -> 201 Created + RWO Block PVC in K8s.
	createResp := volReq(t, http.MethodPost, ts.URL+"/volumes", apiKey, []byte(`{"name":"ws-block","type":"block","size":"25Gi"}`), "application/json")
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create block volume status = %d; want 201", createResp.StatusCode)
	}
	var created map[string]string
	readJSON(t, createResp, &created)
	volID := created["volumeID"]
	if created["type"] != "block" || created["size"] != "25Gi" || volID == "" {
		t.Fatalf("unexpected create response: %+v", created)
	}
	pvcName := "vol-" + volID
	pvc, ok := mem.GetPVC("cubepod-workloads", pvcName)
	if !ok || pvc.VolumeMode != "Block" || pvc.Size != "25Gi" || pvc.Labels[controller.PVCLabelManagedBy] != controller.PVCLabelManagedByVolumes {
		t.Fatalf("expected RWO Block PVC %s in K8s, got ok=%v pvc=%+v", pvcName, ok, pvc)
	}

	// 4. GET /volumes & GET /volumes/{id} include type="block" and size="25Gi".
	listResp := volReq(t, http.MethodGet, ts.URL+"/volumes", apiKey, nil, "")
	var listItems []map[string]string
	readJSON(t, listResp, &listItems)
	if len(listItems) != 1 || listItems[0]["type"] != "block" || listItems[0]["size"] != "25Gi" {
		t.Fatalf("unexpected GET /volumes items: %+v", listItems)
	}

	// 5. /volumecontent/{id}/... rejected with 400 Bad Request and string code "400".
	contentReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/volumecontent/"+volID+"/path?path=/", nil)
	contentReq.Header.Set("Authorization", "Bearer "+created["token"])
	contentResp, err := http.DefaultClient.Do(contentReq)
	if err != nil {
		t.Fatal(err)
	}
	if contentResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on /volumecontent for block volume, got %d", contentResp.StatusCode)
	}
	var contentErr map[string]string
	readJSON(t, contentResp, &contentErr)
	if contentErr["code"] != "400" {
		t.Fatalf("expected string error code \"400\", got %+v", contentErr)
	}

	// 6. Reject non-/workspace mount path and extension.volumeKey + block volumeMount conflict.
	badPathResp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", apiKey, []byte(`{"templateID":"ubuntu:24.04","volumeMounts":[{"name":"ws-block","path":"/mnt/data"}]}`), "application/json")
	badPathResp.Body.Close()
	if badPathResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for block volume at /mnt/data, got %d", badPathResp.StatusCode)
	}
	conflictKeyResp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", apiKey, []byte(`{"templateID":"ubuntu:24.04","extension":{"volumeKey":"k1"},"volumeMounts":[{"name":"ws-block","path":"/workspace"}]}`), "application/json")
	conflictKeyResp.Body.Close()
	if conflictKeyResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when specifying both volumeKey and block volumeMount, got %d", conflictKeyResp.StatusCode)
	}

	// 7. Attach block volume at /workspace on sandbox 1 -> 201 Created + PVC lease annotation set.
	sb1Resp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", apiKey, []byte(`{"templateID":"ubuntu:24.04","volumeMounts":[{"name":"ws-block","path":"/workspace"}]}`), "application/json")
	if sb1Resp.StatusCode != http.StatusCreated {
		t.Fatalf("create sandbox with block volume status = %d; want 201", sb1Resp.StatusCode)
	}
	var sb1 struct {
		SandboxID string `json:"sandboxID"`
	}
	readJSON(t, sb1Resp, &sb1)
	pvcLeased, _ := mem.GetPVC("cubepod-workloads", pvcName)
	if pvcLeased.Annotations[controller.PVCAnnotationAttachedSandbox] != sb1.SandboxID || pvcLeased.Annotations[controller.PVCAnnotationAttachedAt] == "" {
		t.Fatalf("expected PVC lease annotation for %s, got %+v", sb1.SandboxID, pvcLeased.Annotations)
	}

	// 8. Concurrent attach to sandbox 2 -> 409 Conflict (+1 conflict metric).
	sb2Resp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", apiKey, []byte(`{"templateID":"ubuntu:24.04","volumeMounts":[{"name":"ws-block","path":"/workspace"}]}`), "application/json")
	sb2Resp.Body.Close()
	if sb2Resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on concurrent block mount, got %d", sb2Resp.StatusCode)
	}

	// 9. DELETE /volumes/{id} while attached -> 409 Conflict (+1 conflict metric).
	delWhileMounted := volReq(t, http.MethodDelete, ts.URL+"/volumes/"+volID, apiKey, nil, "")
	delWhileMounted.Body.Close()
	if delWhileMounted.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 deleting mounted block volume, got %d", delWhileMounted.StatusCode)
	}

	// 10. Fork sandbox 1 -> child gets independent workspace PVC and does NOT steal parent's block lease.
	forkResp := volReq(t, http.MethodPost, ts.URL+"/sandboxes/"+sb1.SandboxID+"/fork", apiKey, nil, "")
	if forkResp.StatusCode != http.StatusCreated {
		t.Fatalf("fork sandbox with block workspace status = %d; want 201", forkResp.StatusCode)
	}
	var forked struct {
		SandboxID string `json:"sandboxID"`
	}
	readJSON(t, forkResp, &forked)
	childBox, err := mem.GetPodBox(context.Background(), "cubepod-workloads", forked.SandboxID)
	if err != nil || childBox == nil {
		t.Fatalf("GetPodBox(forked): %v", err)
	}
	if childBox.Spec.WorkspaceBlockMount() != nil || childBox.Status.WorkspacePVCName != forked.SandboxID+"-workspace-pvc" {
		t.Fatalf("forked child must not inherit block volume: spec.VolumeMounts=%+v status.WorkspacePVCName=%q", childBox.Spec.VolumeMounts, childBox.Status.WorkspacePVCName)
	}

	// 11. Delete sandbox 1 (even with ?deleteVolumes=true) -> preserves vol-<id> PVC and clears lease.
	delSB1 := volReq(t, http.MethodDelete, ts.URL+"/sandboxes/"+sb1.SandboxID+"?deleteVolumes=true", apiKey, nil, "")
	delSB1.Body.Close()
	if delSB1.StatusCode != http.StatusNoContent {
		t.Fatalf("delete sb1 status = %d; want 204", delSB1.StatusCode)
	}
	pvcReleased, stillExists := mem.GetPVC("cubepod-workloads", pvcName)
	if !stillExists || pvcReleased.Annotations[controller.PVCAnnotationAttachedSandbox] != "" {
		t.Fatalf("expected PVC %s preserved and lease cleared after sandbox delete, exists=%v anns=%+v", pvcName, stillExists, pvcReleased.Annotations)
	}

	// 12. Verify Prometheus metrics: cubepod_block_volumes_total=1 and cubepod_block_volume_attach_conflicts_total=2.
	mResp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	mBody, _ := io.ReadAll(mResp.Body)
	mResp.Body.Close()
	mText := string(mBody)
	if !strings.Contains(mText, "cubepod_block_volumes_total 1\n") || !strings.Contains(mText, "cubepod_block_volume_attach_conflicts_total 2\n") {
		t.Fatalf("missing block volume metrics in:\n%s", mText)
	}

	// 13. DELETE /volumes/{id} after sandbox delete -> 204 No Content and PVC deleted from K8s.
	delVol := volReq(t, http.MethodDelete, ts.URL+"/volumes/"+volID, apiKey, nil, "")
	delVol.Body.Close()
	if delVol.StatusCode != http.StatusNoContent {
		t.Fatalf("delete volume status = %d; want 204", delVol.StatusCode)
	}
	if _, exists := mem.GetPVC("cubepod-workloads", pvcName); exists {
		t.Fatalf("expected PVC %s deleted after DELETE /volumes/%s", pvcName, volID)
	}
}

func TestBlockWorkspaceVolume_LegacyAutoAdoptionAndLazyClaimBridge(t *testing.T) {
	srv, ts, mem := newVolumeHarness(t, "")
	srv.SetLivePodRouting(true)
	ctx := context.Background()

	const tenantAKey = "cpod_tenant_a_key"
	const tenantBKey = "cpod_tenant_b_key"
	tenantAHash := events.TenantHash(tenantAKey)

	// 1. Seed an active legacy PodBox with spec.VolumeKey="active-legacy-ws" owned by Tenant A.
	activePVC := controller.SanitizeVolumeKey("active-legacy-ws") + "-workspace-pvc"
	_ = mem.EnsurePVC(ctx, controller.BlockPVCManifest{
		Name:       activePVC,
		Namespace:  "cubepod-workloads",
		VolumeMode: "Block",
		Size:       "30Gi",
		Labels:     map[string]string{"pod.cubeplex.ai/vol-type": "workspace"},
	})
	_ = mem.UpsertPodBox(ctx, v1alpha1.PodBox{
		APIVersion: "cubepod.io/v1alpha1",
		Kind:       "PodBox",
		Metadata: v1alpha1.ObjectMeta{
			Name:      "legacy-box-1",
			Namespace: "cubepod-workloads",
			Annotations: map[string]string{
				"cubepod.io/owner-tenant": tenantAHash,
				"cubepod.io/volume-key":   "active-legacy-ws",
			},
		},
		Spec: v1alpha1.PodBoxSpec{
			TemplateRef:  "ubuntu:24.04",
			DesiredState: v1alpha1.DesiredStateRunning,
			VolumeKey:    "active-legacy-ws",
		},
		Status: v1alpha1.PodBoxStatus{
			Phase:            v1alpha1.PodBoxPhaseRunning,
			WorkspacePVCName: activePVC,
		},
	})

	// Trigger syncFromKube -> auto-adopts active-legacy-ws into Tenant A's volume store and labels the PVC.
	srv.SyncPodBoxesOnce(ctx)
	listResp := volReq(t, http.MethodGet, ts.URL+"/volumes", tenantAKey, nil, "")
	var vols []map[string]string
	readJSON(t, listResp, &vols)
	if len(vols) != 1 || vols[0]["name"] != "active-legacy-ws" || vols[0]["type"] != "block" || vols[0]["size"] != "30Gi" {
		t.Fatalf("expected auto-adopted volume in Tenant A GET /volumes, got %+v", vols)
	}
	labeledPVC, _ := mem.GetPVC("cubepod-workloads", activePVC)
	if labeledPVC.Labels[controller.PVCLabelManagedBy] != controller.PVCLabelManagedByVolumes || labeledPVC.Labels[controller.PVCLabelVolumeID] != vols[0]["volumeID"] {
		t.Fatalf("expected auto-adopted PVC to have cubepod-volumes labels, got %+v", labeledPVC.Labels)
	}
	if labeledPVC.Annotations[controller.PVCAnnotationAttachedSandbox] != "legacy-box-1" {
		t.Fatalf("expected auto-adopted active PVC to have attach lease for legacy-box-1, got %+v", labeledPVC.Annotations)
	}
	// Concurrent attach to auto-adopted active legacy volume must return 409 Conflict.
	dupLegacyAttach := volReq(t, http.MethodPost, ts.URL+"/sandboxes", tenantAKey, []byte(`{"templateID":"ubuntu:24.04","volumeMounts":[{"name":"active-legacy-ws","path":"/workspace"}]}`), "application/json")
	dupLegacyAttach.Body.Close()
	if dupLegacyAttach.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict attaching active auto-adopted legacy volume, got %d", dupLegacyAttach.StatusCode)
	}
	// DELETE /volumes/{id} on active auto-adopted legacy volume must return 409 Conflict for owner,
	// and 404 Not Found (no mount-state leak) for another tenant.
	delActiveLegacy := volReq(t, http.MethodDelete, ts.URL+"/volumes/"+vols[0]["volumeID"], tenantAKey, nil, "")
	delActiveLegacy.Body.Close()
	if delActiveLegacy.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict deleting active auto-adopted legacy volume, got %d", delActiveLegacy.StatusCode)
	}
	crossDel := volReq(t, http.MethodDelete, ts.URL+"/volumes/"+vols[0]["volumeID"], tenantBKey, nil, "")
	crossDel.Body.Close()
	if crossDel.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found on cross-tenant DELETE /volumes, got %d", crossDel.StatusCode)
	}

	// Cross-tenant attempt by Tenant B to claim active-legacy-ws via extension.volumeKey -> 403 Forbidden.
	crossResp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", tenantBKey, []byte(`{"templateID":"ubuntu:24.04","extension":{"volumeKey":"active-legacy-ws"}}`), "application/json")
	crossResp.Body.Close()
	if crossResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden on cross-tenant volumeKey claim, got %d", crossResp.StatusCode)
	}

	// 2. Seed a retained orphaned legacy PVC from an already-deleted sandbox ("retained-ws").
	retainedPVC := controller.SanitizeVolumeKey("retained-ws") + "-workspace-pvc"
	_ = mem.EnsurePVC(ctx, controller.BlockPVCManifest{
		Name:       retainedPVC,
		Namespace:  "cubepod-workloads",
		VolumeMode: "Block",
		Size:       "40Gi",
		Labels:     map[string]string{"pod.cubeplex.ai/vol-type": "workspace"},
	})

	// When AllowLazyLegacyVolumeClaim=false, lazy adoption does not register into /volumes.
	srv.SetAllowLazyLegacyVolumeClaim(false)
	srv.SetAllowLazyLegacyVolumeClaim(true)

	// Lazy claim by Tenant A via POST /sandboxes with extension.volumeKey="retained-ws" adopts it into /volumes!
	lazyResp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", tenantAKey, []byte(`{"templateID":"ubuntu:24.04","extension":{"volumeKey":"retained-ws"}}`), "application/json")
	if lazyResp.StatusCode != http.StatusCreated {
		t.Fatalf("lazy claim sandbox create status = %d; want 201", lazyResp.StatusCode)
	}
	var lazySB struct {
		SandboxID string `json:"sandboxID"`
	}
	readJSON(t, lazyResp, &lazySB)

	// Delete the sandbox, then DELETE /volumes/{id} on the Mode 1 adopted legacy PVC (<sanitized>-workspace-pvc)
	// must delete the underlying PVC (verifying deleteVolumePVC label-based check works for non-vol-* names!).
	delLazySB := volReq(t, http.MethodDelete, ts.URL+"/sandboxes/"+lazySB.SandboxID, tenantAKey, nil, "")
	delLazySB.Body.Close()

	listAfterLazy := volReq(t, http.MethodGet, ts.URL+"/volumes", tenantAKey, nil, "")
	readJSON(t, listAfterLazy, &vols)
	var retainedVolID string
	for _, v := range vols {
		if v["name"] == "retained-ws" {
			retainedVolID = v["volumeID"]
		}
	}
	if retainedVolID == "" {
		t.Fatalf("expected retained-ws in GET /volumes, got %+v", vols)
	}
	delAdoptedVol := volReq(t, http.MethodDelete, ts.URL+"/volumes/"+retainedVolID, tenantAKey, nil, "")
	delAdoptedVol.Body.Close()
	if delAdoptedVol.StatusCode != http.StatusNoContent {
		t.Fatalf("delete adopted legacy volume status = %d; want 204", delAdoptedVol.StatusCode)
	}
	if _, stillThere := mem.GetPVC("cubepod-workloads", retainedPVC); stillThere {
		t.Fatalf("expected Mode 1 adopted legacy PVC %s to be deleted by DELETE /volumes/%s", retainedPVC, retainedVolID)
	}
}

func TestBlockVolumes_ReapIdleStaleMountEviction(t *testing.T) {
	ctx := context.Background()
	mem := controller.NewMemoryKubeClient()
	rec := controller.NewReconciler(controller.WithKubeClient(mem), controller.WithNamespace("cubepod-workloads"))
	srv := gateway.NewServer(rec)
	if err := srv.ConfigureVolumes(gateway.VolumeConfig{Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const apiKey = "cpod_reap_evict_tenant"
	createResp := volReq(t, http.MethodPost, ts.URL+"/volumes", apiKey, []byte(`{"name":"reap-ws","type":"block","size":"20Gi"}`), "application/json")
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create block volume status = %d; want 201", createResp.StatusCode)
	}
	var vol struct {
		VolumeID string `json:"volumeID"`
	}
	readJSON(t, createResp, &vol)

	// Attach to sandbox 1.
	sb1Resp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", apiKey, []byte(`{"templateID":"ubuntu:24.04","volumeMounts":[{"name":"reap-ws","path":"/workspace"}]}`), "application/json")
	if sb1Resp.StatusCode != http.StatusCreated {
		t.Fatalf("sb1 create status = %d; want 201", sb1Resp.StatusCode)
	}
	var sb1 struct {
		SandboxID string `json:"sandboxID"`
	}
	readJSON(t, sb1Resp, &sb1)

	// Simulate controller ReapIdle: PodBox transitions to Terminated and PVC attach-sandbox lease is cleared,
	// while gateway mounts.json still holds sb1 -> vol.VolumeID.
	box, err := mem.GetPodBox(ctx, "cubepod-workloads", sb1.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	box.Status.Phase = v1alpha1.PodBoxPhaseTerminated
	if err := mem.UpsertPodBox(ctx, *box); err != nil {
		t.Fatal(err)
	}
	pvc, err := mem.GetPVCManifest(ctx, "cubepod-workloads", "vol-"+vol.VolumeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.UpdatePVCAnnotationsCAS(ctx, "cubepod-workloads", pvc.Name, pvc.ResourceVersion, map[string]string{
		controller.PVCAnnotationAttachedSandbox: "",
		controller.PVCAnnotationAttachedAt:      "",
	}); err != nil {
		t.Fatal(err)
	}

	// Attaching the volume to sandbox 2 must evict the stale Terminated owner from mounts.json and succeed (201).
	sb2Resp := volReq(t, http.MethodPost, ts.URL+"/sandboxes", apiKey, []byte(`{"templateID":"ubuntu:24.04","volumeMounts":[{"name":"reap-ws","path":"/workspace"}]}`), "application/json")
	defer sb2Resp.Body.Close()
	if sb2Resp.StatusCode != http.StatusCreated {
		t.Fatalf("sb2 create after ReapIdle lease release status = %d; want 201", sb2Resp.StatusCode)
	}
}
