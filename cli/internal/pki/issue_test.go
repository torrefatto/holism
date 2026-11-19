package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// issueTestCA initializes a CA in a temp dir and returns its config.
func issueTestCA(t *testing.T) Config {
	t.Helper()
	cfg := Config{Dir: t.TempDir()}
	if _, _, err := GenerateCA(cfg, false); err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	return cfg
}

// caPool returns a root pool containing the config's CA certificate.
func caPool(t *testing.T, cfg Config) *x509.CertPool {
	t.Helper()
	caCert, _, err := LoadCA(cfg)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return pool
}

func TestIssueLeafMindServerCert(t *testing.T) {
	cfg := issueTestCA(t)

	cert, err := cfg.IssueLeaf(RoleMind, "mind.internal", IssueOptions{DNSNames: []string{"mind.internal", "mind"}})
	if err != nil {
		t.Fatalf("IssueLeaf(mind): %v", err)
	}

	// Files landed in pki/mind with the right permissions.
	keyInfo, err := os.Stat(cfg.EntityKeyPath(RoleMind, "mind.internal"))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := keyInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("key perm = %o, want 600", perm)
	}
	if _, err := os.Stat(cfg.EntityCertPath(RoleMind, "mind.internal")); err != nil {
		t.Fatalf("stat cert: %v", err)
	}

	// Subject, SANs, EKU.
	if got := cert.Subject.CommonName; got != "mind.internal" {
		t.Errorf("CN = %q, want %q", got, "mind.internal")
	}
	wantSpiffe := "spiffe://holism/mind/mind.internal"
	if len(cert.URIs) != 1 || cert.URIs[0].String() != wantSpiffe {
		t.Errorf("URIs = %v, want [%s]", cert.URIs, wantSpiffe)
	}
	if len(cert.DNSNames) != 2 || cert.DNSNames[0] != "mind.internal" || cert.DNSNames[1] != "mind" {
		t.Errorf("DNSNames = %v", cert.DNSNames)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("ExtKeyUsage = %v, want [ServerAuth]", cert.ExtKeyUsage)
	}

	// ~1 month validity.
	lifetime := cert.NotAfter.Sub(cert.NotBefore)
	want := LeafLifetime + time.Hour
	if lifetime < want-time.Minute || lifetime > want+time.Minute {
		t.Errorf("leaf lifetime = %s, want ~%s", lifetime, want)
	}

	// Verifies against the CA as a TLS server for mind.internal.
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     caPool(t, cfg),
		DNSName:   "mind.internal",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("server cert does not verify against CA: %v", err)
	}
}

func TestIssueLeafDroneAndOperatorClientCerts(t *testing.T) {
	cfg := issueTestCA(t)

	for _, tc := range []struct {
		role Role
		id   string
	}{
		{RoleDrone, "d1"},
		{RoleOperator, "alice"},
	} {
		cert, err := cfg.IssueLeaf(tc.role, tc.id, IssueOptions{})
		if err != nil {
			t.Fatalf("IssueLeaf(%s %s): %v", tc.role, tc.id, err)
		}
		wantCN := string(tc.role) + ":" + tc.id
		if cert.Subject.CommonName != wantCN {
			t.Errorf("%s: CN = %q, want %q", tc.role, cert.Subject.CommonName, wantCN)
		}
		wantSpiffe := SpiffeID(tc.role, tc.id)
		if len(cert.URIs) != 1 || cert.URIs[0].String() != wantSpiffe {
			t.Errorf("%s: URIs = %v, want [%s]", tc.role, cert.URIs, wantSpiffe)
		}
		if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
			t.Errorf("%s: ExtKeyUsage = %v, want [ClientAuth]", tc.role, cert.ExtKeyUsage)
		}
		if len(cert.DNSNames) != 0 {
			t.Errorf("%s: unexpected DNS SANs %v", tc.role, cert.DNSNames)
		}
		// Verifies against the CA as a TLS client.
		if _, err := cert.Verify(x509.VerifyOptions{
			Roots:     caPool(t, cfg),
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}); err != nil {
			t.Errorf("%s: client cert does not verify against CA: %v", tc.role, err)
		}
		// The on-disk key matches the certificate.
		keyPEM, err := os.ReadFile(cfg.EntityKeyPath(tc.role, tc.id))
		if err != nil {
			t.Fatalf("%s: read key: %v", tc.role, err)
		}
		block, _ := pem.Decode(keyPEM)
		if block == nil {
			t.Fatalf("%s: key is not PEM", tc.role)
		}
		keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			t.Fatalf("%s: parse key: %v", tc.role, err)
		}
		signer, ok := keyAny.(crypto.Signer)
		if !ok {
			t.Fatalf("%s: key is not a crypto.Signer", tc.role)
		}
		certPub, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok {
			t.Fatalf("%s: cert public key is %T, want ECDSA", tc.role, cert.PublicKey)
		}
		keyPub, ok := signer.Public().(*ecdsa.PublicKey)
		if !ok || !keyPub.Equal(certPub) {
			t.Errorf("%s: on-disk key does not match certificate", tc.role)
		}
	}
}

func TestIssueLeafRequiresCA(t *testing.T) {
	cfg := Config{Dir: filepath.Join(t.TempDir(), "pki")}
	if _, err := cfg.IssueLeaf(RoleDrone, "d1", IssueOptions{}); err == nil {
		t.Fatal("IssueLeaf without CA: want error, got nil")
	}
}

func TestIssueLeafRefusesOverwrite(t *testing.T) {
	cfg := issueTestCA(t)

	first, err := cfg.IssueLeaf(RoleDrone, "d1", IssueOptions{})
	if err != nil {
		t.Fatalf("first issue: %v", err)
	}
	if _, err := cfg.IssueLeaf(RoleDrone, "d1", IssueOptions{}); err == nil {
		t.Fatal("second issue without Force: want error, got nil")
	}
	second, err := cfg.IssueLeaf(RoleDrone, "d1", IssueOptions{Force: true})
	if err != nil {
		t.Fatalf("issue with Force: %v", err)
	}
	if second.Equal(first) {
		t.Error("forced re-issue returned the same certificate")
	}
}

func TestIssueLeafInvalidRoleOrID(t *testing.T) {
	cfg := issueTestCA(t)
	if _, err := cfg.IssueLeaf(Role("bogus"), "x", IssueOptions{}); err == nil {
		t.Error("invalid role: want error, got nil")
	}
	if _, err := cfg.IssueLeaf(RoleDrone, "", IssueOptions{}); err == nil {
		t.Error("empty id: want error, got nil")
	}
}

func TestListIssued(t *testing.T) {
	cfg := issueTestCA(t)

	if certs, err := cfg.ListIssued(); err != nil || len(certs) != 0 {
		t.Fatalf("empty ListIssued = %v, %v", certs, err)
	}

	if _, err := cfg.IssueLeaf(RoleMind, "mind", IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.IssueLeaf(RoleDrone, "d2", IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.IssueLeaf(RoleDrone, "d1", IssueOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.IssueLeaf(RoleOperator, "alice", IssueOptions{}); err != nil {
		t.Fatal(err)
	}

	certs, err := cfg.ListIssued()
	if err != nil {
		t.Fatalf("ListIssued: %v", err)
	}
	if len(certs) != 4 {
		t.Fatalf("ListIssued returned %d certs, want 4", len(certs))
	}
	// Sorted by role then name: drone/d1, drone/d2, mind, operator/alice.
	want := []struct {
		role Role
		name string
		subj string
	}{
		{RoleDrone, "d1", "drone:d1"},
		{RoleDrone, "d2", "drone:d2"},
		{RoleMind, "mind", "mind"},
		{RoleOperator, "alice", "operator:alice"},
	}
	for i, w := range want {
		got := certs[i]
		if got.Role != w.role || got.Name != w.name || got.Subject != w.subj {
			t.Errorf("certs[%d] = {%s %s %s}, want {%s %s %s}", i, got.Role, got.Name, got.Subject, w.role, w.name, w.subj)
		}
		if got.NotAfter.Before(time.Now()) {
			t.Errorf("certs[%d] already expired", i)
		}
		if _, err := os.Stat(got.CertPath); err != nil {
			t.Errorf("certs[%d].CertPath missing: %v", i, err)
		}
	}
}

func TestSpiffeIDRoundTrip(t *testing.T) {
	u, err := url.Parse(SpiffeID(RoleOperator, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	role, id, err := parseSpiffeID(u)
	if err != nil {
		t.Fatalf("parseSpiffeID: %v", err)
	}
	if role != RoleOperator || id != "alice" {
		t.Errorf("round trip = %s/%s, want operator/alice", role, id)
	}
	if _, _, err := parseSpiffeID(mustParse(t, "spiffe://other/drone/d1")); err == nil {
		t.Error("parseSpiffeID accepted foreign trust domain")
	}
}

func mustParse(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
