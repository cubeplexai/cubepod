package egress

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetrics_BlockedHostsBoundedCardinality(t *testing.T) {
	m := NewMetrics()

	// Populate maxTrackedBlockedHosts - 1 distinct hosts, plus extra increments on an existing host.
	for i := 0; i < maxTrackedBlockedHosts-1; i++ {
		m.IncBlockedHost(fmt.Sprintf("HOST-%d.example.com:443", i))
	}
	// Existing host after approaching the cap should increment its own counter.
	m.IncBlockedHost("host-0.example.com")

	if got := len(m.blockedHosts); got != maxTrackedBlockedHosts-1 {
		t.Fatalf("expected %d tracked hosts before overflow, got %d", maxTrackedBlockedHosts-1, got)
	}

	// Overflow with many more distinct hosts (simulating port/SNI scanning).
	const overflowCount = 500
	for i := 0; i < overflowCount; i++ {
		m.IncBlockedHost(fmt.Sprintf("scan-%d.evil.test", i))
	}

	// Existing tracked host must still increment directly even after the cap is reached.
	m.IncBlockedHost("host-1.example.com")

	if got := len(m.blockedHosts); got != maxTrackedBlockedHosts {
		t.Fatalf("expected blockedHosts map size capped at %d, got %d", maxTrackedBlockedHosts, got)
	}
	if got := m.blockedHosts["host-0.example.com"]; got != 2 {
		t.Fatalf("expected host-0.example.com count 2, got %d", got)
	}
	if got := m.blockedHosts["host-1.example.com"]; got != 2 {
		t.Fatalf("expected host-1.example.com count 2, got %d", got)
	}
	if got := m.blockedHosts["_other"]; got != overflowCount {
		t.Fatalf("expected _other count %d, got %d", overflowCount, got)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}
	body := rec.Body.String()
	wantOther := fmt.Sprintf(`cubepod_egress_blocked_hosts_total{host="_other"} %d`, overflowCount)
	if !strings.Contains(body, wantOther) {
		t.Fatalf("expected %q in metrics output", wantOther)
	}
}
