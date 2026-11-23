package volumenfs

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/willscott/go-nfs"
	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLeaseAPIPutDeleteAndRejectSingularVolumeID(t *testing.T) {
	s := NewServer(Config{VolumesDir: t.TempDir(), Logger: discardLogger()})
	ts := httptest.NewServer(s.LeaseHTTPHandler())
	defer ts.Close()

	id, _ := NewLeaseID()
	vol := "0123456789abcdef0123456789abcdef"
	client := NewClient(ts.URL, ts.Client())

	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/leases/"+id, bytes.NewReader([]byte(`{"volumeID":"`+vol+`","podName":"p"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("singular volumeID: status %d", resp.StatusCode)
	}

	lease, err := client.Put(context.Background(), id, LeaseRequest{VolumeIDs: []string{vol}, PodName: "pod-a", PodBoxName: "box-a"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Bound() {
		t.Fatal("pre-registration must leave podIP empty")
	}

	lease, err = client.Put(context.Background(), id, LeaseRequest{VolumeIDs: []string{vol}, PodIP: "10.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Bound() || lease.PodIP != "10.2.3.4" {
		t.Fatalf("bind failed: %+v", lease)
	}

	if err := client.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Store.Get(id); ok {
		t.Fatal("lease still present after delete")
	}
}

func TestHandlerMountAdmission(t *testing.T) {
	root := t.TempDir()
	id, _ := NewLeaseID()
	vol := "0123456789abcdef0123456789abcdef"
	if err := os.MkdirAll(filepath.Join(root, vol, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	metrics := NewMetrics()
	store := NewStore(WithMetrics(metrics))
	if _, err := store.Put(id, LeaseRequest{VolumeIDs: []string{vol}, PodName: "p"}); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(store, root, metrics, discardLogger())

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	conn := &addrConn{Conn: server, addr: &net.TCPAddr{IP: net.ParseIP("10.9.8.7"), Port: 1234}}
	req := nfs.MountRequest{Dirpath: []byte("/" + id + "/" + vol)}
	st, fs, _ := h.Mount(context.Background(), conn, req)
	if st == nfs.MountStatusOk && fs != nil {
		t.Fatal("unbound lease must not be admitted")
	}

	if _, err := store.Put(id, LeaseRequest{VolumeIDs: []string{vol}, PodIP: "10.9.8.7"}); err != nil {
		t.Fatal(err)
	}
	st, fs, flavors := h.Mount(context.Background(), conn, req)
	if st != nfs.MountStatusOk || fs == nil {
		t.Fatalf("expected admit, status=%v fs=%v denials=%d", st, fs, metrics.ExportDenials())
	}
	if len(flavors) == 0 {
		t.Fatal("expected AUTH_UNIX flavor")
	}
	if metrics.Mounts() < 1 {
		t.Fatal("expected mounts_total increment")
	}
	if _, err := fs.Open("../meta.json"); err == nil {
		t.Fatal("escaped export")
	}
}

func TestClientErrorsWhenUnconfigured(t *testing.T) {
	var c *Client
	if _, err := c.Put(context.Background(), "x", LeaseRequest{}); err == nil {
		t.Fatal("expected error")
	}
}

type addrConn struct {
	net.Conn
	addr net.Addr
}

func (c *addrConn) RemoteAddr() net.Addr { return c.addr }

func TestServeNFSDeniedMountDoesNotCrashAndRevokeEstales(t *testing.T) {
	root := t.TempDir()
	vol := "0123456789abcdef0123456789abcdef"
	if err := os.MkdirAll(filepath.Join(root, vol, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, vol, "data", "ok.txt"), []byte("yes"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{VolumesDir: root, Logger: discardLogger()})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.ServeNFS(ctx, lis, 1024)
	}()

	dial := func() *rpc.Client {
		t.Helper()
		var last error
		for i := 0; i < 20; i++ {
			c, err := rpc.DialTCP(lis.Addr().Network(), lis.Addr().String(), false)
			if err == nil {
				return c
			}
			last = err
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("dial: %v", last)
		return nil
	}

	id, err := NewLeaseID()
	if err != nil {
		t.Fatal(err)
	}
	export := "/" + id + "/" + vol

	// Denied MOUNT (unknown export) must not crash the server.
	c := dial()
	mounter := nfsc.Mount{Client: c}
	if _, err := mounter.Mount(export, rpc.AuthNull); err == nil {
		c.Close()
		t.Fatal("expected denied mount for missing lease")
	}
	c.Close()

	if _, err := s.Store.Put(id, LeaseRequest{VolumeIDs: []string{vol}, PodName: "p"}); err != nil {
		t.Fatal(err)
	}
	c = dial()
	mounter = nfsc.Mount{Client: c}
	if _, err := mounter.Mount(export, rpc.AuthNull); err == nil {
		c.Close()
		t.Fatal("expected denied mount for unbound lease")
	}
	c.Close()

	if _, err := s.Store.Put(id, LeaseRequest{VolumeIDs: []string{vol}, PodIP: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	c = dial()
	mounter = nfsc.Mount{Client: c}
	target, err := mounter.Mount(export, rpc.AuthNull)
	if err != nil {
		c.Close()
		t.Fatalf("expected admitted mount: %v", err)
	}
	if _, err := target.Open("ok.txt"); err != nil {
		t.Fatalf("open before revoke: %v", err)
	}
	s.Store.Delete(id)
	s.forgetHandles(id)
	_, err = target.Open("ok.txt")
	if err == nil {
		t.Fatal("expected ESTALE or error after lease revoke")
	}
	_ = mounter.Unmount()
	c.Close()

	// Server must still accept connections after the earlier denials and revoke.
	c = dial()
	mounter = nfsc.Mount{Client: c}
	if _, err := mounter.Mount("/not-a-valid-path", rpc.AuthNull); err == nil {
		c.Close()
		t.Fatal("expected denied mount after revoke")
	}
	c.Close()

	cancel()
	_ = lis.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ServeNFS: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeNFS did not return after listener close")
	}
}

func TestTrackingHandlerToHandleNilDoesNotPanic(t *testing.T) {
	h := newTrackingHandler(nfshelper.NewCachingHandler(NewHandler(NewStore(), t.TempDir(), nil, discardLogger()), 16), NewStore(), 16)
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("ToHandle(nil) panicked: %v", rec)
		}
	}()
	if fh := h.ToHandle(nil, []string{}); fh != nil {
		t.Fatalf("expected nil handle, got %v", fh)
	}
}
