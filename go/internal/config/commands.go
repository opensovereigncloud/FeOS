// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/ironcore-dev/feos/go/internal/pki"
)

// The subcommands of ferund.
const (
	CmdServe     = "serve"
	CmdInitCA    = "init-ca"
	CmdJoinToken = "join-token"
	CmdMigrate   = "migrate"
)

// InitCA is the configuration of `ferund init-ca`: the platform CA's root
// (key in RootDir) and its issuer (in Dir).
type InitCA struct {
	RootDir, Dir, TrustDomain, ControlPlaneDomain string
	DNSDomains                                    []string
}

// ParseInitCA reads the flags of init-ca (and their variables).
func ParseInitCA(args []string, getenv func(string) string) (InitCA, error) {
	c := InitCA{TrustDomain: Default().TrustDomain}
	fs := newFlagSet(CmdInitCA)
	registerInitCA(fs, &c)
	if err := parseFlags(fs, args, getenv); err != nil {
		return InitCA{}, err
	}
	var errs []error
	if c.RootDir == "" {
		errs = append(errs, errors.New("--root-dir is required"))
	}
	if c.Dir == "" {
		errs = append(errs, errors.New("--dir is required"))
	}
	if len(c.DNSDomains) == 0 {
		errs = append(errs, errors.New("--dns-domain is required"))
	}
	if err := pki.ValidateTrustDomain(c.TrustDomain); err != nil {
		errs = append(errs, fmt.Errorf("--trust-domain: %w", err))
	}
	return c, errors.Join(errs...)
}

func registerInitCA(fs *flag.FlagSet, c *InitCA) {
	fs.StringVar(&c.RootDir, "root-dir", c.RootDir, "directory for the root's certificate and key (keep it offline; required)")
	fs.StringVar(&c.Dir, "dir", c.Dir, "directory for the issuer, issuer.json and bundle.crt: an instance's --ca-dir (required)")
	fs.StringVar(&c.TrustDomain, "trust-domain", c.TrustDomain, "SPIFFE trust domain")
	fs.Var((*listValue)(&c.DNSDomains), "dns-domain", "the platform's DNS domain the CA may issue under, repeatable (required)")
	fs.StringVar(&c.ControlPlaneDomain, "control-plane-domain", c.ControlPlaneDomain,
		"the instances' DNS domain on the nodes' port (default ferun.<first --dns-domain>)")
}

// JoinToken is the configuration of `ferund join-token`.
type JoinToken struct {
	DatabaseURL, CADir, TrustDomain string
	// TTL is the token's lifetime; 0 means JoinTokenTTL.
	TTL time.Duration
	// Node pins the token to a node name; "" lets it create a new name
	// only.
	Node     string
	LogLevel slog.Level
}

// ParseJoinToken reads the flags of join-token, with serve's variable
// names and defaults for the flags they share.
func ParseJoinToken(args []string, getenv func(string) string) (JoinToken, error) {
	d := Default()
	c := JoinToken{CADir: d.CADir, TrustDomain: d.TrustDomain, TTL: JoinTokenTTL, LogLevel: slog.LevelWarn}
	fs := newFlagSet(CmdJoinToken)
	level := registerJoinToken(fs, &c)
	if err := parseFlags(fs, args, getenv); err != nil {
		return JoinToken{}, err
	}
	var errs []error
	if err := c.LogLevel.UnmarshalText([]byte(*level)); err != nil {
		errs = append(errs, fmt.Errorf("--log-level: %w", err))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("--database-url is required"))
	}
	if c.CADir == "" {
		errs = append(errs, errors.New("--ca-dir must not be empty"))
	}
	if err := pki.ValidateTrustDomain(c.TrustDomain); err != nil {
		errs = append(errs, fmt.Errorf("--trust-domain: %w", err))
	}
	if c.TTL <= 0 {
		errs = append(errs, errors.New("--ttl must be positive"))
	}
	if c.Node != "" && !pki.IsNodeName(c.Node) {
		errs = append(errs, fmt.Errorf("--node %q is not a node name (a DNS label)", c.Node))
	}
	return c, errors.Join(errs...)
}

func registerJoinToken(fs *flag.FlagSet, c *JoinToken) *string {
	level := strings.ToLower(c.LogLevel.String())
	fs.StringVar(&c.DatabaseURL, "database-url", c.DatabaseURL, "PostgreSQL DSN (required)")
	fs.StringVar(&c.CADir, "ca-dir", c.CADir, "directory of the issuer (for the token's pin)")
	fs.StringVar(&c.TrustDomain, "trust-domain", c.TrustDomain, "SPIFFE trust domain")
	fs.DurationVar(&c.TTL, "ttl", c.TTL, "the token's lifetime")
	fs.StringVar(&c.Node, "node", c.Node, "pin the token to this node name (a re-join of that name); empty: the node proposes a new name")
	fs.StringVar(&level, "log-level", level, "debug, info, warn or error")
	return &level
}

// Migrate is the configuration of `ferund migrate`.
type Migrate struct {
	DatabaseURL string
	LogLevel    slog.Level
}

// ParseMigrate reads the flags of migrate.
func ParseMigrate(args []string, getenv func(string) string) (Migrate, error) {
	c := Migrate{LogLevel: slog.LevelInfo}
	fs := newFlagSet(CmdMigrate)
	level := registerMigrate(fs, &c)
	if err := parseFlags(fs, args, getenv); err != nil {
		return Migrate{}, err
	}
	var errs []error
	if err := c.LogLevel.UnmarshalText([]byte(*level)); err != nil {
		errs = append(errs, fmt.Errorf("--log-level: %w", err))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("--database-url is required"))
	}
	return c, errors.Join(errs...)
}

func registerMigrate(fs *flag.FlagSet, c *Migrate) *string {
	level := strings.ToLower(c.LogLevel.String())
	fs.StringVar(&c.DatabaseURL, "database-url", c.DatabaseURL, "PostgreSQL DSN (required)")
	fs.StringVar(&level, "log-level", level, "debug, info, warn or error")
	return &level
}

// Usage writes the help of a subcommand ("" or "ferund": the overview).
func Usage(w io.Writer, cmd string) {
	var fs *flag.FlagSet
	switch cmd {
	case CmdServe:
		fs = newFlagSet(cmd)
		c := Default()
		registerServe(fs, &c)
		fmt.Fprintln(w, "Usage: ferund [serve] [flags]\n\nRuns an instance of the control plane: the front doors (REST, MCP, the UI, the\nports) and the nodes' port, on the shared PostgreSQL database.")
	case CmdInitCA:
		fs = newFlagSet(cmd)
		registerInitCA(fs, &InitCA{TrustDomain: Default().TrustDomain})
		fmt.Fprintln(w, "Usage: ferund init-ca --root-dir R --dir D --dns-domain X [--dns-domain Y ...] [flags]\n\nMakes the platform CA: the root (its key stays in R, offline) and the issuer\nevery instance holds in its --ca-dir (D). Existing files are never overwritten.")
	case CmdJoinToken:
		fs = newFlagSet(cmd)
		registerJoinToken(fs, &JoinToken{CADir: Default().CADir, TrustDomain: Default().TrustDomain, TTL: JoinTokenTTL, LogLevel: slog.LevelWarn})
		fmt.Fprintln(w, "Usage: ferund join-token --database-url URL [flags]\n\nIssues a one-time join token for a node and prints it: feos-join-<secret>.<pin>.")
	case CmdMigrate:
		fs = newFlagSet(cmd)
		registerMigrate(fs, &Migrate{LogLevel: slog.LevelInfo})
		fmt.Fprintln(w, "Usage: ferund migrate --database-url URL\n\nApplies the database migrations and exits.")
	default:
		fmt.Fprintln(w, `Usage: ferund <command> [flags]

FeRun, the control plane of FeOS (ADR 0015).

Commands:
  serve        run an instance (the default when no command is given)
  init-ca      make the platform CA's root and issuer
  join-token   issue a join token for a node
  migrate      apply the database migrations and exit

Every flag has an environment variable FERUN_<NAME>, the flag's name in upper
snake case (--database-url: FERUN_DATABASE_URL); a flag wins over its variable.
Run "ferund <command> --help" for a command's flags.`)
		return
	}
	fmt.Fprintln(w, "\nFlags:")
	fs.VisitAll(func(f *flag.Flag) {
		def := f.DefValue
		if def != "" && def != "false" && def != "0" && def != "0s" {
			fmt.Fprintf(w, "  --%s (%s, default %q)\n", f.Name, EnvName(f.Name), def)
		} else {
			fmt.Fprintf(w, "  --%s (%s)\n", f.Name, EnvName(f.Name))
		}
		fmt.Fprintf(w, "        %s\n", f.Usage)
	})
}
