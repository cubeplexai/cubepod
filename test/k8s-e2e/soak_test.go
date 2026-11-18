package k8s_e2e_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubeplexai/cubepod/pkg/controller"
	"github.com/cubeplexai/cubepod/pkg/gateway"
)

// SoakConfig configures concurrent E2B protocol + interactive PTY soak execution.
type SoakConfig struct {
	SuiteName          string
	GatewayURL         string
	TemplateID         string
	APIKey             string
	Duration           time.Duration
	Concurrency        int
	MaxCyclesPerWorker int
	TargetSandboxHours float64
	Environment        map[string]string
}

// SoakReport summarizes a concurrent E2B + PTY soak execution for JSON and Markdown export.
type SoakReport struct {
	SuiteName             string            `json:"suiteName"`
	StartedAt             time.Time         `json:"startedAt"`
	CompletedAt           time.Time         `json:"completedAt"`
	WallClockSeconds      float64           `json:"wallClockSeconds"`
	Concurrency           int               `json:"concurrency"`
	CompletedCycles       uint64            `json:"completedCycles"`
	TotalOperations       uint64            `json:"totalOperations"`
	FailedOperations      uint64            `json:"failedOperations"`
	E2BLifecycleChecks    uint64            `json:"e2bLifecycleChecks"`
	E2BFileChecks         uint64            `json:"e2bFileChecks"`
	E2BCommandChecks      uint64            `json:"e2bCommandChecks"`
	PTYSessionsVerified   uint64            `json:"ptySessionsVerified"`
	PTYResizesVerified    uint64            `json:"ptyResizesVerified"`
	PTYReconnectsVerified uint64            `json:"ptyReconnectsVerified"`
	SandboxSecondsTotal   float64           `json:"sandboxSecondsTotal"`
	SandboxHours          float64           `json:"sandboxHours"`
	TargetSandboxHours    float64           `json:"targetSandboxHours,omitempty"`
	TargetHoursMet        bool              `json:"targetHoursMet"`
	ActiveSandboxesLeft   int               `json:"activeSandboxesLeft"`
	Environment           map[string]string `json:"environment,omitempty"`
	SampleErrors          []string          `json:"sampleErrors,omitempty"`
	LatencySummary        []OperationStats  `json:"latencySummary,omitempty"`
}

// ComputeSandboxHours converts accumulated sandbox-seconds into sandbox-hours.
func ComputeSandboxHours(sandboxSeconds float64) float64 {
	if sandboxSeconds <= 0 {
		return 0
	}
	return sandboxSeconds / 3600.0
}

// RenderJSON serializes the SoakReport as indented JSON.
func (r SoakReport) RenderJSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// RenderMarkdown formats the SoakReport as a structured Markdown summary.
func (r SoakReport) RenderMarkdown() string {
	var b strings.Builder
	title := r.SuiteName
	if title == "" {
		title = "CubePod E2B & Interactive PTY Soak Report"
	}
	b.WriteString("# " + title + "\n\n")
	fmt.Fprintf(&b, "- **Window**: `%s` -> `%s` (`%.2fs` wall-clock)\n",
		r.StartedAt.UTC().Format(time.RFC3339),
		r.CompletedAt.UTC().Format(time.RFC3339),
		r.WallClockSeconds,
	)
	fmt.Fprintf(&b, "- **Concurrency**: `%d` workers\n", r.Concurrency)
	fmt.Fprintf(&b, "- **Completed Full Cycles**: `%d`\n", r.CompletedCycles)
	fmt.Fprintf(&b, "- **Operations**: `%d` total, `%d` failed\n", r.TotalOperations, r.FailedOperations)
	fmt.Fprintf(&b, "- **Accumulated Sandbox Time**: `%.4f` sandbox-seconds (`%.6f` sandbox-hours)\n",
		r.SandboxSecondsTotal, r.SandboxHours)
	if r.TargetSandboxHours > 0 {
		fmt.Fprintf(&b, "- **Target Sandbox Hours (`%.1fh`)**: `%v`\n", r.TargetSandboxHours, r.TargetHoursMet)
	}
	fmt.Fprintf(&b, "- **Leaked Sandboxes Remaining**: `%d`\n\n", r.ActiveSandboxesLeft)

	b.WriteString("## Protocol & PTY Coverage\n\n")
	b.WriteString("| Check Category | Verified Count |\n")
	b.WriteString("| :--- | ---: |\n")
	fmt.Fprintf(&b, "| E2B Lifecycle (`POST/GET/DELETE /sandboxes`, `/refreshes`) | %d |\n", r.E2BLifecycleChecks)
	fmt.Fprintf(&b, "| E2B Filesystem (`PUT/GET/LIST/DELETE /files`) | %d |\n", r.E2BFileChecks)
	fmt.Fprintf(&b, "| E2B Commands (`POST /commands/run`) | %d |\n", r.E2BCommandChecks)
	fmt.Fprintf(&b, "| Interactive PTY Sessions (`Start` + `SendInput` + `List` + `SendSignal`) | %d |\n", r.PTYSessionsVerified)
	fmt.Fprintf(&b, "| PTY Window Resizes (`Update` `80x24` -> `120x40`) | %d |\n", r.PTYResizesVerified)
	fmt.Fprintf(&b, "| PTY Stream Disconnect & Replay Reconnects (`Connect`) | %d |\n", r.PTYReconnectsVerified)

	if len(r.LatencySummary) > 0 {
		b.WriteString("\n## Operation Latency Distribution\n\n")
		b.WriteString("| Operation | Samples (OK/Total) | Min (ms) | p50 (ms) | p95 (ms) | Max (ms) | Mean (ms) |\n")
		b.WriteString("| :--- | :---: | ---: | ---: | ---: | ---: | ---: |\n")
		for _, s := range r.LatencySummary {
			fmt.Fprintf(&b, "| `%s` | %d/%d | %.2f | %.2f | %.2f | %.2f | %.2f |\n",
				s.Operation, s.SuccessCount, s.Count, s.MinMs, s.P50Ms, s.P95Ms, s.MaxMs, s.MeanMs)
		}
	}

	if len(r.SampleErrors) > 0 {
		b.WriteString("\n## Sample Errors\n\n")
		for _, e := range r.SampleErrors {
			fmt.Fprintf(&b, "- `%s`\n", e)
		}
	}
	return b.String()
}

func writeSoakReportFiles(report SoakReport, outPath string) (jsonPath, mdPath string, err error) {
	ext := strings.ToLower(filepath.Ext(outPath))
	base := strings.TrimSuffix(outPath, filepath.Ext(outPath))
	switch ext {
	case ".json":
		jsonPath = outPath
		mdPath = base + ".md"
	case ".md":
		mdPath = outPath
		jsonPath = base + ".json"
	default:
		mdPath = outPath + ".md"
		jsonPath = outPath + ".json"
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return "", "", err
	}
	rawJSON, err := report.RenderJSON()
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(jsonPath, rawJSON, 0644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(mdPath, []byte(report.RenderMarkdown()), 0644); err != nil {
		return "", "", err
	}
	return jsonPath, mdPath, nil
}

// RunSoakSuite executes concurrent E2B + interactive PTY cycles until cfg.Duration
// (or cfg.MaxCyclesPerWorker) completes, guaranteeing cleanup of all created sandboxes.
func RunSoakSuite(ctx context.Context, cfg SoakConfig) SoakReport {
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 2
	}
	duration := cfg.Duration
	if duration <= 0 {
		duration = 2 * time.Second
	}
	tpl := cfg.TemplateID
	if tpl == "" {
		tpl = "base"
	}
	baseURL := strings.TrimRight(cfg.GatewayURL, "/")

	startedAt := time.Now().UTC()
	runCtx, runCancel := context.WithTimeout(ctx, duration)
	defer runCancel()

	collector := NewBenchmarkCollector(cfg.SuiteName, cfg.Environment, map[string]SLOTarget{})

	var (
		completedCycles       atomic.Uint64
		totalOps              atomic.Uint64
		failedOps             atomic.Uint64
		lifecycleChecks       atomic.Uint64
		fileChecks            atomic.Uint64
		commandChecks         atomic.Uint64
		ptySessionsVerified   atomic.Uint64
		ptyResizesVerified    atomic.Uint64
		ptyReconnectsVerified atomic.Uint64
		sandboxNanosTotal     atomic.Int64

		activeMu sync.Mutex
		active   = make(map[string]time.Time)

		errMu      sync.Mutex
		sampleErrs []string
	)

	recordErr := func(op string, err error) {
		if err == nil {
			return
		}
		failedOps.Add(1)
		errMu.Lock()
		if len(sampleErrs) < 16 {
			sampleErrs = append(sampleErrs, fmt.Sprintf("%s: %v", op, err))
		}
		errMu.Unlock()
	}

	trackCreate := func(id string) {
		activeMu.Lock()
		active[id] = time.Now()
		activeMu.Unlock()
	}

	trackDelete := func(id string) {
		activeMu.Lock()
		if st, ok := active[id]; ok {
			sandboxNanosTotal.Add(time.Since(st).Nanoseconds())
			delete(active, id)
		}
		activeMu.Unlock()
	}

	deleteSandboxHTTP := func(httpClient *http.Client, sandboxID string) error {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		req, err := http.NewRequestWithContext(cctx, http.MethodDelete, baseURL+"/sandboxes/"+sandboxID+"?deleteVolumes=true", nil)
		if err != nil {
			return err
		}
		if cfg.APIKey != "" {
			req.Header.Set("X-API-Key", cfg.APIKey)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("DELETE /sandboxes/%s returned HTTP %d", sandboxID, resp.StatusCode)
		}
		trackDelete(sandboxID)
		return nil
	}

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		workerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			unaryTransport := &http.Transport{MaxIdleConnsPerHost: 8, IdleConnTimeout: 5 * time.Second}
			streamTransport := &http.Transport{MaxIdleConnsPerHost: 8, IdleConnTimeout: 5 * time.Second}
			unaryClient := &http.Client{Timeout: 60 * time.Second, Transport: unaryTransport}
			streamClient := &http.Client{Transport: streamTransport}
			defer func() {
				unaryTransport.CloseIdleConnections()
				streamTransport.CloseIdleConnections()
			}()

			for iter := 1; ; iter++ {
				if cfg.MaxCyclesPerWorker > 0 && iter > cfg.MaxCyclesPerWorker {
					return
				}
				select {
				case <-runCtx.Done():
					return
				default:
				}

				opCtx, opCancel := context.WithTimeout(context.Background(), 2*time.Minute)
				err := runSingleSoakCycle(
					opCtx,
					unaryClient,
					streamClient,
					baseURL,
					cfg.APIKey,
					tpl,
					workerID,
					iter,
					collector,
					&totalOps,
					&lifecycleChecks,
					&fileChecks,
					&commandChecks,
					&ptySessionsVerified,
					&ptyResizesVerified,
					&ptyReconnectsVerified,
					trackCreate,
					deleteSandboxHTTP,
				)
				opCancel()
				if err != nil {
					if runCtx.Err() != nil && errors.Is(err, context.Canceled) {
						return
					}
					recordErr(fmt.Sprintf("worker-%d-iter-%d", workerID, iter), err)
					continue
				}
				completedCycles.Add(1)
			}
		}()
	}
	wg.Wait()

	// Guarantee cleanup of any stray sandboxes if a cycle exited mid-flight.
	cleanupTransport := &http.Transport{}
	cleanupClient := &http.Client{Timeout: 30 * time.Second, Transport: cleanupTransport}
	activeMu.Lock()
	leftoverIDs := make([]string, 0, len(active))
	for id := range active {
		leftoverIDs = append(leftoverIDs, id)
	}
	activeMu.Unlock()
	for _, id := range leftoverIDs {
		_ = deleteSandboxHTTP(cleanupClient, id)
	}

	// Query GET /sandboxes to count any sandboxes still bearing suite=cubepod-soak.
	leakedCount := 0
	if status, listBytes, err := doGatewayJSON(context.Background(), cleanupClient, http.MethodGet, baseURL+"/sandboxes", cfg.APIKey, nil); err == nil && status == http.StatusOK {
		var items []struct {
			SandboxID string            `json:"sandboxID"`
			Metadata  map[string]string `json:"metadata"`
		}
		if json.Unmarshal(listBytes, &items) == nil {
			for _, it := range items {
				if it.Metadata["suite"] == "cubepod-soak" {
					leakedCount++
				}
			}
		}
	}
	cleanupTransport.CloseIdleConnections()

	completedAt := time.Now().UTC()
	benchReport := collector.BuildReport()
	sbxSeconds := float64(sandboxNanosTotal.Load()) / float64(time.Second)
	sbxHours := ComputeSandboxHours(sbxSeconds)
	targetMet := cfg.TargetSandboxHours <= 0 || sbxHours >= cfg.TargetSandboxHours

	return SoakReport{
		SuiteName:             cfg.SuiteName,
		StartedAt:             startedAt,
		CompletedAt:           completedAt,
		WallClockSeconds:      completedAt.Sub(startedAt).Seconds(),
		Concurrency:           concurrency,
		CompletedCycles:       completedCycles.Load(),
		TotalOperations:       totalOps.Load(),
		FailedOperations:      failedOps.Load(),
		E2BLifecycleChecks:    lifecycleChecks.Load(),
		E2BFileChecks:         fileChecks.Load(),
		E2BCommandChecks:      commandChecks.Load(),
		PTYSessionsVerified:   ptySessionsVerified.Load(),
		PTYResizesVerified:    ptyResizesVerified.Load(),
		PTYReconnectsVerified: ptyReconnectsVerified.Load(),
		SandboxSecondsTotal:   sbxSeconds,
		SandboxHours:          sbxHours,
		TargetSandboxHours:    cfg.TargetSandboxHours,
		TargetHoursMet:        targetMet,
		ActiveSandboxesLeft:   leakedCount,
		Environment:           benchReport.Environment,
		SampleErrors:          sampleErrs,
		LatencySummary:        benchReport.Summaries,
	}
}

func runSingleSoakCycle(
	ctx context.Context,
	unaryClient *http.Client,
	streamClient *http.Client,
	baseURL, apiKey, templateID string,
	workerID, iter int,
	collector *BenchmarkCollector,
	totalOps, lifecycleChecks, fileChecks, commandChecks, ptySessions, ptyResizes, ptyReconnects *atomic.Uint64,
	trackCreate func(string),
	deleteSandbox func(*http.Client, string) error,
) error {
	var sandboxID string
	defer func() {
		if sandboxID != "" {
			totalOps.Add(1)
			_ = deleteSandbox(unaryClient, sandboxID)
		}
	}()

	// 1. E2B CreateSandbox + schema validation + refresh + detail query (CEP-0018).
	err := collector.Measure("e2b_create_sandbox", iter, 0, nil, func() error {
		totalOps.Add(1)
		status, body, err := doGatewayJSON(ctx, unaryClient, http.MethodPost, baseURL+"/sandboxes", apiKey, map[string]any{
			"templateID": templateID,
			"timeout":    300,
			"metadata": map[string]string{
				"suite":  "cubepod-soak",
				"worker": strconv.Itoa(workerID),
				"iter":   strconv.Itoa(iter),
			},
			"envVars": map[string]string{"SOAK_ITER": strconv.Itoa(iter)},
		})
		if err != nil {
			return err
		}
		if status != http.StatusCreated && status != http.StatusOK {
			return fmt.Errorf("POST /sandboxes HTTP %d: %s", status, string(body))
		}
		var created struct {
			SandboxID   string `json:"sandboxID"`
			TemplateID  string `json:"templateID"`
			ClientID    string `json:"clientID"`
			EnvdVersion string `json:"envdVersion"`
		}
		if err := json.Unmarshal(body, &created); err != nil {
			return err
		}
		if created.SandboxID == "" || created.TemplateID == "" || created.ClientID == "" || created.EnvdVersion == "" {
			return fmt.Errorf("missing mandatory E2B fields in create response: %+v", created)
		}
		sandboxID = created.SandboxID
		trackCreate(sandboxID)
		return nil
	})
	if err != nil {
		return err
	}

	totalOps.Add(1)
	refStatus, refBody, err := doGatewayJSON(ctx, unaryClient, http.MethodPost, baseURL+"/sandboxes/"+sandboxID+"/refreshes", apiKey, map[string]int{"duration": 300})
	if err != nil || (refStatus != http.StatusNoContent && refStatus != http.StatusOK) {
		return fmt.Errorf("POST /refreshes HTTP %d err=%v body=%s", refStatus, err, string(refBody))
	}

	totalOps.Add(1)
	getStatus, getBody, err := doGatewayJSON(ctx, unaryClient, http.MethodGet, baseURL+"/sandboxes/"+sandboxID, apiKey, nil)
	if err != nil || getStatus != http.StatusOK {
		return fmt.Errorf("GET /sandboxes/%s HTTP %d err=%v body=%s", sandboxID, getStatus, err, string(getBody))
	}
	lifecycleChecks.Add(1)

	// 2. E2B Filesystem write, read, list, and delete (CEP-0018).
	// On cold Kata block boot, podd-init may still be completing mkfs.ext4 + pivot_root
	// for a few seconds after Pod phase becomes Running before podd binds :49983.
	filePath := fmt.Sprintf("/workspace/soak-w%d-i%d.txt", workerID, iter)
	payload := []byte(fmt.Sprintf("soak-file-payload-worker-%d-iter-%d", workerID, iter))
	if err := collector.Measure("e2b_file_roundtrip", iter, int64(len(payload)), nil, func() error {
		putURL := fmt.Sprintf("%s/sandboxes/%s/files?path=%s", baseURL, sandboxID, url.QueryEscape(filePath))
		var putStatus int
		var putBody []byte
		readyDeadline := time.Now().Add(60 * time.Second)
		for {
			putReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPut, putURL, bytes.NewReader(payload))
			if reqErr != nil {
				return reqErr
			}
			putReq.Header.Set("Content-Type", "application/octet-stream")
			if apiKey != "" {
				putReq.Header.Set("X-API-Key", apiKey)
			}
			totalOps.Add(1)
			putResp, doErr := unaryClient.Do(putReq)
			if doErr != nil {
				return doErr
			}
			putStatus = putResp.StatusCode
			putBody, _ = io.ReadAll(putResp.Body)
			_ = putResp.Body.Close()
			if putStatus == http.StatusOK || putStatus == http.StatusCreated {
				break
			}
			if putStatus == http.StatusBadGateway && bytes.Contains(putBody, []byte("connection refused")) && time.Now().Before(readyDeadline) {
				time.Sleep(1 * time.Second)
				continue
			}
			return fmt.Errorf("PUT file HTTP %d: %s", putStatus, string(putBody))
		}

		getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, putURL, nil)
		if err != nil {
			return err
		}
		if apiKey != "" {
			getReq.Header.Set("X-API-Key", apiKey)
		}
		totalOps.Add(1)
		readResp, err := unaryClient.Do(getReq)
		if err != nil {
			return err
		}
		gotData, readErr := io.ReadAll(readResp.Body)
		_ = readResp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if readResp.StatusCode != http.StatusOK || !bytes.Equal(gotData, payload) {
			return fmt.Errorf("GET file HTTP %d mismatch: got %q want %q", readResp.StatusCode, string(gotData), string(payload))
		}

		listURL := fmt.Sprintf("%s/sandboxes/%s/files/list?path=%s", baseURL, sandboxID, url.QueryEscape("/workspace"))
		totalOps.Add(1)
		listStatus, _, err := doGatewayJSON(ctx, unaryClient, http.MethodGet, listURL, apiKey, nil)
		if err != nil || listStatus != http.StatusOK {
			return fmt.Errorf("GET /files/list HTTP %d err=%v", listStatus, err)
		}

		delReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, putURL, nil)
		if err != nil {
			return err
		}
		if apiKey != "" {
			delReq.Header.Set("X-API-Key", apiKey)
		}
		totalOps.Add(1)
		delResp, err := unaryClient.Do(delReq)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, delResp.Body)
		_ = delResp.Body.Close()
		if delResp.StatusCode != http.StatusOK && delResp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("DELETE file HTTP %d", delResp.StatusCode)
		}
		return nil
	}); err != nil {
		return err
	}
	fileChecks.Add(1)

	// 3. E2B Command execution (CEP-0018).
	marker := fmt.Sprintf("SOAK_CMD_%d_%d", workerID, iter)
	if err := collector.Measure("e2b_command_run", iter, 0, nil, func() error {
		totalOps.Add(1)
		cmdStatus, cmdBody, err := doGatewayJSON(ctx, unaryClient, http.MethodPost, baseURL+"/sandboxes/"+sandboxID+"/commands/run", apiKey, map[string]any{
			"cmd": fmt.Sprintf("printf '%s'", marker),
		})
		if err != nil || cmdStatus != http.StatusOK {
			return fmt.Errorf("POST /commands/run HTTP %d err=%v body=%s", cmdStatus, err, string(cmdBody))
		}
		if !bytes.Contains(cmdBody, []byte(marker)) {
			return fmt.Errorf("command output missing %q: %s", marker, string(cmdBody))
		}
		return nil
	}); err != nil {
		return err
	}
	commandChecks.Add(1)

	// 4. Interactive PTY Start, SendInput, Resize (Update), List, Disconnect + Connect replay, and SendSignal (CEP-0023).
	if err := collector.Measure("e2b_pty_interactive_cycle", iter, 0, nil, func() error {
		return runSoakPTYCycle(ctx, unaryClient, streamClient, baseURL, sandboxID, workerID, iter, totalOps, ptyResizes, ptyReconnects)
	}); err != nil {
		return err
	}
	ptySessions.Add(1)

	// Explicit DeleteSandbox so sandbox-hour accounting captures exact active lifetime.
	idToDelete := sandboxID
	sandboxID = ""
	totalOps.Add(1)
	if err := deleteSandbox(unaryClient, idToDelete); err != nil {
		return err
	}
	lifecycleChecks.Add(1)
	return nil
}

func runSoakPTYCycle(
	ctx context.Context,
	unaryClient *http.Client,
	streamClient *http.Client,
	baseURL, sandboxID string,
	workerID, iter int,
	totalOps, ptyResizes, ptyReconnects *atomic.Uint64,
) error {
	tag := fmt.Sprintf("soak-pty-%d-%d", workerID, iter)
	startCtx, startCancel := context.WithCancel(ctx)
	defer startCancel()

	totalOps.Add(1)
	resp, frameCh, err := startSandboxConnectStream(startCtx, streamClient, baseURL, sandboxID, "/process.Process/Start", map[string]any{
		"process": map[string]any{
			"cmd": "/bin/sh",
		},
		"tag": tag,
		"pty": map[string]any{
			"size": map[string]any{"cols": 80, "rows": 24},
		},
	})
	if err != nil {
		return fmt.Errorf("PTY Start: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return fmt.Errorf("PTY Start HTTP %d: %s", resp.StatusCode, string(b))
	}

	pid, err := readPTYStartPID(frameCh, 10*time.Second)
	if err != nil {
		_ = resp.Body.Close()
		return err
	}

	ptyToken := fmt.Sprintf("PTY_SOAK_%d_%d", workerID, iter)
	totalOps.Add(1)
	inStatus, inBody, err := postSandboxConnectJSON(ctx, unaryClient, baseURL, sandboxID, "/process.Process/SendInput", map[string]any{
		"process": map[string]any{"pid": pid},
		"input":   map[string]any{"pty": base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("printf '%s\\n'\n", ptyToken)))},
	})
	if err != nil || inStatus != http.StatusOK {
		_ = resp.Body.Close()
		return fmt.Errorf("PTY SendInput HTTP %d err=%v body=%s", inStatus, err, string(inBody))
	}

	if err := waitConnectPTYSubstr(frameCh, ptyToken, 10*time.Second); err != nil {
		_ = resp.Body.Close()
		return fmt.Errorf("initial PTY output: %w", err)
	}

	// Resize window 80x24 -> 120x40.
	totalOps.Add(1)
	upStatus, upBody, err := postSandboxConnectJSON(ctx, unaryClient, baseURL, sandboxID, "/process.Process/Update", map[string]any{
		"process": map[string]any{"pid": pid},
		"pty":     map[string]any{"size": map[string]any{"cols": 120, "rows": 40}},
	})
	if err != nil || upStatus != http.StatusOK {
		_ = resp.Body.Close()
		return fmt.Errorf("PTY Update HTTP %d err=%v body=%s", upStatus, err, string(upBody))
	}
	ptyResizes.Add(1)

	// Verify process appears in /process.Process/List.
	totalOps.Add(1)
	listStatus, listBody, err := postSandboxConnectJSON(ctx, unaryClient, baseURL, sandboxID, "/process.Process/List", map[string]any{})
	if err != nil || listStatus != http.StatusOK || !bytes.Contains(listBody, []byte(strconv.Itoa(pid))) {
		_ = resp.Body.Close()
		return fmt.Errorf("PTY List HTTP %d err=%v body=%s", listStatus, err, string(listBody))
	}

	// Disconnect initial Start stream; PTY must stay alive and replay ptyToken on Connect!
	startCancel()
	_ = resp.Body.Close()
	for range frameCh {
	}

	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	totalOps.Add(1)
	connResp, connCh, err := startSandboxConnectStream(connCtx, streamClient, baseURL, sandboxID, "/process.Process/Connect", map[string]any{
		"process": map[string]any{"pid": pid},
	})
	if err != nil {
		return fmt.Errorf("PTY Connect: %w", err)
	}
	defer func() {
		connCancel()
		_ = connResp.Body.Close()
		for range connCh {
		}
	}()
	if connResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(connResp.Body)
		return fmt.Errorf("PTY Connect HTTP %d: %s", connResp.StatusCode, string(b))
	}

	// Verify 64KiB replay tail delivers ptyToken on reconnect, plus new live input after reconnect.
	postReconnectToken := fmt.Sprintf("PTY_RECONN_%d_%d", workerID, iter)
	totalOps.Add(1)
	in2Status, in2Body, err := postSandboxConnectJSON(ctx, unaryClient, baseURL, sandboxID, "/process.Process/SendInput", map[string]any{
		"process": map[string]any{"pid": pid},
		"input":   map[string]any{"pty": base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("printf '%s\\n'\n", postReconnectToken)))},
	})
	if err != nil || in2Status != http.StatusOK {
		return fmt.Errorf("PTY post-Connect SendInput HTTP %d err=%v body=%s", in2Status, err, string(in2Body))
	}

	if err := waitConnectPTYSubstrs(connCh, []string{ptyToken, postReconnectToken}, 10*time.Second); err != nil {
		return fmt.Errorf("PTY Connect replay/live output: %w", err)
	}
	ptyReconnects.Add(1)

	// Terminate PTY cleanly with SIGKILL (9) and drain end-of-stream.
	totalOps.Add(1)
	killStatus, killBody, err := postSandboxConnectJSON(ctx, unaryClient, baseURL, sandboxID, "/process.Process/SendSignal", map[string]any{
		"process": map[string]any{"pid": pid},
		"signal":  9,
	})
	if err != nil || killStatus != http.StatusOK {
		return fmt.Errorf("PTY SendSignal HTTP %d err=%v body=%s", killStatus, err, string(killBody))
	}

	drainTimer := time.NewTimer(5 * time.Second)
	defer drainTimer.Stop()
	for {
		select {
		case res, ok := <-connCh:
			if !ok || errors.Is(res.err, io.EOF) || (res.frame != nil && res.frame.Flag&0x02 != 0) {
				return nil
			}
			if res.err != nil {
				return fmt.Errorf("PTY Connect stream error after kill: %w", res.err)
			}
		case <-drainTimer.C:
			return fmt.Errorf("timed out waiting for PTY stream termination after SendSignal")
		}
	}
}

func readPTYStartPID(frameCh <-chan struct {
	frame *connectFrame
	err   error
}, timeout time.Duration) (int, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case res, ok := <-frameCh:
			if !ok || res.err != nil {
				return 0, fmt.Errorf("PTY stream closed before start event (ok=%v err=%v)", ok, res.err)
			}
			var ev struct {
				Event struct {
					Start struct {
						Pid int `json:"pid"`
					} `json:"start"`
				} `json:"event"`
			}
			if json.Unmarshal(res.frame.Payload, &ev) == nil && ev.Event.Start.Pid > 0 {
				return ev.Event.Start.Pid, nil
			}
		case <-timer.C:
			return 0, fmt.Errorf("timed out waiting for PTY start pid")
		}
	}
}

func waitConnectPTYSubstr(frameCh <-chan struct {
	frame *connectFrame
	err   error
}, want string, timeout time.Duration) error {
	return waitConnectPTYSubstrs(frameCh, []string{want}, timeout)
}

func waitConnectPTYSubstrs(frameCh <-chan struct {
	frame *connectFrame
	err   error
}, wants []string, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var acc strings.Builder
	allPresent := func(s string) bool {
		for _, w := range wants {
			if !strings.Contains(s, w) {
				return false
			}
		}
		return true
	}
	for {
		select {
		case res, ok := <-frameCh:
			if !ok || res.err != nil {
				return fmt.Errorf("stream ended before %v (got %q, ok=%v, err=%v)", wants, acc.String(), ok, res.err)
			}
			if res.frame != nil && res.frame.Flag == 0 {
				var ev struct {
					Event struct {
						Data struct {
							Pty string `json:"pty"`
						} `json:"data"`
					} `json:"event"`
				}
				if json.Unmarshal(res.frame.Payload, &ev) == nil && ev.Event.Data.Pty != "" {
					if decoded, err := base64.StdEncoding.DecodeString(ev.Event.Data.Pty); err == nil {
						acc.Write(decoded)
						if allPresent(acc.String()) {
							return nil
						}
					}
				}
			}
		case <-timer.C:
			return fmt.Errorf("timed out waiting for %v; accumulated=%q", wants, acc.String())
		}
	}
}

// TestSoakRunnerShortVerification runs in <3s under go test -race against an in-process
// gateway.Server, exercising concurrent E2B lifecycle/files/commands + real OS PTY
// Start/SendInput/Update/Disconnect/Connect/SendSignal cycles, sandbox-hour accounting,
// zero sandbox/goroutine leaks, and structured JSON/Markdown report output.
func TestSoakRunnerShortVerification(t *testing.T) {
	// Verify deterministic >10,000 sandbox-hour accounting math for multi-day GA signoff.
	// Example: 150 concurrent sandboxes * 72h (259,200s each) = 38,880,000 sandbox-seconds = 10,800 sandbox-hours.
	if gotHours := ComputeSandboxHours(150 * 72 * 3600); gotHours != 10800 {
		t.Fatalf("ComputeSandboxHours(150*72h) = %v, want 10800", gotHours)
	}

	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	baselineGoroutines := runtime.NumGoroutine()

	reconciler := controller.NewReconciler()
	gw := gateway.NewServer(reconciler)
	gw.SetWorkspaceBaseDir(t.TempDir())
	ts := httptest.NewServer(gw.Handler())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := time.Now()
	report := RunSoakSuite(ctx, SoakConfig{
		SuiteName:          "CubePod Short Concurrent E2B & PTY Soak Verification",
		GatewayURL:         ts.URL,
		TemplateID:         "base",
		Duration:           2500 * time.Millisecond,
		Concurrency:        3,
		MaxCyclesPerWorker: 2,
		Environment: map[string]string{
			"mode": "in-process-short-verification",
		},
	})
	elapsed := time.Since(start)

	if report.FailedOperations != 0 {
		t.Fatalf("expected 0 failed operations, got %d (errors=%v)", report.FailedOperations, report.SampleErrors)
	}
	if report.CompletedCycles != 6 {
		t.Fatalf("expected 6 completed cycles (3 workers x 2 cycles), got %d", report.CompletedCycles)
	}
	if report.PTYSessionsVerified != 6 || report.PTYResizesVerified != 6 || report.PTYReconnectsVerified != 6 {
		t.Fatalf("unexpected PTY verification counts: sessions=%d resizes=%d reconnects=%d",
			report.PTYSessionsVerified, report.PTYResizesVerified, report.PTYReconnectsVerified)
	}
	if report.E2BLifecycleChecks < 12 || report.E2BFileChecks != 6 || report.E2BCommandChecks != 6 {
		t.Fatalf("unexpected E2B check counts: lifecycle=%d file=%d cmd=%d",
			report.E2BLifecycleChecks, report.E2BFileChecks, report.E2BCommandChecks)
	}
	if report.SandboxSecondsTotal <= 0 || report.SandboxHours <= 0 {
		t.Fatalf("expected positive sandbox-hour accounting, got seconds=%v hours=%v",
			report.SandboxSecondsTotal, report.SandboxHours)
	}
	if report.ActiveSandboxesLeft != 0 {
		t.Fatalf("expected 0 leaked sandboxes after soak suite, got %d", report.ActiveSandboxesLeft)
	}

	// Verify structured JSON and Markdown report serialization and file export.
	outBase := filepath.Join(t.TempDir(), "soak-short-report.md")
	jsonPath, mdPath, err := writeSoakReportFiles(report, outBase)
	if err != nil {
		t.Fatalf("writeSoakReportFiles: %v", err)
	}
	rawJSON, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read JSON report: %v", err)
	}
	var decoded SoakReport
	if err := json.Unmarshal(rawJSON, &decoded); err != nil {
		t.Fatalf("decode JSON report: %v", err)
	}
	if decoded.CompletedCycles != 6 || decoded.PTYSessionsVerified != 6 {
		t.Fatalf("decoded JSON report mismatch: %+v", decoded)
	}
	rawMD, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("read Markdown report: %v", err)
	}
	for _, want := range []string{
		"# CubePod Short Concurrent E2B & PTY Soak Verification",
		"Accumulated Sandbox Time",
		"Interactive PTY Sessions",
		"PTY Stream Disconnect & Replay Reconnects",
		"`e2b_pty_interactive_cycle`",
	} {
		if !strings.Contains(string(rawMD), want) {
			t.Fatalf("Markdown report missing %q:\n%s", want, string(rawMD))
		}
	}

	// Shut down server and verify zero goroutine leaks.
	ts.CloseClientConnections()
	ts.Close()
	gw.Close()

	leakedGoroutines := 0
	for i := 0; i < 25; i++ {
		runtime.GC()
		runtime.Gosched()
		cur := runtime.NumGoroutine()
		if cur <= baselineGoroutines+4 {
			leakedGoroutines = 0
			break
		}
		leakedGoroutines = cur - baselineGoroutines
		time.Sleep(20 * time.Millisecond)
	}
	if leakedGoroutines > 4 {
		t.Fatalf("goroutine leak detected after soak verification: baseline=%d current=%d (+%d)",
			baselineGoroutines, runtime.NumGoroutine(), leakedGoroutines)
	}

	t.Logf("[Soak-Short] Completed 6 concurrent E2B+PTY cycles in %v: ops=%d sandboxSeconds=%.3fs goroutines=%d->%d",
		elapsed, report.TotalOperations, report.SandboxSecondsTotal, baselineGoroutines, runtime.NumGoroutine())
}

// TestK8sLiveSoakE2BAndPTY runs multi-hour / multi-day concurrent E2B protocol and
// interactive PTY soak verification against a live Kubernetes cluster (targeting >10,000
// sandbox-hours for CEP-0018 & CEP-0023 Stable GA signoff).
// Strictly gated on CUBEPOD_K8S_SOAK=1.
func TestK8sLiveSoakE2BAndPTY(t *testing.T) {
	if os.Getenv("CUBEPOD_K8S_SOAK") != "1" {
		t.Skip("Skipping live E2B + PTY soak suite: set CUBEPOD_K8S_SOAK=1 (with CUBEPOD_SOAK_DURATION and CUBEPOD_SOAK_CONCURRENCY)")
	}

	cfg := loadClusterConfig(t)
	if cfg.GatewayURL == "" {
		t.Fatal("set CUBEPOD_GATEWAY_URL or expose svc/cubepod-gateway for live soak run")
	}
	if cfg.SandboxImage == "" {
		t.Fatal("set CUBEPOD_SANDBOX_IMAGE or configure -sandbox-image on cubepod-gateway")
	}

	duration := 1 * time.Hour
	if rawDur := strings.TrimSpace(os.Getenv("CUBEPOD_SOAK_DURATION")); rawDur != "" {
		parsed, err := time.ParseDuration(rawDur)
		if err != nil || parsed <= 0 {
			t.Fatalf("invalid CUBEPOD_SOAK_DURATION=%q: %v", rawDur, err)
		}
		duration = parsed
	}

	concurrency := 4
	if rawConc := strings.TrimSpace(os.Getenv("CUBEPOD_SOAK_CONCURRENCY")); rawConc != "" {
		parsed, err := strconv.Atoi(rawConc)
		if err != nil || parsed <= 0 {
			t.Fatalf("invalid CUBEPOD_SOAK_CONCURRENCY=%q: %v", rawConc, err)
		}
		concurrency = parsed
	}

	var targetHours float64
	if rawTarget := strings.TrimSpace(os.Getenv("CUBEPOD_SOAK_TARGET_HOURS")); rawTarget != "" {
		parsed, err := strconv.ParseFloat(rawTarget, 64)
		if err != nil || parsed < 0 {
			t.Fatalf("invalid CUBEPOD_SOAK_TARGET_HOURS=%q: %v", rawTarget, err)
		}
		targetHours = parsed
	}

	ctx, cancel := context.WithTimeout(context.Background(), duration+10*time.Minute)
	defer cancel()

	t.Logf("[K8s-Soak] Starting live E2B + PTY soak: gateway=%s image=%s duration=%s concurrency=%d targetHours=%.1f",
		cfg.GatewayURL, cfg.SandboxImage, duration, concurrency, targetHours)

	report := RunSoakSuite(ctx, SoakConfig{
		SuiteName:          "CubePod Live Kubernetes E2B & Interactive PTY Soak",
		GatewayURL:         cfg.GatewayURL,
		TemplateID:         cfg.SandboxImage,
		APIKey:             os.Getenv("CUBEPOD_API_KEY"),
		Duration:           duration,
		Concurrency:        concurrency,
		TargetSandboxHours: targetHours,
		Environment: map[string]string{
			"gatewayURL":   cfg.GatewayURL,
			"namespace":    cfg.Namespace,
			"storageClass": cfg.StorageClass,
			"sandboxImage": cfg.SandboxImage,
		},
	})

	if outPath := strings.TrimSpace(os.Getenv("CUBEPOD_SOAK_OUT")); outPath != "" {
		jsonPath, mdPath, err := writeSoakReportFiles(report, outPath)
		if err != nil {
			t.Fatalf("failed to write soak report to %s: %v", outPath, err)
		}
		t.Logf("[K8s-Soak] Wrote soak reports: json=%s markdown=%s", jsonPath, mdPath)
	} else {
		t.Logf("[K8s-Soak] Summary:\n%s", report.RenderMarkdown())
	}

	if report.FailedOperations > 0 {
		t.Fatalf("live soak recorded %d failed operations out of %d: %v",
			report.FailedOperations, report.TotalOperations, report.SampleErrors)
	}
	if report.ActiveSandboxesLeft > 0 {
		t.Fatalf("live soak leaked %d active sandboxes", report.ActiveSandboxesLeft)
	}
	if targetHours > 0 && !report.TargetHoursMet {
		t.Fatalf("live soak accumulated %.2f sandbox-hours, below target %.2f sandbox-hours",
			report.SandboxHours, targetHours)
	}
}
