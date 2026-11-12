// Command holism is the Holism setup CLI: it bootstraps and maintains the
// PKI used for mTLS between the mind, the drones and human operators.
// See docs/design/agent-swarm-framework.md section 6.2.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
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
				Name:  "cert",
				Usage: "issue / renew / revoke / list leaf certificates",
				Commands: []*cli.Command{
					{
						Name:      "issue",
						Usage:     "issue a leaf certificate: 'mind' (server) or 'drone|operator <id>' (client)",
						ArgsUsage: "<mind|drone|operator> [id]",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "dir", Value: pki.DefaultDir, Usage: "PKI directory"},
							&cli.StringFlag{Name: "cn", Value: string(pki.RoleMind), Usage: "common name for the mind server certificate"},
							&cli.StringSliceFlag{Name: "san", Usage: "SAN, openssl-style 'DNS:name' or 'URI:uri' (repeatable; mind defaults to its CN as DNS SAN)"},
							&cli.BoolFlag{Name: "force", Usage: "re-issue even if a certificate already exists"},
						},
						OnUsageError: onUsageError,
						Action:       runCertIssue,
					},
					{
						Name:  "list",
						Usage: "list issued certificates (name, role, subject, expiry)",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "dir", Value: pki.DefaultDir, Usage: "PKI directory"},
						},
						OnUsageError: onUsageError,
						Action:       runCertList,
					},
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

func runCertIssue(_ context.Context, cmd *cli.Command) error {
	role := pki.Role(cmd.Args().First())
	if !role.Valid() {
		return cli.Exit(fmt.Sprintf("unknown role %q: want mind, drone or operator", cmd.Args().First()), 2)
	}

	cfg := pki.Config{Dir: cmd.String("dir")}

	var id string
	switch role {
	case pki.RoleMind:
		id = cmd.String("cn")
	default:
		id = cmd.Args().Get(1)
		if id == "" {
			return cli.Exit(fmt.Sprintf("missing id: usage 'holism cert issue %s <id>'", role), 2)
		}
	}

	opts := pki.IssueOptions{Force: cmd.Bool("force")}
	for _, san := range cmd.StringSlice("san") {
		dns, uri, err := parseSAN(san)
		if err != nil {
			return cli.Exit(fmt.Sprintf("invalid --san %q: %v", san, err), 2)
		}
		if dns != "" {
			opts.DNSNames = append(opts.DNSNames, dns)
		}
		if uri != nil {
			opts.URIs = append(opts.URIs, uri)
		}
	}
	if role == pki.RoleMind && len(opts.DNSNames) == 0 {
		opts.DNSNames = []string{id} // default the server SAN to the CN
	}

	cert, err := cfg.IssueLeaf(role, id, opts)
	if err != nil {
		return err
	}
	w := cmd.Root().Writer
	fmt.Fprintf(w, "issued %s certificate for %q\n", role, id)
	fmt.Fprintf(w, "  cert: %s (CN=%q, valid until %s)\n",
		cfg.EntityCertPath(role, id), cert.Subject.CommonName, cert.NotAfter.Format(time.RFC3339))
	fmt.Fprintf(w, "  key:  %s\n", cfg.EntityKeyPath(role, id))
	return nil
}

// parseSAN parses one --san value: "DNS:name" or "URI:uri" (openssl-style
// prefixes); a bare value is treated as a DNS name.
func parseSAN(s string) (dns string, uri *url.URL, err error) {
	if rest, ok := strings.CutPrefix(s, "DNS:"); ok {
		return rest, nil, nil
	}
	if rest, ok := strings.CutPrefix(s, "URI:"); ok {
		u, err := url.Parse(rest)
		if err != nil {
			return "", nil, err
		}
		return "", u, nil
	}
	return s, nil, nil
}

func runCertList(_ context.Context, cmd *cli.Command) error {
	cfg := pki.Config{Dir: cmd.String("dir")}
	certs, err := cfg.ListIssued()
	if err != nil {
		return err
	}
	w := cmd.Root().Writer
	if len(certs) == 0 {
		fmt.Fprintln(w, "no certificates issued yet")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tROLE\tSUBJECT\tEXPIRES\tCERT")
	for _, c := range certs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			c.Name, c.Role, c.Subject, c.NotAfter.Format(time.RFC3339), c.CertPath)
	}
	return tw.Flush()
}
