package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestRunHelpListsSubcommands(t *testing.T) {
	for _, args := range [][]string{
		{"holism", "--help"},
		{"holism", "-h"},
		{"holism", "help"},
		{"holism"}, // bare invocation shows help
	} {
		var stdout, stderr bytes.Buffer
		if err := run(args, &stdout, &stderr); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
		for _, want := range []string{"init", "cert"} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("run(%v) stdout missing %q:\n%s", args, want, stdout.String())
			}
		}
	}
}

func TestRunUnknownCommandIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"holism", "frobnicate"}, &stdout, &stderr)
	var exitErr cli.ExitCoder
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("run(frobnicate) err = %v, want ExitCoder with code 2", err)
	}
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("error %q does not name the unknown command", err)
	}
}

func TestRunInitBadFlagIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"holism", "init", "--nope"}, &stdout, &stderr)
	var exitErr cli.ExitCoder
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("run(init --nope) err = %v, want ExitCoder with code 2", err)
	}
}

func TestRunInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	var stdout, stderr bytes.Buffer

	if err := run([]string{"holism", "init", "--dir", dir}, &stdout, &stderr); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, name := range []string{"ca.key", "ca.crt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("after init, %s missing: %v", name, err)
		}
	}
	if !strings.Contains(stdout.String(), "root CA generated") {
		t.Errorf("unexpected init output:\n%s", stdout.String())
	}

	// Second init without --force fails and leaves the CA untouched.
	err := run([]string{"holism", "init", "--dir", dir}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "already initialized") {
		t.Fatalf("second init without --force: want 'already initialized' error, got %v", err)
	}

	// With --force it succeeds.
	if err := run([]string{"holism", "init", "--dir", dir, "--force"}, &stdout, &stderr); err != nil {
		t.Fatalf("init --force: %v", err)
	}
}

func TestRunCertHelpListsSubcommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"holism", "cert"}, &stdout, &stderr); err != nil {
		t.Fatalf("run(cert): %v", err)
	}
	for _, want := range []string{"issue", "list"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("run(cert) stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunCertIssueAndList(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	var stdout, stderr bytes.Buffer

	mustRun := func(args ...string) {
		t.Helper()
		full := append([]string{"holism"}, args...)
		if err := run(full, &stdout, &stderr); err != nil {
			t.Fatalf("run(%v): %v", full, err)
		}
	}

	mustRun("init", "--dir", dir)
	mustRun("cert", "issue", "mind", "--dir", dir, "--san", "DNS:mind.local", "--san", "localhost")
	mustRun("cert", "issue", "drone", "d1", "--dir", dir)
	mustRun("cert", "issue", "operator", "alice", "--dir", dir)

	for _, p := range []string{
		"mind/cert.pem", "mind/key.pem",
		"drones/d1/cert.pem", "drones/d1/key.pem",
		"operators/alice/cert.pem", "operators/alice/key.pem",
	} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}

	// Re-issuing without --force fails; with --force it succeeds.
	if err := run([]string{"holism", "cert", "issue", "drone", "d1", "--dir", dir}, &stdout, &stderr); err == nil {
		t.Error("re-issue without --force: want error, got nil")
	}
	mustRun("cert", "issue", "drone", "d1", "--dir", dir, "--force")

	stdout.Reset()
	mustRun("cert", "list", "--dir", dir)
	out := stdout.String()
	for _, want := range []string{"mind", "drone", "operator", "d1", "alice"} {
		if !strings.Contains(out, want) {
			t.Errorf("cert list output missing %q:\n%s", want, out)
		}
	}
}

func TestRunCertIssueRequiresCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	var stdout, stderr bytes.Buffer
	err := run([]string{"holism", "cert", "issue", "drone", "d1", "--dir", dir}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "holism init") {
		t.Fatalf("issue without CA: want 'run holism init' error, got %v", err)
	}
}

func TestRunCertIssueUsageErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	var stdout, stderr bytes.Buffer

	assertExit2 := func(args ...string) {
		t.Helper()
		full := append([]string{"holism"}, args...)
		err := run(full, &stdout, &stderr)
		var exitErr cli.ExitCoder
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
			t.Fatalf("run(%v) err = %v, want ExitCoder with code 2", full, err)
		}
	}

	assertExit2("cert", "issue", "--dir", dir)                                // no role
	assertExit2("cert", "issue", "bogus", "--dir", dir)                       // unknown role
	assertExit2("cert", "issue", "drone", "--dir", dir)                       // missing id
	assertExit2("cert", "issue", "operator", "--dir", dir)                    // missing id
	assertExit2("cert", "issue", "mind", "--dir", dir, "--san", "URI:://bad") // malformed SAN
}

func TestParseSAN(t *testing.T) {
	dns, uri, err := parseSAN("DNS:mind.local")
	if err != nil || dns != "mind.local" || uri != nil {
		t.Errorf(`parseSAN("DNS:mind.local") = %q, %v, %v`, dns, uri, err)
	}
	dns, uri, err = parseSAN("URI:spiffe://holism/mind/x")
	if err != nil || dns != "" || uri == nil || uri.String() != "spiffe://holism/mind/x" {
		t.Errorf(`parseSAN("URI:...") = %q, %v, %v`, dns, uri, err)
	}
	dns, uri, err = parseSAN("bare.example")
	if err != nil || dns != "bare.example" || uri != nil {
		t.Errorf(`parseSAN("bare.example") = %q, %v, %v`, dns, uri, err)
	}
	if _, _, err := parseSAN("URI:://bad"); err == nil {
		t.Error(`parseSAN("URI:://bad"): want error, got nil`)
	}
}

func TestRunCertListEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	var stdout, stderr bytes.Buffer
	if err := run([]string{"holism", "cert", "list", "--dir", dir}, &stdout, &stderr); err != nil {
		t.Fatalf("cert list on empty PKI: %v", err)
	}
	if !strings.Contains(stdout.String(), "no certificates") {
		t.Errorf("unexpected output:\n%s", stdout.String())
	}
}
