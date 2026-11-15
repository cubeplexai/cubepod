package volume

import (
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func mapUnixErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EXDEV):
		return ErrInvalidPath
	case errors.Is(err, unix.ENOENT):
		return ErrNotExist
	case errors.Is(err, unix.ENOTDIR):
		return ErrNotDir
	case errors.Is(err, unix.EISDIR):
		return ErrIsDir
	default:
		return err
	}
}

// openDataRoot opens a volume's data directory without following symlinks.
func openDataRoot(dataDir string) (*os.File, error) {
	fd, err := unix.Open(dataDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, mapUnixErr(err)
	}
	return os.NewFile(uintptr(fd), dataDir), nil
}

// resolveDirFD walks rel components under rootFD using O_NOFOLLOW at every step.
// When createMissing is true, missing directories are created via mkdirat and
// chowned/chmodded on their open fd.
func resolveDirFD(rootFD int, rel string, createMissing bool, dirMode os.FileMode) (*os.File, error) {
	dupFD, err := unix.Dup(rootFD)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(dupFD)
	cur := os.NewFile(uintptr(dupFD), ".")
	if rel == "" || rel == "." {
		return cur, nil
	}
	parts := strings.Split(rel, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			_ = cur.Close()
			return nil, ErrInvalidPath
		}
		nextFD, err := unix.Openat(int(cur.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil && errors.Is(err, unix.ENOENT) && createMissing {
			mode := uint32(dirMode.Perm() & 0o777)
			if mode == 0 {
				mode = 0o755
			}
			mkErr := unix.Mkdirat(int(cur.Fd()), part, mode)
			if mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
				_ = cur.Close()
				return nil, mapUnixErr(mkErr)
			}
			nextFD, err = unix.Openat(int(cur.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err == nil && mkErr == nil {
				nf := os.NewFile(uintptr(nextFD), part)
				if chErr := ownFileFD(nf, os.FileMode(mode)); chErr != nil {
					_ = nf.Close()
					_ = cur.Close()
					return nil, chErr
				}
				_ = cur.Close()
				cur = nf
				continue
			}
		}
		_ = cur.Close()
		if err != nil {
			return nil, mapUnixErr(err)
		}
		cur = os.NewFile(uintptr(nextFD), part)
	}
	return cur, nil
}

// openFileBeneath opens rel under rootFD with RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS,
// falling back to component-by-component O_NOFOLLOW if openat2 returns ENOSYS.
func openFileBeneath(rootFD int, rel string, flags int, mode uint32) (*os.File, error) {
	if rel == "" || rel == "." {
		dupFD, err := unix.Dup(rootFD)
		if err != nil {
			return nil, err
		}
		unix.CloseOnExec(dupFD)
		return os.NewFile(uintptr(dupFD), "."), nil
	}
	how := &unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC),
		Mode:    uint64(mode & 0o777),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(rootFD, rel, how)
	if err == nil {
		return os.NewFile(uintptr(fd), rel), nil
	}
	if !errors.Is(err, unix.ENOSYS) {
		return nil, mapUnixErr(err)
	}
	parentRel, base := splitRel(rel)
	parent, err := resolveDirFD(rootFD, parentRel, false, 0)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err = unix.Openat(int(parent.Fd()), base, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode&0o777)
	if err != nil {
		return nil, mapUnixErr(err)
	}
	return os.NewFile(uintptr(fd), base), nil
}

func splitRel(rel string) (parentRel, base string) {
	idx := strings.LastIndex(rel, "/")
	if idx < 0 {
		return "", rel
	}
	return rel[:idx], rel[idx+1:]
}

// ownFileFD sets on-disk ownership to defaultDataUID:defaultDataGID (1000:1000)
// and applies mode masked to 0o777 via fchown/fchmod on the open fd.
// Chown is best-effort so non-root unit tests can still chmod.
func ownFileFD(f *os.File, mode os.FileMode) error {
	_ = f.Chown(defaultDataUID, defaultDataGID)
	return f.Chmod(mode & 0o777)
}

func entryFromStat(abs string, st unix.Stat_t) Entry {
	name := path.Base(abs)
	if abs == "/" {
		name = "/"
	}
	typ := "unknown"
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		typ = "symlink"
	case unix.S_IFDIR:
		typ = "directory"
	case unix.S_IFREG:
		typ = "file"
	}
	return Entry{
		Name:  name,
		Type:  typ,
		Path:  abs,
		Size:  st.Size,
		Mode:  int(st.Mode & 0o777),
		UID:   int(st.Uid),
		GID:   int(st.Gid),
		Atime: time.Unix(st.Atim.Sec, st.Atim.Nsec).UTC(),
		Mtime: time.Unix(st.Mtim.Sec, st.Mtim.Nsec).UTC(),
		Ctime: time.Unix(st.Ctim.Sec, st.Ctim.Nsec).UTC(),
	}
}

func applyAttrOverrides(ent *Entry, attrs map[string]fileAttr, abs string) {
	a, ok := attrs[abs]
	if !ok {
		return
	}
	if a.UID != nil {
		ent.UID = *a.UID
	}
	if a.GID != nil {
		ent.GID = *a.GID
	}
	if a.Mode != nil {
		ent.Mode = *a.Mode & 0o777
	}
}

func readlinkAtString(dirFD int, name string) (string, error) {
	buf := make([]byte, 4096)
	n, err := unix.Readlinkat(dirFD, name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

func statBeneath(rootFD int, abs, rel string, attrs map[string]fileAttr) (Entry, error) {
	if rel == "" {
		var st unix.Stat_t
		if err := unix.Fstat(rootFD, &st); err != nil {
			return Entry{}, mapUnixErr(err)
		}
		ent := entryFromStat("/", st)
		applyAttrOverrides(&ent, attrs, "/")
		return ent, nil
	}
	parentRel, base := splitRel(rel)
	parent, err := resolveDirFD(rootFD, parentRel, false, 0)
	if err != nil {
		return Entry{}, err
	}
	defer parent.Close()
	var st unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return Entry{}, mapUnixErr(err)
	}
	ent := entryFromStat(abs, st)
	if ent.Type == "symlink" {
		if t, err := readlinkAtString(int(parent.Fd()), base); err == nil {
			ent.Target = t
		}
	}
	applyAttrOverrides(&ent, attrs, abs)
	return ent, nil
}

func readDirNames(dir *os.File) ([]string, error) {
	dupFD, err := unix.Dup(int(dir.Fd()))
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(dupFD)
	f := os.NewFile(uintptr(dupFD), dir.Name())
	defer f.Close()
	if _, err := f.Seek(0, io.SeekStart); err != nil && !errors.Is(err, syscall.ESPIPE) {
		// Ignore seek errors on directories where offset is already 0.
	}
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	return names, nil
}

func walkDirFD(dir *os.File, abs string, depth int, attrs map[string]fileAttr, out *[]Entry) error {
	if depth < 1 {
		return nil
	}
	names, err := readDirNames(dir)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == "." || name == ".." {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return mapUnixErr(err)
		}
		childAbs := path.Join(abs, name)
		ent := entryFromStat(childAbs, st)
		if ent.Type == "symlink" {
			if t, err := readlinkAtString(int(dir.Fd()), name); err == nil {
				ent.Target = t
			}
		}
		applyAttrOverrides(&ent, attrs, childAbs)
		*out = append(*out, ent)
		if ent.Type == "directory" && depth > 1 {
			childFD, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return mapUnixErr(err)
			}
			childDir := os.NewFile(uintptr(childFD), name)
			walkErr := walkDirFD(childDir, childAbs, depth-1, attrs, out)
			_ = childDir.Close()
			if walkErr != nil {
				return walkErr
			}
		}
	}
	return nil
}

func removeDirContentsFD(dir *os.File) error {
	names, err := readDirNames(dir)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name == "." || name == ".." {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return mapUnixErr(err)
		}
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			subFD, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return mapUnixErr(err)
			}
			subDir := os.NewFile(uintptr(subFD), name)
			rmErr := removeDirContentsFD(subDir)
			_ = subDir.Close()
			if rmErr != nil {
				return rmErr
			}
			if err := unix.Unlinkat(int(dir.Fd()), name, unix.AT_REMOVEDIR); err != nil {
				return mapUnixErr(err)
			}
		} else {
			if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil {
				return mapUnixErr(err)
			}
		}
	}
	return nil
}

func removeBeneath(rootFD int, rel string) (isDir bool, err error) {
	parentRel, base := splitRel(rel)
	parent, err := resolveDirFD(rootFD, parentRel, false, 0)
	if err != nil {
		return false, err
	}
	defer parent.Close()
	var st unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, mapUnixErr(err)
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		subFD, err := unix.Openat(int(parent.Fd()), base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return false, mapUnixErr(err)
		}
		subDir := os.NewFile(uintptr(subFD), base)
		rmErr := removeDirContentsFD(subDir)
		_ = subDir.Close()
		if rmErr != nil {
			return true, rmErr
		}
		if err := unix.Unlinkat(int(parent.Fd()), base, unix.AT_REMOVEDIR); err != nil {
			return true, mapUnixErr(err)
		}
		return true, nil
	}
	if err := unix.Unlinkat(int(parent.Fd()), base, 0); err != nil {
		return false, mapUnixErr(err)
	}
	return false, nil
}
