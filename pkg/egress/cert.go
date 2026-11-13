// Package egress implements zero-trust egress network policies, mTLS sandbox
// client certificate issuance and verification, and secret placeholder vault exchange.
package egress

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// CA manages the in-memory root certificate authority used to mint and verify
// per-sandbox mTLS client certificates and selective MITM server certificates.
type CA struct {
	caCert    *x509.Certificate
	caKey     crypto.Signer
	caCertPEM []byte
	certPool  *x509.CertPool
}

// NewCA creates a new in-memory ECDSA P-256 Certificate Authority.
func NewCA() (*CA, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA private key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CA serial number: %w", err)
	}

	notBefore := time.Now().Add(-1 * time.Hour)
	notAfter := notBefore.Add(10 * 365 * 24 * time.Hour)

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "CubePod Egress Root CA",
			Organization: []string{"CubePod"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create self-signed CA certificate: %w", err)
	}

	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("failed to parse created CA certificate: %w", err)
	}

	caCertPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: caDER,
	})

	certPool := x509.NewCertPool()
	certPool.AddCert(caCert)

	return &CA{
		caCert:    caCert,
		caKey:     caKey,
		caCertPEM: caCertPEM,
		certPool:  certPool,
	}, nil
}

// LoadCA constructs a CA from PEM-encoded certificate and private key bytes.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("failed to decode CA certificate PEM")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CA certificate: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, errors.New("failed to decode CA private key PEM")
	}

	var signer crypto.Signer
	switch keyBlock.Type {
	case "EC PRIVATE KEY":
		k, err := x509.ParseECPrivateKey(keyBlock.Bytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse EC private key: %w", err)
		}
		signer = k
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse RSA private key: %w", err)
		}
		signer = k
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse PKCS8 private key: %w", err)
		}
		s, ok := k.(crypto.Signer)
		if !ok {
			return nil, errors.New("PKCS8 private key does not implement crypto.Signer")
		}
		signer = s
	default:
		return nil, fmt.Errorf("unsupported CA private key PEM type %q", keyBlock.Type)
	}

	certPool := x509.NewCertPool()
	certPool.AddCert(caCert)

	return &CA{
		caCert:    caCert,
		caKey:     signer,
		caCertPEM: certPEM,
		certPool:  certPool,
	}, nil
}

// LoadCAFromFiles loads a CA from PEM-encoded certificate and private key files on disk.
func LoadCAFromFiles(certPath, keyPath string) (*CA, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA cert %q: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA key %q: %w", keyPath, err)
	}
	return LoadCA(certPEM, keyPEM)
}

// CACertPEM returns the PEM-encoded root CA certificate.
func (ca *CA) CACertPEM() []byte {
	return ca.caCertPEM
}

// CertPool returns the x509.CertPool containing the root CA certificate.
func (ca *CA) CertPool() *x509.CertPool {
	return ca.certPool
}

// MintSandboxCert issues a short-lived ECDSA P-256 client certificate with CommonName set to sandboxID.
func (ca *CA) MintSandboxCert(sandboxID string, ttl time.Duration) (certPEM, keyPEM []byte, err error) {
	if sandboxID == "" {
		return nil, nil, errors.New("sandboxID cannot be empty")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate client key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate client serial number: %w", err)
	}

	notBefore := time.Now().Add(-5 * time.Minute)
	notAfter := notBefore.Add(ttl)

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: sandboxID,
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.caCert, &clientKey.PublicKey, ca.caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to issue client certificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal client EC private key: %w", err)
	}

	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyDER,
	})

	return certPEM, keyPEM, nil
}

// MintServerCert issues an ECDSA P-256 server certificate for the given DNS names and IP addresses.
func (ca *CA) MintServerCert(hosts []string, ttl time.Duration) (certPEM, keyPEM []byte, err error) {
	if len(hosts) == 0 {
		hosts = []string{"localhost", "127.0.0.1"}
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate server key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate server serial number: %w", err)
	}

	notBefore := time.Now().Add(-5 * time.Minute)
	notAfter := notBefore.Add(ttl)

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: hosts[0],
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.caCert, &serverKey.PublicKey, ca.caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to issue server certificate: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certDER,
	})

	keyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal server EC private key: %w", err)
	}

	keyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyDER,
	})

	return certPEM, keyPEM, nil
}

// ServerTLSConfig returns a *tls.Config for an mTLS server requiring and verifying client certs against ca.
func (ca *CA) ServerTLSConfig(hosts ...string) (*tls.Config, error) {
	if len(hosts) == 0 {
		hosts = []string{"localhost", "127.0.0.1"}
	}
	certPEM, keyPEM, err := ca.MintServerCert(hosts, 24*time.Hour)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to load server keypair: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.certPool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLSConfig returns a *tls.Config configured with the client cert and trusted CA root pool.
func (ca *CA) ClientTLSConfig(certPEM, keyPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to load client keypair: %w", err)
	}

	return &tls.Config{
		RootCAs:      ca.certPool,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// VerifySandboxCert parses and validates a client certificate PEM against the CA,
// returning the sandboxID extracted from the CommonName.
func (ca *CA) VerifySandboxCert(certPEM []byte) (sandboxID string, err error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("failed to parse certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse x509 certificate: %w", err)
	}

	opts := x509.VerifyOptions{
		Roots:     ca.certPool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	if _, err := cert.Verify(opts); err != nil {
		return "", fmt.Errorf("certificate verification failed: %w", err)
	}

	if cert.Subject.CommonName == "" {
		return "", errors.New("certificate CommonName is empty")
	}

	return cert.Subject.CommonName, nil
}

// VerifySandboxCert is a package-level helper that verifies certPEM against ca.
func VerifySandboxCert(ca *CA, certPEM []byte) (string, error) {
	if ca == nil {
		return "", errors.New("ca cannot be nil")
	}
	return ca.VerifySandboxCert(certPEM)
}
