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

func TestRunCertNotImplemented(t *testing.T) {
	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{{"holism", "cert"}, {"holism", "cert", "list"}} {
		err := run(args, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "not implemented") {
			t.Fatalf("run(%v) err = %v, want not-implemented error", args, err)
		}
	}
}
