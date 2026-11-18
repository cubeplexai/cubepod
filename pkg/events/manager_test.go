package events

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestDeadLetterQueueFullPersistenceAndAPI(t *testing.T) {
	release := make(chan struct{})
	var released atomic.Bool
	unblock := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	dir := t.TempDir()
	m, err := Open(dir, Config{AllowPrivateIPs: true, QueueSize: 1, MaxRetries: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	wh, err := m.CreateWebhook("tenant-key-1", CreateWebhookInput{Name: "slow", URL: receiver.URL, Events: []string{"*"}})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	tenant := TenantHash("tenant-key-1")
	for i := 0; i < 5; i++ {
		m.Emit(tenant, EventCreated, "cpod-"+string(rune('a'+i)), "base", "", "", nil)
	}
	dropped := m.WebhookDeliveriesDrop.Load()
	if dropped < 3 {
		t.Fatalf("expected >=3 dropped deliveries with queue size 1, got %d", dropped)
	}
	items, _, err := m.ListDeadLetters("tenant-key-1", DeadLetterFilter{Reason: DeadLetterReasonQueueFull}, 100, "")
	if err != nil || uint64(len(items)) != dropped {
		t.Fatalf("queue_full dead letters = %d, %v; want %d", len(items), err, dropped)
	}
	if m.DeadLettersTotal.Load() != dropped || m.DeadLettersPending() != int(dropped) {
		t.Fatalf("dead letter metrics total=%d pending=%d; want %d", m.DeadLettersTotal.Load(), m.DeadLettersPending(), dropped)
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].CreatedAt.Before(items[i].CreatedAt) {
			t.Fatal("dead letters must be listed newest first")
		}
	}

	// Pagination and filters.
	page, next, err := m.ListDeadLetters("tenant-key-1", DeadLetterFilter{}, 2, "")
	if err != nil || len(page) != 2 || next == "" {
		t.Fatalf("first page = %d next=%q err=%v", len(page), next, err)
	}
	rest, next2, err := m.ListDeadLetters("tenant-key-1", DeadLetterFilter{}, 100, next)
	if err != nil || len(rest) != int(dropped)-2 || next2 != "" {
		t.Fatalf("second page = %d next=%q err=%v", len(rest), next2, err)
	}
	bySandbox, _, _ := m.ListDeadLetters("tenant-key-1", DeadLetterFilter{SandboxID: items[0].SandboxID}, 100, "")
	if len(bySandbox) != 1 {
		t.Fatalf("sandbox filter = %d; want 1", len(bySandbox))
	}
	if _, _, err := m.ListDeadLetters("tenant-key-1", DeadLetterFilter{}, 10, "!!"); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("bad cursor err = %v", err)
	}
	// Tenant isolation.
	if other, _, _ := m.ListDeadLetters("tenant-key-2", DeadLetterFilter{}, 100, ""); len(other) != 0 {
		t.Fatalf("tenant-key-2 sees %d dead letters", len(other))
	}
	if _, _, err := m.ListDeadLetters("tenant-key-2", DeadLetterFilter{WebhookID: wh.ID}, 100, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant webhook-scoped list err = %v; want ErrNotFound", err)
	}
	if err := m.DeleteDeadLetter("tenant-key-2", wh.ID, items[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant delete err = %v; want ErrNotFound", err)
	}

	// Discard one; the rest must survive a restart.
	if err := m.DeleteDeadLetter("tenant-key-1", wh.ID, items[0].ID); err != nil {
		t.Fatalf("DeleteDeadLetter: %v", err)
	}
	if err := m.DeleteDeadLetter("tenant-key-1", wh.ID, items[0].ID); !errors.Is(err, ErrDeadLetterNotFound) {
		t.Fatalf("double delete err = %v; want ErrDeadLetterNotFound", err)
	}
	unblock()
	m.Close()

	raw, err := os.ReadFile(filepath.Join(dir, deadLettersFileName))
	if err != nil {
		t.Fatalf("read dead_letters.json: %v", err)
	}
	if strings.Contains(string(raw), "whsec_") || strings.Contains(string(raw), receiver.URL) {
		t.Fatal("dead_letters.json must not contain signatureKey or webhook URL")
	}

	m2, err := Open(dir, Config{AllowPrivateIPs: true, MaxRetries: 1})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer m2.Close()
	reloaded, _, _ := m2.ListDeadLetters("tenant-key-1", DeadLetterFilter{WebhookID: wh.ID}, 100, "")
	if len(reloaded) != int(dropped)-1 {
		t.Fatalf("reloaded dead letters = %d; want %d", len(reloaded), dropped-1)
	}

	// Disabled webhooks cannot be retried; enabled retries are delivered and cleared.
	disabled := false
	if _, err := m2.UpdateWebhook("tenant-key-1", wh.ID, UpdateWebhookInput{Enabled: &disabled}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := m2.RetryDeadLetter("tenant-key-1", wh.ID, reloaded[0].ID); !errors.Is(err, ErrWebhookDisabled) {
		t.Fatalf("retry disabled err = %v; want ErrWebhookDisabled", err)
	}
	enabled := true
	if _, err := m2.UpdateWebhook("tenant-key-1", wh.ID, UpdateWebhookInput{Enabled: &enabled}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, err := m2.RetryDeadLetter("tenant-key-1", wh.ID, reloaded[0].ID); err != nil {
		t.Fatalf("RetryDeadLetter: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && m2.WebhookDeliveriesOK.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if m2.WebhookDeliveriesOK.Load() != 1 || m2.DeadLettersPending() != int(dropped)-2 {
		t.Fatalf("after retry ok=%d pending=%d; want 1 and %d", m2.WebhookDeliveriesOK.Load(), m2.DeadLettersPending(), dropped-2)
	}

	// Deleting the webhook removes its dead letters.
	if err := m2.DeleteWebhook("tenant-key-1", wh.ID); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}
	if m2.DeadLettersPending() != 0 {
		t.Fatalf("dead letters remain after webhook deletion: %d", m2.DeadLettersPending())
	}
}

func TestDeadLetterFailedRetryReRecordsSameID(t *testing.T) {
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer receiver.Close()

	m, err := Open(t.TempDir(), Config{AllowPrivateIPs: true, MaxRetries: 1, MaxDeadLettersPerTenant: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close()
	wh, err := m.CreateWebhook("tenant-key-1", CreateWebhookInput{Name: "down", URL: receiver.URL, Events: []string{"*"}})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	m.Emit(TenantHash("tenant-key-1"), EventPaused, "cpod-1", "base", "", "", nil)
	waitForPending := func(n int) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && m.DeadLettersPending() != n {
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitForPending(1)
	items, _, _ := m.ListDeadLetters("tenant-key-1", DeadLetterFilter{}, 10, "")
	if len(items) != 1 || items[0].LastStatusCode != http.StatusBadGateway || items[0].Attempts != 1 {
		t.Fatalf("unexpected dead letters: %+v", items)
	}
	info, _ := m.GetWebhook("tenant-key-1", wh.ID)
	if info.ConsecutiveFailures != 1 || info.LastDeliveryTime != nil {
		t.Fatalf("unexpected status after failure: %+v", info)
	}
	if _, err := m.RetryDeadLetter("tenant-key-1", wh.ID, items[0].ID); err != nil {
		t.Fatalf("RetryDeadLetter: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && m.WebhookDeliveriesFailed.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	waitForPending(1)
	again, _, _ := m.ListDeadLetters("tenant-key-1", DeadLetterFilter{}, 10, "")
	if len(again) != 1 || again[0].ID != items[0].ID || again[0].RetryCount != 1 || !again[0].CreatedAt.Equal(items[0].CreatedAt) {
		t.Fatalf("failed retry must re-record same ID with retryCount=1: %+v", again)
	}

	// Per-tenant bound evicts the oldest dead letters.
	for i := 0; i < 3; i++ {
		m.Emit(TenantHash("tenant-key-1"), EventResumed, "cpod-bound", "base", "", "", nil)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && m.DeadLettersTotal.Load() < 5 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := m.DeadLettersPending(); got != 2 {
		t.Fatalf("pending dead letters = %d; want bounded at 2", got)
	}
}

// gateBackend wraps a Backend with configurable hooks/gates to simulate slow backend I/O
// and queued persistWorker operations under -race.
type gateBackend struct {
	Backend
	putWebhookHook       func(context.Context, StoredWebhook) error
	putDeadLetterHook    func(context.Context, StoredDeadLetter) error
	updateStatusHook     func(context.Context, string, WebhookStatus) error
	putDeadLetterCalls   atomic.Int32
	deleteDeadLetterCall atomic.Int32
}

func (g *gateBackend) PutWebhook(ctx context.Context, rec StoredWebhook) error {
	if g.putWebhookHook != nil {
		if err := g.putWebhookHook(ctx, rec); err != nil {
			return err
		}
	}
	return g.Backend.PutWebhook(ctx, rec)
}

func (g *gateBackend) PutDeadLetter(ctx context.Context, rec StoredDeadLetter) error {
	g.putDeadLetterCalls.Add(1)
	if g.putDeadLetterHook != nil {
		if err := g.putDeadLetterHook(ctx, rec); err != nil {
			return err
		}
	}
	return g.Backend.PutDeadLetter(ctx, rec)
}

func (g *gateBackend) DeleteDeadLetter(ctx context.Context, rec StoredDeadLetter) error {
	g.deleteDeadLetterCall.Add(1)
	return g.Backend.DeleteDeadLetter(ctx, rec)
}

func (g *gateBackend) UpdateWebhookStatus(ctx context.Context, id string, status WebhookStatus) error {
	if g.updateStatusHook != nil {
		if err := g.updateStatusHook(ctx, id, status); err != nil {
			return err
		}
	}
	return g.Backend.UpdateWebhookStatus(ctx, id, status)
}

func TestCreateUpdateWebhookDoesNotBlockEmitOrRead(t *testing.T) {
	fb, err := NewFileBackend(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewFileBackend: %v", err)
	}
	putEntered := make(chan struct{})
	releasePut := make(chan struct{})
	var gateOnce sync.Once
	gb := &gateBackend{
		Backend: fb,
		putWebhookHook: func(_ context.Context, _ StoredWebhook) error {
			gateOnce.Do(func() {
				close(putEntered)
				<-releasePut
			})
			return nil
		},
	}
	m, err := OpenWithBackend(gb, Config{AllowPrivateIPs: true, MaxRetries: 1})
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer m.Close()

	// Seed one webhook before engaging the slow hook? Here gateOnce only blocks the first PutWebhook,
	// so start CreateWebhook in a goroutine and verify Emit / ListWebhooks / DeadLettersPending return promptly.
	createDone := make(chan error, 1)
	go func() {
		_, err := m.CreateWebhook("tenant-key-1", CreateWebhookInput{
			Name:   "slow-create",
			URL:    "https://example.com/hook",
			Events: []string{EventCreated},
		})
		createDone <- err
	}()

	select {
	case <-putEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for PutWebhook to enter")
	}

	opDone := make(chan struct{})
	go func() {
		defer close(opDone)
		m.Emit(TenantHash("tenant-key-1"), EventCreated, "cpod-fast", "base", "", "", nil)
		if _, err := m.ListWebhooks("tenant-key-1"); err != nil {
			t.Errorf("ListWebhooks err: %v", err)
		}
		_ = m.DeadLettersPending()
	}()

	select {
	case <-opDone:
	case <-time.After(500 * time.Millisecond):
		close(releasePut)
		t.Fatal("Emit/ListWebhooks/DeadLettersPending blocked behind slow PutWebhook")
	}
	close(releasePut)
	if err := <-createDone; err != nil {
		t.Fatalf("CreateWebhook failed: %v", err)
	}
}

func TestUpdateWebhookPreservesConcurrentDeliveryStatus(t *testing.T) {
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	kc := NewKubeBackendForTest(t)
	var gateActive atomic.Bool
	putEntered := make(chan struct{})
	releasePut := make(chan struct{})
	gb := &gateBackend{
		Backend: kc,
		putWebhookHook: func(_ context.Context, _ StoredWebhook) error {
			if gateActive.Load() {
				close(putEntered)
				<-releasePut
			}
			return nil
		},
	}
	m, err := OpenWithBackend(gb, Config{AllowPrivateIPs: true, MaxRetries: 1})
	if err != nil {
		t.Fatalf("OpenWithBackend: %v", err)
	}
	defer m.Close()

	wh, err := m.CreateWebhook("tenant-key-1", CreateWebhookInput{
		Name:   "hook",
		URL:    receiver.URL,
		Events: []string{"*"},
	})
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}

	gateActive.Store(true)
	newURL := receiver.URL + "/v2"
	updDone := make(chan WebhookWithSecret, 1)
	go func() {
		res, err := m.UpdateWebhook("tenant-key-1", wh.ID, UpdateWebhookInput{URL: &newURL})
		if err != nil {
			t.Errorf("UpdateWebhook: %v", err)
		}
		updDone <- res
	}()

	<-putEntered
	// While UpdateWebhook's PutWebhook is blocked on network I/O, emit and deliver an event
	// so recordDeliveryResult updates LastDeliveryTime in memory and in KubeBackend /status.
	m.Emit(TenantHash("tenant-key-1"), EventCreated, "cpod-status", "base", "", "", nil)
	waitFor(t, "delivery while PutWebhook blocked", func() bool {
		return m.WebhookDeliveriesOK.Load() == 1
	})
	// Give persistWorker a moment to flush UpdateWebhookStatus onto the CR.
	time.Sleep(30 * time.Millisecond)

	close(releasePut)
	res := <-updDone
	if res.URL != newURL || res.LastDeliveryTime == nil {
		t.Fatalf("UpdateWebhook clobbered concurrent LastDeliveryTime: %+v", res)
	}
	// Verify KubeBackend /status on the CR also kept LastDeliveryTime.
	st, err := kc.LoadState(t.Context())
	if err != nil || len(st.Webhooks) != 1 || st.Webhooks[0].LastDeliveryTime == nil || st.Webhooks[0].URL != newURL {
		t.Fatalf("KubeBackend lost concurrent status or URL update: %+v, %v", st.Webhooks, err)
	}
}

func TestDeadLetterDeleteAndRetryWhilePutDeadLetterQueued(t *testing.T) {
	t.Run("delete while PutDeadLetter queued behind slow UpdateWebhookStatus", func(t *testing.T) {
		var fail atomic.Bool
		fail.Store(true)
		receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if fail.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer receiver.Close()

		fb, err := NewFileBackend(t.TempDir(), nil)
		if err != nil {
			t.Fatalf("NewFileBackend: %v", err)
		}
		statusEntered := make(chan struct{})
		releaseStatus := make(chan struct{})
		var statusOnce sync.Once
		gb := &gateBackend{
			Backend: fb,
			updateStatusHook: func(_ context.Context, _ string, _ WebhookStatus) error {
				statusOnce.Do(func() {
					close(statusEntered)
					<-releaseStatus
				})
				return nil
			},
		}
		m, err := OpenWithBackend(gb, Config{AllowPrivateIPs: true, MaxRetries: 1})
		if err != nil {
			t.Fatalf("OpenWithBackend: %v", err)
		}

		wh, err := m.CreateWebhook("tenant-key-1", CreateWebhookInput{Name: "hook", URL: receiver.URL, Events: []string{"*"}})
		if err != nil {
			t.Fatalf("CreateWebhook: %v", err)
		}
		m.Emit(TenantHash("tenant-key-1"), EventCreated, "cpod-q1", "base", "", "", nil)

		// persistWorker is now blocked inside UpdateWebhookStatus BEFORE PutDeadLetter runs!
		<-statusEntered
		waitFor(t, "dead letter in memory", func() bool {
			return m.DeadLettersPending() == 1
		})
		items, _, _ := m.ListDeadLetters("tenant-key-1", DeadLetterFilter{}, 10, "")
		if len(items) != 1 {
			close(releaseStatus)
			t.Fatalf("expected 1 dead letter, got %d", len(items))
		}
		// Delete while PutDeadLetter is still sitting in persistCh behind UpdateWebhookStatus.
		if err := m.DeleteDeadLetter("tenant-key-1", wh.ID, items[0].ID); err != nil {
			close(releaseStatus)
			t.Fatalf("DeleteDeadLetter: %v", err)
		}

		// Now unblock persistWorker and flush via Close; superseded PutDeadLetter must NOT run!
		close(releaseStatus)
		m.Close()

		if calls := gb.putDeadLetterCalls.Load(); calls != 0 {
			t.Fatalf("superseded PutDeadLetter called %d times; want 0", calls)
		}
		st, err := fb.LoadState(t.Context())
		if err != nil || len(st.DeadLetters) != 0 {
			t.Fatalf("dead letter resurrected in backend: %+v, %v", st.DeadLetters, err)
		}
	})

	t.Run("delete waits for in-flight PutDeadLetter without resurrection", func(t *testing.T) {
		receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer receiver.Close()

		fb, err := NewFileBackend(t.TempDir(), nil)
		if err != nil {
			t.Fatalf("NewFileBackend: %v", err)
		}
		putEntered := make(chan struct{})
		releasePut := make(chan struct{})
		var putOnce sync.Once
		gb := &gateBackend{
			Backend: fb,
			putDeadLetterHook: func(_ context.Context, _ StoredDeadLetter) error {
				putOnce.Do(func() {
					close(putEntered)
					<-releasePut
				})
				return nil
			},
		}
		m, err := OpenWithBackend(gb, Config{AllowPrivateIPs: true, MaxRetries: 1})
		if err != nil {
			t.Fatalf("OpenWithBackend: %v", err)
		}

		wh, err := m.CreateWebhook("tenant-key-1", CreateWebhookInput{Name: "hook", URL: receiver.URL, Events: []string{"*"}})
		if err != nil {
			t.Fatalf("CreateWebhook: %v", err)
		}
		m.Emit(TenantHash("tenant-key-1"), EventCreated, "cpod-inflight", "base", "", "", nil)

		<-putEntered
		items, _, _ := m.ListDeadLetters("tenant-key-1", DeadLetterFilter{}, 10, "")
		if len(items) != 1 {
			close(releasePut)
			t.Fatalf("expected 1 dead letter, got %d", len(items))
		}
		delDone := make(chan error, 1)
		go func() {
			delDone <- m.DeleteDeadLetter("tenant-key-1", wh.ID, items[0].ID)
		}()
		// Ensure DeleteDeadLetter blocks behind in-flight PutDeadLetter rather than racing ahead.
		select {
		case err := <-delDone:
			close(releasePut)
			t.Fatalf("DeleteDeadLetter returned prematurely while PutDeadLetter in-flight: %v", err)
		case <-time.After(40 * time.Millisecond):
		}
		close(releasePut)
		if err := <-delDone; err != nil {
			t.Fatalf("DeleteDeadLetter: %v", err)
		}
		m.Close()

		st, err := fb.LoadState(t.Context())
		if err != nil || len(st.DeadLetters) != 0 {
			t.Fatalf("dead letter survived after serialized delete: %+v, %v", st.DeadLetters, err)
		}
	})

	t.Run("retry while PutDeadLetter queued does not resurrect after success", func(t *testing.T) {
		var fail atomic.Bool
		fail.Store(true)
		receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if fail.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer receiver.Close()

		fb, err := NewFileBackend(t.TempDir(), nil)
		if err != nil {
			t.Fatalf("NewFileBackend: %v", err)
		}
		statusEntered := make(chan struct{})
		releaseStatus := make(chan struct{})
		var statusOnce sync.Once
		gb := &gateBackend{
			Backend: fb,
			updateStatusHook: func(_ context.Context, _ string, _ WebhookStatus) error {
				statusOnce.Do(func() {
					close(statusEntered)
					<-releaseStatus
				})
				return nil
			},
		}
		m, err := OpenWithBackend(gb, Config{AllowPrivateIPs: true, MaxRetries: 1})
		if err != nil {
			t.Fatalf("OpenWithBackend: %v", err)
		}

		wh, err := m.CreateWebhook("tenant-key-1", CreateWebhookInput{Name: "hook", URL: receiver.URL, Events: []string{"*"}})
		if err != nil {
			t.Fatalf("CreateWebhook: %v", err)
		}
		m.Emit(TenantHash("tenant-key-1"), EventCreated, "cpod-qretry", "base", "", "", nil)

		<-statusEntered
		waitFor(t, "dead letter queued", func() bool { return m.DeadLettersPending() == 1 })
		items, _, _ := m.ListDeadLetters("tenant-key-1", DeadLetterFilter{}, 10, "")
		fail.Store(false)
		if _, err := m.RetryDeadLetter("tenant-key-1", wh.ID, items[0].ID); err != nil {
			close(releaseStatus)
			t.Fatalf("RetryDeadLetter: %v", err)
		}
		close(releaseStatus)
		waitFor(t, "retry succeeded", func() bool { return m.WebhookDeliveriesOK.Load() == 1 })
		m.Close()

		st, err := fb.LoadState(t.Context())
		if err != nil || len(st.DeadLetters) != 0 {
			t.Fatalf("dead letter resurrected after queued retry: %+v, %v", st.DeadLetters, err)
		}
	})
}
