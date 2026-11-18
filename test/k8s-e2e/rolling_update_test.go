package k8s_e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/client"
	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/gateway"
)

// routableMemoryKubeClient wraps MemoryKubeClient so newly reconciled PodBoxes
// receive the shared mock podd host:port in Status.PodIP, mirroring real K8s Pod IPs.
type routableMemoryKubeClient struct {
	*controller.MemoryKubeClient
	poddAddr string
}

func (r *routableMemoryKubeClient) EnsurePod(ctx context.Context, pod controller.PodManifest) (string, error) {
	_, err := r.MemoryKubeClient.EnsurePod(ctx, pod)
	return r.poddAddr, err
}

func (r *routableMemoryKubeClient) ListPods(ctx context.Context, namespace, labelSelector string) ([]controller.PodManifest, error) {
	pods, err := r.MemoryKubeClient.ListPods(ctx, namespace, labelSelector)
	if err != nil {
		return nil, err
	}
	for i := range pods {
		if pods[i].PodIP == "" || pods[i].PodIP == "10.244.0.42" {
			pods[i].PodIP = r.poddAddr
		}
	}
	return pods, nil
}

type mockRollingPodd struct {
	mu     sync.RWMutex
	files  map[string][]byte
	cmdSeq atomic.Uint64
}

func newMockRollingPoddServer() *httptest.Server {
	p := &mockRollingPodd{
		files: make(map[string][]byte),
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","daemon":"podd-rs"}`))

		case r.Method == http.MethodPost && r.URL.Path == "/v1/commands/run":
			var req struct {
				Cmd        string `json:"cmd"`
				Command    string `json:"command"`
				Background bool   `json:"background"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			cmdStr := req.Command
			if cmdStr == "" {
				cmdStr = req.Cmd
			}
			seq := p.cmdSeq.Add(1)
			cmdID := fmt.Sprintf("cmd-roll-%d", seq)
			out := fmt.Sprintf("podd-exec-ok:%s\n", cmdStr)
			w.Header().Set("Content-Type", "application/json")
			exitZero := 0
			if req.Background {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":        cmdID,
					"cmdID":     cmdID,
					"pid":       int(1000 + seq),
					"running":   false,
					"exit_code": exitZero,
					"exitCode":  exitZero,
					"output":    out,
					"stdout":    out,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":        cmdID,
				"cmdID":     cmdID,
				"pid":       int(1000 + seq),
				"running":   false,
				"exit_code": exitZero,
				"exitCode":  exitZero,
				"output":    out,
				"stdout":    out,
				"stderr":    "",
			})

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/commands/") && strings.HasSuffix(r.URL.Path, "/logs"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"cmd-bg","content":"podd-bg-log\n","cursor":12,"running":false,"exit_code":0}`))

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/commands/") && strings.HasSuffix(r.URL.Path, "/status"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"cmd-bg","pid":1001,"running":false,"exit_code":0,"cursor":12}`))

		case (r.Method == http.MethodPut || r.Method == http.MethodPost) && r.URL.Path == "/v1/files":
			targetPath := r.URL.Query().Get("path")
			data, _ := io.ReadAll(r.Body)
			p.mu.Lock()
			p.files[targetPath] = data
			p.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"path":   targetPath,
				"size":   len(data),
			})

		case r.Method == http.MethodGet && r.URL.Path == "/v1/files":
			targetPath := r.URL.Query().Get("path")
			p.mu.RLock()
			data, ok := p.files[targetPath]
			p.mu.RUnlock()
			if !ok {
				http.Error(w, "file not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)

		case r.Method == http.MethodDelete && r.URL.Path == "/v1/files":
			targetPath := r.URL.Query().Get("path")
			p.mu.Lock()
			delete(p.files, targetPath)
			p.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/lifecycle/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))

		default:
			http.NotFound(w, r)
		}
	}))
}

type gatewayReplica struct {
	name     string
	gw       *gateway.Server
	httpSrv  *http.Server
	url      *url.URL
	hits     atomic.Uint64
	inFlight atomic.Int64
}

func startGatewayReplica(t *testing.T, name string, sharedKube controller.KubeClient, panelSecret, wsBase string) *gatewayReplica {
	t.Helper()
	rec := controller.NewReconciler(controller.WithKubeClient(sharedKube))
	gw := gateway.NewServer(rec)
	gw.SetWorkspaceBaseDir(wsBase)
	gw.SetPanelSecret(panelSecret)
	gw.SetFeatureGate(gateway.FeatureGateStatelessGatewayHA, true)
	gw.SetPodBoxSyncInterval(250 * time.Millisecond)
	gw.SetLivePodRouting(true)

	rep := &gatewayReplica{
		name: name,
		gw:   gw,
	}
	baseHandler := gw.Handler()
	countedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rep.hits.Add(1)
		baseHandler.ServeHTTP(w, r)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		gw.Close()
		t.Fatalf("listen replica %s: %v", name, err)
	}
	u, err := url.Parse("http://" + ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		gw.Close()
		t.Fatalf("parse replica url: %v", err)
	}
	rep.url = u
	rep.httpSrv = &http.Server{
		Handler:           countedHandler,
		ReadHeaderTimeout: 1 * time.Second,
	}
	go func() {
		_ = rep.httpSrv.Serve(ln)
	}()

	// Verify readiness probe (/health) before joining Service endpoints.
	readyClient := &http.Client{Timeout: 2 * time.Second}
	defer readyClient.CloseIdleConnections()
	resp, err := readyClient.Get(rep.url.String() + "/health")
	if err != nil {
		_ = rep.httpSrv.Close()
		gw.Close()
		t.Fatalf("replica %s readiness check failed: %v", name, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = rep.httpSrv.Close()
		gw.Close()
		t.Fatalf("replica %s /health status = %d", name, resp.StatusCode)
	}
	return rep
}

func drainGatewayReplica(t *testing.T, lb *rollingLoadBalancer, rep *gatewayReplica) {
	t.Helper()
	// Equivalent to Kubernetes preStop endpoint-propagation grace: wait until the load balancer
	// has finished dispatching any request already routed to this replica before closing its listener.
	waitDeadline := time.Now().Add(5 * time.Second)
	for rep.inFlight.Load() > 0 && time.Now().Before(waitDeadline) {
		time.Sleep(1 * time.Millisecond)
	}
	if lb != nil {
		lb.transport.CloseIdleConnections()
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rep.httpSrv.Shutdown(drainCtx); err != nil {
		t.Errorf("graceful shutdown of replica %s failed: %v", rep.name, err)
	}
	rep.gw.Close()
}

type rollingLoadBalancer struct {
	mu        sync.RWMutex
	backends  []*gatewayReplica
	rrCounter atomic.Uint64
	transport *http.Transport
}

func newRollingLoadBalancer(initial ...*gatewayReplica) *rollingLoadBalancer {
	return &rollingLoadBalancer{
		backends: append([]*gatewayReplica(nil), initial...),
		transport: &http.Transport{
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     5 * time.Second,
		},
	}
}

func (lb *rollingLoadBalancer) addBackend(rep *gatewayReplica) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.backends = append(lb.backends, rep)
}

func (lb *rollingLoadBalancer) removeBackend(target *gatewayReplica) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	kept := make([]*gatewayReplica, 0, len(lb.backends))
	for _, b := range lb.backends {
		if b != target {
			kept = append(kept, b)
		}
	}
	lb.backends = kept
}

func (lb *rollingLoadBalancer) pickBackendAndRetain() *gatewayReplica {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	if len(lb.backends) == 0 {
		return nil
	}
	idx := lb.rrCounter.Add(1) - 1
	chosen := lb.backends[int(idx%uint64(len(lb.backends)))]
	chosen.inFlight.Add(1)
	return chosen
}

func (lb *rollingLoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	backend := lb.pickBackendAndRetain()
	if backend == nil {
		http.Error(w, "no healthy gateway backends", http.StatusServiceUnavailable)
		return
	}
	defer backend.inFlight.Add(-1)
	target := backend.url
	proxy := &httputil.ReverseProxy{
		Transport: lb.transport,
		Director: func(req *http.Request) {
			origHost := req.Host
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			if origHost != "" {
				req.Host = origHost
			} else {
				req.Host = target.Host
			}
		},
	}
	proxy.ServeHTTP(w, r)
}

func (lb *rollingLoadBalancer) Close() {
	lb.transport.CloseIdleConnections()
}

// TestStatelessGatewayRollingHandoverUnderLoad verifies CEP-0022 StatelessGatewayHA
// zero-downtime rolling updates in-process (under go test -race, without cluster mutation):
//   - Launches 2 gateway.Server replicas sharing MemoryKubeClient + mock podd behind a
//     dynamic round-robin reverse proxy.
//   - Drives continuous concurrent sandbox create/exec/file/panel/delete load across workers
//     while rolling through 2 full replica generations (gen0 -> gen1 -> gen2) with graceful
//     endpoint removal and HTTP connection draining.
//   - Asserts 0 failed requests and verifies gen2 replicas reconstruct pre-existing sandboxes
//     strictly from Kubernetes PodBox CRDs.
func TestStatelessGatewayRollingHandoverUnderLoad(t *testing.T) {
	mockPodd := newMockRollingPoddServer()
	defer mockPodd.Close()

	poddURLParsed, err := url.Parse(mockPodd.URL)
	if err != nil {
		t.Fatalf("parse mockPodd URL: %v", err)
	}
	_, poddPortStr, err := net.SplitHostPort(poddURLParsed.Host)
	if err != nil {
		t.Fatalf("split mockPodd host/port: %v", err)
	}
	poddPort, _ := strconv.Atoi(poddPortStr)

	sharedKube := &routableMemoryKubeClient{
		MemoryKubeClient: controller.NewMemoryKubeClient(),
		poddAddr:         poddURLParsed.Host,
	}
	panelSecret := "cep0022-rolling-handover-hmac-secret"
	wsBase := t.TempDir()

	gen0A := startGatewayReplica(t, "gen0-a", sharedKube, panelSecret, wsBase)
	gen0B := startGatewayReplica(t, "gen0-b", sharedKube, panelSecret, wsBase)
	activeReplicas := []*gatewayReplica{gen0A, gen0B}

	lb := newRollingLoadBalancer(activeReplicas...)
	defer lb.Close()
	lbSrv := httptest.NewServer(lb)
	defer func() {
		lbSrv.CloseClientConnections()
		lbSrv.Close()
	}()

	defer func() {
		for _, r := range activeReplicas {
			drainGatewayReplica(t, lb, r)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	seedClient := client.New(lbSrv.URL, client.WithTimeout(10*time.Second))
	httpClient := &http.Client{Timeout: 10 * time.Second}
	defer httpClient.CloseIdleConnections()

	type pinnedSandbox struct {
		id       string
		filePath string
		payload  string
		panelURL string
	}
	pinned := make([]pinnedSandbox, 0, 3)
	for i := 0; i < 3; i++ {
		sb, err := seedClient.CreateSandbox(ctx, client.CreateSandboxRequest{
			TemplateID: "cubeplex-ubuntu24-desktop",
			Metadata:   map[string]string{"role": "pinned-across-rollout", "idx": strconv.Itoa(i)},
		})
		if err != nil {
			t.Fatalf("seed CreateSandbox[%d]: %v", i, err)
		}
		fpath := fmt.Sprintf("/workspace/pinned-%d.txt", i)
		payload := fmt.Sprintf("pinned-state-%d-%s", i, sb.SandboxID)
		if err := seedClient.WriteFile(ctx, sb.SandboxID, fpath, []byte(payload), "0644"); err != nil {
			t.Fatalf("seed WriteFile[%d]: %v", i, err)
		}

		mintResp, err := httpClient.Post(
			fmt.Sprintf("%s/sandboxes/%s/endpoints/%d", lbSrv.URL, sb.SandboxID, poddPort),
			"application/json",
			strings.NewReader(`{"expiresInSeconds":3600}`),
		)
		if err != nil || mintResp.StatusCode != http.StatusOK {
			t.Fatalf("seed mint panel token[%d]: err=%v resp=%v", i, err, mintResp)
		}
		var minted struct {
			URL string `json:"url"`
		}
		_ = json.NewDecoder(mintResp.Body).Decode(&minted)
		_ = mintResp.Body.Close()

		pinned = append(pinned, pinnedSandbox{
			id:       sb.SandboxID,
			filePath: fpath,
			payload:  payload,
			panelURL: minted.URL,
		})
	}

	var (
		totalOps  atomic.Uint64
		failedOps atomic.Uint64
		errMu     sync.Mutex
		firstErrs []string
	)
	recordFailure := func(format string, args ...any) {
		failedOps.Add(1)
		msg := fmt.Sprintf(format, args...)
		errMu.Lock()
		if len(firstErrs) < 10 {
			firstErrs = append(firstErrs, msg)
		}
		errMu.Unlock()
	}

	stopLoad := make(chan struct{})
	const workerCount = 6
	var workerWG sync.WaitGroup
	for w := 0; w < workerCount; w++ {
		workerID := w
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			wHTTP := &http.Client{Timeout: 8 * time.Second}
			defer wHTTP.CloseIdleConnections()
			wClient := client.New(lbSrv.URL, client.WithHTTPClient(wHTTP))

			iter := 0
			for {
				select {
				case <-stopLoad:
					return
				default:
				}
				iter++

				// 1. Exercise a pre-existing pinned sandbox across round-robin cold/warm replicas.
				target := pinned[(workerID+iter)%len(pinned)]
				gotSB, err := wClient.GetSandbox(ctx, target.id)
				totalOps.Add(1)
				if err != nil || gotSB.SandboxID != target.id {
					recordFailure("worker %d iter %d GetSandbox(%s): err=%v", workerID, iter, target.id, err)
					continue
				}

				gotBytes, err := wClient.ReadFile(ctx, target.id, target.filePath)
				totalOps.Add(1)
				if err != nil || string(gotBytes) != target.payload {
					recordFailure("worker %d iter %d ReadFile(%s): err=%v got=%q", workerID, iter, target.id, err, string(gotBytes))
					continue
				}

				cmdRes, err := wClient.RunCommand(ctx, target.id, client.RunCommandRequest{
					Command: fmt.Sprintf("worker-%d-iter-%d", workerID, iter),
				})
				totalOps.Add(1)
				if err != nil || cmdRes.ExitCode == nil || *cmdRes.ExitCode != 0 || !strings.Contains(cmdRes.Output, "podd-exec-ok:") {
					recordFailure("worker %d iter %d RunCommand(%s): err=%v res=%+v", workerID, iter, target.id, err, cmdRes)
					continue
				}

				panelResp, err := wHTTP.Get(lbSrv.URL + target.panelURL + "health")
				totalOps.Add(1)
				if err != nil {
					recordFailure("worker %d iter %d panel proxy(%s): err=%v", workerID, iter, target.id, err)
					continue
				}
				panelBody, _ := io.ReadAll(panelResp.Body)
				_ = panelResp.Body.Close()
				if panelResp.StatusCode != http.StatusOK || !bytes.Contains(panelBody, []byte("podd-rs")) {
					recordFailure("worker %d iter %d panel proxy status=%d body=%q", workerID, iter, panelResp.StatusCode, string(panelBody))
					continue
				}

				// 2. Create, mutate, read across round-robin, and delete an ephemeral sandbox.
				eph, err := wClient.CreateSandbox(ctx, client.CreateSandboxRequest{
					TemplateID: "cubeplex-ubuntu24-desktop",
					Metadata:   map[string]string{"worker": strconv.Itoa(workerID), "iter": strconv.Itoa(iter)},
				})
				totalOps.Add(1)
				if err != nil {
					recordFailure("worker %d iter %d CreateSandbox: %v", workerID, iter, err)
					continue
				}

				ephPath := fmt.Sprintf("/workspace/eph-%s-%d-%d.txt", eph.SandboxID, workerID, iter)
				ephData := fmt.Sprintf("ephemeral-payload-%d-%d", workerID, iter)
				err = wClient.WriteFile(ctx, eph.SandboxID, ephPath, []byte(ephData), "0644")
				totalOps.Add(1)
				if err != nil {
					recordFailure("worker %d iter %d WriteFile(%s): %v", workerID, iter, eph.SandboxID, err)
					_ = wClient.DeleteSandboxWithOptions(ctx, eph.SandboxID, true)
					continue
				}

				readEph, err := wClient.ReadFile(ctx, eph.SandboxID, ephPath)
				totalOps.Add(1)
				if err != nil || string(readEph) != ephData {
					recordFailure("worker %d iter %d ReadFile(%s): err=%v got=%q", workerID, iter, eph.SandboxID, err, string(readEph))
					_ = wClient.DeleteSandboxWithOptions(ctx, eph.SandboxID, true)
					continue
				}

				err = wClient.DeleteSandboxWithOptions(ctx, eph.SandboxID, true)
				totalOps.Add(1)
				if err != nil {
					recordFailure("worker %d iter %d DeleteSandbox(%s): %v", workerID, iter, eph.SandboxID, err)
				}
			}
		}()
	}

	// Execute 2 full rolling update waves (gen0 -> gen1 -> gen2) with maxSurge=1, maxUnavailable=0:
	// start replacement replica -> register in LB -> remove retiring replica from LB -> graceful HTTP drain.
	var drainedHits uint64
	for gen := 1; gen <= 2; gen++ {
		time.Sleep(60 * time.Millisecond)
		nextWave := make([]*gatewayReplica, 0, len(activeReplicas))
		for idx, retiring := range activeReplicas {
			replacement := startGatewayReplica(t, fmt.Sprintf("gen%d-%c", gen, 'a'+idx), sharedKube, panelSecret, wsBase)
			lb.addBackend(replacement)
			lb.removeBackend(retiring)
			drainGatewayReplica(t, lb, retiring)
			drainedHits += retiring.hits.Load()
			nextWave = append(nextWave, replacement)
			time.Sleep(40 * time.Millisecond)
		}
		activeReplicas = nextWave
	}

	// Continue steady-state traffic on gen2 replicas (100% cold-reconstructed from PodBox CRDs) briefly.
	time.Sleep(80 * time.Millisecond)
	close(stopLoad)
	workerWG.Wait()

	// Clean up pinned sandboxes through gen2 replicas and verify 404 propagation across all gen2 replicas.
	for _, p := range pinned {
		if err := seedClient.DeleteSandboxWithOptions(ctx, p.id, true); err != nil {
			t.Fatalf("cleanup pinned sandbox %s on gen2 failed: %v", p.id, err)
		}
		for _, rep := range activeReplicas {
			resp, err := httpClient.Get(rep.url.String() + "/sandboxes/" + p.id)
			if err != nil {
				t.Fatalf("verify 404 on %s: %v", rep.name, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("expected 404 on %s after deleting %s, got %d", rep.name, p.id, resp.StatusCode)
			}
		}
	}

	if failed := failedOps.Load(); failed != 0 {
		t.Fatalf("expected 0 failed requests during multi-generation gateway rolling update, got %d failures (totalOps=%d): %v",
			failed, totalOps.Load(), firstErrs)
	}
	if totalOps.Load() < 40 {
		t.Fatalf("expected >= 40 requests during rolling handover load test, got %d", totalOps.Load())
	}

	var gen2Hits uint64
	for _, rep := range activeReplicas {
		gen2Hits += rep.hits.Load()
	}
	if gen2Hits == 0 {
		t.Fatalf("expected final gen2 replicas to serve traffic after rollout, got 0 hits")
	}

	t.Logf("[Rolling-HA] Zero-downtime handover verified: totalOps=%d failedOps=0 waves=2 drainedHits=%d gen2Hits=%d",
		totalOps.Load(), drainedHits, gen2Hits)
}

// TestK8sLiveGatewayRollingUpdateZeroDowntime drives continuous sandbox traffic against
// a live multi-replica cubepod-gateway Deployment while executing a rolling restart.
// Strictly gated on CUBEPOD_K8S_DISRUPTIVE=1 so shared clusters are never mutated accidentally.
func TestK8sLiveGatewayRollingUpdateZeroDowntime(t *testing.T) {
	if os.Getenv("CUBEPOD_K8S_DISRUPTIVE") != "1" {
		t.Skip("Skipping disruptive live gateway rolling-update test: set CUBEPOD_K8S_DISRUPTIVE=1 on a dedicated multi-replica cluster")
	}

	cfg := loadClusterConfig(t)
	if cfg.GatewayURL == "" {
		t.Fatal("gateway URL required for live rolling-update verification")
	}
	if cfg.SandboxImage == "" {
		t.Fatal("sandbox image required for live rolling-update verification")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	deployObj := kubectlJSON(ctx, t, "get", "deploy", cfg.GatewayName, "-n", cfg.SystemNamespace, "-o", "json")
	spec, _ := deployObj["spec"].(map[string]any)
	replicas := jsonInt(spec["replicas"])
	if replicas < 2 {
		t.Skipf("Skipping live zero-downtime rollout: deploy %s/%s has replicas=%d (requires >= 2 replicas + PodDisruptionBudget)",
			cfg.SystemNamespace, cfg.GatewayName, replicas)
	}

	c := client.New(cfg.GatewayURL, client.WithTimeout(3*time.Minute))
	sb, err := c.CreateSandbox(ctx, client.CreateSandboxRequest{
		TemplateID: cfg.SandboxImage,
		Metadata:   map[string]string{"env": "k8s-e2e", "suite": "live-rolling-update"},
	})
	if err != nil {
		t.Fatalf("CreateSandbox before rollout: %v", err)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer ccancel()
		_ = c.DeleteSandboxWithOptions(cctx, sb.SandboxID, true)
	}()

	if err := c.WriteFile(ctx, sb.SandboxID, "/workspace/rollout-check.txt", []byte("live-ha-ok"), "0644"); err != nil {
		t.Fatalf("WriteFile before rollout: %v", err)
	}

	var (
		totalReqs  atomic.Uint64
		failedReqs atomic.Uint64
	)
	stopLoad := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wc := client.New(cfg.GatewayURL, client.WithTimeout(30*time.Second))
			for {
				select {
				case <-stopLoad:
					return
				default:
				}
				data, rErr := wc.ReadFile(ctx, sb.SandboxID, "/workspace/rollout-check.txt")
				totalReqs.Add(1)
				if rErr != nil || string(data) != "live-ha-ok" {
					failedReqs.Add(1)
				}
				res, cErr := wc.RunCommand(ctx, sb.SandboxID, client.RunCommandRequest{Command: "printf live-rollout-ok"})
				totalReqs.Add(1)
				if cErr != nil || res.ExitCode == nil || *res.ExitCode != 0 {
					failedReqs.Add(1)
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
	}

	if err := kubectlRun(ctx, "rollout", "restart", "deploy/"+cfg.GatewayName, "-n", cfg.SystemNamespace); err != nil {
		close(stopLoad)
		wg.Wait()
		t.Fatalf("kubectl rollout restart failed: %v", err)
	}
	if err := kubectlRun(ctx, "rollout", "status", "deploy/"+cfg.GatewayName, "-n", cfg.SystemNamespace, "--timeout=5m"); err != nil {
		close(stopLoad)
		wg.Wait()
		t.Fatalf("kubectl rollout status failed: %v", err)
	}

	close(stopLoad)
	wg.Wait()

	if failedReqs.Load() > 0 {
		t.Fatalf("expected 0 failed requests during live gateway rollout, got %d/%d failed", failedReqs.Load(), totalReqs.Load())
	}
	t.Logf("[K8s-Rolling-HA] Live zero-downtime rollout passed: replicas=%d totalRequests=%d failedRequests=0", replicas, totalReqs.Load())
}
