package controller_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/controller"
)

type mockS3Object struct {
	data         []byte
	contentType  string
	lastModified time.Time
}

type mockS3Server struct {
	server            *httptest.Server
	mu                sync.Mutex
	objects           map[string]mockS3Object
	bucket            string
	region            string
	accessKeyID       string
	secretAccessKey   string
	sessionToken      string
	requireSigV4      bool
	virtualHost       bool
	rejectIfNoneMatch bool
	listMaxKeys       int
	requests          []string
}

func newMockS3Server(bucket, region, akid, secret, token string) *mockS3Server {
	m := &mockS3Server{
		objects:         make(map[string]mockS3Object),
		bucket:          bucket,
		region:          region,
		accessKeyID:     akid,
		secretAccessKey: secret,
		sessionToken:    token,
		requireSigV4:    true,
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.serveHTTP))
	return m
}

func (m *mockS3Server) Close() {
	m.server.Close()
}

func (m *mockS3Server) URL() string {
	return m.server.URL
}

func (m *mockS3Server) DialHTTPClient() *http.Client {
	addr := m.server.Listener.Addr().String()
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
	}
}

func (m *mockS3Server) SetLastModified(key string, ts time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if obj, ok := m.objects[key]; ok {
		obj.lastModified = ts.UTC()
		m.objects[key] = obj
	}
}

func (m *mockS3Server) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (m *mockS3Server) RequestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *mockS3Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	if m.requireSigV4 {
		if status, msg := m.verifySigV4(r, bodyBytes); status != 0 {
			http.Error(w, msg, status)
			return
		}
	}

	m.mu.Lock()
	m.requests = append(m.requests, r.Method+" "+r.URL.RequestURI())
	m.mu.Unlock()

	key, bucketOK := m.extractBucketAndKey(r)
	if !bucketOK {
		http.Error(w, "NoSuchBucket", http.StatusNotFound)
		return
	}

	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		m.handleListV2(w, r)
		return
	}

	if key == "" {
		http.Error(w, "MissingKey", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodHead:
		m.mu.Lock()
		obj, ok := m.objects[key]
		m.mu.Unlock()
		if !ok {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
			return
		}
		if obj.contentType != "" {
			w.Header().Set("Content-Type", obj.contentType)
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		m.mu.Lock()
		obj, ok := m.objects[key]
		m.mu.Unlock()
		if !ok {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
			return
		}
		if obj.contentType != "" {
			w.Header().Set("Content-Type", obj.contentType)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(obj.data)

	case http.MethodPut:
		if r.Header.Get("If-None-Match") == "*" {
			if m.rejectIfNoneMatch {
				http.Error(w, "NotImplemented", http.StatusNotImplemented)
				return
			}
			m.mu.Lock()
			_, exists := m.objects[key]
			if !exists {
				m.objects[key] = mockS3Object{
					data:         append([]byte(nil), bodyBytes...),
					contentType:  r.Header.Get("Content-Type"),
					lastModified: time.Now().UTC(),
				}
			}
			m.mu.Unlock()
			if exists {
				http.Error(w, "PreconditionFailed", http.StatusPreconditionFailed)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}

		m.mu.Lock()
		m.objects[key] = mockS3Object{
			data:         append([]byte(nil), bodyBytes...),
			contentType:  r.Header.Get("Content-Type"),
			lastModified: time.Now().UTC(),
		}
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)

	case http.MethodDelete:
		m.mu.Lock()
		delete(m.objects, key)
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed)
	}
}

func (m *mockS3Server) extractBucketAndKey(r *http.Request) (string, bool) {
	path := strings.TrimLeft(r.URL.Path, "/")
	if m.virtualHost {
		host := r.Host
		if idx := strings.IndexByte(host, ':'); idx >= 0 {
			host = host[:idx]
		}
		if !strings.HasPrefix(host, m.bucket+".") {
			return "", false
		}
		return path, true
	}

	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] != m.bucket {
		return "", false
	}
	if len(parts) == 1 {
		return "", true
	}
	return parts[1], true
}

func (m *mockS3Server) handleListV2(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	tokenStr := r.URL.Query().Get("continuation-token")
	startIdx := 0
	if tokenStr != "" {
		parsed, err := strconv.Atoi(tokenStr)
		if err != nil || parsed < 0 {
			http.Error(w, "InvalidToken", http.StatusBadRequest)
			return
		}
		startIdx = parsed
	}

	type item struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		Size         int    `xml:"Size"`
	}

	m.mu.Lock()
	matchedKeys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			matchedKeys = append(matchedKeys, k)
		}
	}
	sort.Strings(matchedKeys)

	endIdx := len(matchedKeys)
	isTruncated := false
	nextToken := ""
	if m.listMaxKeys > 0 && startIdx+m.listMaxKeys < len(matchedKeys) {
		endIdx = startIdx + m.listMaxKeys
		isTruncated = true
		nextToken = strconv.Itoa(endIdx)
	}

	var items []item
	if startIdx < len(matchedKeys) {
		for _, k := range matchedKeys[startIdx:endIdx] {
			obj := m.objects[k]
			items = append(items, item{
				Key:          k,
				LastModified: obj.lastModified.UTC().Format(time.RFC3339Nano),
				Size:         len(obj.data),
			})
		}
	}
	m.mu.Unlock()

	type listResp struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		IsTruncated           bool     `xml:"IsTruncated"`
		NextContinuationToken string   `xml:"NextContinuationToken,omitempty"`
		Contents              []item   `xml:"Contents"`
	}

	payload, err := xml.Marshal(listResp{
		IsTruncated:           isTruncated,
		NextContinuationToken: nextToken,
		Contents:              items,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func (m *mockS3Server) verifySigV4(r *http.Request, body []byte) (int, string) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		return http.StatusForbidden, "MissingSigV4"
	}

	parts := strings.Split(strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 "), ",")
	kv := make(map[string]string, len(parts))
	for _, part := range parts {
		sub := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(sub) == 2 {
			kv[sub[0]] = sub[1]
		}
	}

	credParts := strings.Split(kv["Credential"], "/")
	if len(credParts) != 5 || credParts[0] != m.accessKeyID || credParts[2] != m.region || credParts[3] != "s3" || credParts[4] != "aws4_request" {
		return http.StatusForbidden, "InvalidCredentialScope"
	}
	if m.sessionToken != "" && r.Header.Get("x-amz-security-token") != m.sessionToken {
		return http.StatusForbidden, "InvalidSecurityToken"
	}

	amzDate := r.Header.Get("x-amz-date")
	if len(amzDate) < 8 || amzDate[:8] != credParts[1] {
		return http.StatusForbidden, "DateMismatch"
	}

	sum := sha256.Sum256(body)
	bodyHex := hex.EncodeToString(sum[:])
	payloadHash := r.Header.Get("x-amz-content-sha256")
	if payloadHash != bodyHex {
		return http.StatusForbidden, "PayloadHashMismatch"
	}

	signedHeaderList := strings.Split(kv["SignedHeaders"], ";")
	var canonicalHeaders strings.Builder
	for _, h := range signedHeaderList {
		var val string
		if h == "host" {
			val = r.Host
		} else {
			val = strings.Join(r.Header.Values(h), ",")
		}
		canonicalHeaders.WriteString(h)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.TrimSpace(val))
		canonicalHeaders.WriteByte('\n')
	}

	canonicalQuery := mockCanonicalQuery(r.URL.Query())
	canonicalPath := mockCanonicalPath(r.URL.EscapedPath())
	canonicalReq := strings.Join([]string{
		r.Method,
		canonicalPath,
		canonicalQuery,
		canonicalHeaders.String(),
		kv["SignedHeaders"],
		payloadHash,
	}, "\n")

	crSum := sha256.Sum256([]byte(canonicalReq))
	scope := strings.Join(credParts[1:], "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(crSum[:]),
	}, "\n")

	kDate := mockHMACSHA256([]byte("AWS4"+m.secretAccessKey), credParts[1])
	kRegion := mockHMACSHA256(kDate, m.region)
	kService := mockHMACSHA256(kRegion, "s3")
	kSigning := mockHMACSHA256(kService, "aws4_request")
	expectedSig := hex.EncodeToString(mockHMACSHA256(kSigning, stringToSign))
	if subtle.ConstantTimeCompare([]byte(expectedSig), []byte(kv["Signature"])) != 1 {
		return http.StatusForbidden, fmt.Sprintf("SignatureDoesNotMatch: got=%s expected=%s", kv["Signature"], expectedSig)
	}
	return 0, ""
}

func mockHMACSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

func mockSigV4Escape(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		isUnreserved := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~'
		if isUnreserved || (!encodeSlash && c == '/') {
			b.WriteByte(c)
		} else {
			b.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return b.String()
}

func mockCanonicalPath(escapedPath string) string {
	if escapedPath == "" {
		return "/"
	}
	decoded, err := url.PathUnescape(escapedPath)
	if err != nil {
		return escapedPath
	}
	return mockSigV4Escape(decoded, false)
}

func mockCanonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	type pair struct{ k, v string }
	var pairs []pair
	for k, vals := range q {
		ek := mockSigV4Escape(k, true)
		for _, v := range vals {
			pairs = append(pairs, pair{k: ek, v: mockSigV4Escape(v, true)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k == pairs[j].k {
			return pairs[i].v < pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

type tarTestEntry struct {
	name     string
	typeflag byte
	body     []byte
	mode     int64
	linkname string
}

func buildTestTar(t *testing.T, gzipped bool, entries []tarTestEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	var tw *tar.Writer
	var gw *gzip.Writer
	if gzipped {
		gw = gzip.NewWriter(&buf)
		tw = tar.NewWriter(gw)
	} else {
		tw = tar.NewWriter(&buf)
	}
	for _, entry := range entries {
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{
			Name:     entry.name,
			Typeflag: entry.typeflag,
			Mode:     mode,
			Size:     int64(len(entry.body)),
			Linkname: entry.linkname,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%s): %v", entry.name, err)
		}
		if len(entry.body) > 0 {
			if _, err := tw.Write(entry.body); err != nil {
				t.Fatalf("Write(%s): %v", entry.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if gw != nil {
		if err := gw.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
	}
	return buf.Bytes()
}

func TestS3ContextStore_PathStyleSigV4AndGzipLifecycle(t *testing.T) {
	srv := newMockS3Server("cubepod-ctx", "us-west-2", "AKIAEXAMPLE", "super-secret-s3-key", "sess-tok-1")
	defer srv.Close()

	store, err := controller.NewS3ContextStore(controller.S3ContextStoreConfig{
		Endpoint:        srv.URL(),
		Bucket:          "cubepod-ctx",
		Region:          "us-west-2",
		Prefix:          "prod/templates",
		ForcePathStyle:  true,
		AccessKeyID:     "AKIAEXAMPLE",
		SecretAccessKey: "super-secret-s3-key",
		SessionToken:    "sess-tok-1",
	})
	if err != nil {
		t.Fatalf("NewS3ContextStore: %v", err)
	}

	if desc := store.String(); strings.Contains(desc, "super-secret-s3-key") || strings.Contains(desc, "AKIAEXAMPLE") || strings.Contains(desc, "sess-tok-1") {
		t.Fatalf("String() must redact credentials, got: %s", desc)
	}

	gzTar := buildTestTar(t, true, []tarTestEntry{
		{name: "app/hello.txt", typeflag: tar.TypeReg, body: []byte("hello-s3"), mode: 0o644},
		{name: "app/run.sh", typeflag: tar.TypeReg, body: []byte("#!/bin/sh\necho ok\n"), mode: 0o755},
	})
	rawTar := buildTestTar(t, false, []tarTestEntry{
		{name: "app/hello.txt", typeflag: tar.TypeReg, body: []byte("hello-s3"), mode: 0o644},
		{name: "app/run.sh", typeflag: tar.TypeReg, body: []byte("#!/bin/sh\necho ok\n"), mode: 0o755},
	})

	const hash = "sha256-ctx-1"
	if store.IsPresent(hash) {
		t.Fatalf("expected hash %s absent before Store", hash)
	}

	if err := store.Store(hash, bytes.NewReader(gzTar)); err != nil {
		t.Fatalf("Store(gzTar): %v", err)
	}
	if !store.IsPresent(hash) {
		t.Fatalf("expected hash %s present after Store", hash)
	}

	// Idempotent upload with equivalent tar succeeds.
	if err := store.Store(hash, bytes.NewReader(rawTar)); err != nil {
		t.Fatalf("idempotent Store(rawTar): %v", err)
	}

	// Different bytes for same hash must return ErrTarMismatch.
	differentTar := buildTestTar(t, false, []tarTestEntry{
		{name: "app/hello.txt", typeflag: tar.TypeReg, body: []byte("mutated")},
	})
	if err := store.Store(hash, bytes.NewReader(differentTar)); !errors.Is(err, controller.ErrTarMismatch) {
		t.Fatalf("expected ErrTarMismatch, got: %v", err)
	}

	got, err := store.Get(hash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, rawTar) {
		t.Fatalf("Get bytes mismatch: got %d bytes, want %d bytes", len(got), len(rawTar))
	}

	unpackDir := t.TempDir()
	if err := store.Unpack(hash, unpackDir); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(unpackDir, hash, "app", "hello.txt"))
	if err != nil || string(data) != "hello-s3" {
		t.Fatalf("unexpected unpacked file: data=%q err=%v", string(data), err)
	}
	info, err := os.Stat(filepath.Join(unpackDir, hash, "app", "run.sh"))
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("expected executable permission preserved on run.sh: mode=%v err=%v", info.Mode(), err)
	}
}

func TestS3ContextStore_VirtualHostedStyleAndFallbackPut(t *testing.T) {
	srv := newMockS3Server("my-bucket", "eu-central-1", "AKIAVHOST", "secret-vhost", "")
	srv.virtualHost = true
	srv.rejectIfNoneMatch = true
	defer srv.Close()

	store, err := controller.NewS3ContextStore(controller.S3ContextStoreConfig{
		Endpoint:        "http://s3.eu-central-1.amazonaws.com",
		Bucket:          "my-bucket",
		Region:          "eu-central-1",
		ForcePathStyle:  false,
		AccessKeyID:     "AKIAVHOST",
		SecretAccessKey: "secret-vhost",
		HTTPClient:      srv.DialHTTPClient(),
	})
	if err != nil {
		t.Fatalf("NewS3ContextStore: %v", err)
	}

	tar1 := buildTestTar(t, false, []tarTestEntry{{name: "main.py", typeflag: tar.TypeReg, body: []byte("print(1)")}})
	tar2 := buildTestTar(t, false, []tarTestEntry{{name: "main.py", typeflag: tar.TypeReg, body: []byte("print(2)")}})

	if err := store.Store("hash-vh", bytes.NewReader(tar1)); err != nil {
		t.Fatalf("Store(501 fallback): %v", err)
	}
	if !store.IsPresent("hash-vh") {
		t.Fatalf("expected hash-vh present via virtual-hosted HEAD")
	}
	if err := store.Store("hash-vh", bytes.NewReader(tar2)); !errors.Is(err, controller.ErrTarMismatch) {
		t.Fatalf("expected ErrTarMismatch on fallback pre-check, got: %v", err)
	}
}

func TestS3ContextStore_SecurityLimitsAndValidation(t *testing.T) {
	srv := newMockS3Server("sec-bkt", "us-east-1", "AKIASEC", "sec-key", "")
	defer srv.Close()

	store, err := controller.NewS3ContextStore(controller.S3ContextStoreConfig{
		Endpoint:        srv.URL(),
		Bucket:          "sec-bkt",
		Region:          "us-east-1",
		ForcePathStyle:  true,
		AccessKeyID:     "AKIASEC",
		SecretAccessKey: "sec-key",
	})
	if err != nil {
		t.Fatalf("NewS3ContextStore: %v", err)
	}
	store.SetMaxBytes(4096)

	// Raw tar > 4096 bytes rejected without uploading to S3.
	bigTar := buildTestTar(t, false, []tarTestEntry{{name: "big.bin", typeflag: tar.TypeReg, body: bytes.Repeat([]byte("A"), 5000)}})
	if err := store.Store("too-big-raw", bytes.NewReader(bigTar)); !errors.Is(err, controller.ErrContextTooLarge) {
		t.Fatalf("expected ErrContextTooLarge on oversized raw tar, got: %v", err)
	}

	// Gzipped bomb whose compressed size < 4096 bytes but decompressed size > 4096 bytes.
	gzBomb := buildTestTar(t, true, []tarTestEntry{{name: "bomb.txt", typeflag: tar.TypeReg, body: bytes.Repeat([]byte("Z"), 32768)}})
	if len(gzBomb) >= 4096 {
		t.Fatalf("expected test gzBomb to compress under 4096 bytes, got %d", len(gzBomb))
	}
	if err := store.Store("too-big-gz", bytes.NewReader(gzBomb)); !errors.Is(err, controller.ErrContextTooLarge) {
		t.Fatalf("expected ErrContextTooLarge on gzip bomb, got: %v", err)
	}

	// Traversal, symlink, hardlink, and FIFO entries must all fail validation before any S3 upload.
	badCases := []struct {
		name  string
		entry tarTestEntry
		want  error
	}{
		{name: "traversal", entry: tarTestEntry{name: "../etc/passwd", typeflag: tar.TypeReg, body: []byte("x")}, want: controller.ErrPathTraversal},
		{name: "symlink", entry: tarTestEntry{name: "sub/escape", typeflag: tar.TypeSymlink, linkname: "../../etc/passwd"}, want: controller.ErrPathTraversal},
		{name: "hardlink", entry: tarTestEntry{name: "hard", typeflag: tar.TypeLink, linkname: "../secret"}, want: controller.ErrPathTraversal},
		{name: "fifo", entry: tarTestEntry{name: "pipe", typeflag: tar.TypeFifo, mode: 0o644}, want: controller.ErrInvalidContextTar},
	}
	for _, tc := range badCases {
		badTar := buildTestTar(t, false, []tarTestEntry{tc.entry})
		if err := store.Store("bad-"+tc.name, bytes.NewReader(badTar)); !errors.Is(err, tc.want) {
			t.Fatalf("%s: expected %v, got %v", tc.name, tc.want, err)
		}
	}

	if err := store.Store("not-tar", bytes.NewReader([]byte("not valid tar bytes"))); !errors.Is(err, controller.ErrInvalidContextTar) {
		t.Fatalf("expected ErrInvalidContextTar, got: %v", err)
	}
	if srv.RequestCount() != 0 {
		t.Fatalf("expected 0 S3 HTTP requests on invalid client archives, got %d", srv.RequestCount())
	}
}

func TestS3ContextStore_ConcurrentLogsAndPaginatedSweep(t *testing.T) {
	srv := newMockS3Server("ha-bkt", "us-east-1", "AKIAHA", "ha-secret", "")
	srv.listMaxKeys = 2 // force ListObjectsV2 continuation-token pagination
	defer srv.Close()

	cfg := controller.S3ContextStoreConfig{
		Endpoint:        srv.URL(),
		Bucket:          "ha-bkt",
		Region:          "us-east-1",
		Prefix:          "cluster-a/",
		ForcePathStyle:  true,
		AccessKeyID:     "AKIAHA",
		SecretAccessKey: "ha-secret",
	}
	replicaA, err := controller.NewS3ContextStore(cfg)
	if err != nil {
		t.Fatalf("replicaA NewS3ContextStore: %v", err)
	}
	replicaB, err := controller.NewS3ContextStore(cfg)
	if err != nil {
		t.Fatalf("replicaB NewS3ContextStore: %v", err)
	}

	// Concurrent multi-replica AppendLog on same buildID.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			st := replicaA
			tag := "A"
			if idx%2 == 1 {
				st = replicaB
				tag = "B"
			}
			_ = st.AppendLog("bld-ha-1", []string{
				fmt.Sprintf("INFO: replica-%s step-%d", tag, idx),
				fmt.Sprintf("ERROR: replica-%s err-%d", tag, idx),
			})
		}(i)
	}
	wg.Wait()

	allLogs, err := replicaB.ReadLogs("bld-ha-1", 0, 100, "")
	if err != nil {
		t.Fatalf("ReadLogs all: %v", err)
	}
	if len(allLogs) != 8 {
		t.Fatalf("expected 8 log entries across concurrent replicas, got %d", len(allLogs))
	}

	errLogs, err := replicaA.ReadLogs("bld-ha-1", 1, 2, "error")
	if err != nil {
		t.Fatalf("ReadLogs filtered: %v", err)
	}
	if len(errLogs) != 2 {
		t.Fatalf("expected 2 paginated error entries, got %d", len(errLogs))
	}
	for _, entry := range errLogs {
		if entry.Level != "error" {
			t.Fatalf("expected level=error, got %+v", entry)
		}
	}

	// Empty buildID yields empty slice.
	none, err := replicaA.ReadLogs("bld-nonexistent", 0, 10, "")
	if err != nil || len(none) != 0 {
		t.Fatalf("expected empty logs without error, got len=%d err=%v", len(none), err)
	}

	// Store two contexts and mark one old alongside some old log chunks to test paginated SweepExpired.
	tarOld := buildTestTar(t, false, []tarTestEntry{{name: "old.txt", typeflag: tar.TypeReg, body: []byte("old")}})
	tarNew := buildTestTar(t, false, []tarTestEntry{{name: "new.txt", typeflag: tar.TypeReg, body: []byte("new")}})
	if err := replicaA.Store("ctx-old", bytes.NewReader(tarOld)); err != nil {
		t.Fatalf("Store ctx-old: %v", err)
	}
	if err := replicaB.Store("ctx-new", bytes.NewReader(tarNew)); err != nil {
		t.Fatalf("Store ctx-new: %v", err)
	}

	oldTime := time.Now().UTC().Add(-48 * time.Hour)
	srv.SetLastModified("cluster-a/contexts/ctx-old.tar", oldTime)
	var logKeys []string
	for _, k := range srv.Keys() {
		if strings.HasPrefix(k, "cluster-a/logs/bld-ha-1/") {
			logKeys = append(logKeys, k)
		}
	}
	if len(logKeys) < 2 {
		t.Fatalf("expected multiple log chunk keys, got %v", logKeys)
	}
	// Expire half of the log chunks.
	for _, k := range logKeys[:len(logKeys)/2] {
		srv.SetLastModified(k, oldTime)
	}

	if err := replicaA.SweepExpired(time.Now().UTC(), 24*time.Hour, 24*time.Hour); err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}

	if replicaB.IsPresent("ctx-old") {
		t.Fatalf("expected expired ctx-old deleted")
	}
	if !replicaB.IsPresent("ctx-new") {
		t.Fatalf("expected fresh ctx-new retained")
	}
	remainingLogs, err := replicaA.ReadLogs("bld-ha-1", 0, 100, "")
	if err != nil {
		t.Fatalf("ReadLogs after sweep: %v", err)
	}
	if len(remainingLogs) == 0 || len(remainingLogs) >= 8 {
		t.Fatalf("expected partial log retention after targeted sweep, got %d entries", len(remainingLogs))
	}
}

func TestNewContextStoreFromConfig(t *testing.T) {
	localStore, err := controller.NewContextStoreFromConfig("local", t.TempDir(), controller.S3ContextStoreConfig{})
	if err != nil {
		t.Fatalf("local NewContextStoreFromConfig: %v", err)
	}
	if _, ok := localStore.(*controller.MemoryContextStore); !ok {
		t.Fatalf("expected *MemoryContextStore, got %T", localStore)
	}

	srv := newMockS3Server("cfg-bkt", "us-east-1", "ENVKEY123", "env-secret-456", "env-tok-789")
	defer srv.Close()

	t.Setenv("CUBEPOD_S3_ACCESS_KEY_ID", "ENVKEY123")
	t.Setenv("CUBEPOD_S3_SECRET_ACCESS_KEY", "env-secret-456")
	t.Setenv("CUBEPOD_S3_SESSION_TOKEN", "env-tok-789")

	s3Store, err := controller.NewContextStoreFromConfig("s3", "", controller.S3ContextStoreConfig{
		Endpoint:       srv.URL(),
		Bucket:         "cfg-bkt",
		Region:         "us-east-1",
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("s3 NewContextStoreFromConfig: %v", err)
	}
	typed, ok := s3Store.(*controller.S3ContextStore)
	if !ok {
		t.Fatalf("expected *S3ContextStore, got %T", s3Store)
	}
	if err := typed.AppendLog("bld-env", []string{"env auth works"}); err != nil {
		t.Fatalf("AppendLog with env credentials failed: %v", err)
	}

	if _, err := controller.NewContextStoreFromConfig("gcs", "", controller.S3ContextStoreConfig{}); err == nil {
		t.Fatalf("expected unsupported backend error")
	}
	if _, err := controller.NewContextStoreFromConfig("s3", "", controller.S3ContextStoreConfig{Endpoint: srv.URL()}); err == nil {
		t.Fatalf("expected error when S3 bucket is empty")
	}
}
