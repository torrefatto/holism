package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	holismv1 "github.com/torrefatto/holism/api/holism/v1"
	eventbusmemory "github.com/torrefatto/holism/mind/internal/eventbus/memory"
	"github.com/torrefatto/holism/mind/internal/planner/rules"
	registrymemory "github.com/torrefatto/holism/mind/internal/registry/memory"
	taskstorememory "github.com/torrefatto/holism/mind/internal/taskstore/memory"
)

// testCA is a throwaway root CA for mTLS tests.
type testCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "holism-test-root", Organization: []string{"holism"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &testCA{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// issueLeaf creates a leaf certificate signed by the CA, mirroring the CLI's
// issuance (EKU by role, SPIFFE URI SAN) but without touching disk.
func (ca *testCA) issueLeaf(t *testing.T, cn string, dnsNames []string, uriSAN string, eku x509.ExtKeyUsage) (*x509.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"holism"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
		DNSNames:     dnsNames,
	}
	if uriSAN != "" {
		u, err := url.Parse(uriSAN)
		require.NoError(t, err)
		tmpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return cert, certPEM, keyPEM
}

// writePKI writes a CA + server + client leaf into dir and returns their paths.
func writePKI(t *testing.T, dir string, ca *testCA, serverCertPEM, serverKeyPEM, clientCertPEM, clientKeyPEM []byte) (certPath, keyPath, caPath string) {
	t.Helper()
	caPath = filepath.Join(dir, "ca.pem")
	certPath = filepath.Join(dir, "server-cert.pem")
	keyPath = filepath.Join(dir, "server-key.pem")
	require.NoError(t, os.WriteFile(caPath, ca.certPEM, 0o644))
	require.NoError(t, os.WriteFile(certPath, serverCertPEM, 0o644))
	require.NoError(t, os.WriteFile(keyPath, serverKeyPEM, 0o600))
	if clientCertPEM != nil {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "client-cert.pem"), clientCertPEM, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "client-key.pem"), clientKeyPEM, 0o600))
	}
	return certPath, keyPath, caPath
}

func TestLoadTLSConfig(t *testing.T) {
	ca := newTestCA(t)
	_, serverCertPEM, serverKeyPEM := ca.issueLeaf(t, "mind.internal", []string{"localhost"}, "spiffe://holism/mind/mind.internal", x509.ExtKeyUsageServerAuth)
	dir := t.TempDir()
	certPath, keyPath, caPath := writePKI(t, dir, ca, serverCertPEM, serverKeyPEM, nil, nil)

	cfg, err := LoadTLSConfig(certPath, keyPath, caPath)
	require.NoError(t, err)
	assert.Equal(t, tls.RequireAndVerifyClientCert, cfg.ClientAuth)
	require.Len(t, cfg.Certificates, 1)
	assert.NotNil(t, cfg.ClientCAs)

	_, err = LoadTLSConfig(certPath, filepath.Join(dir, "missing.pem"), caPath)
	assert.Error(t, err)

	badCA := filepath.Join(dir, "bad-ca.pem")
	require.NoError(t, os.WriteFile(badCA, []byte("not a pem"), 0o644))
	_, err = LoadTLSConfig(certPath, keyPath, badCA)
	assert.Error(t, err, "a CA file without certificates must be rejected")
}

func TestTLSConfigFromEnv(t *testing.T) {
	ca := newTestCA(t)
	_, serverCertPEM, serverKeyPEM := ca.issueLeaf(t, "mind.internal", []string{"localhost"}, "", x509.ExtKeyUsageServerAuth)
	dir := t.TempDir()
	certPath, keyPath, caPath := writePKI(t, dir, ca, serverCertPEM, serverKeyPEM, nil, nil)

	t.Setenv(EnvTLSCert, certPath)
	t.Setenv(EnvTLSKey, keyPath)
	t.Setenv(EnvTLSCA, caPath)
	cfg, err := TLSConfigFromEnv()
	require.NoError(t, err)
	assert.Equal(t, tls.RequireAndVerifyClientCert, cfg.ClientAuth)

	t.Setenv(EnvTLSCA, "")
	_, err = TLSConfigFromEnv()
	assert.Error(t, err, "unset CA path must be an error")
}

func TestPeerIdentity(t *testing.T) {
	ca := newTestCA(t)
	withURI, _, _ := ca.issueLeaf(t, "drone:d1", nil, "spiffe://holism/drone/d1", x509.ExtKeyUsageClientAuth)
	cnOnly, _, _ := ca.issueLeaf(t, "operator:alice", nil, "", x509.ExtKeyUsageClientAuth)

	ctxWith := func(cert *x509.Certificate) context.Context {
		return peer.NewContext(context.Background(), &peer.Peer{
			AuthInfo: &credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}},
		})
	}

	id, err := PeerIdentity(ctxWith(withURI))
	require.NoError(t, err)
	assert.Equal(t, "spiffe://holism/drone/d1", id, "URI SAN wins over CN")

	id, err = PeerIdentity(ctxWith(cnOnly))
	require.NoError(t, err)
	assert.Equal(t, "operator:alice", id, "CN is the fallback")

	_, err = PeerIdentity(context.Background())
	assert.Error(t, err, "no peer in context")

	_, err = PeerIdentity(peer.NewContext(context.Background(), &peer.Peer{
		AuthInfo: &credentials.TLSInfo{State: tls.ConnectionState{}},
	}))
	assert.Error(t, err, "no client certificate")
}

// TestMTLSServer runs the real MindService on a localhost listener with the
// mTLS config loaded from files, and checks that only clients presenting a
// certificate from the right CA get through.
func TestMTLSServer(t *testing.T) {
	ca := newTestCA(t)
	_, serverCertPEM, serverKeyPEM := ca.issueLeaf(t, "mind.internal", []string{"localhost"}, "spiffe://holism/mind/mind.internal", x509.ExtKeyUsageServerAuth)
	clientCert, clientCertPEM, clientKeyPEM := ca.issueLeaf(t, "drone:d1", nil, "spiffe://holism/drone/d1", x509.ExtKeyUsageClientAuth)
	wrongCA := newTestCA(t)
	_, wrongCertPEM, wrongKeyPEM := wrongCA.issueLeaf(t, "drone:evil", nil, "spiffe://holism/drone/evil", x509.ExtKeyUsageClientAuth)

	dir := t.TempDir()
	certPath, keyPath, caPath := writePKI(t, dir, ca, serverCertPEM, serverKeyPEM, clientCertPEM, clientKeyPEM)

	tlsCfg, err := LoadTLSConfig(certPath, keyPath, caPath)
	require.NoError(t, err)

	srv := New(Deps{
		Store:    taskstorememory.New(nil, nil),
		Bus:      eventbusmemory.New(nil, nil),
		Planner:  rules.New(),
		Registry: registrymemory.New(nil, 0),
	})
	grpcSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsCfg)))
	holismv1.RegisterMindServiceServer(grpcSrv, srv)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.Stop)
	addr := lis.Addr().String()

	caPool := x509.NewCertPool()
	require.True(t, caPool.AppendCertsFromPEM(ca.certPEM))
	clientTLS := func(certPEM, keyPEM []byte, pool *x509.CertPool) *tls.Config {
		cfg := &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
		if certPEM != nil {
			pair, err := tls.X509KeyPair(certPEM, keyPEM)
			require.NoError(t, err)
			cfg.Certificates = []tls.Certificate{pair}
		}
		return cfg
	}
	dial := func(t *testing.T, cfg *tls.Config) holismv1.MindServiceClient {
		t.Helper()
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(cfg)))
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return holismv1.NewMindServiceClient(conn)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A client with a valid certificate from the right CA works.
	okClient := dial(t, clientTLS(clientCertPEM, clientKeyPEM, caPool))
	p, err := okClient.CreateProject(ctx, &holismv1.CreateProjectRequest{Name: "mtls"})
	require.NoError(t, err)
	assert.NotEmpty(t, p.GetId())

	// The same client can open the Connect stream (peer identity is extracted).
	stream, err := okClient.Connect(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&holismv1.DroneMessage{
		Id:      "m1",
		Payload: &holismv1.DroneMessage_Register{Register: &holismv1.Register{DroneId: clientCert.Subject.CommonName}},
	}))
	msg, err := stream.Recv()
	require.NoError(t, err)
	assert.NotNil(t, msg.GetPong())

	// A client without a certificate is rejected at the handshake.
	noCert := dial(t, clientTLS(nil, nil, caPool))
	_, err = noCert.CreateProject(ctx, &holismv1.CreateProjectRequest{Name: "x"})
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err), "no client cert must fail the handshake")

	// A client with a certificate from a different CA is rejected too.
	wrongPool := x509.NewCertPool()
	require.True(t, wrongPool.AppendCertsFromPEM(wrongCA.certPEM))
	wrongClient := dial(t, clientTLS(wrongCertPEM, wrongKeyPEM, wrongPool))
	_, err = wrongClient.CreateProject(ctx, &holismv1.CreateProjectRequest{Name: "x"})
	require.Error(t, err)
	assert.Equal(t, codes.Unavailable, status.Code(err), "wrong-CA client cert must fail verification")
}
