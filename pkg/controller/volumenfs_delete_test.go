package controller

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
	"github.com/cubeplexai/cubepod/pkg/volumenfs"
)

func TestDeleteBoxRevokesAnnotationLeaseWhenCacheIsStale(t *testing.T) {
	nfs := volumenfs.NewServer(volumenfs.Config{VolumesDir: t.TempDir()})
	ts := httptest.NewServer(nfs.LeaseHTTPHandler())
	t.Cleanup(ts.Close)
	id := "0123456789abcdef0123456789abcdef"
	r := NewReconciler(WithVolumeNFS(VolumeNFSSettings{
		Enabled:     true,
		ServiceName: "cubepod-volume-nfs",
		ServiceNS:   "cubepod-system",
		NFSPort:     2049,
		LeaseClient: volumenfs.NewClient(ts.URL, ts.Client()),
		Lookup:      FakeVolumeNFSLookup{ClusterIP: "10.96.10.20", Ready: true},
	}))
	r.RegisterTemplate("fc", v1alpha1.PodTemplateSpec{RuntimeClassName: "kata-fc", Image: "ubuntu:24.04"})
	spec := v1alpha1.PodBoxSpec{
		TemplateRef:  "fc",
		VolumeMounts: []v1alpha1.VolumeMount{{VolumeID: id, PVCName: "cubepod-volumes", Path: "/data", SubPath: id + "/data"}},
	}
	if _, err := r.Reconcile(context.Background(), "box-fc", spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	live := r.volumeNFSLeases["box-fc"]
	if live == "" {
		t.Fatal("expected live lease in cache")
	}
	stale, err := volumenfs.NewLeaseID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nfs.Store.Put(stale, volumenfs.LeaseRequest{
		VolumeIDs:  []string{id},
		PodIP:      "10.244.0.99",
		PodName:    "stale",
		PodBoxName: "box-fc",
	}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.volumeNFSLeases["box-fc"] = stale
	r.mu.Unlock()
	if err := r.DeleteBox(context.Background(), "box-fc", false); err != nil {
		t.Fatalf("DeleteBox: %v", err)
	}
	if _, ok := nfs.Store.Get(live); ok {
		t.Fatalf("annotation lease %s must be revoked", live)
	}
	if _, ok := nfs.Store.Get(stale); ok {
		t.Fatalf("cached stale lease %s must be revoked", stale)
	}
}
