package controller_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cubeplexai/cubepod/pkg/controller"
)

type mockBuildKitServer struct {
	controlapi.UnimplementedControlServer
	listWorkerCalls atomic.Int32
	solveCalls      atomic.Int32
	logLine         string
	solveDone       chan struct{}
}

func (m *mockBuildKitServer) ListWorkers(_ context.Context, _ *controlapi.ListWorkersRequest) (*controlapi.ListWorkersResponse, error) {
	m.listWorkerCalls.Add(1)
	return &controlapi.ListWorkersResponse{}, nil
}

func (m *mockBuildKitServer) Session(srv grpc.BidiStreamingServer[controlapi.BytesMessage, controlapi.BytesMessage]) error {
	for {
		_, err := srv.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return nil
		}
	}
}

func (m *mockBuildKitServer) Status(req *controlapi.StatusRequest, srv grpc.ServerStreamingServer[controlapi.StatusResponse]) error {
	now := timestamppb.Now()
	line := m.logLine
	if line == "" {
		line = "step 1 ok"
	}
	if err := srv.Send(&controlapi.StatusResponse{
		Vertexes: []*controlapi.Vertex{
			{
				Digest:    "sha256:1111",
				Name:      "[1/1] RUN echo ok",
				Started:   now,
				Completed: now,
			},
		},
		Logs: []*controlapi.VertexLog{
			{
				Vertex:    "sha256:1111",
				Timestamp: now,
				Stream:    1,
				Msg:       []byte(line + "\n"),
			},
		},
	}); err != nil {
		return err
	}
	if m.solveDone != nil {
		select {
		case <-m.solveDone:
		case <-srv.Context().Done():
		}
	}
	return nil
}

func (m *mockBuildKitServer) Solve(_ context.Context, _ *controlapi.SolveRequest) (*controlapi.SolveResponse, error) {
	m.solveCalls.Add(1)
	if m.solveDone != nil {
		select {
		case <-m.solveDone:
		default:
			close(m.solveDone)
		}
	}
	return &controlapi.SolveResponse{}, nil
}

type testMTLSCerts struct {
	caPath          string
	serverCertPath  string
	serverKeyPath   string
	clientCertPath  string
	clientKeyPath   string
	rogueCertPath   string
	rogueKeyPath    string
	serverTLSConfig *tls.Config
}

func generateTestMTLSCerts(t *testing.T, serverSAN string) testMTLSCerts {
	t.Helper()
	dir := t.TempDir()

	genCert := func(cn string, isCA bool, extUsage x509.ExtKeyUsage, dnsSANs []string, ipSANs []net.IP, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, string, string) {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("GenerateKey(%s): %v", cn, err)
		}
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
		if err != nil {
			t.Fatalf("serial: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber:          serial,
			Subject:               pkix.Name{CommonName: cn},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			BasicConstraintsValid: true,
			IsCA:                  isCA,
			DNSNames:              dnsSANs,
			IPAddresses:           ipSANs,
		}
		if isCA {
			tmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
		} else {
			tmpl.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{extUsage}
		}

		signerCert := tmpl
		signerKey := priv
		if parent != nil {
			signerCert = parent
			signerKey = parentKey
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, signerCert, &priv.PublicKey, signerKey)
		if err != nil {
			t.Fatalf("CreateCertificate(%s): %v", cn, err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("ParseCertificate(%s): %v", cn, err)
		}

		certFile := filepath.Join(dir, cn+".crt")
		keyFile := filepath.Join(dir, cn+".key")
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
			t.Fatalf("WriteFile(%s): %v", certFile, err)
		}
		keyDER, err := x509.MarshalECPrivateKey(priv)
		if err != nil {
			t.Fatalf("MarshalECPrivateKey(%s): %v", cn, err)
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
		if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
			t.Fatalf("WriteFile(%s): %v", keyFile, err)
		}
		return cert, priv, certFile, keyFile
	}

	caCert, caKey, caPath, _ := genCert("test-ca", true, 0, nil, nil, nil, nil)
	_, _, srvCertPath, srvKeyPath := genCert("server", false, x509.ExtKeyUsageServerAuth, []string{serverSAN, "localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, caCert, caKey)
	_, _, cliCertPath, cliKeyPath := genCert("client", false, x509.ExtKeyUsageClientAuth, nil, nil, caCert, caKey)

	rogueCA, rogueKey, _, _ := genCert("rogue-ca", true, 0, nil, nil, nil, nil)
	_, _, rogueCertPath, rogueKeyPath := genCert("rogue-client", false, x509.ExtKeyUsageClientAuth, nil, nil, rogueCA, rogueKey)

	srvKP, err := tls.LoadX509KeyPair(srvCertPath, srvKeyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("ReadFile(ca): %v", err)
	}
	clientPool := x509.NewCertPool()
	if !clientPool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("AppendCertsFromPEM failed")
	}

	return testMTLSCerts{
		caPath:         caPath,
		serverCertPath: srvCertPath,
		serverKeyPath:  srvKeyPath,
		clientCertPath: cliCertPath,
		clientKeyPath:  cliKeyPath,
		rogueCertPath:  rogueCertPath,
		rogueKeyPath:   rogueKeyPath,
		serverTLSConfig: &tls.Config{
			Certificates: []tls.Certificate{srvKP},
			ClientAuth:   tls.RequireAndVerifyClientCert,
			ClientCAs:    clientPool,
			MinVersion:   tls.VersionTLS13,
		},
	}
}

func startMockBuildKitServer(t *testing.T, tlsCfg *tls.Config, logLine string) (string, *mockBuildKitServer, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	var grpcOpts []grpc.ServerOption
	if tlsCfg != nil {
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	mock := &mockBuildKitServer{
		logLine:   logLine,
		solveDone: make(chan struct{}),
	}
	controlapi.RegisterControlServer(grpcSrv, mock)
	go func() {
		_ = grpcSrv.Serve(lis)
	}()
	addr := "tcp://" + lis.Addr().String()
	return addr, mock, func() {
		grpcSrv.Stop()
		_ = lis.Close()
	}
}

func TestBuildKitBuilder_MTLSAndValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	certs := generateTestMTLSCerts(t, "cubepod-buildkit")
	addr, mock, stop := startMockBuildKitServer(t, certs.serverTLSConfig, "pulling with token super-secret-pw-99")
	defer stop()

	// 1. Valid mTLS with custom ServerName passes Ping and Build, streaming scrubbed logs to S3ContextStore.
	s3Srv := newMockS3Server("bk-logs", "us-east-1", "AKIA", "secret", "")
	defer s3Srv.Close()
	s3Store, err := controller.NewS3ContextStore(controller.S3ContextStoreConfig{
		Endpoint:        s3Srv.URL(),
		Bucket:          "bk-logs",
		Region:          "us-east-1",
		ForcePathStyle:  true,
		AccessKeyID:     "AKIA",
		SecretAccessKey: "secret",
	})
	if err != nil {
		t.Fatalf("NewS3ContextStore: %v", err)
	}

	bValid := controller.NewBuildKitBuilder(
		addr,
		controller.WithBuildKitTLS(certs.caPath, certs.clientCertPath, certs.clientKeyPath, "cubepod-buildkit"),
	)
	if err := bValid.Ping(ctx); err != nil {
		t.Fatalf("valid mTLS Ping failed: %v", err)
	}
	if mock.listWorkerCalls.Load() != 1 {
		t.Fatalf("expected 1 ListWorkers call, got %d", mock.listWorkerCalls.Load())
	}

	workDir := t.TempDir()
	if err := bValid.Build(ctx, controller.BuildOptions{
		Tag:          "registry.local/test:v1",
		ContextDir:   workDir,
		Dockerfile:   "FROM alpine:3.20\n",
		BuildID:      "bld-mtls-1",
		Passwords:    []string{"super-secret-pw-99"},
		ContextStore: s3Store,
	}); err != nil {
		t.Fatalf("valid mTLS Build failed: %v", err)
	}
	if mock.solveCalls.Load() != 1 {
		t.Fatalf("expected 1 Solve call, got %d", mock.solveCalls.Load())
	}

	logs, err := s3Store.ReadLogs("bld-mtls-1", 0, 50, "")
	if err != nil {
		t.Fatalf("S3 ReadLogs failed: %v", err)
	}
	var joined strings.Builder
	for _, entry := range logs {
		joined.WriteString(entry.Message)
		joined.WriteByte('\n')
	}
	if strings.Contains(joined.String(), "super-secret-pw-99") {
		t.Fatalf("build logs leaked unredacted password: %s", joined.String())
	}
	if !strings.Contains(joined.String(), "[REDACTED]") {
		t.Fatalf("expected [REDACTED] in S3 build logs, got: %s", joined.String())
	}

	// 2. Missing client cert fails mTLS handshake on Ping.
	noClientCert := controller.NewBuildKitBuilder(
		addr,
		controller.WithBuildKitTLS(certs.caPath, "", "", "cubepod-buildkit"),
	)
	if err := noClientCert.Ping(ctx); err == nil {
		t.Fatalf("expected Ping without client certificate to fail mTLS handshake")
	}

	// 3. Rogue untrusted client cert fails mTLS handshake.
	rogueClient := controller.NewBuildKitBuilder(
		addr,
		controller.WithBuildKitTLS(certs.caPath, certs.rogueCertPath, certs.rogueKeyPath, "cubepod-buildkit"),
	)
	if err := rogueClient.Ping(ctx); err == nil {
		t.Fatalf("expected Ping with rogue client certificate to fail mTLS handshake")
	}

	// 4. Wrong serverName fails server cert verification.
	wrongSAN := controller.NewBuildKitBuilder(
		addr,
		controller.WithBuildKitTLS(certs.caPath, certs.clientCertPath, certs.clientKeyPath, "wrong.buildkit.local"),
	)
	if err := wrongSAN.Ping(ctx); err == nil {
		t.Fatalf("expected Ping with mismatched serverName to fail TLS verification")
	}

	// 5. Incomplete client cert/key pair fails fast with explicit config error.
	incomplete := controller.NewBuildKitBuilder(
		addr,
		controller.WithBuildKitTLS(certs.caPath, certs.clientCertPath, "", "cubepod-buildkit"),
	)
	if err := incomplete.Ping(ctx); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("expected incomplete mTLS config error, got: %v", err)
	}
}

func TestBuildKitBuilder_MultiAddrRoundRobinAndFailover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Allocate and immediately close a port to simulate a down BuildKit replica.
	closedLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := "tcp://" + closedLis.Addr().String()
	_ = closedLis.Close()

	addr1, mock1, stop1 := startMockBuildKitServer(t, nil, "replica-1 ok")
	defer stop1()
	addr2, mock2, stop2 := startMockBuildKitServer(t, nil, "replica-2 ok")
	defer stop2()

	// Dead first replica + live second replica: Ping and Build both fail over cleanly.
	failoverBuilder := controller.NewBuildKitBuilder(deadAddr + " , " + addr1)
	if err := failoverBuilder.Ping(ctx); err != nil {
		t.Fatalf("Ping failover failed: %v", err)
	}
	if err := failoverBuilder.Build(ctx, controller.BuildOptions{
		Tag:        "registry.local/failover:v1",
		ContextDir: t.TempDir(),
		Dockerfile: "FROM alpine:3.20\n",
		BuildID:    "bld-failover-1",
	}); err != nil {
		t.Fatalf("Build failover failed: %v", err)
	}
	if mock1.solveCalls.Load() != 1 {
		t.Fatalf("expected live replica1 Solve=1 after failover, got %d", mock1.solveCalls.Load())
	}

	// Round-robin across two live replicas distributes Pings across both.
	rrBuilder := controller.NewBuildKitBuilder(addr1 + "," + addr2)
	before1 := mock1.listWorkerCalls.Load()
	before2 := mock2.listWorkerCalls.Load()
	for i := 0; i < 4; i++ {
		if err := rrBuilder.Ping(ctx); err != nil {
			t.Fatalf("round-robin Ping %d failed: %v", i, err)
		}
	}
	delta1 := mock1.listWorkerCalls.Load() - before1
	delta2 := mock2.listWorkerCalls.Load() - before2
	if delta1 == 0 || delta2 == 0 {
		t.Fatalf("expected Ping traffic distributed across both replicas, got delta1=%d delta2=%d", delta1, delta2)
	}

	// Empty address returns clear error.
	emptyBuilder := controller.NewBuildKitBuilder("")
	if err := emptyBuilder.Ping(ctx); err == nil {
		t.Fatalf("expected empty address error on Ping")
	}
}
