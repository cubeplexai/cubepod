package volumenfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
)

// OpenVolumeFS returns a billy filesystem chrooted to <volumesDir>/<volumeID>/data.
// Path traversal outside that directory is rejected.
func OpenVolumeFS(volumesDir, volumeID string) (billy.Filesystem, string, error) {
	if !ValidVolumeID(volumeID) {
		return nil, "", ErrInvalidVolumeID
	}
	root := VolumeDataDir(volumesDir, volumeID)
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, "", fmt.Errorf("resolve volume data dir: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, "", fmt.Errorf("volume data dir: %w", err)
	}
	if !info.IsDir() {
		return nil, "", fmt.Errorf("volume data dir %s is not a directory", abs)
	}
	base := osfs.New(abs, osfs.WithBoundOS())
	return &jailedFS{Filesystem: base, root: abs}, abs, nil
}

type jailedFS struct {
	billy.Filesystem
	root string
}

var _ billy.Change = (*jailedFS)(nil)

func (j *jailedFS) Join(elem ...string) string {
	return j.Filesystem.Join(elem...)
}

// jailPath rejects any path that, once cleaned relative to the export root,
// would escape via "..". Absolute paths are treated as relative to the root.
func jailPath(elem ...string) error {
	for _, e := range elem {
		if strings.Contains(e, "\x00") {
			return os.ErrPermission
		}
	}
	raw := strings.Join(elem, "/")
	if raw == "" {
		return nil
	}
	raw = strings.TrimPrefix(raw, "/")
	clean := filepath.Clean(raw)
	if clean == "." || clean == "" {
		return nil
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return os.ErrPermission
	}
	return nil
}

// jailSymlinkTarget rejects absolute targets and relative targets that would
// resolve outside the export root from the link's directory.
func jailSymlinkTarget(link, target string) error {
	link = strings.TrimPrefix(link, "/")
	if err := jailPath(link); err != nil {
		return err
	}
	if strings.Contains(target, "\x00") || filepath.IsAbs(target) {
		return os.ErrPermission
	}
	// Reject any ".." component so a later rename cannot turn an in-jail
	// relative link into a path that escapes the export root.
	for _, part := range strings.Split(filepath.ToSlash(target), "/") {
		if part == ".." {
			return os.ErrPermission
		}
	}
	dir := filepath.Dir(link)
	resolved := filepath.Clean(filepath.Join(dir, target))
	if filepath.IsAbs(resolved) {
		resolved = strings.TrimPrefix(resolved, string(filepath.Separator))
	}
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(filepath.Separator)) {
		return os.ErrPermission
	}
	return nil
}

func pathInsideRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolve maps a jail-relative name to a path under j.root.
// Intermediate symlinks are evaluated and must stay inside the jail.
// If followFinal is true, a final symlink is also resolved and must stay inside.
func (j *jailedFS) resolve(name string, followFinal bool) (string, error) {
	if err := jailPath(name); err != nil {
		return "", err
	}
	rel := strings.TrimPrefix(filepath.Clean(name), string(filepath.Separator))
	if rel == "." || rel == "" {
		return j.root, nil
	}
	cur := j.root
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return "", os.ErrPermission
		}
		next := filepath.Join(cur, part)
		if !pathInsideRoot(j.root, next) {
			return "", os.ErrPermission
		}
		info, err := os.Lstat(next)
		isLast := i == len(parts)-1
		if err != nil {
			if os.IsNotExist(err) && isLast && pathInsideRoot(j.root, next) {
				return next, nil
			}
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if isLast && !followFinal {
				return next, nil
			}
			target, err := filepath.EvalSymlinks(next)
			if err != nil {
				return "", err
			}
			if !pathInsideRoot(j.root, target) {
				return "", os.ErrPermission
			}
			cur = target
			continue
		}
		cur = next
	}
	return cur, nil
}

func (j *jailedFS) Open(filename string) (billy.File, error) {
	if err := jailPath(filename); err != nil {
		return nil, err
	}
	return j.Filesystem.Open(filename)
}

func (j *jailedFS) OpenFile(filename string, flag int, perm os.FileMode) (billy.File, error) {
	if err := jailPath(filename); err != nil {
		return nil, err
	}
	return j.Filesystem.OpenFile(filename, flag, perm)
}

func (j *jailedFS) Stat(filename string) (os.FileInfo, error) {
	if err := jailPath(filename); err != nil {
		return nil, err
	}
	return j.Filesystem.Stat(filename)
}

func (j *jailedFS) Lstat(filename string) (os.FileInfo, error) {
	if err := jailPath(filename); err != nil {
		return nil, err
	}
	return j.Filesystem.Lstat(filename)
}

func (j *jailedFS) ReadDir(path string) ([]os.FileInfo, error) {
	if err := jailPath(path); err != nil {
		return nil, err
	}
	return j.Filesystem.ReadDir(path)
}

func (j *jailedFS) Create(filename string) (billy.File, error) {
	if err := jailPath(filename); err != nil {
		return nil, err
	}
	return j.Filesystem.Create(filename)
}

func (j *jailedFS) Remove(filename string) error {
	if err := jailPath(filename); err != nil {
		return err
	}
	return j.Filesystem.Remove(filename)
}

func (j *jailedFS) Rename(from, to string) error {
	if err := jailPath(from); err != nil {
		return err
	}
	if err := jailPath(to); err != nil {
		return err
	}
	return j.Filesystem.Rename(from, to)
}

func (j *jailedFS) MkdirAll(filename string, perm os.FileMode) error {
	if err := jailPath(filename); err != nil {
		return err
	}
	return j.Filesystem.MkdirAll(filename, perm)
}

func (j *jailedFS) Symlink(target, link string) error {
	if err := jailSymlinkTarget(link, target); err != nil {
		return err
	}
	if s, ok := j.Filesystem.(billy.Symlink); ok {
		return s.Symlink(target, link)
	}
	return billy.ErrNotSupported
}

func (j *jailedFS) Readlink(link string) (string, error) {
	if err := jailPath(link); err != nil {
		return "", err
	}
	if s, ok := j.Filesystem.(billy.Symlink); ok {
		return s.Readlink(link)
	}
	return "", billy.ErrNotSupported
}

func (j *jailedFS) Chroot(path string) (billy.Filesystem, error) {
	if err := jailPath(path); err != nil {
		return nil, err
	}
	root, err := j.resolve(path, true)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("chroot target %s is not a directory", root)
	}
	base := osfs.New(root, osfs.WithBoundOS())
	return &jailedFS{Filesystem: base, root: root}, nil
}

func (j *jailedFS) Root() string {
	return j.root
}

func (j *jailedFS) Chmod(name string, mode os.FileMode) error {
	full, err := j.resolve(name, true)
	if err != nil {
		return err
	}
	return os.Chmod(full, mode)
}

func (j *jailedFS) Lchown(name string, uid, gid int) error {
	full, err := j.resolve(name, false)
	if err != nil {
		return err
	}
	return os.Lchown(full, uid, gid)
}

func (j *jailedFS) Chown(name string, uid, gid int) error {
	full, err := j.resolve(name, true)
	if err != nil {
		return err
	}
	return os.Chown(full, uid, gid)
}

func (j *jailedFS) Chtimes(name string, atime, mtime time.Time) error {
	full, err := j.resolve(name, true)
	if err != nil {
		return err
	}
	return os.Chtimes(full, atime, mtime)
}
