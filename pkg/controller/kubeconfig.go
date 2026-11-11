package controller

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KubeconfigInfo holds parsed Kubernetes authentication and endpoint details.
type KubeconfigInfo struct {
	Server                string
	Namespace             string
	Token                 string
	ClientCertificateData []byte
	ClientKeyData         []byte
	CACertificateData     []byte
	InsecureSkipTLSVerify bool
}

// ParseKubeconfig parses kubeconfig YAML bytes into KubeconfigInfo using standard library line parsing and regexes.
func ParseKubeconfig(content []byte) (*KubeconfigInfo, error) {
	info := &KubeconfigInfo{}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if cIdx := strings.Index(val, " #"); cIdx != -1 {
			val = strings.TrimSpace(val[:cIdx])
		}
		val = strings.Trim(val, `"'`)

		switch key {
		case "server":
			if info.Server == "" {
				info.Server = val
			}
		case "namespace":
			if info.Namespace == "" {
				info.Namespace = val
			}
		case "token":
			if info.Token == "" {
				info.Token = val
			}
		case "insecure-skip-tls-verify":
			info.InsecureSkipTLSVerify = strings.EqualFold(val, "true")
		case "certificate-authority-data":
			if len(info.CACertificateData) == 0 {
				if data, err := base64.StdEncoding.DecodeString(val); err == nil {
					info.CACertificateData = data
				}
			}
		case "certificate-authority":
			if len(info.CACertificateData) == 0 {
				if data, err := os.ReadFile(val); err == nil {
					info.CACertificateData = data
				}
			}
		case "client-certificate-data":
			if len(info.ClientCertificateData) == 0 {
				if data, err := base64.StdEncoding.DecodeString(val); err == nil {
					info.ClientCertificateData = data
				}
			}
		case "client-certificate":
			if len(info.ClientCertificateData) == 0 {
				if data, err := os.ReadFile(val); err == nil {
					info.ClientCertificateData = data
				}
			}
		case "client-key-data":
			if len(info.ClientKeyData) == 0 {
				if data, err := base64.StdEncoding.DecodeString(val); err == nil {
					info.ClientKeyData = data
				}
			}
		case "client-key":
			if len(info.ClientKeyData) == 0 {
				if data, err := os.ReadFile(val); err == nil {
					info.ClientKeyData = data
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan kubeconfig: %w", err)
	}
	return info, nil
}

// LoadKubeconfig reads and parses a kubeconfig file from the specified path,
// or defaults to $KUBECONFIG or ~/.kube/config.
func LoadKubeconfig(path string) (*KubeconfigInfo, error) {
	if path == "" {
		path = os.Getenv("KUBECONFIG")
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("unable to determine home directory: %w", err)
		}
		path = filepath.Join(home, ".kube", "config")
	} else if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			path = filepath.Join(home, path[2:])
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read kubeconfig at %s: %w", path, err)
	}
	return ParseKubeconfig(data)
}

// LoadInClusterConfig reads Kubernetes credentials provided by in-cluster service account secrets.
func LoadInClusterConfig() (*KubeconfigInfo, error) {
	tokenPath := "/var/run/secrets/kubernetes.io/serviceaccount/token"
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("in-cluster token not found: %w", err)
	}

	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		host = "kubernetes.default.svc"
	}
	if port == "" {
		port = "443"
	}
	serverURL := fmt.Sprintf("https://%s:%s", host, port)

	info := &KubeconfigInfo{
		Server: serverURL,
		Token:  strings.TrimSpace(string(tokenBytes)),
	}

	if nsBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		info.Namespace = strings.TrimSpace(string(nsBytes))
	}
	if caBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"); err == nil {
		info.CACertificateData = caBytes
	}

	return info, nil
}

// buildTLSConfigFromKubeconfig constructs a *tls.Config from parsed KubeconfigInfo.
func buildTLSConfigFromKubeconfig(info *KubeconfigInfo) (*tls.Config, error) {
	if info == nil {
		return &tls.Config{}, nil
	}
	tlsConfig := &tls.Config{
		InsecureSkipVerify: info.InsecureSkipTLSVerify,
	}
	if len(info.ClientCertificateData) > 0 && len(info.ClientKeyData) > 0 {
		cert, err := tls.X509KeyPair(info.ClientCertificateData, info.ClientKeyData)
		if err != nil {
			return nil, fmt.Errorf("failed to parse client certificate/key: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	if len(info.CACertificateData) > 0 && !info.InsecureSkipTLSVerify {
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(info.CACertificateData) {
			return nil, fmt.Errorf("failed to append CA certificate to pool")
		}
		tlsConfig.RootCAs = caPool
	}
	return tlsConfig, nil
}

// NewRESTKubeClientFromKubeconfig initializes a RESTKubeClient using a parsed KubeconfigInfo.
func NewRESTKubeClientFromKubeconfig(info *KubeconfigInfo, opts ...RESTKubeClientOption) (*RESTKubeClient, error) {
	if info == nil {
		return nil, fmt.Errorf("kubeconfig info is nil")
	}
	cfg := RESTKubeClientConfig{
		APIServerURL:          info.Server,
		Namespace:             info.Namespace,
		BearerToken:           info.Token,
		InsecureTLS:           info.InsecureSkipTLSVerify,
		ClientCertificateData: info.ClientCertificateData,
		ClientKeyData:         info.ClientKeyData,
		CACertificateData:     info.CACertificateData,
	}
	c, err := NewRESTKubeClientWithConfig(cfg)
	if err != nil {
		return nil, err
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// NewRESTKubeClientFromEnv initializes a RESTKubeClient by checking $KUBECONFIG, ~/.kube/config,
// or in-cluster ServiceAccount credentials.
func NewRESTKubeClientFromEnv(opts ...RESTKubeClientOption) (*RESTKubeClient, error) {
	info, err := LoadKubeconfig("")
	if err == nil && info.Server != "" {
		c, err := NewRESTKubeClientFromKubeconfig(info, opts...)
		if err == nil {
			return c, nil
		}
	}

	inClusterInfo, inErr := LoadInClusterConfig()
	if inErr == nil && inClusterInfo.Server != "" {
		c, err := NewRESTKubeClientFromKubeconfig(inClusterInfo, opts...)
		if err == nil {
			return c, nil
		}
	}

	if err != nil {
		return nil, fmt.Errorf("failed to initialize KubeClient from env: kubeconfig error (%w), in-cluster error (%v)", err, inErr)
	}
	return nil, fmt.Errorf("no valid Kubernetes configuration found in environment")
}
