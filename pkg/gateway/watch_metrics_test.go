package gateway

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/cubeplexai/cubepod/pkg/controller"
)

func newMetricsTestWatcher(m *Metrics, poll bool) *dirWatcher {
	return &dirWatcher{
		metrics: m,
		poll:    poll,
		fd:      -1,
		stopFD:  -1,
		watches: map[int]string{},
		subs:    map[*watchSub]struct{}{},
		done:    make(chan struct{}),
	}
}

func TestWatchMetricsCountEventsAndEachOverflow(t *testing.T) {
	m := &Metrics{}

	// Subscriber queue overflow counts once, events keep flowing.
	w := newMetricsTestWatcher(m, true)
	w.subscribe()
	for i := 0; i < watchSubQueue+1; i++ {
		w.publish(fsEvent{Name: fmt.Sprintf("n%d", i), Type: "EVENT_TYPE_WRITE"})
	}
	if got := m.WatchEventsTotal.Load(); got != uint64(watchSubQueue+1) {
		t.Fatalf("events = %d", got)
	}
	if got := m.WatchOverflowsTotal.Load(); got != 1 {
		t.Fatalf("overflows after subscriber overflow = %d", got)
	}

	// Ring overflow: one overflow, the dropped event is not counted as emitted.
	r := newMetricsTestWatcher(m, true)
	before := m.WatchEventsTotal.Load()
	for i := 0; i < watchRingCap+5; i++ {
		r.publish(fsEvent{Name: "x", Type: "EVENT_TYPE_WRITE"})
	}
	if got := m.WatchEventsTotal.Load() - before; got != uint64(watchRingCap) {
		t.Fatalf("ring events = %d want %d", got, watchRingCap)
	}
	if got := m.WatchOverflowsTotal.Load(); got != 2 {
		t.Fatalf("overflows after ring overflow = %d", got)
	}

	// Kernel IN_Q_OVERFLOW, reported twice, counts once.
	k := newMetricsTestWatcher(m, true)
	k.onRaw(0, unix.IN_Q_OVERFLOW, "")
	k.noteOverflow(watchBufferOverflowMsg)
	if got := m.WatchOverflowsTotal.Load(); got != 3 {
		t.Fatalf("overflows after kernel overflow = %d", got)
	}

	// nil metrics must not panic.
	newMetricsTestWatcher(nil, true).publish(fsEvent{Name: "x"})
}

func TestWatchGaugesRenderedOnMetrics(t *testing.T) {
	srv := NewServer(controller.NewReconciler())
	srv.watchers = map[string]*dirWatcher{
		"a": newMetricsTestWatcher(nil, true),
		"b": newMetricsTestWatcher(nil, false),
	}
	m := srv.Metrics()
	m.WatchStreamsActive.Add(-3) // must clamp to zero
	m.WatchEventsTotal.Add(7)
	m.WatchOverflowsTotal.Add(2)

	rec := httptest.NewRecorder()
	srv.handleMetrics(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"cubepod_watch_streams_active 0\n",
		"cubepod_watchers_active 1\n",
		"cubepod_watch_events_total 7\n",
		"cubepod_watch_overflows_total 2\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
	m.WatchStreamsActive.Store(2)
	rec = httptest.NewRecorder()
	srv.handleMetrics(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "cubepod_watch_streams_active 2\n") {
		t.Fatal("streams gauge not rendered")
	}
}

func TestSecretLatencyHistogramRenderedOnMetrics(t *testing.T) {
	srv := NewServer(controller.NewReconciler())
	key := make([]byte, 32)
	if err := srv.ConfigureE2BSecrets(E2BSecretConfig{Dir: t.TempDir(), EncryptionKey: key, MaxSecretsPerTenant: 5}); err != nil {
		t.Fatal(err)
	}
	st := srv.SecretStore()
	if _, err := st.Create("key-a", "n", "v"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.handleMetrics(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE cubepod_secrets_operation_duration_seconds histogram\n",
		`cubepod_secrets_operation_duration_seconds_bucket{operation="create",le="0.005"}`,
		`cubepod_secrets_operation_duration_seconds_bucket{operation="create",le="+Inf"} 1` + "\n",
		`cubepod_secrets_operation_duration_seconds_count{operation="create"} 1` + "\n",
		`cubepod_secrets_operation_duration_seconds_sum{operation="create"} `,
		`cubepod_secrets_operation_duration_seconds_count{operation="resolve"} 0` + "\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
}
