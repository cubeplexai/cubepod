package controller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultS3Region        = "us-east-1"
	emptyPayloadSHA256Hex  = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	defaultS3HTTPTimeout   = 5 * time.Minute
	maxS3ErrorResponseBody = 4096
)

// S3ContextStoreConfig configures an S3-compatible object-storage ContextStore.
type S3ContextStoreConfig struct {
	Endpoint        string
	Bucket          string
	Region          string
	Prefix          string
	ForcePathStyle  bool
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	MaxBytes        int64
	HTTPClient      *http.Client
}

// S3ContextStore implements ContextStore backed by an S3-compatible object store
// using pure Go stdlib net/http and AWS SigV4 (AWS4-HMAC-SHA256) authentication.
type S3ContextStore struct {
	endpointURL     *url.URL
	bucket          string
	region          string
	prefix          string
	forcePathStyle  bool
	accessKeyID     string
	secretAccessKey string
	sessionToken    string
	maxBytes        atomic.Int64
	httpClient      *http.Client
	logSeq          atomic.Uint64
	seqBase         uint64
	lastLogNano     atomic.Int64
}

// NewS3ContextStore constructs a new S3ContextStore from cfg.
func NewS3ContextStore(cfg S3ContextStoreConfig) (*S3ContextStore, error) {
	bucket := strings.TrimSpace(cfg.Bucket)
	if bucket == "" {
		return nil, fmt.Errorf("s3 context store bucket is required")
	}

	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = defaultS3Region
	}

	rawEndpoint := strings.TrimSpace(cfg.Endpoint)
	if rawEndpoint == "" {
		if region == "us-east-1" {
			rawEndpoint = "https://s3.us-east-1.amazonaws.com"
		} else {
			rawEndpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", region)
		}
	} else if !strings.Contains(rawEndpoint, "://") {
		rawEndpoint = "https://" + rawEndpoint
	}

	parsedURL, err := url.Parse(strings.TrimRight(rawEndpoint, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid s3 endpoint URL: %w", err)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, fmt.Errorf("unsupported s3 endpoint scheme %q (expected http or https)", parsedURL.Scheme)
	}
	if parsedURL.Host == "" {
		return nil, fmt.Errorf("s3 endpoint host is required")
	}

	prefix := strings.Trim(strings.TrimSpace(cfg.Prefix), "/")
	if prefix != "" {
		prefix += "/"
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultS3HTTPTimeout}
	}

	// Generate a per-instance random seqBase so concurrent controller replicas
	// never collide on <tsNano>-<seq>.jsonl even within the same nanosecond.
	var randBuf [4]byte
	if _, err := rand.Read(randBuf[:]); err != nil {
		binary.BigEndian.PutUint32(randBuf[:], uint32(time.Now().UnixNano()))
	}
	instanceNum := uint64(binary.BigEndian.Uint32(randBuf[:])%900000) + 100000
	seqBase := instanceNum * 1_000_000

	s := &S3ContextStore{
		endpointURL:     parsedURL,
		bucket:          bucket,
		region:          region,
		prefix:          prefix,
		forcePathStyle:  cfg.ForcePathStyle,
		accessKeyID:     strings.TrimSpace(cfg.AccessKeyID),
		secretAccessKey: cfg.SecretAccessKey,
		sessionToken:    cfg.SessionToken,
		httpClient:      httpClient,
		seqBase:         seqBase,
	}
	if cfg.MaxBytes > 0 {
		s.maxBytes.Store(cfg.MaxBytes)
	} else {
		s.maxBytes.Store(DefaultMaxContextBytes)
	}
	return s, nil
}

// NewContextStoreFromConfig builds either a MemoryContextStore ("local") or S3ContextStore ("s3").
// S3 credentials are read solely from CUBEPOD_S3_ACCESS_KEY_ID, CUBEPOD_S3_SECRET_ACCESS_KEY,
// and CUBEPOD_S3_SESSION_TOKEN and are never logged.
func NewContextStoreFromConfig(backend, stateDir string, s3Cfg S3ContextStoreConfig) (ContextStore, error) {
	normalized := strings.ToLower(strings.TrimSpace(backend))
	switch normalized {
	case "", "local":
		var dir string
		if strings.TrimSpace(stateDir) != "" {
			dir = filepath.Join(stateDir, "template-context")
		}
		return NewMemoryContextStore(dir), nil
	case "s3":
		if s3Cfg.AccessKeyID == "" {
			s3Cfg.AccessKeyID = os.Getenv("CUBEPOD_S3_ACCESS_KEY_ID")
		}
		if s3Cfg.SecretAccessKey == "" {
			s3Cfg.SecretAccessKey = os.Getenv("CUBEPOD_S3_SECRET_ACCESS_KEY")
		}
		if s3Cfg.SessionToken == "" {
			s3Cfg.SessionToken = os.Getenv("CUBEPOD_S3_SESSION_TOKEN")
		}
		return NewS3ContextStore(s3Cfg)
	default:
		return nil, fmt.Errorf("unsupported context-store-backend %q (expected local or s3)", backend)
	}
}

// String formats non-secret configuration details for safe diagnostics.
func (s *S3ContextStore) String() string {
	return fmt.Sprintf("S3ContextStore{endpoint=%s,bucket=%s,region=%s,prefix=%s,forcePathStyle=%t}",
		s.endpointURL.String(), s.bucket, s.region, s.prefix, s.forcePathStyle)
}

// SetMaxBytes overrides the maximum accepted compressed and decompressed archive size.
func (s *S3ContextStore) SetMaxBytes(n int64) {
	if n > 0 {
		s.maxBytes.Store(n)
	} else {
		s.maxBytes.Store(DefaultMaxContextBytes)
	}
}

func sanitizeS3PathSegment(seg string) string {
	seg = strings.TrimSpace(seg)
	seg = strings.ReplaceAll(seg, "/", "_")
	seg = strings.ReplaceAll(seg, "\\", "_")
	seg = strings.ReplaceAll(seg, "..", "_")
	return seg
}

func validateContextHashKey(hash string) error {
	if hash == "" || hash == "." || hash == ".." || strings.ContainsAny(hash, "/\\") {
		return fmt.Errorf("%w: invalid context hash %q", ErrInvalidContextTar, hash)
	}
	return nil
}

func (s *S3ContextStore) contextKey(hash string) string {
	return fmt.Sprintf("%scontexts/%s.tar", s.prefix, hash)
}

func (s *S3ContextStore) nextLogKey(buildID string, now time.Time) string {
	tsNano := now.UnixNano()
	for {
		prev := s.lastLogNano.Load()
		if tsNano < prev {
			tsNano = prev
		}
		if s.lastLogNano.CompareAndSwap(prev, tsNano) {
			break
		}
	}
	seq := s.seqBase + s.logSeq.Add(1)
	return fmt.Sprintf("%slogs/%s/%020d-%012d.jsonl", s.prefix, sanitizeS3PathSegment(buildID), tsNano, seq)
}

func (s *S3ContextStore) buildURL(key string, query url.Values) string {
	u := *s.endpointURL
	basePath := strings.TrimRight(u.Path, "/")
	cleanKey := strings.TrimLeft(key, "/")

	if s.forcePathStyle {
		if cleanKey != "" {
			u.Path = basePath + "/" + s.bucket + "/" + cleanKey
		} else {
			u.Path = basePath + "/" + s.bucket
		}
	} else {
		if !strings.HasPrefix(u.Host, s.bucket+".") {
			u.Host = s.bucket + "." + u.Host
		}
		if cleanKey != "" {
			u.Path = basePath + "/" + cleanKey
		} else if basePath == "" {
			u.Path = "/"
		} else {
			u.Path = basePath + "/"
		}
	}
	if len(query) > 0 {
		u.RawQuery = encodeSigV4Query(query)
	} else {
		u.RawQuery = ""
	}
	return u.String()
}

func sigV4Escape(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else if c == '/' && !encodeSlash {
			b.WriteByte('/')
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func sigV4CanonicalURI(u *url.URL) string {
	p := u.Path
	if p == "" {
		return "/"
	}
	return sigV4Escape(p, false)
}

func encodeSigV4Query(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	type kv struct {
		k string
		v string
	}
	pairs := make([]kv, 0, len(q))
	for k, vals := range q {
		ek := sigV4Escape(k, true)
		if len(vals) == 0 {
			pairs = append(pairs, kv{k: ek, v: ""})
			continue
		}
		for _, v := range vals {
			pairs = append(pairs, kv{k: ek, v: sigV4Escape(v, true)})
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

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))
	return mac.Sum(nil)
}

func (s *S3ContextStore) signRequest(req *http.Request, payload []byte, now time.Time) {
	payloadHash := emptyPayloadSHA256Hex
	if len(payload) > 0 {
		sum := sha256.Sum256(payload)
		payloadHash = hex.EncodeToString(sum[:])
	}
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", amzDate)
	if s.sessionToken != "" {
		req.Header.Set("x-amz-security-token", s.sessionToken)
	}

	if s.accessKeyID == "" || s.secretAccessKey == "" {
		return
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	type hdr struct {
		name string
		val  string
	}
	headers := []hdr{
		{name: "host", val: strings.TrimSpace(host)},
		{name: "x-amz-content-sha256", val: payloadHash},
		{name: "x-amz-date", val: amzDate},
	}
	if s.sessionToken != "" {
		headers = append(headers, hdr{name: "x-amz-security-token", val: strings.TrimSpace(s.sessionToken)})
	}
	if ct := strings.TrimSpace(req.Header.Get("Content-Type")); ct != "" {
		headers = append(headers, hdr{name: "content-type", val: ct})
	}
	sort.Slice(headers, func(i, j int) bool {
		return headers[i].name < headers[j].name
	})

	var canonicalHeaders strings.Builder
	var signedHeaders strings.Builder
	for i, h := range headers {
		canonicalHeaders.WriteString(h.name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(h.val)
		canonicalHeaders.WriteByte('\n')
		if i > 0 {
			signedHeaders.WriteByte(';')
		}
		signedHeaders.WriteString(h.name)
	}

	canonicalQuery := encodeSigV4Query(req.URL.Query())
	canonicalReq := strings.Join([]string{
		req.Method,
		sigV4CanonicalURI(req.URL),
		canonicalQuery,
		canonicalHeaders.String(),
		signedHeaders.String(),
		payloadHash,
	}, "\n")

	crSum := sha256.Sum256([]byte(canonicalReq))
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, s.region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(crSum[:]),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+s.secretAccessKey), dateStamp)
	kRegion := hmacSHA256(kDate, s.region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKeyID, scope, signedHeaders.String(), signature)
	req.Header.Set("Authorization", authHeader)
}

func (s *S3ContextStore) doS3Request(ctx context.Context, method, key string, query url.Values, payload []byte, contentType string, extraHeaders map[string]string) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	targetURL := s.buildURL(key, query)
	var bodyReader io.Reader
	if len(payload) > 0 {
		bodyReader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to construct s3 %s request: %w", method, err)
	}
	if len(payload) > 0 {
		req.ContentLength = int64(len(payload))
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	s.signRequest(req, payload, time.Now().UTC())

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3 %s %s failed: %w", method, key, err)
	}
	return resp, nil
}

func readS3ErrorSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, maxS3ErrorResponseBody))
	return strings.TrimSpace(string(b))
}

func (s *S3ContextStore) getObject(ctx context.Context, key string, maxRead int64) ([]byte, bool, error) {
	resp, err := s.doS3Request(ctx, http.MethodGet, key, nil, nil, "", nil)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("s3 GET %s returned status %d: %s", key, resp.StatusCode, readS3ErrorSnippet(resp.Body))
	}

	if maxRead > 0 {
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxRead+1))
		if err != nil {
			return nil, false, fmt.Errorf("s3 GET %s read failed: %w", key, err)
		}
		if int64(len(data)) > maxRead {
			return nil, false, ErrContextTooLarge
		}
		return data, true, nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("s3 GET %s read failed: %w", key, err)
	}
	return data, true, nil
}

func (s *S3ContextStore) headObject(ctx context.Context, key string) (bool, error) {
	resp, err := s.doS3Request(ctx, http.MethodHead, key, nil, nil, "", nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("s3 HEAD %s returned status %d", key, resp.StatusCode)
	}
}

func (s *S3ContextStore) putObject(ctx context.Context, key string, payload []byte, contentType string) error {
	resp, err := s.doS3Request(ctx, http.MethodPut, key, nil, payload, contentType, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return fmt.Errorf("s3 PUT %s returned status %d: %s", key, resp.StatusCode, readS3ErrorSnippet(resp.Body))
}

func (s *S3ContextStore) putContextObject(ctx context.Context, key, hash string, tarBytes []byte, limit int64) error {
	resp, err := s.doS3Request(ctx, http.MethodPut, key, nil, tarBytes, "application/x-tar", map[string]string{
		"If-None-Match": "*",
	})
	if err != nil {
		return err
	}
	status := resp.StatusCode
	errSnippet := ""
	if status != http.StatusOK && status != http.StatusCreated && status != http.StatusNoContent {
		errSnippet = readS3ErrorSnippet(resp.Body)
	}
	resp.Body.Close()

	switch status {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	case http.StatusPreconditionFailed:
		// Another replica stored this hash concurrently; verify byte equality.
		existing, found, getErr := s.getObject(ctx, key, limit)
		if getErr != nil {
			return getErr
		}
		if found && bytes.Equal(existing, tarBytes) {
			return nil
		}
		return fmt.Errorf("%w: hash %s", ErrTarMismatch, hash)
	case http.StatusNotImplemented:
		// Older S3-compatible servers that do not implement If-None-Match: fallback to plain PUT.
		return s.putObject(ctx, key, tarBytes, "application/x-tar")
	default:
		return fmt.Errorf("s3 PUT %s returned status %d: %s", key, status, errSnippet)
	}
}

func (s *S3ContextStore) deleteObject(ctx context.Context, key string) error {
	resp, err := s.doS3Request(ctx, http.MethodDelete, key, nil, nil, "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("s3 DELETE %s returned status %d: %s", key, resp.StatusCode, readS3ErrorSnippet(resp.Body))
}

type s3ListBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	IsTruncated           bool           `xml:"IsTruncated"`
	NextContinuationToken string         `xml:"NextContinuationToken"`
	Contents              []s3ObjectMeta `xml:"Contents"`
}

type s3ObjectMeta struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	Size         int64  `xml:"Size"`
}

type s3ListedObject struct {
	Key          string
	LastModified time.Time
}

func (s *S3ContextStore) listObjectsV2(ctx context.Context, prefix string) ([]s3ListedObject, error) {
	var all []s3ListedObject
	var continuationToken string

	for {
		q := url.Values{}
		q.Set("list-type", "2")
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if continuationToken != "" {
			q.Set("continuation-token", continuationToken)
		}

		resp, err := s.doS3Request(ctx, http.MethodGet, "", q, nil, "", nil)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("s3 ListObjectsV2 read failed: %w", readErr)
		}
		if resp.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("s3 ListObjectsV2 returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var res s3ListBucketResult
		if err := xml.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("s3 ListObjectsV2 XML decode failed: %w", err)
		}
		for _, item := range res.Contents {
			var lm time.Time
			if item.LastModified != "" {
				if parsed, parseErr := time.Parse(time.RFC3339Nano, item.LastModified); parseErr == nil {
					lm = parsed.UTC()
				} else if parsed, parseErr := time.Parse(time.RFC3339, item.LastModified); parseErr == nil {
					lm = parsed.UTC()
				}
			}
			all = append(all, s3ListedObject{
				Key:          item.Key,
				LastModified: lm,
			})
		}

		if !res.IsTruncated || res.NextContinuationToken == "" {
			break
		}
		continuationToken = res.NextContinuationToken
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].Key < all[j].Key
	})
	return all, nil
}

// Store validates, optionally gunzips, and uploads a context tar under <prefix>contexts/<hash>.tar,
// preserving existing-hash byte equality semantics (ErrTarMismatch on different bytes).
func (s *S3ContextStore) Store(hash string, r io.Reader) error {
	if err := validateContextHashKey(hash); err != nil {
		return err
	}
	limit := s.maxBytes.Load()
	tarBytes, err := parseAndValidateContextTar(r, limit)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultS3HTTPTimeout)
	defer cancel()

	key := s.contextKey(hash)
	if existing, found, err := s.getObject(ctx, key, limit); err != nil {
		return err
	} else if found {
		if bytes.Equal(existing, tarBytes) {
			return nil
		}
		return fmt.Errorf("%w: hash %s", ErrTarMismatch, hash)
	}

	return s.putContextObject(ctx, key, hash, tarBytes, limit)
}

// IsPresent checks whether the context tar object exists in S3.
func (s *S3ContextStore) IsPresent(hash string) bool {
	if err := validateContextHashKey(hash); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	present, err := s.headObject(ctx, s.contextKey(hash))
	return err == nil && present
}

// Get downloads the validated decompressed tar bytes from S3.
func (s *S3ContextStore) Get(hash string) ([]byte, error) {
	if err := validateContextHashKey(hash); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultS3HTTPTimeout)
	defer cancel()

	data, found, err := s.getObject(ctx, s.contextKey(hash), s.maxBytes.Load())
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("context tar %s not found", hash)
	}
	return data, nil
}

// Unpack fetches the context tar from S3 and extracts it under destDir/<hash>/...
func (s *S3ContextStore) Unpack(hash, destDir string) error {
	data, err := s.Get(hash)
	if err != nil {
		return err
	}
	return unpackTarBytes(data, hash, destDir)
}

// AppendLog writes a chunked JSONL object under <prefix>logs/<buildID>/<tsNano>-<seq>.jsonl.
func (s *S3ContextStore) AppendLog(buildID string, lines []string) error {
	if buildID == "" || len(lines) == 0 {
		return nil
	}
	now := time.Now().UTC()
	_, encodedLines := encodeBuildLogLines(lines, now)
	key := s.nextLogKey(buildID, now)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.putObject(ctx, key, encodedLines, "application/x-ndjson")
}

// ReadLogs lists and reads ordered log chunks under <prefix>logs/<buildID>/, applying optional level filtering and pagination.
func (s *S3ContextStore) ReadLogs(buildID string, offset, limit int, level string) ([]BuildLogEntry, error) {
	if buildID == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logPrefix := fmt.Sprintf("%slogs/%s/", s.prefix, sanitizeS3PathSegment(buildID))
	objects, err := s.listObjectsV2(ctx, logPrefix)
	if err != nil {
		return nil, err
	}
	if len(objects) == 0 {
		return []BuildLogEntry{}, nil
	}

	var all []BuildLogEntry
	for _, obj := range objects {
		data, found, err := s.getObject(ctx, obj.Key, 0)
		if err != nil {
			return nil, err
		}
		if !found || len(data) == 0 {
			continue
		}
		var fallbackTS string
		if !obj.LastModified.IsZero() {
			fallbackTS = obj.LastModified.UTC().Format(time.RFC3339Nano)
		}
		parsed, err := parseLogLines(bytes.NewReader(data), fallbackTS)
		if err != nil {
			return nil, err
		}
		all = append(all, parsed...)
	}

	return filterAndPaginateLogs(all, offset, limit, level), nil
}

// SweepExpired deletes expired context tar objects and log chunk objects from S3 older than their TTLs.
func (s *S3ContextStore) SweepExpired(now time.Time, contextTTL, logTTL time.Duration) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if contextTTL > 0 {
		cutoff := now.Add(-contextTTL)
		objects, err := s.listObjectsV2(ctx, s.prefix+"contexts/")
		if err != nil {
			return err
		}
		for _, obj := range objects {
			if !obj.LastModified.IsZero() && obj.LastModified.Before(cutoff) {
				if err := s.deleteObject(ctx, obj.Key); err != nil {
					return err
				}
			}
		}
	}

	if logTTL > 0 {
		cutoff := now.Add(-logTTL)
		objects, err := s.listObjectsV2(ctx, s.prefix+"logs/")
		if err != nil {
			return err
		}
		for _, obj := range objects {
			if !obj.LastModified.IsZero() && obj.LastModified.Before(cutoff) {
				if err := s.deleteObject(ctx, obj.Key); err != nil {
					return err
				}
			}
		}
	}

	return nil
}
