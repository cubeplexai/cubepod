package k8s_e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// clusterConfig is everything a live verification needs from the target cluster.
// Set the fields with environment variables, or leave them empty and the loader
// reads the current kubeconfig: the gateway Service, and the gateway or
// controller Deployment's -sandbox-image, -storage-class, and -namespace arguments.
type clusterConfig struct {
	GatewayURL        string
	GatewayName       string
	Namespace         string
	SystemNamespace   string
	SandboxImage      string
	StorageClass      string
	LonghornNamespace string
	NodeIP            string
}

func loadClusterConfig(t *testing.T) clusterConfig {
	t.Helper()
	cfg := clusterConfig{
		GatewayURL:        os.Getenv("CUBEPOD_GATEWAY_URL"),
		GatewayName:       os.Getenv("CUBEPOD_GATEWAY_NAME"),
		Namespace:         getenv("CUBEPOD_WORKLOADS_NAMESPACE", "cubepod-workloads"),
		SystemNamespace:   getenv("CUBEPOD_SYSTEM_NAMESPACE", "cubepod-system"),
		SandboxImage:      os.Getenv("CUBEPOD_SANDBOX_IMAGE"),
		StorageClass:      os.Getenv("CUBEPOD_STORAGE_CLASS"),
		LonghornNamespace: getenv("CUBEPOD_LONGHORN_NAMESPACE", "longhorn-system"),
		NodeIP:            os.Getenv("CUBEPOD_NODE_IP"),
	}
	discoverClusterConfig(t, &cfg)
	t.Logf("[K8s-E2E] cluster config gateway=%s name=%s namespace=%s image=%s storageClass=%s",
		cfg.GatewayURL, cfg.GatewayName, cfg.Namespace, cfg.SandboxImage, cfg.StorageClass)
	return cfg
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func discoverClusterConfig(t *testing.T, cfg *clusterConfig) {
	t.Helper()
	if cfg.SystemNamespace == "" {
		cfg.SystemNamespace = "cubepod-system"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	explicit := cfg.GatewayName
	svc, svcName := lookupGatewayService(ctx, t, cfg.SystemNamespace, explicit)
	if svcName != "" {
		cfg.GatewayName = svcName
	}
	if cfg.GatewayURL == "" && svc != nil {
		nodeIP := cfg.NodeIP
		if nodeIP == "" && serviceType(svc) == "NodePort" {
			nodeIP = gatewayPodHostIP(ctx, cfg.SystemNamespace, svcName)
			if nodeIP == "" {
				nodeIP = firstSchedulableNodeIP(ctx)
			}
		}
		if url := serviceEndpoint(svc, nodeIP); url != "" {
			cfg.GatewayURL = url
		} else {
			t.Logf("[K8s-E2E] service %s/%s has no NodePort or LoadBalancer address", cfg.SystemNamespace, svcName)
		}
	}

	if cfg.GatewayName == "" {
		cfg.GatewayName = "cubepod-gateway"
	}
	args, err := deploymentArgs(ctx, cfg.SystemNamespace, cfg.GatewayName)
	if err != nil {
		t.Logf("[K8s-E2E] deployment %s/%s: %v", cfg.SystemNamespace, cfg.GatewayName, err)
	}
	if ctrl := siblingName(cfg.GatewayName, "gateway", "controller"); ctrl != "" {
		cargs, cerr := deploymentArgs(ctx, cfg.SystemNamespace, ctrl)
		if cerr != nil {
			t.Logf("[K8s-E2E] deployment %s/%s: %v", cfg.SystemNamespace, ctrl, cerr)
		}
		args = append(args, cargs...)
	}
	if cfg.SandboxImage == "" {
		cfg.SandboxImage = flagValue(args, "-sandbox-image")
	}
	if cfg.StorageClass == "" {
		cfg.StorageClass = flagValue(args, "-storage-class")
	}
	if os.Getenv("CUBEPOD_WORKLOADS_NAMESPACE") == "" {
		if ns := flagValue(args, "-namespace"); ns != "" {
			cfg.Namespace = ns
		}
	}
}

func lookupGatewayService(ctx context.Context, t *testing.T, ns, explicit string) (map[string]any, string) {
	t.Helper()
	if explicit != "" {
		svc, err := getService(ctx, ns, explicit)
		if err != nil {
			t.Logf("[K8s-E2E] service %s/%s: %v", ns, explicit, err)
			return nil, explicit
		}
		return svc, explicit
	}
	svc, err := getService(ctx, ns, "cubepod-gateway")
	if err == nil {
		return svc, "cubepod-gateway"
	}
	t.Logf("[K8s-E2E] svc/cubepod-gateway: %v; searching namespace %s", err, ns)
	raw, listErr := kubectlOutput(ctx, "get", "svc", "-n", ns, "-o", "json")
	if listErr != nil {
		t.Logf("[K8s-E2E] list services: %v: %s", listErr, strings.TrimSpace(raw))
		return nil, ""
	}
	var list struct {
		Items []any `json:"items"`
	}
	if json.Unmarshal([]byte(raw), &list) != nil {
		return nil, ""
	}
	chosen := selectGatewayService(list.Items)
	if chosen == nil {
		t.Logf("[K8s-E2E] namespace %s has no single gateway Service; set CUBEPOD_GATEWAY_NAME", ns)
		return nil, ""
	}
	return chosen, objectName(chosen)
}

func getService(ctx context.Context, ns, name string) (map[string]any, error) {
	raw, err := kubectlOutput(ctx, "get", "svc", name, "-n", ns, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(raw))
	}
	var svc map[string]any
	if err := json.Unmarshal([]byte(raw), &svc); err != nil {
		return nil, err
	}
	return svc, nil
}

func selectGatewayService(items []any) map[string]any {
	var labeled, named []map[string]any
	for _, item := range items {
		svc, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch gatewayServiceMatch(svc) {
		case "label":
			labeled = append(labeled, svc)
		case "name":
			named = append(named, svc)
		}
	}
	if len(labeled) == 1 {
		return labeled[0]
	}
	if len(labeled) == 0 && len(named) == 1 {
		return named[0]
	}
	return nil
}

func gatewayServiceMatch(svc map[string]any) string {
	meta, _ := svc["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	labels, _ := meta["labels"].(map[string]any)
	spec, _ := svc["spec"].(map[string]any)
	selector, _ := spec["selector"].(map[string]any)
	if selector["app"] == "cubepod-gateway" || labels["app"] == "cubepod-gateway" || labels["app.kubernetes.io/component"] == "gateway" {
		return "label"
	}
	if strings.HasSuffix(name, "-gateway") {
		return "name"
	}
	return ""
}

func objectName(obj map[string]any) string {
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	return name
}

func deploymentArgs(ctx context.Context, ns, name string) ([]string, error) {
	if name == "" {
		return nil, fmt.Errorf("empty deployment name")
	}
	raw, err := kubectlOutput(ctx, "get", "deploy", name, "-n", ns, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(raw))
	}
	var deploy map[string]any
	if err := json.Unmarshal([]byte(raw), &deploy); err != nil {
		return nil, err
	}
	spec, _ := deploy["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	podSpec, _ := template["spec"].(map[string]any)
	containers, _ := podSpec["containers"].([]any)
	if len(containers) == 0 {
		return nil, fmt.Errorf("deployment has no containers")
	}
	c0, _ := containers[0].(map[string]any)
	rawArgs, _ := c0["args"].([]any)
	args := make([]string, 0, len(rawArgs))
	for _, a := range rawArgs {
		if s, ok := a.(string); ok {
			args = append(args, s)
		}
	}
	return args, nil
}

func siblingName(name, from, to string) string {
	suffix := "-" + from
	if !strings.HasSuffix(name, suffix) {
		return ""
	}
	return strings.TrimSuffix(name, suffix) + "-" + to
}

// flagValue reads a Go-style -name=value or --name=value argument.
func flagValue(args []string, name string) string {
	names := []string{name}
	if strings.HasPrefix(name, "-") && !strings.HasPrefix(name, "--") {
		names = append(names, "-"+name)
	}
	for _, arg := range args {
		for _, n := range names {
			prefix := n + "="
			if strings.HasPrefix(arg, prefix) {
				return strings.TrimPrefix(arg, prefix)
			}
		}
	}
	return ""
}

// serviceEndpoint returns an HTTP URL for a Kubernetes Service.
// NodePort uses nodeIP. LoadBalancer uses the first ingress host.
// A port named https is returned as an https URL.
func serviceEndpoint(svc map[string]any, nodeIP string) string {
	spec, _ := svc["spec"].(map[string]any)
	port, nodePort, scheme := servicePorts(spec)
	switch serviceType(svc) {
	case "LoadBalancer":
		if host := loadBalancerHost(svc); host != "" && port > 0 {
			return fmt.Sprintf("%s://%s:%d", scheme, hostForURL(host), port)
		}
	case "NodePort":
		if nodeIP != "" && nodePort > 0 {
			return fmt.Sprintf("%s://%s:%d", scheme, hostForURL(nodeIP), nodePort)
		}
	}
	return ""
}

func serviceType(svc map[string]any) string {
	spec, _ := svc["spec"].(map[string]any)
	typ, _ := spec["type"].(string)
	if typ == "" {
		return "ClusterIP"
	}
	return typ
}

func servicePorts(spec map[string]any) (port, nodePort int, scheme string) {
	ports, _ := spec["ports"].([]any)
	chosen := pickServicePort(ports)
	if chosen == nil {
		return 0, 0, "http"
	}
	scheme = "http"
	if chosen["name"] == "https" {
		scheme = "https"
	}
	return jsonInt(chosen["port"]), jsonInt(chosen["nodePort"]), scheme
}

func pickServicePort(ports []any) map[string]any {
	var first, httpPort, httpsPort map[string]any
	for _, p := range ports {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if first == nil {
			first = pm
		}
		switch pm["name"] {
		case "http":
			httpPort = pm
		case "https":
			httpsPort = pm
		}
	}
	if httpPort != nil {
		return httpPort
	}
	if httpsPort != nil {
		return httpsPort
	}
	return first
}

func hostForURL(host string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]"
	}
	return host
}

func loadBalancerHost(svc map[string]any) string {
	status, _ := svc["status"].(map[string]any)
	lb, _ := status["loadBalancer"].(map[string]any)
	ingress, _ := lb["ingress"].([]any)
	if len(ingress) == 0 {
		return ""
	}
	first, _ := ingress[0].(map[string]any)
	if ip, _ := first["ip"].(string); ip != "" {
		return ip
	}
	host, _ := first["hostname"].(string)
	return host
}

func jsonInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

func gatewayPodHostIP(ctx context.Context, ns, deployName string) string {
	raw, err := kubectlOutput(ctx, "get", "pods", "-n", ns, "-o", "json")
	if err != nil {
		return ""
	}
	return hostIPForDeployment(raw, deployName)
}

func hostIPForDeployment(raw, deployName string) string {
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Phase  string `json:"phase"`
				HostIP string `json:"hostIP"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(raw), &list) != nil {
		return ""
	}
	prefix := ""
	if deployName != "" {
		prefix = deployName + "-"
	}
	for _, pod := range list.Items {
		if pod.Status.Phase != "Running" || pod.Status.HostIP == "" {
			continue
		}
		if prefix != "" && strings.HasPrefix(pod.Metadata.Name, prefix) {
			return pod.Status.HostIP
		}
	}
	for _, pod := range list.Items {
		if pod.Status.Phase != "Running" || pod.Status.HostIP == "" {
			continue
		}
		if pod.Metadata.Labels["app"] == "cubepod-gateway" || pod.Metadata.Labels["app.kubernetes.io/component"] == "gateway" {
			return pod.Status.HostIP
		}
	}
	return ""
}

func firstSchedulableNodeIP(ctx context.Context) string {
	raw, err := kubectlOutput(ctx, "get", "nodes", "-o", "json")
	if err != nil {
		return ""
	}
	var list struct {
		Items []struct {
			Spec struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				Addresses []struct {
					Type    string `json:"type"`
					Address string `json:"address"`
				} `json:"addresses"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(raw), &list) != nil {
		return ""
	}
	for _, node := range list.Items {
		if node.Spec.Unschedulable {
			continue
		}
		ready := false
		for _, cond := range node.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == "True" {
				ready = true
			}
		}
		if !ready {
			continue
		}
		var external string
		for _, addr := range node.Status.Addresses {
			if addr.Address == "" {
				continue
			}
			if addr.Type == "InternalIP" {
				return addr.Address
			}
			if addr.Type == "ExternalIP" && external == "" {
				external = addr.Address
			}
		}
		if external != "" {
			return external
		}
	}
	return ""
}
