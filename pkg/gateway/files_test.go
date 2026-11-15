package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"testing"
)

func TestResolveUploadPath(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		query    string
		single   bool
		want     string
		wantErr  bool
	}{
		{"single relative nested query wins", "dir/a.txt", "dir/a.txt", true, "/workspace/dir/a.txt", false},
		{"single absolute query wins over filename", "other.txt", "/tmp/x.txt", true, "/tmp/x.txt", false},
		{"single trailing slash is directory", "/a/b/c.txt", "out/", true, "/workspace/out/c.txt", false},
		{"single no query uses filename", "/abs/f.txt", "", true, "/abs/f.txt", false},
		{"multi absolute filename", "/abs/f.txt", "", false, "/abs/f.txt", false},
		{"multi relative filename under workdir", "f.txt", "", false, "/workspace/f.txt", false},
		{"multi relative filename under query dir", "f.txt", "d", false, "/workspace/d/f.txt", false},
		{"missing everything", "", "", true, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveUploadPath(tc.filename, tc.query, tc.single, sandboxWorkdir)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func gzipPayload(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readSandboxFile(t *testing.T, baseURL, sandboxID, p string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/files?path="+p, nil)
	req.Header.Set("E2b-Sandbox-Id", sandboxID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", p, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", p, resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
	return data
}

func postSandboxFile(t *testing.T, baseURL, sandboxID, query, contentType, contentEncoding string, body []byte) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/files"+query, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	req.Header.Set("E2b-Sandbox-Id", sandboxID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /files%s: %v", query, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestFiles_POST_PreservesUndeclaredGzipBytes(t *testing.T) {
	_, ts, rec := setupTestServerWithSandbox(t)
	gz := gzipPayload(t, []byte("inner-archive-content"))

	// Octet-stream upload of a real .gz file without Content-Encoding.
	if st := postSandboxFile(t, ts.URL, rec.SandboxID, "?path=/workspace/raw.tar.gz", "application/octet-stream", "", gz); st != http.StatusOK {
		t.Fatalf("octet-stream upload: status %d", st)
	}
	if got := readSandboxFile(t, ts.URL, rec.SandboxID, "/workspace/raw.tar.gz"); !bytes.Equal(got, gz) {
		t.Fatalf("octet-stream .gz bytes were modified: got %q", got)
	}

	// Multipart upload of a real .gz file without a part Content-Encoding.
	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	part, _ := mw.CreateFormFile("file", "/workspace/mp.tar.gz")
	_, _ = part.Write(gz)
	mw.Close()
	if st := postSandboxFile(t, ts.URL, rec.SandboxID, "?path=/workspace/mp.tar.gz", mw.FormDataContentType(), "", mp.Bytes()); st != http.StatusOK {
		t.Fatalf("multipart upload: status %d", st)
	}
	if got := readSandboxFile(t, ts.URL, rec.SandboxID, "/workspace/mp.tar.gz"); !bytes.Equal(got, gz) {
		t.Fatalf("multipart .gz bytes were modified: got %q", got)
	}

	// Multipart part that declares Content-Encoding: gzip is inflated.
	var mpEnc bytes.Buffer
	mwEnc := multipart.NewWriter(&mpEnc)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="/workspace/declared-part.txt"`)
	h.Set("Content-Encoding", "gzip")
	encPart, _ := mwEnc.CreatePart(h)
	_, _ = encPart.Write(gz)
	mwEnc.Close()
	if st := postSandboxFile(t, ts.URL, rec.SandboxID, "", mwEnc.FormDataContentType(), "", mpEnc.Bytes()); st != http.StatusOK {
		t.Fatalf("declared-gzip multipart upload: status %d", st)
	}
	if got := readSandboxFile(t, ts.URL, rec.SandboxID, "/workspace/declared-part.txt"); string(got) != "inner-archive-content" {
		t.Fatalf("declared-gzip part not inflated: got %q", got)
	}

	// Declared gzip that is not gzip data is rejected.
	if st := postSandboxFile(t, ts.URL, rec.SandboxID, "?path=/workspace/bad.txt", "application/octet-stream", "gzip", []byte("plain text")); st != http.StatusBadRequest {
		t.Fatalf("expected 400 for bogus gzip, got %d", st)
	}
}

func TestFiles_POST_RelativeNestedMultipartPath(t *testing.T) {
	_, ts, rec := setupTestServerWithSandbox(t)

	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	part, _ := mw.CreateFormFile("file", "dir/a.txt")
	_, _ = part.Write([]byte("rel-nested"))
	mw.Close()
	if st := postSandboxFile(t, ts.URL, rec.SandboxID, "?path=dir/a.txt", mw.FormDataContentType(), "", mp.Bytes()); st != http.StatusOK {
		t.Fatalf("relative multipart upload: status %d", st)
	}
	if got := readSandboxFile(t, ts.URL, rec.SandboxID, "/workspace/dir/a.txt"); string(got) != "rel-nested" {
		t.Fatalf("expected file at /workspace/dir/a.txt, got %q", got)
	}
}
