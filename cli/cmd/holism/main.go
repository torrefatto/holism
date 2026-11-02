// Command holism is the Holism setup CLI: it bootstraps and maintains the
// PKI used for mTLS between the mind, the drones and human operators.
// See docs/design/agent-swarm-framework.md section 6.2.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/torrefatto/holism/cli/internal/pki"
)

const usage = `holism is the Holism setup CLI (PKI and certificate lifecycle).

Usage:
  holism <command> [flags]

Commands:
  init    generate the root CA into the PKI directory (default ./pki)
  cert    issue / renew / revoke / list leaf certificates (not implemented yet)

Use "holism <command> -help" for command-specific flags.
`

// errUsage marks usage errors (unknown command, bad flags): exit code 2.
var errUsage = errors.New("usage error")

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "holism:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}
	switch args[0] {
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "cert":
		return errors.New("the cert command is not implemented yet")
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprintf(stderr, "holism: unknown command %q\n\n%s", args[0], usage)
		return errUsage
	}
}

func runInit(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", pki.DefaultDir, "PKI directory")
	force := fs.Bool("force", false, "overwrite an existing PKI")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}

	cfg := pki.Config{Dir: *dir}
	cert, _, err := pki.GenerateCA(cfg, *force)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "root CA generated in %s\n", cfg.Dir)
	fmt.Fprintf(stdout, "  cert: %s (CN=%q, valid until %s)\n",
		cfg.CACertPath(), cert.Subject.CommonName, cert.NotAfter.Format(time.RFC3339))
	fmt.Fprintf(stdout, "  key:  %s\n", cfg.CAKeyPath())
	return nil
}
