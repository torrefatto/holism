// Command holism is the Holism setup CLI: it bootstraps and maintains the
// PKI used for mTLS between the mind, the drones and human operators.
// See docs/design/agent-swarm-framework.md section 6.2.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/torrefatto/holism/cli/internal/pki"
)

func main() {
	if err := run(os.Args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "holism:", err)
		var exitErr cli.ExitCoder
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}
}

// run executes the CLI with args (including argv[0]) and the given output
// streams. It is kept separate from main so tests can drive it.
func run(args []string, stdout, stderr io.Writer) error {
	cmd := newCommand()
	cmd.Writer = stdout
	cmd.ErrWriter = stderr
	return cmd.Run(context.Background(), args)
}

func newCommand() *cli.Command {
	return &cli.Command{
		Name:        "holism",
		Usage:       "Holism setup CLI (PKI and certificate lifecycle)",
		Description: "Bootstraps and maintains the PKI used for mTLS between the mind, drones and operators.",
		Commands: []*cli.Command{
			{
				Name:         "init",
				Usage:        "generate the root CA into the PKI directory (default ./pki)",
				OnUsageError: onUsageError,
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "dir", Value: pki.DefaultDir, Usage: "PKI directory"},
					&cli.BoolFlag{Name: "force", Usage: "overwrite an existing PKI"},
				},
				Action: runInit,
			},
			{
				Name:         "cert",
				Usage:        "issue / renew / revoke / list leaf certificates (not implemented yet)",
				OnUsageError: onUsageError,
				Action: func(context.Context, *cli.Command) error {
					return errors.New("the cert command is not implemented yet")
				},
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Present() {
				return cli.Exit(fmt.Sprintf("unknown command %q", cmd.Args().First()), 2)
			}
			return cli.ShowRootCommandHelp(cmd)
		},
		OnUsageError: onUsageError,
		// Keep exit-code handling in main: the framework must not os.Exit
		// from inside Run, so tests can drive run() directly.
		ExitErrHandler: func(context.Context, *cli.Command, error) {},
	}
}

// onUsageError prints the command help and returns a usage error (exit 2).
func onUsageError(_ context.Context, cmd *cli.Command, err error, isSubcommand bool) error {
	if isSubcommand {
		_ = cli.ShowSubcommandHelp(cmd)
	} else {
		_ = cli.ShowRootCommandHelp(cmd)
	}
	return cli.Exit(err.Error(), 2)
}

func runInit(_ context.Context, cmd *cli.Command) error {
	cfg := pki.Config{Dir: cmd.String("dir")}
	cert, _, err := pki.GenerateCA(cfg, cmd.Bool("force"))
	if err != nil {
		return err
	}
	w := cmd.Root().Writer
	fmt.Fprintf(w, "root CA generated in %s\n", cfg.Dir)
	fmt.Fprintf(w, "  cert: %s (CN=%q, valid until %s)\n",
		cfg.CACertPath(), cert.Subject.CommonName, cert.NotAfter.Format(time.RFC3339))
	fmt.Fprintf(w, "  key:  %s\n", cfg.CAKeyPath())
	return nil
}
