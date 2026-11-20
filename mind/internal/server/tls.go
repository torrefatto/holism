package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// Environment variables configuring the mind's mTLS material (12-factor: paths
// to files mounted by the runtime, credentials via env-from-secret).
const (
	EnvTLSCert = "MIND_TLS_CERT"
	EnvTLSKey  = "MIND_TLS_KEY"
	EnvTLSCA   = "MIND_TLS_CA"
)

// TLSConfigFromEnv builds the mind's server tls.Config from the paths in
// MIND_TLS_CERT, MIND_TLS_KEY and MIND_TLS_CA. It returns an error if any of
// the variables is unset or the material cannot be loaded.
func TLSConfigFromEnv() (*tls.Config, error) {
	certPath, keyPath, caPath := os.Getenv(EnvTLSCert), os.Getenv(EnvTLSKey), os.Getenv(EnvTLSCA)
	if certPath == "" || keyPath == "" || caPath == "" {
		return nil, fmt.Errorf("server: %s, %s and %s must all be set", EnvTLSCert, EnvTLSKey, EnvTLSCA)
	}
	return LoadTLSConfig(certPath, keyPath, caPath)
}

// LoadTLSConfig loads the mind's server certificate/key pair and the root CA
// pool from files, and returns an mTLS server config: client certificates are
// required and verified against the CA pool (tls.RequireAndVerifyClientCert),
// which is the only authentication the mind's listener accepts.
func LoadTLSConfig(certPath, keyPath, caPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("server: load key pair: %w", err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("server: read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("server: no certificates found in CA file %s", caPath)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// PeerIdentity extracts the caller's identity from the mTLS peer certificate
// in ctx: the first URI SAN (SPIFFE-style, e.g. spiffe://holism/drone/d1) when
// present, otherwise the subject CN. It returns an error when the context
// carries no TLS peer with a certificate.
func PeerIdentity(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", errors.New("server: no peer in context")
	}
	tlsInfo, ok := p.AuthInfo.(*credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return "", errors.New("server: peer did not present a client certificate")
	}
	cert := tlsInfo.State.PeerCertificates[0]
	if len(cert.URIs) > 0 {
		return cert.URIs[0].String(), nil
	}
	if cert.Subject.CommonName != "" {
		return cert.Subject.CommonName, nil
	}
	return "", errors.New("server: client certificate has neither URI SAN nor CN")
}
