package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunHelpListsSubcommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		stdout.Reset()
		if err := run(args, &stdout, &stderr); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
		out := stdout.String()
		for _, want := range []string{"init", "cert"} {
			if !strings.Contains(out, want) {
				t.Errorf("run(%v) output missing %q:\n%s", args, want, out)
			}
		}
	}
}

func TestRunNoArgsIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(nil, &stdout, &stderr)
	if !errors.Is(err, errUsage) {
		t.Fatalf("run(nil) err = %v, want errUsage", err)
	}
	if !strings.Contains(stderr.String(), "init") {
		t.Errorf("usage output missing subcommands:\n%s", stderr.String())
	}
}

func TestRunUnknownCommandIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"frobnicate"}, &stdout, &stderr); !errors.Is(err, errUsage) {
		t.Fatalf("run(frobnicate) err = %v, want errUsage", err)
	}
}

func TestRunInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	var stdout, stderr bytes.Buffer

	if err := run([]string{"init", "--dir", dir}, &stdout, &stderr); err != nil {
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
	if err := run([]string{"init", "--dir", dir}, &stdout, &stderr); err == nil {
		t.Fatal("second init without --force: want error, got nil")
	}

	// With --force it succeeds.
	if err := run([]string{"init", "--dir", dir, "--force"}, &stdout, &stderr); err != nil {
		t.Fatalf("init --force: %v", err)
	}
}

func TestRunCertNotImplemented(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"cert", "list"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("run(cert list) err = %v, want not-implemented error", err)
	}
}
