package gateway

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Watch ring keeps at most watchRingCap events for GetWatcherEvents. Overflow does
// not return that truncated ring as success: the watcher is marked and both
// GetWatcherEvents and live WatchDir consumers fail with resource_exhausted.
// WatchDir subscribers use a bounded queue; a slow consumer is failed with
// resource_exhausted rather than a silently truncated success stream.
//
// RENAME is one event per MOVED_FROM and one per MOVED_TO (no old path in the proto).
// Recursive new-subdirectory race (Linux inotify, not closed): a directory created
// under a recursive watch is armed only after the create event is handled and
// inotify_add_watch returns. The kernel does not queue events for a watch that is
// not installed yet, so a file that appears before inotify_add_watch returns is
// missed. A retry loop still races. This code does not scan the new directory to
// invent those events.
// Kernel IN_Q_OVERFLOW fails the watcher with resource_exhausted. It is not ignored.
// Symlink directories are not recursed into.
// Network/FUSE detection uses statfs magic (nfs, cifs, smb, fuse, 9p, ceph).
// Unknown types are treated as local. proc, sysfs, and devpts are always refused,
// as are user paths under /proc, /dev, and /sys.
const (
	watchRingCap           = 4096
	watchSubQueue          = 64
	watchKeepaliveInterval = 15 * time.Second

	watchBufferOverflowMsg = "watch event buffer overflow: events were dropped"
	watchQueueOverflowMsg  = "watch subscriber queue overflow"

	procSuperMagic  = 0x9fa0
	sysfsMagic      = 0x62656572
	devptsMagic     = 0x1cd1
	nfsSuperMagic   = 0x6969
	cifsMagic       = 0xFF534D42
	smbSuperMagic   = 0x517B
	smb2Magic       = 0xFE534D42
	fuseSuperMagic  = 0x65735546
	ninepMagic      = 0x01021997
	cephMagic       = 0x00c36400
	inotifyEvtBytes = 16
)

const watchMask = unix.IN_CREATE | unix.IN_MODIFY | unix.IN_CLOSE_WRITE |
	unix.IN_DELETE | unix.IN_DELETE_SELF | unix.IN_MOVED_FROM | unix.IN_MOVED_TO |
	unix.IN_ATTRIB | unix.IN_ONLYDIR

var watcherSeq atomic.Uint64

type fsEvent struct {
	Name  string
	Type  string
	Entry map[string]interface{}
}

type watchSub struct {
	ch     chan fsEvent
	failed chan struct{}
	once   sync.Once
	reason atomic.Value
}

func (s *watchSub) failWith(reason string) {
	s.once.Do(func() {
		if reason != "" {
			s.reason.Store(reason)
		}
		close(s.failed)
	})
}

func (s *watchSub) fail() {
	s.failWith(watchQueueOverflowMsg)
}

func (s *watchSub) overflowReason() string {
	if v := s.reason.Load(); v != nil {
		if msg, ok := v.(string); ok && msg != "" {
			return msg
		}
	}
	return watchQueueOverflowMsg
}

func (s *watchSub) overflowed() bool {
	select {
	case <-s.failed:
		return true
	default:
		return false
	}
}

type dirWatcher struct {
	id           string
	sandboxID    string
	root         string
	recursive    bool
	includeEntry bool
	poll         bool
	fd           int
	stopFD       int
	mu           sync.Mutex
	watches      map[int]string
	pending      []fsEvent
	subs         map[*watchSub]struct{}
	stopped      bool
	overflowed   atomic.Bool
	stopOnce     sync.Once
	done         chan struct{}
}

type connectStatusError struct {
	code string
	msg  string
}

func (e *connectStatusError) Error() string { return e.msg }

func statusErr(code, msg string) error {
	return &connectStatusError{code: code, msg: msg}
}

func (w *dirWatcher) stop() {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.stopped = true
		stopFD := w.stopFD
		// Drop subscribers without closing their failed channel. Cleanup is not
		// an overflow; the WatchDir handler returns on done.
		w.subs = map[*watchSub]struct{}{}
		w.mu.Unlock()
		// Wake loop() via eventfd; loop() owns and closes w.fd and w.stopFD.
		if stopFD >= 0 {
			var one [8]byte
			binary.NativeEndian.PutUint64(one[:], 1)
			_, _ = unix.Write(stopFD, one[:])
		}
		close(w.done)
	})
}

func (w *dirWatcher) failOverflowLocked(msg string) {
	w.overflowed.Store(true)
	for sub := range w.subs {
		delete(w.subs, sub)
		sub.failWith(msg)
	}
}

func (w *dirWatcher) noteOverflow(msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.failOverflowLocked(msg)
}

func (w *dirWatcher) publish(ev fsEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.overflowed.Load() {
		return
	}
	if w.poll {
		if len(w.pending) >= watchRingCap {
			// Bound memory. The dropped event is not returned as a successful poll.
			w.pending = w.pending[1:]
			w.failOverflowLocked(watchBufferOverflowMsg)
			return
		}
		w.pending = append(w.pending, ev)
	}
	var drop []*watchSub
	for sub := range w.subs {
		select {
		case sub.ch <- ev:
		default:
			drop = append(drop, sub)
		}
	}
	for _, sub := range drop {
		delete(w.subs, sub)
		sub.fail()
	}
}

func (w *dirWatcher) drain() ([]fsEvent, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.overflowed.Load() {
		return nil, statusErr("resource_exhausted", watchBufferOverflowMsg)
	}
	evs := w.pending
	w.pending = nil
	return evs, nil
}

func (w *dirWatcher) subscribe() *watchSub {
	sub := &watchSub{
		ch:     make(chan fsEvent, watchSubQueue),
		failed: make(chan struct{}),
	}
	w.mu.Lock()
	if w.subs == nil {
		w.subs = make(map[*watchSub]struct{})
	}
	w.subs[sub] = struct{}{}
	w.mu.Unlock()
	return sub
}

func (w *dirWatcher) removeSub(sub *watchSub) {
	w.mu.Lock()
	delete(w.subs, sub)
	w.mu.Unlock()
}

func (w *dirWatcher) addWatch(full, rel string) error {
	w.mu.Lock()
	fd := w.fd
	stopped := w.stopped
	w.mu.Unlock()
	if stopped || fd < 0 {
		return fmt.Errorf("watcher stopped")
	}
	wd, err := unix.InotifyAddWatch(fd, full, uint32(watchMask))
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.watches[wd] = rel
	w.mu.Unlock()
	return nil
}

func (w *dirWatcher) arm() error {
	if err := w.addWatch(w.root, ""); err != nil {
		return err
	}
	if !w.recursive {
		return nil
	}
	return w.walk(w.root, "")
}

func (w *dirWatcher) walk(dir, rel string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		childRel := joinRel(rel, e.Name())
		full := filepath.Join(dir, e.Name())
		if err := w.addWatch(full, childRel); err != nil {
			return err
		}
		if err := w.walk(full, childRel); err != nil {
			return err
		}
	}
	return nil
}

func (w *dirWatcher) loop() {
	w.mu.Lock()
	fd := w.fd
	stopFD := w.stopFD
	w.mu.Unlock()
	defer func() {
		// Reader goroutine owns and closes fd and stopFD after the loop exits.
		w.mu.Lock()
		if w.fd >= 0 {
			_ = unix.Close(w.fd)
			w.fd = -1
		}
		if w.stopFD >= 0 {
			_ = unix.Close(w.stopFD)
			w.stopFD = -1
		}
		w.mu.Unlock()
	}()
	buf := make([]byte, 4096)
	for {
		w.mu.Lock()
		stopped := w.stopped
		w.mu.Unlock()
		if stopped || fd < 0 {
			return
		}
		pollfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if stopFD >= 0 {
			pollfd = append(pollfd, unix.PollFd{Fd: int32(stopFD), Events: unix.POLLIN})
		}
		n, err := unix.Poll(pollfd, 200)
		w.mu.Lock()
		stopped = w.stopped
		w.mu.Unlock()
		if stopped || (len(pollfd) > 1 && pollfd[1].Revents&unix.POLLIN != 0) {
			return
		}
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}
		if n == 0 {
			continue
		}
		rn, err := unix.Read(fd, buf)
		if rn > 0 {
			w.parse(buf[:rn])
			continue
		}
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		return
	}
}

func (w *dirWatcher) parse(buf []byte) {
	off := 0
	for off+inotifyEvtBytes <= len(buf) {
		wd := int(int32(binary.NativeEndian.Uint32(buf[off : off+4])))
		mask := binary.NativeEndian.Uint32(buf[off+4 : off+8])
		nlen := int(binary.NativeEndian.Uint32(buf[off+12 : off+16]))
		nameStart := off + inotifyEvtBytes
		nameEnd := nameStart + nlen
		if nameEnd > len(buf) {
			return
		}
		name := string(trimNul(buf[nameStart:nameEnd]))
		off = nameEnd
		w.onRaw(wd, mask, name)
	}
}

func trimNul(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

func (w *dirWatcher) onRaw(wd int, mask uint32, name string) {
	if mask&unix.IN_Q_OVERFLOW != 0 {
		// Events were dropped in the kernel queue. Do not skip the flag.
		w.noteOverflow(watchBufferOverflowMsg)
		return
	}
	if w.overflowed.Load() {
		return
	}
	if mask&unix.IN_IGNORED != 0 {
		w.mu.Lock()
		delete(w.watches, wd)
		w.mu.Unlock()
		return
	}
	w.mu.Lock()
	dirRel := w.watches[wd]
	w.mu.Unlock()
	if w.recursive && mask&unix.IN_CREATE != 0 && mask&unix.IN_ISDIR != 0 && name != "" {
		rel := joinRel(dirRel, name)
		full := filepath.Join(w.root, filepath.FromSlash(rel))
		// Linux inotify limitation, not closed: the new directory is armed here,
		// after the create is observed. Anything created in it before
		// inotify_add_watch returns is not queued and is not synthesized.
		// A retry still races.
		if fi, err := os.Lstat(full); err == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
			_ = w.addWatch(full, rel)
		}
	}
	kind, ok := mapInotify(mask)
	if !ok || name == "" {
		return
	}
	rel := joinRel(dirRel, name)
	var entry map[string]interface{}
	if w.includeEntry && kind != "EVENT_TYPE_REMOVE" {
		entry = watchEntryInfo(w.root, rel)
	}
	w.publish(fsEvent{Name: rel, Type: kind, Entry: entry})
}

func mapInotify(mask uint32) (string, bool) {
	switch {
	case mask&unix.IN_CREATE != 0:
		return "EVENT_TYPE_CREATE", true
	case mask&(unix.IN_DELETE|unix.IN_DELETE_SELF) != 0:
		return "EVENT_TYPE_REMOVE", true
	case mask&(unix.IN_MOVED_FROM|unix.IN_MOVED_TO) != 0:
		return "EVENT_TYPE_RENAME", true
	case mask&unix.IN_ATTRIB != 0:
		return "EVENT_TYPE_CHMOD", true
	case mask&(unix.IN_MODIFY|unix.IN_CLOSE_WRITE) != 0:
		return "EVENT_TYPE_WRITE", true
	default:
		return "", false
	}
}

func joinRel(dir, name string) string {
	if name == "" {
		return dir
	}
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func watchEntryInfo(root, rel string) map[string]interface{} {
	if rel == "" {
		return nil
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Lstat(full)
	if err != nil {
		return nil
	}
	typ := "FILE_TYPE_FILE"
	if fi.IsDir() {
		typ = "FILE_TYPE_DIRECTORY"
	}
	return map[string]interface{}{
		"name":        filepath.Base(rel),
		"type":        typ,
		"path":        rel,
		"size":        fi.Size(),
		"mode":        uint32(fi.Mode().Perm()),
		"permissions": fi.Mode().String(),
		"isDir":       fi.IsDir(),
	}
}

func (s *Server) resolveWatchPath(rec *SandboxRecord, userPath string) (string, error) {
	userPath = strings.TrimSpace(userPath)
	if userPath == "" {
		return "", fmt.Errorf("path is required")
	}
	logical := path.Clean(userPath)
	if isSpecialAbs(logical) || isSpecialAbs(userPath) {
		return "", fmt.Errorf("refusing to watch proc, dev, or sys")
	}
	base := rec.WorkspaceDir
	if base == "" {
		base = s.workspaceDirForSandbox(rec.SandboxID, rec.VolumeKey)
		_ = os.MkdirAll(base, 0755)
		rec.WorkspaceDir = base
	}
	base = filepath.Clean(base)
	var joined string
	switch {
	case logical == "/workspace":
		joined = base
	case strings.HasPrefix(logical, "/workspace/"):
		joined = filepath.Join(base, filepath.FromSlash(strings.TrimPrefix(logical, "/workspace/")))
	case strings.HasPrefix(logical, "/"):
		joined = filepath.FromSlash(logical)
	default:
		joined = filepath.Join(base, filepath.FromSlash(logical))
	}
	joined = filepath.Clean(joined)
	if joined != base && !strings.HasPrefix(joined, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes sandbox work tree")
	}
	if isSpecialAbs(filepath.ToSlash(joined)) {
		return "", fmt.Errorf("refusing to watch proc, dev, or sys")
	}
	return joined, nil
}

func isSpecialAbs(p string) bool {
	p = path.Clean(p)
	return p == "/proc" || strings.HasPrefix(p, "/proc/") ||
		p == "/dev" || strings.HasPrefix(p, "/dev/") ||
		p == "/sys" || strings.HasPrefix(p, "/sys/")
}

func classifyMount(full string) (network, forbidden bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(full, &st); err != nil {
		return false, false
	}
	magic := uint64(st.Type)
	switch magic {
	case procSuperMagic, sysfsMagic, devptsMagic:
		return false, true
	case nfsSuperMagic, cifsMagic, smbSuperMagic, smb2Magic, fuseSuperMagic, ninepMagic, cephMagic:
		return true, false
	default:
		return false, false
	}
}

type watchCreateReq struct {
	Path                    string `json:"path"`
	Recursive               bool   `json:"recursive"`
	IncludeEntry            bool   `json:"includeEntry"`
	IncludeEntrySnake       bool   `json:"include_entry"`
	AllowNetworkMounts      bool   `json:"allowNetworkMounts"`
	AllowNetworkMountsSnake bool   `json:"allow_network_mounts"`
	WatcherID               string `json:"watcherId"`
	WatcherIDSnake          string `json:"watcher_id"`
}

func (r watchCreateReq) includeEntry() bool { return r.IncludeEntry || r.IncludeEntrySnake }
func (r watchCreateReq) allowNet() bool {
	return r.AllowNetworkMounts || r.AllowNetworkMountsSnake
}
func (r watchCreateReq) watcherID() string {
	if r.WatcherID != "" {
		return r.WatcherID
	}
	return r.WatcherIDSnake
}

func (s *Server) startDirWatcher(rec *SandboxRecord, req watchCreateReq, poll bool) (*dirWatcher, error) {
	full, err := s.resolveWatchPath(rec, req.Path)
	if err != nil {
		return nil, statusErr("invalid_argument", err.Error())
	}
	fi, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, statusErr("not_found", "directory not found")
		}
		return nil, statusErr("invalid_argument", err.Error())
	}
	if !fi.IsDir() {
		return nil, statusErr("invalid_argument", "path is not a directory")
	}
	if network, forbidden := classifyMount(full); forbidden {
		return nil, statusErr("invalid_argument", "refusing to watch proc, sysfs, or devpts")
	} else if network && !req.allowNet() {
		return nil, statusErr("invalid_argument", "network or fuse mount requires allowNetworkMounts")
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, statusErr("internal", "inotify_init1: "+err.Error())
	}
	stopFD, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		_ = unix.Close(fd)
		return nil, statusErr("internal", "eventfd: "+err.Error())
	}
	w := &dirWatcher{
		id:           fmt.Sprintf("w%d", watcherSeq.Add(1)),
		sandboxID:    rec.SandboxID,
		root:         full,
		recursive:    req.Recursive,
		includeEntry: req.includeEntry(),
		poll:         poll,
		fd:           fd,
		stopFD:       stopFD,
		watches:      make(map[int]string),
		subs:         make(map[*watchSub]struct{}),
		done:         make(chan struct{}),
	}
	if err := w.arm(); err != nil {
		w.stop()
		_ = unix.Close(fd)
		_ = unix.Close(stopFD)
		w.fd = -1
		w.stopFD = -1
		return nil, statusErr("internal", "inotify_add_watch: "+err.Error())
	}
	s.mu.Lock()
	if s.watchers == nil {
		s.watchers = make(map[string]*dirWatcher)
	}
	s.watchers[w.id] = w
	s.mu.Unlock()
	go w.loop()
	return w, nil
}

func (s *Server) watcherByID(sandboxID, id string) *dirWatcher {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w := s.watchers[id]
	if w == nil || (sandboxID != "" && w.sandboxID != "" && w.sandboxID != sandboxID) {
		return nil
	}
	return w
}

func (s *Server) removeWatcher(id string) bool {
	s.mu.Lock()
	w := s.watchers[id]
	delete(s.watchers, id)
	s.mu.Unlock()
	if w == nil {
		return false
	}
	w.stop()
	return true
}

func (s *Server) writeWatchStatus(w http.ResponseWriter, err error) {
	st, ok := err.(*connectStatusError)
	if !ok {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.writeConnectError(w, connectStatusHTTP(st.code), st.code, st.msg)
}

func (s *Server) writeWatchStreamStatus(w http.ResponseWriter, err error) {
	st, ok := err.(*connectStatusError)
	if !ok {
		s.writeConnectStreamError(w, "internal", err.Error())
		return
	}
	s.writeConnectStreamError(w, st.code, st.msg)
}

func decodeWatchReq(r *http.Request) (watchCreateReq, error) {
	body, _, err := unpackConnectRPCBody(r)
	if err != nil {
		return watchCreateReq{}, err
	}
	var req watchCreateReq
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return watchCreateReq{}, err
		}
	}
	return req, nil
}

func (s *Server) handleConnectRPCCreateWatcher(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()
	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}
	req, err := decodeWatchReq(r)
	if err != nil {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	watcher, err := s.startDirWatcher(rec, req, true)
	if err != nil {
		s.writeWatchStatus(w, err)
		return
	}
	s.writeUnaryResponse(w, r, map[string]string{"watcherId": watcher.id})
}

func (s *Server) handleConnectRPCGetWatcherEvents(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()
	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}
	req, err := decodeWatchReq(r)
	if err != nil {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	id := req.watcherID()
	if id == "" {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", "watcherId is required")
		return
	}
	watcher := s.watcherByID(rec.SandboxID, id)
	if watcher == nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", "watcher not found")
		return
	}
	events, err := watcher.drain()
	if err != nil {
		s.writeWatchStatus(w, err)
		return
	}
	out := make([]map[string]interface{}, 0, len(events))
	for _, ev := range events {
		item := map[string]interface{}{
			"name": ev.Name,
			"type": ev.Type,
		}
		if ev.Entry != nil {
			item["entry"] = ev.Entry
		}
		out = append(out, item)
	}
	s.writeUnaryResponse(w, r, map[string]interface{}{"events": out})
}

func (s *Server) handleConnectRPCRemoveWatcher(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectError(w, http.StatusInternalServerError, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()
	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}
	req, err := decodeWatchReq(r)
	if err != nil {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	id := req.watcherID()
	if id == "" {
		s.writeConnectError(w, http.StatusBadRequest, "invalid_argument", "watcherId is required")
		return
	}
	watcher := s.watcherByID(rec.SandboxID, id)
	if watcher == nil || !s.removeWatcher(id) {
		s.writeConnectError(w, http.StatusNotFound, "not_found", "watcher not found")
		return
	}
	s.writeUnaryResponse(w, r, map[string]interface{}{})
}

func (s *Server) handleConnectRPCWatchDir(w http.ResponseWriter, r *http.Request) {
	rec, err := s.resolveConnectRPCSandbox(r)
	if err != nil {
		s.writeConnectStreamError(w, "not_found", err.Error())
		return
	}
	if err := s.ensureRunning(r.Context(), rec); err != nil {
		s.writeConnectStreamError(w, "internal", "failed to resume sandbox: "+err.Error())
		return
	}
	s.mu.Lock()
	s.recordActivity(rec)
	s.mu.Unlock()
	if poddURL, forward := s.shouldForwardConnectRPC(rec); forward {
		s.forwardConnectRPCToPodd(w, r, poddURL)
		return
	}
	req, err := decodeWatchReq(r)
	if err != nil {
		s.writeConnectStreamError(w, "invalid_argument", err.Error())
		return
	}
	watcher, err := s.startDirWatcher(rec, req, false)
	if err != nil {
		s.writeWatchStreamStatus(w, err)
		return
	}
	defer s.removeWatcher(watcher.id)
	sub := watcher.subscribe()
	defer watcher.removeSub(sub)

	w.Header().Set("Content-Type", "application/connect+json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	var writeMu sync.Mutex
	start, _ := json.Marshal(map[string]interface{}{
		"start": map[string]interface{}{},
	})
	if s.writeConnectFrameSafe(w, &writeMu, start, false, flusher) != nil {
		return
	}
	ticker := time.NewTicker(watchKeepaliveInterval)
	defer ticker.Stop()
	// timeoutMs is a client stream deadline. Disconnect stops the watch.
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.failed:
			s.writeStreamError(w, &writeMu, flusher, "resource_exhausted", sub.overflowReason())
			return
		case <-watcher.done:
			// Server-side stop (RemoveWatcher, sandbox delete): emit clean Connect end-stream frame.
			_ = s.writeConnectFrameSafe(w, &writeMu, []byte("{}"), true, flusher)
			return
		case <-ticker.C:
			if s.writeConnectFrameSafe(w, &writeMu, []byte(`{"keepalive":{}}`), false, flusher) != nil {
				return
			}
		case ev := <-sub.ch:
			item := map[string]interface{}{
				"name": ev.Name,
				"type": ev.Type,
			}
			if ev.Entry != nil {
				item["entry"] = ev.Entry
			}
			payload, _ := json.Marshal(map[string]interface{}{
				"filesystem": item,
			})
			if s.writeConnectFrameSafe(w, &writeMu, payload, false, flusher) != nil {
				return
			}
			ticker.Reset(watchKeepaliveInterval)
		}
	}
}
