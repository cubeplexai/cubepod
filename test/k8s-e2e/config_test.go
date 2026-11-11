package k8s_e2e_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFlagValue(t *testing.T) {
	args := []string{
		"-namespace=cubepod-workloads",
		"-storage-class=longhorn-v2-block-thin",
		"--sandbox-image=example.com/sandbox:1",
	}
	if got := flagValue(args, "-storage-class"); got != "longhorn-v2-block-thin" {
		t.Fatalf("storage class = %q", got)
	}
	if got := flagValue(args, "-sandbox-image"); got != "example.com/sandbox:1" {
		t.Fatalf("image = %q", got)
	}
	if got := flagValue(args, "-missing"); got != "" {
		t.Fatalf("missing flag = %q", got)
	}
}

func TestServiceEndpointNodePort(t *testing.T) {
	svc := map[string]any{
		"spec": map[string]any{
			"type": "NodePort",
			"ports": []any{
				map[string]any{"name": "http", "port": float64(8080), "nodePort": float64(32380)},
			},
		},
	}
	if got := serviceEndpoint(svc, "10.0.0.8"); got != "http://10.0.0.8:32380" {
		t.Fatalf("nodeport url = %q", got)
	}
	if got := serviceEndpoint(svc, ""); got != "" {
		t.Fatalf("nodeport without a node IP = %q", got)
	}
	if got := serviceEndpoint(svc, "2001:db8::8"); got != "http://[2001:db8::8]:32380" {
		t.Fatalf("ipv6 nodeport url = %q", got)
	}
}

func TestServiceEndpointLoadBalancer(t *testing.T) {
	svc := map[string]any{
		"spec": map[string]any{
			"type": "LoadBalancer",
			"ports": []any{
				map[string]any{"name": "http", "port": float64(8080), "nodePort": float64(30000)},
			},
		},
		"status": map[string]any{
			"loadBalancer": map[string]any{
				"ingress": []any{map[string]any{"ip": "203.0.113.10"}},
			},
		},
	}
	if got := serviceEndpoint(svc, "10.0.0.8"); got != "http://203.0.113.10:8080" {
		t.Fatalf("loadbalancer url = %q", got)
	}
}

func TestServiceEndpointHTTPS(t *testing.T) {
	svc := map[string]any{
		"spec": map[string]any{
			"type": "LoadBalancer",
			"ports": []any{
				map[string]any{"name": "https", "port": float64(443)},
			},
		},
		"status": map[string]any{
			"loadBalancer": map[string]any{
				"ingress": []any{map[string]any{"hostname": "gw.example.com"}},
			},
		},
	}
	if got := serviceEndpoint(svc, ""); got != "https://gw.example.com:443" {
		t.Fatalf("https url = %q", got)
	}
}

func TestSelectGatewayService(t *testing.T) {
	labeled := map[string]any{
		"metadata": map[string]any{
			"name":   "prod-cubepod-gateway",
			"labels": map[string]any{"app.kubernetes.io/component": "gateway"},
		},
	}
	other := map[string]any{
		"metadata": map[string]any{"name": "ingress-gateway"},
	}
	got := selectGatewayService([]any{other, labeled})
	if objectName(got) != "prod-cubepod-gateway" {
		t.Fatalf("selected %q", objectName(got))
	}
	if selectGatewayService([]any{other, map[string]any{"metadata": map[string]any{"name": "alt-gateway"}}}) != nil {
		t.Fatal("two name-only gateway services should not be guessed")
	}
}

func TestHostIPForDeployment(t *testing.T) {
	raw := `{
	  "items": [
	    {"metadata": {"name": "other", "labels": {"app": "other"}}, "status": {"phase": "Running", "hostIP": "10.0.0.1"}},
	    {"metadata": {"name": "cubepod-gateway-abc-def", "labels": {"app": "cubepod-gateway"}}, "status": {"phase": "Running", "hostIP": "192.0.2.14"}}
	  ]
	}`
	if got := hostIPForDeployment(raw, "cubepod-gateway"); got != "192.0.2.14" {
		t.Fatalf("hostIP = %q", got)
	}
}

func TestSiblingName(t *testing.T) {
	if got := siblingName("prod-cubepod-gateway", "gateway", "controller"); got != "prod-cubepod-controller" {
		t.Fatalf("sibling = %q", got)
	}
}

func TestLoadClusterConfigLive(t *testing.T) {
	if os.Getenv("CUBEPOD_DISCOVER_CHECK") != "1" {
		t.Skip("set CUBEPOD_DISCOVER_CHECK=1 to resolve the current kubeconfig")
	}
	cfg := loadClusterConfig(t)
	if cfg.GatewayURL == "" || cfg.SandboxImage == "" || cfg.StorageClass == "" || cfg.Namespace == "" {
		t.Fatalf("discovery incomplete: %+v", cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.GatewayURL, "/")+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s/health: %v", cfg.GatewayURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status %d: %s", resp.StatusCode, body)
	}
	t.Logf("health %s", strings.TrimSpace(string(body)))
}
