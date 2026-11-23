package volumenfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

// Server serves the internal lease HTTP API and a userspace NFSv3 proxy on
// two separate TCP ports. NFS must never share the lease API port.
type Server struct {
	Store      *Store
	Metrics    *Metrics
	VolumesDir string
	Logger     *slog.Logger
	nfsServer  *nfs.Server

	mu      sync.Mutex
	handles *trackingHandler
}

// Config configures Server listeners.
type Config struct {
	VolumesDir   string
	NFSAddr      string
	LeaseAPIAddr string
	UnboundTTL   time.Duration
	HandleCache  int
	Logger       *slog.Logger
}

// NewServer constructs the proxy and lease API (listeners are started by Serve).
func NewServer(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	metrics := NewMetrics()
	ttl := cfg.UnboundTTL
	if ttl <= 0 {
		ttl = DefaultUnboundTTL
	}
	store := NewStore(WithUnboundTTL(ttl), WithMetrics(metrics), WithOnExpire(func(leaseID string) {
		logger.Info("lease expired", "leaseID", leaseID)
	}))
	return &Server{
		Store:      store,
		Metrics:    metrics,
		VolumesDir: cfg.VolumesDir,
		Logger:     logger,
	}
}

// LeaseHTTPHandler returns the internal lease API mux (not the official E2B surface).
func (s *Server) LeaseHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.Metrics.WritePrometheus(w)
	})
	mux.HandleFunc("PUT /leases/{leaseID}", s.handlePutLease)
	mux.HandleFunc("DELETE /leases/{leaseID}", s.handleDeleteLease)
	return mux
}

func (s *Server) handlePutLease(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("leaseID")
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpJSON(w, http.StatusBadRequest, "failed to read body")
		return
	}
	if err := rejectSingularVolumeID(body); err != nil {
		httpJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	var req LeaseRequest
	if err := json.Unmarshal(body, &req); err != nil {
		httpJSON(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	lease, err := s.Store.Put(leaseID, req)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrVolumeSetConflict) {
			status = http.StatusConflict
		}
		s.Logger.Info("lease put rejected", "leaseID", leaseID, "err", err)
		httpJSON(w, status, err.Error())
		return
	}
	s.Logger.Info("lease put",
		"leaseID", lease.LeaseID,
		"podName", lease.PodName,
		"podBoxName", lease.PodBoxName,
		"podIP", lease.PodIP,
		"volumeIDs", lease.VolumeIDs,
		"bound", lease.Bound(),
	)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(lease)
}

func (s *Server) handleDeleteLease(w http.ResponseWriter, r *http.Request) {
	leaseID := r.PathValue("leaseID")
	ok := s.Store.Delete(leaseID)
	s.forgetHandles(leaseID)
	s.Logger.Info("lease deleted", "leaseID", leaseID, "existed", ok)
	w.WriteHeader(http.StatusNoContent)
}

func rejectSingularVolumeID(body []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil // let the typed unmarshal report the error
	}
	if _, ok := raw["volumeID"]; ok {
		return fmt.Errorf("%w: singular volumeID is invalid; use volumeIDs[]", ErrInvalidLeaseBody)
	}
	if _, ok := raw["volumeId"]; ok {
		return fmt.Errorf("%w: singular volumeId is invalid; use volumeIDs[]", ErrInvalidLeaseBody)
	}
	return nil
}

func httpJSON(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code":    status,
		"message": message,
	})
}

func (s *Server) forgetHandles(leaseID string) {
	s.mu.Lock()
	h := s.handles
	s.mu.Unlock()
	if h != nil {
		h.ForgetLease(leaseID)
	}
}

// ServeNFS starts the userspace NFSv3+MOUNT server on lis (typically :2049).
// Filehandles are in-memory; a process restart ESTALEs active mounts (Alpha).
func (s *Server) ServeNFS(ctx context.Context, lis net.Listener, handleCache int) error {
	if handleCache < 2 {
		handleCache = 1024
	}
	inner := NewHandler(s.Store, s.VolumesDir, s.Metrics, s.Logger)
	handler := newTrackingHandler(nfshelper.NewCachingHandler(inner, handleCache), s.Store, handleCache)
	s.mu.Lock()
	s.handles = handler
	s.mu.Unlock()
	s.nfsServer = &nfs.Server{
		Handler: handler,
		Context: ctx,
	}
	s.Logger.Info("volume nfs listening", "addr", lis.Addr().String(), "volumesDir", s.VolumesDir)
	if err := s.nfsServer.Serve(lis); err != nil {
		if ctx.Err() != nil || strings.Contains(err.Error(), "use of closed network connection") {
			return nil
		}
		return err
	}
	return nil
}

// Serve starts the lease HTTP API, NFS listener, and unbound-lease expiry loop.
func Serve(ctx context.Context, cfg Config) error {
	if strings.TrimSpace(cfg.VolumesDir) == "" {
		return fmt.Errorf("volumes dir is required")
	}
	s := NewServer(cfg)

	nfsAddr := cfg.NFSAddr
	if nfsAddr == "" {
		nfsAddr = fmt.Sprintf(":%d", DefaultNFSPort)
	}
	leaseAddr := cfg.LeaseAPIAddr
	if leaseAddr == "" {
		leaseAddr = fmt.Sprintf(":%d", DefaultLeaseAPIPort)
	}

	nfsLis, err := net.Listen("tcp", nfsAddr)
	if err != nil {
		return fmt.Errorf("listen nfs: %w", err)
	}
	leaseLis, err := net.Listen("tcp", leaseAddr)
	if err != nil {
		_ = nfsLis.Close()
		return fmt.Errorf("listen lease api: %w", err)
	}

	httpSrv := &http.Server{
		Handler:           s.LeaseHTTPHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	stopExpiry := make(chan struct{})
	go s.Store.RunExpiry(stopExpiry)

	errCh := make(chan error, 2)
	go func() {
		errCh <- s.ServeNFS(ctx, nfsLis, cfg.HandleCache)
	}()
	go func() {
		if err := httpSrv.Serve(leaseLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		close(stopExpiry)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		_ = nfsLis.Close()
		_ = leaseLis.Close()
		return nil
	case err := <-errCh:
		close(stopExpiry)
		_ = httpSrv.Close()
		_ = nfsLis.Close()
		_ = leaseLis.Close()
		return err
	}
}
