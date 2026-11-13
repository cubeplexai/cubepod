package egress

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Metrics tracks Prometheus counters for the cubepod-egress sidecar.
type Metrics struct {
	mu              sync.RWMutex
	requests        map[requestMetricKey]uint64
	secretExchanges map[string]uint64
	blockedHosts    map[string]uint64
}

type requestMetricKey struct {
	mode     string
	decision string
}

// NewMetrics constructs an initialized Metrics registry.
func NewMetrics() *Metrics {
	return &Metrics{
		requests:        make(map[requestMetricKey]uint64),
		secretExchanges: make(map[string]uint64),
		blockedHosts:    make(map[string]uint64),
	}
}

// IncRequest increments cubepod_egress_requests_total{mode, decision}.
func (m *Metrics) IncRequest(mode, decision string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.requests[requestMetricKey{mode: mode, decision: decision}]++
	m.mu.Unlock()
}

// IncSecretExchange increments cubepod_egress_secret_exchanges_total{result}.
func (m *Metrics) IncSecretExchange(result string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.secretExchanges[result]++
	m.mu.Unlock()
}

const maxTrackedBlockedHosts = 1024

// IncBlockedHost increments cubepod_egress_blocked_hosts_total{host}.
func (m *Metrics) IncBlockedHost(host string) {
	if m == nil {
		return
	}
	h := normalizeHost(host)
	if h == "" {
		h = "unknown"
	}
	m.mu.Lock()
	if _, exists := m.blockedHosts[h]; !exists {
		limit := maxTrackedBlockedHosts - 1
		if _, hasOther := m.blockedHosts["_other"]; hasOther {
			limit = maxTrackedBlockedHosts
		}
		if len(m.blockedHosts) >= limit {
			h = "_other"
		}
	}
	m.blockedHosts[h]++
	m.mu.Unlock()
}

// ServeHTTP renders Prometheus text exposition format (0.0.4).
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	m.mu.RLock()
	reqKeys := make([]requestMetricKey, 0, len(m.requests))
	for k := range m.requests {
		reqKeys = append(reqKeys, k)
	}
	sort.Slice(reqKeys, func(i, j int) bool {
		if reqKeys[i].mode != reqKeys[j].mode {
			return reqKeys[i].mode < reqKeys[j].mode
		}
		return reqKeys[i].decision < reqKeys[j].decision
	})
	reqVals := make([]uint64, len(reqKeys))
	for i, k := range reqKeys {
		reqVals[i] = m.requests[k]
	}

	exKeys := make([]string, 0, len(m.secretExchanges))
	for k := range m.secretExchanges {
		exKeys = append(exKeys, k)
	}
	sort.Strings(exKeys)
	exVals := make([]uint64, len(exKeys))
	for i, k := range exKeys {
		exVals[i] = m.secretExchanges[k]
	}

	blockedKeys := make([]string, 0, len(m.blockedHosts))
	for k := range m.blockedHosts {
		blockedKeys = append(blockedKeys, k)
	}
	sort.Strings(blockedKeys)
	blockedVals := make([]uint64, len(blockedKeys))
	for i, k := range blockedKeys {
		blockedVals[i] = m.blockedHosts[k]
	}
	m.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder

	b.WriteString("# HELP cubepod_egress_requests_total Total number of outbound egress requests processed.\n")
	b.WriteString("# TYPE cubepod_egress_requests_total counter\n")
	for i, k := range reqKeys {
		fmt.Fprintf(&b, "cubepod_egress_requests_total{mode=%q,decision=%q} %d\n", k.mode, k.decision, reqVals[i])
	}

	b.WriteString("# HELP cubepod_egress_secret_exchanges_total Total number of mTLS cpodref_ secret exchanges.\n")
	b.WriteString("# TYPE cubepod_egress_secret_exchanges_total counter\n")
	for i, k := range exKeys {
		fmt.Fprintf(&b, "cubepod_egress_secret_exchanges_total{result=%q} %d\n", k, exVals[i])
	}

	b.WriteString("# HELP cubepod_egress_blocked_hosts_total Total number of blocked egress attempts by host.\n")
	b.WriteString("# TYPE cubepod_egress_blocked_hosts_total counter\n")
	for i, k := range blockedKeys {
		fmt.Fprintf(&b, "cubepod_egress_blocked_hosts_total{host=%q} %d\n", k, blockedVals[i])
	}

	_, _ = w.Write([]byte(b.String()))
}
