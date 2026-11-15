package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// codeFromHTTPStatusFallback matches a Connect client that could not parse an
// error body: 400 becomes internal, 404 becomes unimplemented, 429 becomes unavailable.
func codeFromHTTPStatusFallback(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "internal"
	case http.StatusNotFound:
		return "unimplemented"
	case http.StatusTooManyRequests:
		return "unavailable"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "unavailable"
	default:
		return "unknown"
	}
}

// parsedUnaryCode uses the JSON code when the body is Connect unary JSON.
// Otherwise it falls back the way a client does when the body is not parsed.
func parsedUnaryCode(status int, contentType string, body []byte) string {
	ct := strings.ToLower(contentType)
	if strings.HasPrefix(ct, "application/json") && !strings.Contains(ct, "connect+") {
		var payload struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(body, &payload) == nil && payload.Code != "" {
			return payload.Code
		}
	}
	return codeFromHTTPStatusFallback(status)
}

// parsedStreamCode reads a Connect end-stream error. A non-200 stream is reported
// as "HTTP <status>" because the official stream client does not parse that body.
func parsedStreamCode(status int, contentType string, body []byte) string {
	if status == http.StatusOK && strings.Contains(strings.ToLower(contentType), "application/connect+json") {
		frames, err := parseConnectFrames(body)
		if err == nil {
			for i := len(frames) - 1; i >= 0; i-- {
				if frames[i].Flag&0x02 == 0 {
					continue
				}
				var payload struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if json.Unmarshal(frames[i].Payload, &payload) == nil && payload.Error.Code != "" {
					return payload.Error.Code
				}
			}
		}
	}
	return fmt.Sprintf("HTTP %d", status)
}

func TestPTYReplayIsBoundedAndSlowSubscriberIsFailed(t *testing.T) {
	buf := &bytes.Buffer{}
	appendReplay(buf, bytes.Repeat([]byte("A"), ptyReplayCap))
	appendReplay(buf, []byte("XYZ"))
	if buf.Len() != ptyReplayCap {
		t.Fatalf("replay len %d, cap %d", buf.Len(), ptyReplayCap)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("XYZ")) {
		t.Fatal("replay must keep the newest bytes")
	}
	if buf.Bytes()[0] != 'A' {
		t.Fatal("replay must drop only the oldest bytes")
	}
	appendReplay(nil, []byte("no panic"))

	entry := &commandEntry{
		running:     true,
		isPTY:       true,
		buf:         &bytes.Buffer{},
		subscribers: map[*ptySubscriber]struct{}{},
		done:        make(chan struct{}),
	}
	_, _, _, slow := entry.snapshotAndSubscribe()
	_, _, _, fast := entry.snapshotAndSubscribe()
	if slow == nil || fast == nil {
		t.Fatal("running pty must subscribe")
	}
	chunk := []byte("x")
	for i := 0; i < ptySubscriberQueue; i++ {
		entry.broadcastPTY(chunk)
		select {
		case <-fast.ch:
		default:
			t.Fatalf("fast subscriber missed chunk %d", i)
		}
	}
	if slow.overflowed() || fast.overflowed() {
		t.Fatal("a full queue is not yet overflow")
	}
	entry.broadcastPTY(chunk)
	if !slow.overflowed() {
		t.Fatal("slow pty subscriber must be failed")
	}
	if fast.overflowed() {
		t.Fatal("a subscriber that keeps up must stay attached")
	}
	entry.mu.Lock()
	_, slowPresent := entry.subscribers[slow]
	_, fastPresent := entry.subscribers[fast]
	entry.mu.Unlock()
	if slowPresent || !fastPresent {
		t.Fatalf("slow present %v fast present %v", slowPresent, fastPresent)
	}
	select {
	case got := <-fast.ch:
		if !bytes.Equal(got, chunk) {
			t.Fatalf("fast tail %q", got)
		}
	default:
		t.Fatal("fast subscriber did not receive the last chunk")
	}
}

func TestWatchQueueOverflowDoesNotTaintWatcher(t *testing.T) {
	w := &dirWatcher{
		poll:    true,
		fd:      -1,
		stopFD:  -1,
		watches: map[int]string{},
		subs:    map[*watchSub]struct{}{},
		done:    make(chan struct{}),
	}
	slow := w.subscribe()
	fast := w.subscribe()
	for i := 0; i < watchSubQueue; i++ {
		w.publish(fsEvent{Name: fmt.Sprintf("n%d", i), Type: "EVENT_TYPE_WRITE"})
		select {
		case <-fast.ch:
		default:
			t.Fatalf("fast subscriber missed event %d", i)
		}
	}
	w.publish(fsEvent{Name: "overflow", Type: "EVENT_TYPE_WRITE"})
	if !slow.overflowed() || slow.overflowReason() != watchQueueOverflowMsg {
		t.Fatalf("slow reason %q overflowed %v", slow.overflowReason(), slow.overflowed())
	}
	if fast.overflowed() {
		t.Fatal("fast subscriber was failed")
	}
	if w.overflowed.Load() {
		t.Fatal("one slow consumer must not taint the watcher")
	}
	evs, err := w.drain()
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(evs) != watchSubQueue+1 {
		t.Fatalf("ring len %d", len(evs))
	}
}

func TestWatchRingAndKernelOverflowFailDrain(t *testing.T) {
	w := &dirWatcher{
		poll:    true,
		fd:      -1,
		stopFD:  -1,
		watches: map[int]string{},
		subs:    map[*watchSub]struct{}{},
		done:    make(chan struct{}),
	}
	for i := 0; i < watchRingCap; i++ {
		w.publish(fsEvent{Name: fmt.Sprintf("f%d", i), Type: "EVENT_TYPE_WRITE"})
	}
	if w.overflowed.Load() {
		t.Fatal("a full ring is still a complete view")
	}
	evs, err := w.drain()
	if err != nil || len(evs) != watchRingCap || evs[0].Name != "f0" {
		t.Fatalf("drain full ring: n=%d err=%v", len(evs), err)
	}

	for i := 0; i < watchRingCap; i++ {
		w.publish(fsEvent{Name: fmt.Sprintf("g%d", i), Type: "EVENT_TYPE_WRITE"})
	}
	w.publish(fsEvent{Name: "dropped", Type: "EVENT_TYPE_WRITE"})
	if !w.overflowed.Load() {
		t.Fatal("the event past the cap must taint the watcher")
	}
	if _, err := w.drain(); err == nil {
		t.Fatal("GetWatcherEvents must not return a truncated ring")
	} else if st, ok := err.(*connectStatusError); !ok || st.code != "resource_exhausted" || !strings.Contains(st.msg, "events were dropped") {
		t.Fatalf("drain error %#v", err)
	}

	kernel := &dirWatcher{
		poll:    true,
		fd:      -1,
		stopFD:  -1,
		watches: map[int]string{},
		subs:    map[*watchSub]struct{}{},
		done:    make(chan struct{}),
	}
	ksub := kernel.subscribe()
	kernel.onRaw(0, unix.IN_Q_OVERFLOW, "")
	if !kernel.overflowed.Load() || !ksub.overflowed() || ksub.overflowReason() != watchBufferOverflowMsg {
		t.Fatalf("kernel overflow watcher %v reason %q", kernel.overflowed.Load(), ksub.overflowReason())
	}
	if _, err := kernel.drain(); err == nil {
		t.Fatal("kernel overflow must fail the poll")
	}
}

func TestWatchStreamDoesNotOverflowPastRingCap(t *testing.T) {
	w := &dirWatcher{
		poll:    false,
		fd:      -1,
		stopFD:  -1,
		watches: map[int]string{},
		subs:    map[*watchSub]struct{}{},
		done:    make(chan struct{}),
	}
	sub := w.subscribe()
	total := watchRingCap + 100
	for i := 0; i < total; i++ {
		w.publish(fsEvent{Name: fmt.Sprintf("s%d", i), Type: "EVENT_TYPE_WRITE"})
		select {
		case ev := <-sub.ch:
			if ev.Name != fmt.Sprintf("s%d", i) {
				t.Fatalf("unexpected event %q", ev.Name)
			}
		default:
			t.Fatalf("stream subscriber missed event %d", i)
		}
	}
	if w.overflowed.Load() || sub.overflowed() {
		t.Fatal("stream watcher must not overflow past poll ring cap")
	}
}

func TestRecursiveArmDoesNotReplayFilesCreatedBeforeAddWatch(t *testing.T) {
	// Linux inotify limitation, recorded here and not closed: the new directory
	// is armed when the create event is handled. A file that already exists in
	// that directory is not delivered and is not invented by a scan.
	dir := t.TempDir()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	w := &dirWatcher{
		root:      dir,
		recursive: true,
		poll:      true,
		fd:        fd,
		stopFD:    -1,
		watches:   map[int]string{},
		subs:      map[*watchSub]struct{}{},
		done:      make(chan struct{}),
	}
	t.Cleanup(func() {
		w.stop()
		_ = unix.Close(fd)
	})
	if err := w.addWatch(dir, ""); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(dir, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "early.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	var rootWd int
	for wd := range w.watches {
		rootWd = wd
	}
	w.mu.Unlock()
	w.onRaw(rootWd, unix.IN_CREATE|unix.IN_ISDIR, "child")
	evs, err := w.drain()
	if err != nil {
		t.Fatal(err)
	}
	sawChild := false
	for _, ev := range evs {
		if ev.Name == "child/early.txt" {
			t.Fatalf("files created before addWatch are not synthesized: %+v", evs)
		}
		if ev.Name == "child" {
			sawChild = true
		}
	}
	if !sawChild {
		t.Fatalf("directory create must be reported: %+v", evs)
	}
	w.mu.Lock()
	armed := false
	for _, rel := range w.watches {
		if rel == "child" {
			armed = true
		}
	}
	w.mu.Unlock()
	if !armed {
		t.Fatal("the new directory is armed for later events only")
	}
}

func TestLocalWatchDirStreamErrorsAndGetEventsOverflow(t *testing.T) {
	srv, ts, rec := setupTestServerWithSandbox(t)

	post := func(ctx context.Context, path string, payload []byte) (int, string, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+path, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return resp.StatusCode, resp.Header.Get("Content-Type"), body
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, ct, body := post(ctx, "/filesystem.Filesystem/WatchDir", []byte(`{"path":"/workspace/missing-dir"}`))
	if status != http.StatusOK {
		t.Fatalf("missing dir status %d body %s", status, body)
	}
	code := parsedStreamCode(status, ct, body)
	if code != "not_found" || code == "unimplemented" {
		t.Fatalf("missing dir stream code %q body %q", code, body)
	}
	if !strings.Contains(string(body), "directory not found") {
		t.Fatalf("missing dir body %s", body)
	}
	frames, err := parseConnectFrames(body)
	if err != nil || len(frames) != 1 || frames[0].Flag != 2 {
		t.Fatalf("missing dir frames %+v err %v", frames, err)
	}

	if err := os.WriteFile(filepath.Join(rec.WorkspaceDir, "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, ct, body = post(ctx, "/filesystem.Filesystem/WatchDir", []byte(`{"path":"/workspace/plain.txt"}`))
	code = parsedStreamCode(status, ct, body)
	if status != http.StatusOK || code != "invalid_argument" || code == "internal" {
		t.Fatalf("file watch status %d code %q body %s", status, code, body)
	}
	if !strings.Contains(string(body), "path is not a directory") {
		t.Fatalf("file watch body %s", body)
	}

	watchCtx, watchCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer watchCancel()
	watchReq, err := http.NewRequestWithContext(watchCtx, http.MethodPost, ts.URL+"/filesystem.Filesystem/WatchDir", bytes.NewReader([]byte(`{"path":"/workspace"}`)))
	if err != nil {
		t.Fatal(err)
	}
	watchReq.Header.Set("Content-Type", "application/json")
	watchReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	stream, err := http.DefaultClient.Do(watchReq)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(stream.Body)
		t.Fatalf("watch dir %d %s", stream.StatusCode, b)
	}
	start, err := readNextConnectFrame(stream.Body)
	if err != nil {
		t.Fatal(err)
	}
	if start.Flag != 0 || !bytes.Contains(start.Payload, []byte("start")) {
		t.Fatalf("start frame flag %d payload %s", start.Flag, start.Payload)
	}
	srv.mu.RLock()
	var watcher *dirWatcher
	for _, w := range srv.watchers {
		if w.sandboxID == rec.SandboxID {
			watcher = w
			break
		}
	}
	srv.mu.RUnlock()
	if watcher == nil {
		t.Fatal("watcher was not registered")
	}
	watcher.mu.Lock()
	var sub *watchSub
	for s := range watcher.subs {
		sub = s
		break
	}
	watcher.mu.Unlock()
	if sub == nil {
		t.Fatal("watch subscriber missing")
	}
	sub.failWith(watchBufferOverflowMsg)
	var end *parsedConnectFrame
	for i := 0; i < 8; i++ {
		frame, err := readNextConnectFrame(stream.Body)
		if err != nil {
			t.Fatal(err)
		}
		if frame.Flag&0x02 != 0 {
			end = frame
			break
		}
	}
	if end == nil {
		t.Fatal("watch overflow did not end the stream")
	}
	var endPayload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(end.Payload, &endPayload); err != nil {
		t.Fatal(err)
	}
	if endPayload.Error.Code != "resource_exhausted" || !strings.Contains(endPayload.Error.Message, "events were dropped") {
		t.Fatalf("overflow end %+v", endPayload.Error)
	}
	if endPayload.Error.Code == "unavailable" || endPayload.Error.Code == "internal" {
		t.Fatalf("overflow classified as %s", endPayload.Error.Code)
	}

	status, ct, body = post(ctx, "/filesystem.Filesystem/CreateWatcher", []byte(`{"path":"/workspace"}`))
	if status != http.StatusOK {
		t.Fatalf("create watcher %d %s", status, body)
	}
	var created struct {
		WatcherID string `json:"watcherId"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.WatcherID == "" {
		t.Fatalf("watcher id: %v body %s", err, body)
	}
	srv.mu.RLock()
	poller := srv.watchers[created.WatcherID]
	srv.mu.RUnlock()
	if poller == nil {
		t.Fatal("poll watcher missing")
	}
	for i := 0; i <= watchRingCap; i++ {
		poller.publish(fsEvent{Name: fmt.Sprintf("e%d", i), Type: "EVENT_TYPE_WRITE"})
	}
	if !poller.overflowed.Load() {
		t.Fatal("poll watcher was not tainted")
	}
	status, ct, body = post(ctx, "/filesystem.Filesystem/GetWatcherEvents", []byte(`{"watcherId":"`+created.WatcherID+`"}`))
	code = parsedUnaryCode(status, ct, body)
	if status != http.StatusTooManyRequests || code != "resource_exhausted" || code == "unavailable" {
		t.Fatalf("get events status %d code %q body %s", status, code, body)
	}
	if len(body) == 0 || body[0] != '{' || !strings.Contains(string(body), "events were dropped") {
		t.Fatalf("get events body %s", body)
	}
}

func TestLocalSandboxDeleteCleansPTYAndWatch(t *testing.T) {
	srv, ts, rec := setupTestServerWithSandbox(t)
	payload, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "/bin/sleep",
			"args": []string{"30"},
		},
		"pty": map[string]interface{}{
			"size": map[string]interface{}{"cols": 80, "rows": 24},
		},
	})
	ptyCtx, ptyCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer ptyCancel()
	req, err := http.NewRequestWithContext(ptyCtx, http.MethodPost, ts.URL+"/process.Process/Start", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("pty start %d %s", resp.StatusCode, b)
	}
	start, err := readNextConnectFrame(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var startEvent struct {
		Event struct {
			Start struct {
				Pid int `json:"pid"`
			} `json:"start"`
		} `json:"event"`
	}
	if err := json.Unmarshal(start.Payload, &startEvent); err != nil {
		t.Fatalf("start frame: %v %s", err, start.Payload)
	}
	pid := startEvent.Event.Start.Pid
	if pid <= 1 {
		t.Fatalf("pid %d", pid)
	}
	srv.mu.RLock()
	entry := srv.commands[strconv.Itoa(pid)]
	srv.mu.RUnlock()
	if entry == nil || entry.masterFile == nil || !entry.isPTY {
		t.Fatal("pty command entry was not registered")
	}

	wPayload := []byte(`{"path":"/workspace"}`)
	wReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/filesystem.Filesystem/CreateWatcher", bytes.NewReader(wPayload))
	wReq.Header.Set("Content-Type", "application/json")
	wReq.Header.Set("E2b-Sandbox-Id", rec.SandboxID)
	wResp, err := http.DefaultClient.Do(wReq)
	if err != nil {
		t.Fatal(err)
	}
	wBody, _ := io.ReadAll(wResp.Body)
	wResp.Body.Close()
	if wResp.StatusCode != http.StatusOK {
		t.Fatalf("create watcher %d %s", wResp.StatusCode, wBody)
	}
	var created struct {
		WatcherID string `json:"watcherId"`
	}
	if err := json.Unmarshal(wBody, &created); err != nil {
		t.Fatal(err)
	}
	srv.mu.RLock()
	watcher := srv.watchers[created.WatcherID]
	srv.mu.RUnlock()
	if watcher == nil {
		t.Fatal("watcher missing before delete")
	}

	// Keep reading so the end frame cannot block subscriber removal.
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(io.Discard, resp.Body)
	}()

	del, err := http.NewRequest(http.MethodDelete, ts.URL+"/sandboxes/"+rec.SandboxID, nil)
	if err != nil {
		t.Fatal(err)
	}
	delResp, err := http.DefaultClient.Do(del)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status %d", delResp.StatusCode)
	}

	select {
	case <-copied:
	case <-time.After(10 * time.Second):
		t.Fatal("pty stream did not finish after sandbox delete")
	}

	srv.mu.RLock()
	for id, e := range srv.commands {
		if e.sandboxID == rec.SandboxID {
			t.Errorf("command %s still registered", id)
		}
	}
	for id, w := range srv.watchers {
		if w.sandboxID == rec.SandboxID {
			t.Errorf("watcher %s still registered", id)
		}
	}
	srv.mu.RUnlock()

	entry.mu.RLock()
	master := entry.masterFile
	nsub := len(entry.subscribers)
	running := entry.running
	entry.mu.RUnlock()
	if master != nil || nsub != 0 || running {
		t.Fatalf("master %v subs %d running %v", master, nsub, running)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still alive after sandbox delete", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-watcher.done:
	default:
		t.Fatal("watcher was not stopped")
	}
	fdDeadline := time.Now().Add(2 * time.Second)
	for {
		watcher.mu.Lock()
		fd := watcher.fd
		watcher.mu.Unlock()
		if fd == -1 {
			break
		}
		if time.Now().After(fdDeadline) {
			t.Fatalf("watcher fd %d did not close", fd)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResolveWatchPathAllowsHomeUserAndBlocksSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(work, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}
	srv := &Server{}
	rec := &SandboxRecord{SandboxID: "sbx-sym", WorkspaceDir: work}

	// /home/user is re-rooted under the sandbox workspace directory in local mode,
	// matching resolveFilePath.
	got, err := srv.resolveWatchPath(rec, "/home/user/proj")
	if err != nil {
		t.Fatalf("resolve /home/user/proj: %v", err)
	}
	want := filepath.Join(work, "home", "user", "proj")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}

	// Symlink pointing outside the workspace must be rejected.
	link := filepath.Join(work, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.resolveWatchPath(rec, "/workspace/escape"); err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
	if _, err := srv.resolveWatchPath(rec, "/workspace/escape/sub"); err == nil {
		t.Fatal("expected symlink ancestor escape to be rejected")
	}
}

func TestWatchCapAndIdlePollTTL(t *testing.T) {
	work := t.TempDir()
	srv := &Server{watchers: make(map[string]*dirWatcher)}
	rec := &SandboxRecord{SandboxID: "sbx-cap", WorkspaceDir: work}

	for i := 0; i < 2; i++ {
		if _, err := srv.startDirWatcherWithCap(rec, watchCreateReq{Path: "/workspace"}, true, 2); err != nil {
			t.Fatalf("watcher %d failed: %v", i, err)
		}
	}
	if _, err := srv.startDirWatcherWithCap(rec, watchCreateReq{Path: "/workspace"}, true, 2); err == nil {
		t.Fatal("expected watcher cap error")
	} else if st, ok := err.(*connectStatusError); !ok || st.code != "resource_exhausted" {
		t.Fatalf("expected resource_exhausted, got %#v", err)
	}

	if reaped := srv.reapExpiredWatchers(0); reaped != 2 {
		t.Fatalf("reaped %d want 2", reaped)
	}
	srv.mu.RLock()
	remaining := len(srv.watchers)
	srv.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("remaining watchers %d", remaining)
	}
}

func TestMapInotifyMatchesFsnotifyAndDropsCloseWrite(t *testing.T) {
	if typ, ok := mapInotify(unix.IN_CREATE); !ok || typ != "EVENT_TYPE_CREATE" {
		t.Fatalf("IN_CREATE: %q %v", typ, ok)
	}
	if typ, ok := mapInotify(unix.IN_MOVED_TO); !ok || typ != "EVENT_TYPE_CREATE" {
		t.Fatalf("IN_MOVED_TO: %q %v", typ, ok)
	}
	if typ, ok := mapInotify(unix.IN_MOVED_FROM); !ok || typ != "EVENT_TYPE_RENAME" {
		t.Fatalf("IN_MOVED_FROM: %q %v", typ, ok)
	}
	if typ, ok := mapInotify(unix.IN_MODIFY); !ok || typ != "EVENT_TYPE_WRITE" {
		t.Fatalf("IN_MODIFY: %q %v", typ, ok)
	}
	if typ, ok := mapInotify(unix.IN_CLOSE_WRITE); ok {
		t.Fatalf("IN_CLOSE_WRITE must be ignored, got %q", typ)
	}
	if watchMask&unix.IN_CLOSE_WRITE != 0 {
		t.Fatal("watchMask must not include IN_CLOSE_WRITE")
	}
}
