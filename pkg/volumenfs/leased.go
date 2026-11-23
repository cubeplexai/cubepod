package volumenfs

import (
	"os"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs"
)

// errStaleHandle is returned for operations or filehandles after a lease is revoked.
var errStaleHandle = &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusStale}

// leasedFS wraps an export filesystem and re-checks lease liveness on every
// operation so cached go-nfs filehandles cannot outlive DELETE /leases/{id}.
type leasedFS struct {
	billy.Filesystem
	store    *Store
	leaseID  string
	volumeID string
}

var (
	_ billy.Change  = (*leasedFS)(nil)
	_ billy.Symlink = (*leasedFS)(nil)
)

func newLeasedFS(fs billy.Filesystem, store *Store, leaseID, volumeID string) billy.Filesystem {
	return &leasedFS{Filesystem: fs, store: store, leaseID: leaseID, volumeID: volumeID}
}

func (l *leasedFS) check() error {
	if l == nil || l.store == nil {
		return errStaleHandle
	}
	lease, ok := l.store.Get(l.leaseID)
	if !ok || !lease.Allows(l.volumeID) {
		return errStaleHandle
	}
	return nil
}

func (l *leasedFS) Open(filename string) (billy.File, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	return l.Filesystem.Open(filename)
}

func (l *leasedFS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	return l.Filesystem.OpenFile(filename, flag, perm)
}

func (l *leasedFS) Stat(filename string) (os.FileInfo, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	return l.Filesystem.Stat(filename)
}

func (l *leasedFS) Lstat(filename string) (os.FileInfo, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	return l.Filesystem.Lstat(filename)
}

func (l *leasedFS) ReadDir(path string) ([]os.FileInfo, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	return l.Filesystem.ReadDir(path)
}

func (l *leasedFS) Create(filename string) (billy.File, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	return l.Filesystem.Create(filename)
}

func (l *leasedFS) Remove(filename string) error {
	if err := l.check(); err != nil {
		return err
	}
	return l.Filesystem.Remove(filename)
}

func (l *leasedFS) Rename(from, to string) error {
	if err := l.check(); err != nil {
		return err
	}
	return l.Filesystem.Rename(from, to)
}

func (l *leasedFS) MkdirAll(filename string, perm os.FileMode) error {
	if err := l.check(); err != nil {
		return err
	}
	return l.Filesystem.MkdirAll(filename, perm)
}

func (l *leasedFS) Symlink(target, link string) error {
	if err := l.check(); err != nil {
		return err
	}
	if s, ok := l.Filesystem.(billy.Symlink); ok {
		return s.Symlink(target, link)
	}
	return billy.ErrNotSupported
}

func (l *leasedFS) Readlink(link string) (string, error) {
	if err := l.check(); err != nil {
		return "", err
	}
	if s, ok := l.Filesystem.(billy.Symlink); ok {
		return s.Readlink(link)
	}
	return "", billy.ErrNotSupported
}

func (l *leasedFS) Chroot(path string) (billy.Filesystem, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	inner, err := l.Filesystem.Chroot(path)
	if err != nil {
		return nil, err
	}
	return newLeasedFS(inner, l.store, l.leaseID, l.volumeID), nil
}

func (l *leasedFS) Chmod(name string, mode os.FileMode) error {
	if err := l.check(); err != nil {
		return err
	}
	if c, ok := l.Filesystem.(billy.Change); ok {
		return c.Chmod(name, mode)
	}
	return billy.ErrNotSupported
}

func (l *leasedFS) Lchown(name string, uid, gid int) error {
	if err := l.check(); err != nil {
		return err
	}
	if c, ok := l.Filesystem.(billy.Change); ok {
		return c.Lchown(name, uid, gid)
	}
	return billy.ErrNotSupported
}

func (l *leasedFS) Chown(name string, uid, gid int) error {
	if err := l.check(); err != nil {
		return err
	}
	if c, ok := l.Filesystem.(billy.Change); ok {
		return c.Chown(name, uid, gid)
	}
	return billy.ErrNotSupported
}

func (l *leasedFS) Chtimes(name string, atime, mtime time.Time) error {
	if err := l.check(); err != nil {
		return err
	}
	if c, ok := l.Filesystem.(billy.Change); ok {
		return c.Chtimes(name, atime, mtime)
	}
	return billy.ErrNotSupported
}

// trackingHandler wraps a go-nfs Handler (typically CachingHandler) so
// FromHandle re-checks lease liveness and DELETE /leases can drop cached handles.
type trackingHandler struct {
	nfs.Handler
	store       *Store
	handleLimit int
	mu          sync.Mutex
	byLease     map[string]map[string]struct{}
}

func newTrackingHandler(inner nfs.Handler, store *Store, handleLimit int) *trackingHandler {
	if handleLimit < 2 {
		handleLimit = 1024
	}
	return &trackingHandler{Handler: inner, store: store, handleLimit: handleLimit, byLease: map[string]map[string]struct{}{}}
}

func leaseIDFromFS(f billy.Filesystem) string {
	if l, ok := f.(*leasedFS); ok {
		return l.leaseID
	}
	return ""
}

func (t *trackingHandler) ToHandle(f billy.Filesystem, path []string) []byte {
	// go-nfs onMount calls ToHandle even when Mount returns a nil FS (denied).
	if f == nil || t.Handler == nil {
		return nil
	}
	fh := t.Handler.ToHandle(f, path)
	if id := leaseIDFromFS(f); id != "" && len(fh) > 0 {
		key := string(fh)
		t.mu.Lock()
		if t.byLease[id] == nil {
			t.byLease[id] = map[string]struct{}{}
		}
		if _, exists := t.byLease[id][key]; !exists && t.handleLimit > 0 && len(t.byLease[id]) >= t.handleLimit {
			for old := range t.byLease[id] {
				delete(t.byLease[id], old)
				break
			}
		}
		t.byLease[id][key] = struct{}{}
		t.mu.Unlock()
	}
	return fh
}

func (t *trackingHandler) FromHandle(fh []byte) (billy.Filesystem, []string, error) {
	fs, p, err := t.Handler.FromHandle(fh)
	if err != nil {
		return nil, nil, err
	}
	if l, ok := fs.(*leasedFS); ok {
		if err := l.check(); err != nil {
			_ = t.Handler.InvalidateHandle(fs, fh)
			return nil, nil, errStaleHandle
		}
	}
	return fs, p, nil
}

func (t *trackingHandler) InvalidateHandle(fs billy.Filesystem, handle []byte) error {
	if t.Handler != nil {
		_ = t.Handler.InvalidateHandle(fs, handle)
	}
	if len(handle) == 0 {
		return nil
	}
	key := string(handle)
	t.mu.Lock()
	for id, set := range t.byLease {
		delete(set, key)
		if len(set) == 0 {
			delete(t.byLease, id)
		}
	}
	t.mu.Unlock()
	return nil
}

func (t *trackingHandler) ForgetLease(leaseID string) {
	if t == nil || leaseID == "" {
		return
	}
	t.mu.Lock()
	set := t.byLease[leaseID]
	delete(t.byLease, leaseID)
	t.mu.Unlock()
	for key := range set {
		_ = t.Handler.InvalidateHandle(nil, []byte(key))
	}
}
