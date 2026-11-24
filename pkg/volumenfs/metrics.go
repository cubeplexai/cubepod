package volumenfs

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metrics holds the CEP-0031 volume NFS counters.
type Metrics struct {
	mountsTotal        atomic.Int64
	mountErrorsTotal   atomic.Int64
	exportDenialsTotal atomic.Int64
	leasesActive       atomic.Int64

	mu         sync.Mutex
	denials    map[string]int64
	rpcCount   int64
	rpcSeconds float64
}

// NewMetrics returns zeroed metrics.
func NewMetrics() *Metrics {
	return &Metrics{denials: make(map[string]int64)}
}

func (m *Metrics) IncMounts() {
	if m != nil {
		m.mountsTotal.Add(1)
	}
}

func (m *Metrics) IncMountErrors() {
	if m != nil {
		m.mountErrorsTotal.Add(1)
	}
}

func (m *Metrics) IncExportDenials(reason string) {
	if m == nil {
		return
	}
	m.exportDenialsTotal.Add(1)
	if reason == "" {
		reason = "unspecified"
	}
	m.mu.Lock()
	m.denials[reason]++
	m.mu.Unlock()
}

func (m *Metrics) ObserveRPC(d time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.rpcCount++
	m.rpcSeconds += d.Seconds()
	m.mu.Unlock()
}

func (m *Metrics) SetLeasesActive(n int) {
	if m != nil {
		m.leasesActive.Store(int64(n))
	}
}

func (m *Metrics) ExportDenials() int64 {
	if m == nil {
		return 0
	}
	return m.exportDenialsTotal.Load()
}

func (m *Metrics) Mounts() int64 {
	if m == nil {
		return 0
	}
	return m.mountsTotal.Load()
}

func (m *Metrics) MountErrors() int64 {
	if m == nil {
		return 0
	}
	return m.mountErrorsTotal.Load()
}

// WritePrometheus writes Prometheus text exposition.
func (m *Metrics) WritePrometheus(w io.Writer) {
	if m == nil {
		return
	}
	fmt.Fprintf(w, "# HELP cubepod_volume_nfs_mounts_total Admitted NFS MOUNT requests.\n")
	fmt.Fprintf(w, "# TYPE cubepod_volume_nfs_mounts_total counter\n")
	fmt.Fprintf(w, "cubepod_volume_nfs_mounts_total %d\n", m.mountsTotal.Load())
	fmt.Fprintf(w, "# HELP cubepod_volume_nfs_mount_errors_total Failed NFS MOUNT requests.\n")
	fmt.Fprintf(w, "# TYPE cubepod_volume_nfs_mount_errors_total counter\n")
	fmt.Fprintf(w, "cubepod_volume_nfs_mount_errors_total %d\n", m.mountErrorsTotal.Load())
	fmt.Fprintf(w, "# HELP cubepod_volume_nfs_export_denials_total Denied NFS MOUNT requests by reason.\n")
	fmt.Fprintf(w, "# TYPE cubepod_volume_nfs_export_denials_total counter\n")
	m.mu.Lock()
	denials := make(map[string]int64, len(m.denials))
	for k, v := range m.denials {
		denials[k] = v
	}
	rpcCount := m.rpcCount
	rpcSeconds := m.rpcSeconds
	m.mu.Unlock()
	if len(denials) == 0 {
		fmt.Fprintf(w, "cubepod_volume_nfs_export_denials_total{reason=\"unspecified\"} %d\n", m.exportDenialsTotal.Load())
	} else {
		keys := make([]string, 0, len(denials))
		for k := range denials {
			keys = append(keys, k)
		}
		// stable-ish order
		for i := 0; i < len(keys); i++ {
			for j := i + 1; j < len(keys); j++ {
				if keys[j] < keys[i] {
					keys[i], keys[j] = keys[j], keys[i]
				}
			}
		}
		for _, k := range keys {
			fmt.Fprintf(w, "cubepod_volume_nfs_export_denials_total{reason=%q} %d\n", k, denials[k])
		}
	}
	fmt.Fprintf(w, "# HELP cubepod_volume_nfs_rpc_duration_seconds NFS MOUNT RPC handling time.\n")
	fmt.Fprintf(w, "# TYPE cubepod_volume_nfs_rpc_duration_seconds summary\n")
	fmt.Fprintf(w, "cubepod_volume_nfs_rpc_duration_seconds_sum %s\n", formatFloat(rpcSeconds))
	fmt.Fprintf(w, "cubepod_volume_nfs_rpc_duration_seconds_count %d\n", rpcCount)
	fmt.Fprintf(w, "# HELP cubepod_volume_nfs_leases_active Live volume NFS leases.\n")
	fmt.Fprintf(w, "# TYPE cubepod_volume_nfs_leases_active gauge\n")
	fmt.Fprintf(w, "cubepod_volume_nfs_leases_active %d\n", m.leasesActive.Load())
}

func formatFloat(f float64) string {
	s := fmt.Sprintf("%.6f", f)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}
