// Package volumenfs implements the CEP-0031 in-cluster userspace NFSv3 proxy
// and its lease-scoped export admission API.
package volumenfs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultNFSPort is the single TCP port used for both MOUNT and NFS.
	DefaultNFSPort = 2049
	// DefaultLeaseAPIPort is the HTTP port for the internal lease API.
	DefaultLeaseAPIPort = 8080
	// DefaultUnboundTTL is aligned with the EnsurePod poll timeout (60s class).
	DefaultUnboundTTL = 60 * time.Second
	// LeaseIDBytes is the length of the unguessable lease identifier.
	LeaseIDBytes = 16
	// VolumeIDLen is the official E2B volume ID length (32 lowercase hex chars).
	VolumeIDLen = 32
)

// Common admission and API errors.
var (
	ErrLeaseNotFound     = errors.New("lease not found")
	ErrLeaseUnbound      = errors.New("lease podIP is unbound")
	ErrVolumeNotAllowed  = errors.New("volumeID is not allow-listed on the lease")
	ErrSourceIPMismatch  = errors.New("client source IP does not match bound podIP")
	ErrInvalidLeaseID    = errors.New("invalid leaseID")
	ErrInvalidVolumeID   = errors.New("invalid volumeID")
	ErrInvalidExportPath = errors.New("invalid export path")
	ErrInvalidLeaseBody  = errors.New("invalid lease body")
	ErrVolumeSetConflict = errors.New("lease volumeIDs cannot be replaced")
	ErrEmptySourceIP     = errors.New("client source IP is empty")
)

// Lease is a per-sandbox-start export grant covering one or more official volumes.
type Lease struct {
	LeaseID    string
	VolumeIDs  []string
	PodName    string
	PodBoxName string
	PodIP      string
	CreatedAt  time.Time
	BoundAt    time.Time
}

// Bound reports whether the lease has a non-empty podIP.
func (l Lease) Bound() bool {
	return strings.TrimSpace(l.PodIP) != ""
}

// Allows reports whether volumeID is in the lease allow-list.
func (l Lease) Allows(volumeID string) bool {
	for _, id := range l.VolumeIDs {
		if id == volumeID {
			return true
		}
	}
	return false
}

// LeaseRequest is the PUT /leases/{leaseID} body. volumeIDs must be an array;
// a singular volumeID field is rejected by the HTTP API.
type LeaseRequest struct {
	VolumeIDs  []string `json:"volumeIDs"`
	PodName    string   `json:"podName"`
	PodBoxName string   `json:"podBoxName"`
	PodIP      string   `json:"podIP,omitempty"`
}

// Store holds live leases and expires unbound entries after TTL.
type Store struct {
	mu       sync.Mutex
	leases   map[string]*storedLease
	ttl      time.Duration
	now      func() time.Time
	onExpire func(leaseID string)
	denials  *Metrics
}

type storedLease struct {
	Lease
	expireAt time.Time // zero when bound
}

// StoreOption configures a Store.
type StoreOption func(*Store)

// WithUnboundTTL sets the unbound lease expiry. Bound leases do not expire.
func WithUnboundTTL(ttl time.Duration) StoreOption {
	return func(s *Store) {
		if ttl > 0 {
			s.ttl = ttl
		}
	}
}

// WithNow overrides the clock used for TTL (tests).
func WithNow(now func() time.Time) StoreOption {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// WithMetrics attaches metrics incrementers.
func WithMetrics(m *Metrics) StoreOption {
	return func(s *Store) {
		s.denials = m
	}
}

// WithOnExpire registers a callback invoked when an unbound lease expires.
func WithOnExpire(fn func(leaseID string)) StoreOption {
	return func(s *Store) {
		s.onExpire = fn
	}
}

// NewStore returns an empty lease store.
func NewStore(opts ...StoreOption) *Store {
	s := &Store{
		leases: make(map[string]*storedLease),
		ttl:    DefaultUnboundTTL,
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Put creates or refreshes a lease. VolumeIDs on an existing lease must match
// the previously stored set; podIP may be bound or refreshed.
func (s *Store) Put(leaseID string, req LeaseRequest) (Lease, error) {
	if !ValidLeaseID(leaseID) {
		return Lease{}, ErrInvalidLeaseID
	}
	ids, err := normalizeVolumeIDs(req.VolumeIDs)
	if err != nil {
		return Lease{}, err
	}
	podIP := strings.TrimSpace(req.PodIP)
	if podIP != "" {
		if ip := net.ParseIP(podIP); ip == nil {
			return Lease{}, fmt.Errorf("%w: invalid podIP %q", ErrInvalidLeaseBody, req.PodIP)
		}
		podIP = podIPCanonical(podIP)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()

	now := s.now()
	if existing, ok := s.leases[leaseID]; ok {
		if !sameVolumeSet(existing.VolumeIDs, ids) {
			return Lease{}, ErrVolumeSetConflict
		}
		if req.PodName != "" {
			existing.PodName = req.PodName
		}
		if req.PodBoxName != "" {
			existing.PodBoxName = req.PodBoxName
		}
		if podIP != "" {
			existing.PodIP = podIP
			if existing.BoundAt.IsZero() {
				existing.BoundAt = now
			}
			existing.expireAt = time.Time{}
		}
		return existing.Lease, nil
	}

	l := Lease{
		LeaseID:    leaseID,
		VolumeIDs:  ids,
		PodName:    req.PodName,
		PodBoxName: req.PodBoxName,
		PodIP:      podIP,
		CreatedAt:  now,
	}
	st := &storedLease{Lease: l}
	if podIP != "" {
		st.BoundAt = now
		st.Lease.BoundAt = now
	} else {
		st.expireAt = now.Add(s.ttl)
	}
	s.leases[leaseID] = st
	if s.denials != nil {
		s.denials.SetLeasesActive(len(s.leases))
	}
	return st.Lease, nil
}

// Get returns a copy of the lease if it is still live.
func (s *Store) Get(leaseID string) (Lease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	st, ok := s.leases[leaseID]
	if !ok {
		return Lease{}, false
	}
	return st.Lease, true
}

// Delete revokes a lease and all of its exports.
func (s *Store) Delete(leaseID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	if _, ok := s.leases[leaseID]; !ok {
		return false
	}
	delete(s.leases, leaseID)
	if s.denials != nil {
		s.denials.SetLeasesActive(len(s.leases))
	}
	return true
}

// Active returns the number of live leases (after TTL expiry).
func (s *Store) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	return len(s.leases)
}

// AdmitMount checks a MOUNT of /<leaseID>/<volumeID> from sourceIP.
func (s *Store) AdmitMount(leaseID, volumeID, sourceIP string) error {
	if !ValidLeaseID(leaseID) {
		s.deny("invalid_lease")
		return ErrInvalidLeaseID
	}
	if !ValidVolumeID(volumeID) {
		s.deny("invalid_volume")
		return ErrInvalidVolumeID
	}
	src := canonicalSourceIP(sourceIP)
	if src == "" {
		s.deny("empty_source_ip")
		return ErrEmptySourceIP
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()

	st, ok := s.leases[leaseID]
	if !ok {
		s.deny("unknown_lease")
		return ErrLeaseNotFound
	}
	if !st.Allows(volumeID) {
		s.deny("volume_not_allowed")
		return ErrVolumeNotAllowed
	}
	if !st.Bound() {
		s.deny("unbound")
		return ErrLeaseUnbound
	}
	if !ipEqual(st.PodIP, src) {
		s.deny("source_ip_mismatch")
		return ErrSourceIPMismatch
	}
	return nil
}

func (s *Store) deny(reason string) {
	if s.denials != nil {
		s.denials.IncExportDenials(reason)
	}
}

func (s *Store) expireLocked() {
	now := s.now()
	for id, st := range s.leases {
		if st.Bound() {
			continue
		}
		if !st.expireAt.IsZero() && !now.Before(st.expireAt) {
			delete(s.leases, id)
			if s.onExpire != nil {
				s.onExpire(id)
			}
		}
	}
	if s.denials != nil {
		s.denials.SetLeasesActive(len(s.leases))
	}
}

// RunExpiry periodically drops unbound leases that have exceeded the TTL.
func (s *Store) RunExpiry(stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.mu.Lock()
			s.expireLocked()
			s.mu.Unlock()
		}
	}
}

// NewLeaseID returns an unguessable hex lease identifier.
func NewLeaseID() (string, error) {
	var b [LeaseIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate leaseID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ValidLeaseID reports whether id is a hex string of LeaseIDBytes.
func ValidLeaseID(id string) bool {
	if len(id) != LeaseIDBytes*2 {
		return false
	}
	for _, c := range id {
		if !isLowerHex(byte(c)) {
			return false
		}
	}
	return true
}

// ValidVolumeID reports whether id is a 32-char lowercase hex official volume ID.
func ValidVolumeID(id string) bool {
	if len(id) != VolumeIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !isLowerHex(id[i]) {
			return false
		}
	}
	return true
}

func isLowerHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

func normalizeVolumeIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: volumeIDs is required", ErrInvalidLeaseBody)
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if !ValidVolumeID(id) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidVolumeID, raw)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: volumeIDs is required", ErrInvalidLeaseBody)
	}
	return out, nil
}

func sameVolumeSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]string(nil), a...)
	sb := append([]string(nil), b...)
	sort.Strings(sa)
	sort.Strings(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}

func podIPCanonical(ip string) string {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return strings.TrimSpace(ip)
	}
	return parsed.String()
}

func canonicalSourceIP(addr string) string {
	host := strings.TrimSpace(addr)
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return podIPCanonical(host)
}

func ipEqual(a, b string) bool {
	pa, pb := net.ParseIP(a), net.ParseIP(b)
	if pa == nil || pb == nil {
		return a == b
	}
	return pa.Equal(pb)
}
