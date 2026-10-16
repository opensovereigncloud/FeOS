// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Command ferund is FeRun, the control plane of FeOS (ADR 0015): every
// instance serves the REST API, MCP, the management UI and the ports to
// clients, and the nodes' port to feosd, on one shared PostgreSQL.
//
//	ferund [serve] [flags]          run an instance (the default)
//	ferund init-ca --root-dir R --dir D --dns-domain X ...
//	ferund join-token --database-url URL [--ttl 1h] [--node NAME]
//	ferund migrate --database-url URL
//
// Every flag has an environment variable FERUN_<NAME> (internal/config).
// SIGTERM and SIGINT stop an instance as §7 of the design says: requests
// and ops in flight finish within --shutdown-grace.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/ironcore-dev/feos/go/internal/config"
	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/fleet"
	"github.com/ironcore-dev/feos/go/internal/pki"
	"github.com/ironcore-dev/feos/go/internal/server"
)

// version is the build's version, set with -ldflags "-X main.version=…";
// otherwise the module's version from the build info, or "dev".
var version = ""

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// run runs a subcommand and returns the exit code: 0, 1 for a failure, 2
// for a usage error.
func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := config.CmdServe
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	} else if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		config.Usage(stdout, "")
		return 0
	}
	var err error
	switch cmd {
	case config.CmdServe:
		err = serve(args, getenv, stderr)
	case config.CmdInitCA:
		err = initCA(args, getenv, stdout)
	case config.CmdJoinToken:
		err = joinToken(args, getenv, stdout, stderr)
	case config.CmdMigrate:
		err = migrate(args, getenv, stderr)
	case "help":
		topic := ""
		if len(args) > 0 {
			topic = args[0]
		}
		config.Usage(stdout, topic)
		return 0
	case "version":
		fmt.Fprintln(stdout, buildVersion())
		return 0
	default:
		fmt.Fprintf(stderr, "ferund: unknown command %q\n\n", cmd)
		config.Usage(stderr, "")
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		config.Usage(stdout, cmd)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "ferund %s: %v\n", cmd, err)
		var u usageError
		if errors.As(err, &u) {
			return 2
		}
		return 1
	}
	return 0
}

// usageError marks an error in the flags.
type usageError struct{ error }

func (u usageError) Unwrap() error { return u.error }

func flagErr(err error) error {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return err
	}
	return usageError{err}
}

// buildVersion is the version to report.
func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// serve runs an instance until SIGTERM or SIGINT, then stops it.
func serve(args []string, getenv func(string) string, stderr io.Writer) error {
	cfg, err := config.Parse(args, getenv)
	if err != nil {
		return flagErr(err)
	}
	cfg.Version = buildVersion()
	log := newLogger(stderr, cfg.LogLevel)
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		// A second signal ends the process at once.
		<-ctx.Done()
		stop()
	}()
	inst, err := server.New(ctx, cfg, log)
	if err != nil {
		return err
	}
	return inst.Run(ctx)
}

// initCA makes the platform CA.
func initCA(args []string, getenv func(string) string, stdout io.Writer) error {
	c, err := config.ParseInitCA(args, getenv)
	if err != nil {
		return flagErr(err)
	}
	if err := pki.InitCA(pki.InitCAOptions{RootDir: c.RootDir, Dir: c.Dir, TrustDomain: c.TrustDomain,
		ControlPlaneDomain: c.ControlPlaneDomain, DNSDomains: c.DNSDomains}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "made the root in %s (keep its key offline) and the issuer in %s (an instance's --ca-dir)\n", c.RootDir, c.Dir)
	return nil
}

// joinToken issues a join token in the database and prints it.
func joinToken(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	c, err := config.ParseJoinToken(args, getenv)
	if err != nil {
		return flagErr(err)
	}
	log := newLogger(stderr, c.LogLevel)
	iss, err := pki.LoadIssuer(c.CADir, c.TrustDomain)
	if err != nil {
		return fmt.Errorf("load the issuer from %s: %w", c.CADir, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	d, err := db.Open(ctx, c.DatabaseURL, log)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer d.Close()
	fl := fleet.New(fleet.Config{TrustDomain: c.TrustDomain, Log: log}, fleet.Deps{DB: d, Issuer: iss})
	id, token, expires, err := fl.IssueJoinToken(ctx, "ferund join-token", c.Node, c.TTL)
	if err != nil {
		return err
	}
	pinned := "any new name"
	if c.Node != "" {
		pinned = "node " + c.Node
	}
	fmt.Fprintf(stderr, "join token %s for %s, valid until %s\n", id, pinned, expires.UTC().Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintln(stdout, token)
	return nil
}

// migrate applies the migrations.
func migrate(args []string, getenv func(string) string, stderr io.Writer) error {
	c, err := config.ParseMigrate(args, getenv)
	if err != nil {
		return flagErr(err)
	}
	log := newLogger(stderr, c.LogLevel)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	d, err := db.Open(ctx, c.DatabaseURL, log)
	if err != nil {
		return fmt.Errorf("open the database: %w", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx, nil); err != nil {
		return err
	}
	log.Info("the database is migrated")
	return nil
}
