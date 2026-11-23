package volumenfs

import (
	"context"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs"
)

// Handler is a go-nfs Handler that admits MOUNT requests against the lease store
// and chroots each export to <volumesDir>/<volumeID>/data.
type Handler struct {
	store      *Store
	volumesDir string
	metrics    *Metrics
	logger     *slog.Logger
}

var _ nfs.Handler = (*Handler)(nil)

// NewHandler returns an admission-aware NFS handler. Wrap the result with
// helpers.NewCachingHandler for in-memory filehandles (Alpha ESTALE limits apply).
func NewHandler(store *Store, volumesDir string, metrics *Metrics, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		store:      store,
		volumesDir: volumesDir,
		metrics:    metrics,
		logger:     logger,
	}
}

// Mount admits /<leaseID>/<volumeID> only for a live, bound, source-IP-matching lease.
func (h *Handler) Mount(ctx context.Context, conn net.Conn, req nfs.MountRequest) (nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	start := time.Now()
	defer func() {
		if h.metrics != nil {
			h.metrics.ObserveRPC(time.Since(start))
		}
	}()

	sourceIP := ""
	if conn != nil && conn.RemoteAddr() != nil {
		sourceIP = conn.RemoteAddr().String()
	}

	ref, err := ParseExportPath(string(req.Dirpath))
	if err != nil {
		if h.metrics != nil {
			h.metrics.IncExportDenials("invalid_export_path")
		}
		h.fail(ctx, sourceIP, string(req.Dirpath), err)
		return nfs.MountStatusErrAcces, nil, nil
	}
	if err := h.store.AdmitMount(ref.LeaseID, ref.VolumeID, sourceIP); err != nil {
		h.fail(ctx, sourceIP, string(req.Dirpath), err)
		return nfs.MountStatusErrAcces, nil, nil
	}
	fs, root, err := OpenVolumeFS(h.volumesDir, ref.VolumeID)
	if err != nil {
		h.fail(ctx, sourceIP, string(req.Dirpath), err)
		return nfs.MountStatusErrAcces, nil, nil
	}
	if h.metrics != nil {
		h.metrics.IncMounts()
	}
	h.logger.InfoContext(ctx, "volume nfs mount admitted",
		"leaseID", ref.LeaseID,
		"volumeID", ref.VolumeID,
		"sourceIP", sourceIP,
		"root", root,
	)
	return nfs.MountStatusOk, newLeasedFS(fs, h.store, ref.LeaseID, ref.VolumeID), []nfs.AuthFlavor{nfs.AuthFlavorUnix}
}

func (h *Handler) fail(ctx context.Context, sourceIP, dirpath string, err error) {
	if h.metrics != nil {
		h.metrics.IncMountErrors()
	}
	h.logger.InfoContext(ctx, "volume nfs mount denied",
		"dirpath", dirpath,
		"sourceIP", sourceIP,
		"err", err,
	)
}

// Change enables Unix ownership operations on the export. Files on disk remain 1000:1000
// by convention of the volume store; AUTH_UNIX credentials are accepted.
func (h *Handler) Change(fs billy.Filesystem) billy.Change {
	if c, ok := fs.(billy.Change); ok {
		return c
	}
	return nil
}

func (h *Handler) FSStat(_ context.Context, fs billy.Filesystem, out *nfs.FSStat) error {
	root := "."
	if r, ok := fs.(interface{ Root() string }); ok && r.Root() != "" {
		root = r.Root()
	}
	var stat os.FileInfo
	var err error
	if root == "." {
		stat, err = fs.Stat("/")
		if err != nil {
			stat, err = fs.Stat(".")
		}
	} else {
		stat, err = os.Stat(root)
	}
	if err != nil {
		return err
	}
	_ = stat
	// Conservative defaults; free-space is not a correctness requirement for Alpha.
	if out != nil {
		out.TotalSize = 1 << 40
		out.FreeSize = 1 << 40
		out.AvailableSize = 1 << 40
		out.TotalFiles = 1 << 20
		out.FreeFiles = 1 << 20
	}
	return nil
}

// ToHandle is provided by helpers.NewCachingHandler when wrapped.
func (h *Handler) ToHandle(_ billy.Filesystem, _ []string) []byte { return nil }

func (h *Handler) FromHandle(_ []byte) (billy.Filesystem, []string, error) {
	return nil, nil, &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusStale}
}

func (h *Handler) InvalidateHandle(_ billy.Filesystem, _ []byte) error { return nil }

func (h *Handler) HandleLimit() int { return 1024 }
