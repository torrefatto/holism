package pki

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGenerateCAProducesValidCA(t *testing.T) {
	cfg := Config{Dir: t.TempDir()}

	cert, key, err := GenerateCA(cfg, false)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	// Files exist with the expected names and permissions.
	keyInfo, err := os.Stat(cfg.CAKeyPath())
	if err != nil {
		t.Fatalf("stat ca.key: %v", err)
	}
	if perm := keyInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("ca.key perm = %o, want 600", perm)
	}
	if _, err := os.Stat(cfg.CACertPath()); err != nil {
		t.Fatalf("stat ca.crt: %v", err)
	}

	// The returned material round-trips through LoadCA.
	loadedCert, loadedKey, err := LoadCA(cfg)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if !loadedCert.Equal(cert) {
		t.Error("loaded cert differs from generated cert")
	}
	if !loadedKey.PublicKey.Equal(&key.PublicKey) {
		t.Error("loaded key differs from generated key")
	}

	// The certificate is a parseable, self-signed CA valid for ~10 years.
	if !cert.IsCA {
		t.Error("cert.IsCA = false, want true")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("cert key usage missing CertSign")
	}
	if !cert.BasicConstraintsValid {
		t.Error("BasicConstraintsValid = false, want true")
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		t.Errorf("cert is not self-signed: %v", err)
	}
	if got := cert.Subject.CommonName; got == "" {
		t.Error("cert has empty CommonName")
	}
	lifetime := cert.NotAfter.Sub(cert.NotBefore)
	want := CALifetime + time.Hour // NotBefore is backdated by one hour
	if lifetime < want-time.Minute || lifetime > want+time.Minute {
		t.Errorf("CA lifetime = %s, want ~%s", lifetime, want)
	}
}

func TestGenerateCARefusesToClobber(t *testing.T) {
	cfg := Config{Dir: t.TempDir()}

	first, _, err := GenerateCA(cfg, false)
	if err != nil {
		t.Fatalf("first GenerateCA: %v", err)
	}
	if !cfg.Initialized() {
		t.Fatal("Initialized() = false after GenerateCA")
	}

	if _, _, err := GenerateCA(cfg, false); err == nil {
		t.Fatal("second GenerateCA without force: want error, got nil")
	}

	// Without force the original CA must still be there.
	loaded, _, err := LoadCA(cfg)
	if err != nil {
		t.Fatalf("LoadCA after refused clobber: %v", err)
	}
	if !loaded.Equal(first) {
		t.Error("CA was modified by refused GenerateCA")
	}

	// With force a new CA replaces the old one.
	second, _, err := GenerateCA(cfg, true)
	if err != nil {
		t.Fatalf("GenerateCA with force: %v", err)
	}
	if second.Equal(first) {
		t.Error("GenerateCA with force returned the same CA")
	}
	loaded, _, err = LoadCA(cfg)
	if err != nil {
		t.Fatalf("LoadCA after force: %v", err)
	}
	if !loaded.Equal(second) {
		t.Error("on-disk CA does not match the forced regeneration")
	}
}

func TestLoadCAMissing(t *testing.T) {
	cfg := Config{Dir: filepath.Join(t.TempDir(), "does-not-exist")}
	if cfg.Initialized() {
		t.Fatal("Initialized() = true on empty dir")
	}
	if _, _, err := LoadCA(cfg); err == nil {
		t.Fatal("LoadCA on empty dir: want error, got nil")
	}
}

func TestLoadCAMismatchedPair(t *testing.T) {
	cfg := Config{Dir: t.TempDir()}
	cert, _, err := GenerateCA(cfg, false)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	// Overwrite the key with a different one: LoadCA must detect the mismatch.
	_, otherKey, err := GenerateCA(Config{Dir: t.TempDir()}, false)
	if err != nil {
		t.Fatalf("GenerateCA (other): %v", err)
	}
	if err := cfg.SaveCA(otherKey, cert); err != nil {
		t.Fatalf("SaveCA: %v", err)
	}
	if _, _, err := LoadCA(cfg); err == nil {
		t.Fatal("LoadCA with mismatched key/cert: want error, got nil")
	}
}
