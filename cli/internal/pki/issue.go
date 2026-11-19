package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Leaf file names inside each entity directory.
const (
	CertFile = "cert.pem"
	KeyFile  = "key.pem"
)

// Role identifies the kind of entity a leaf certificate authenticates.
type Role string

const (
	RoleMind     Role = "mind"
	RoleDrone    Role = "drone"
	RoleOperator Role = "operator"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleMind, RoleDrone, RoleOperator:
		return true
	}
	return false
}

// SpiffeID returns the SPIFFE-style identity URI for a role/id pair,
// e.g. spiffe://holism/drone/d1 (see design doc §6.3).
func SpiffeID(r Role, id string) string {
	return "spiffe://holism/" + string(r) + "/" + id
}

// parseSpiffeID splits a spiffe://holism/<role>/<id> URI back into its parts.
func parseSpiffeID(u *url.URL) (Role, string, error) {
	if u.Scheme != "spiffe" || u.Host != "holism" {
		return "", "", fmt.Errorf("not a holism SPIFFE ID: %s", u)
	}
	var parts []string
	for _, p := range strings.Split(u.EscapedPath(), "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) != 2 || !Role(parts[0]).Valid() {
		return "", "", fmt.Errorf("malformed holism SPIFFE ID: %s", u)
	}
	return Role(parts[0]), parts[1], nil
}

// EntityDir returns the PKI subdirectory holding the leaf certificate for a
// role/id pair: <pki>/mind, <pki>/drones/<id>, <pki>/operators/<id>.
func (c Config) EntityDir(role Role, id string) string {
	switch role {
	case RoleMind:
		return filepath.Join(c.Dir, "mind")
	case RoleDrone:
		return filepath.Join(c.Dir, "drones", id)
	case RoleOperator:
		return filepath.Join(c.Dir, "operators", id)
	}
	return filepath.Join(c.Dir, string(role), id)
}

// EntityCertPath returns the leaf certificate path for a role/id pair.
func (c Config) EntityCertPath(role Role, id string) string {
	return filepath.Join(c.EntityDir(role, id), CertFile)
}

// EntityKeyPath returns the leaf private key path for a role/id pair.
func (c Config) EntityKeyPath(role Role, id string) string {
	return filepath.Join(c.EntityDir(role, id), KeyFile)
}

// DefaultCommonName returns the default subject CN for a role/id pair:
// the id itself for the mind, "<role>:<id>" for drones and operators.
func DefaultCommonName(role Role, id string) string {
	if role == RoleMind {
		return id
	}
	return string(role) + ":" + id
}

// IssueOptions controls leaf certificate issuance.
type IssueOptions struct {
	// CommonName overrides the subject CN (default: DefaultCommonName).
	CommonName string
	// DNSNames are added as DNS SANs (mind server certificates).
	DNSNames []string
	// URIs are added as extra URI SANs, after the SPIFFE identity SAN.
	URIs []*url.URL
	// Force re-issues even if a certificate already exists for the entity.
	Force bool
}

// IssueLeaf issues a leaf certificate for role/id, signed by the PKI root CA,
// and stores it in the entity directory (key 0600, cert 0644). The mind gets
// a serverAuth certificate; drones and operators get clientAuth certificates.
// Every leaf carries the SPIFFE identity as a URI SAN and lives for
// LeafLifetime (~1 month). It fails if the PKI is not initialized, and
// refuses to overwrite an existing certificate unless opts.Force is set.
func (c Config) IssueLeaf(role Role, id string, opts IssueOptions) (*x509.Certificate, error) {
	if !role.Valid() {
		return nil, fmt.Errorf("invalid role %q (want mind, drone or operator)", role)
	}
	if id == "" {
		return nil, errors.New("empty id")
	}
	if !c.Initialized() {
		return nil, fmt.Errorf("PKI not initialized in %s: run 'holism init' first", c.Dir)
	}
	caCert, caKey, err := LoadCA(c)
	if err != nil {
		return nil, err
	}

	certPath := c.EntityCertPath(role, id)
	if _, statErr := os.Stat(certPath); statErr == nil && !opts.Force {
		return nil, fmt.Errorf("certificate for %s %q already exists at %s (use --force to re-issue)", role, id, certPath)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, fmt.Errorf("generate leaf serial: %w", err)
	}
	spiffe, err := url.Parse(SpiffeID(role, id))
	if err != nil {
		return nil, fmt.Errorf("build SPIFFE ID: %w", err)
	}

	cn := opts.CommonName
	if cn == "" {
		cn = DefaultCommonName(role, id)
	}
	eku := x509.ExtKeyUsageClientAuth
	if role == RoleMind {
		eku = x509.ExtKeyUsageServerAuth
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: []string{"holism"},
		},
		// NotBefore is backdated slightly to tolerate clock skew.
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(LeafLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{eku},
		BasicConstraintsValid: true,
		DNSNames:              opts.DNSNames,
		URIs:                  append([]*url.URL{spiffe}, opts.URIs...),
	}

	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("create leaf certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse leaf certificate: %w", err)
	}

	if err := os.MkdirAll(c.EntityDir(role, id), 0o755); err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal leaf key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := writeFile(c.EntityKeyPath(role, id), keyPEM, 0o600); err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := writeFile(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	return cert, nil
}

// IssuedCert describes one leaf certificate found in the PKI directory.
type IssuedCert struct {
	Name     string // entity id (directory name); "mind" for the mind
	Role     Role
	Subject  string    // certificate subject CN
	NotAfter time.Time // expiry
	CertPath string
}

// ListIssued scans the PKI directory and returns all issued leaf
// certificates, ordered by role then name. A missing PKI directory simply
// yields an empty list.
func (c Config) ListIssued() ([]IssuedCert, error) {
	var out []IssuedCert
	add := func(role Role, dir string) error {
		path := filepath.Join(dir, CertFile)
		pemBytes, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		block, _ := pem.Decode(pemBytes)
		if block == nil || block.Type != "CERTIFICATE" {
			return fmt.Errorf("%s is not a PEM certificate", path)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		out = append(out, IssuedCert{
			Name:     filepath.Base(dir),
			Role:     role,
			Subject:  cert.Subject.CommonName,
			NotAfter: cert.NotAfter,
			CertPath: path,
		})
		return nil
	}

	if err := add(RoleMind, filepath.Join(c.Dir, "mind")); err != nil {
		return nil, err
	}
	for _, rd := range []struct {
		role Role
		sub  string
	}{{RoleDrone, "drones"}, {RoleOperator, "operators"}} {
		entries, err := os.ReadDir(filepath.Join(c.Dir, rd.sub))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if err := add(rd.role, filepath.Join(c.Dir, rd.sub, e.Name())); err != nil {
				return nil, err
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
