package events

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestEventsAndWebhookSSRFAndSignedDelivery(t *testing.T) {
	var attempts atomic.Int32
	var gotSig, gotCubeSig string
	var gotBody []byte

	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			// Force one retry
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		gotSig = r.Header.Get("e2b-signature")
		gotCubeSig = r.Header.Get("X-CubePod-Signature")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	// 1. SSRF checks when AllowPrivateIPs=false
	mStrict, err := Open(t.TempDir(), Config{AllowPrivateIPs: false})
	if err != nil {
		t.Fatalf("Open strict: %v", err)
	}
	defer mStrict.Close()

	for _, badURL := range []string{
		"http://example.com/hook",
		"https://127.0.0.1/hook",
		"https://10.0.0.5/hook",
		"https://169.254.169.254/latest/meta-data",
		"https://100.64.0.1/hook",
		"https://user:pass@example.com/hook",
		"https://localhost/hook",
	} {
		if _, err := mStrict.CreateWebhook("key-1", CreateWebhookInput{
			Name:   "bad",
			URL:    badURL,
			Events: []string{EventCreated},
		}); !errors.Is(err, ErrInvalidWebhook) {
			t.Fatalf("expected ErrInvalidWebhook for %s, got %v", badURL, err)
		}
	}

	// 2. AllowPrivateIPs=true for local receiver test + retry + HMAC verification
	m, err := Open(t.TempDir(), Config{
		AllowPrivateIPs:    true,
		MaxEventsPerTenant: 5,
		RetryBaseDelay:     20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()

	wh, err := m.CreateWebhook("tenant-key-1", CreateWebhookInput{
		Name:   "billing",
		URL:    receiver.URL,
		Events: []string{EventCreated, EventKilled},
	})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	if wh.SignatureKey == "" {
		t.Fatal("expected SignatureKey on creation")
	}

	// Cross-tenant isolation
	if _, err := m.GetWebhook("tenant-key-2", wh.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-tenant GetWebhook, got %v", err)
	}

	// Emit event
	ev := m.Emit(TenantHash("tenant-key-1"), EventCreated, "cpod-1", "base", "", "", nil)
	if ev.Version != "v1" || ev.SandboxID != "cpod-1" {
		t.Fatalf("unexpected event: %+v", ev)
	}

	// Wait for retry delivery
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && m.WebhookDeliveriesOK.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if m.WebhookDeliveriesOK.Load() != 1 || attempts.Load() != 2 {
		t.Fatalf("expected 1 OK after 2 attempts, got ok=%d attempts=%d", m.WebhookDeliveriesOK.Load(), attempts.Load())
	}

	// Verify HMAC signature
	var ts string
	for _, part := range []byte(gotCubeSig) {
		_ = part
	}
	var parsed LifecycleEvent
	if err := json.Unmarshal(gotBody, &parsed); err != nil || parsed.ID != ev.ID {
		t.Fatalf("unmarshal delivered body: %v (ev=%+v)", err, parsed)
	}
	// Parse "t=<ts>,v1=<hex>"
	var parsedTS int64
	var parsedHex string
	if _, err := scanSig(gotCubeSig, &parsedTS, &parsedHex); err != nil {
		t.Fatalf("invalid X-CubePod-Signature %q: %v", gotCubeSig, err)
	}
	expectedHex, _ := ComputeSignature(wh.SignatureKey, parsedTS, gotBody)
	if !hmac.Equal([]byte(parsedHex), []byte(expectedHex)) || gotSig != expectedHex {
		t.Fatalf("HMAC mismatch: e2b-signature=%q expected=%q", gotSig, expectedHex)
	}
	_ = ts

	// Emit 5 more events -> MaxEventsPerTenant=5 retains newest 5
	for i := 0; i < 5; i++ {
		m.Emit(TenantHash("tenant-key-1"), EventUpdated, "cpod-1", "base", "", "", nil)
	}
	items, next, err := m.ListEvents("tenant-key-1", "cpod-1", "", 3, "")
	if err != nil || len(items) != 3 || next == "" {
		t.Fatalf("ListEvents page 1 unexpected: len=%d next=%q err=%v", len(items), next, err)
	}
	page2, next2, err := m.ListEvents("tenant-key-1", "cpod-1", "", 3, next)
	if err != nil || len(page2) != 2 || next2 != "" {
		t.Fatalf("ListEvents page 2 unexpected: len=%d next=%q err=%v", len(page2), next2, err)
	}

	// Rotate secret
	rot, err := m.UpdateWebhook("tenant-key-1", wh.ID, UpdateWebhookInput{RotateSecret: true})
	if err != nil || rot.SignatureKey == "" || rot.SignatureKey == wh.SignatureKey {
		t.Fatalf("expected new SignatureKey on rotation, got %+v err=%v", rot, err)
	}

	// Delete webhook
	if err := m.DeleteWebhook("tenant-key-1", wh.ID); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}
}

func scanSig(sig string, ts *int64, hexSig *string) (bool, error) {
	// "t=123,v1=abc"
	var tStr, vStr string
	for i := 0; i < len(sig); i++ {
		if sig[i] == ',' {
			tStr = sig[:i]
			vStr = sig[i+1:]
			break
		}
	}
	if len(tStr) < 3 || tStr[:2] != "t=" || len(vStr) < 4 || vStr[:3] != "v1=" {
		return false, errors.New("bad signature format")
	}
	var parsed int64
	for _, ch := range tStr[2:] {
		if ch < '0' || ch > '9' {
			return false, errors.New("bad timestamp")
		}
		parsed = parsed*10 + int64(ch-'0')
	}
	if _, err := hex.DecodeString(vStr[3:]); err != nil {
		return false, err
	}
	*ts = parsed
	*hexSig = vStr[3:]
	return true, nil
}

func TestSSRFRedirectBlocked(t *testing.T) {
	_ = sha256.New()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("redirect target should never be reached")
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	m, err := Open(t.TempDir(), Config{
		AllowPrivateIPs: true,
		MaxRetries:      1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()

	if _, err := m.CreateWebhook("k", CreateWebhookInput{
		Name:   "redir",
		URL:    redirector.URL,
		Events: []string{EventCreated},
	}); err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	m.Emit(TenantHash("k"), EventCreated, "sbx-1", "base", "", "", nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && m.WebhookDeliveriesFailed.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if m.WebhookDeliveriesFailed.Load() != 1 {
		t.Fatalf("expected 1 failed delivery on 302 redirect, got %d", m.WebhookDeliveriesFailed.Load())
	}
}
