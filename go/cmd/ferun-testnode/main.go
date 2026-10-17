// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Command ferun-testnode runs a test node (internal/testnode): a fake of
// feosd that joins a control plane and serves pods in memory, for local
// development of ferund and its UI without microVMs.
//
//	ferun-testnode --join-token feos-join-<secret>.<pin> --server-name ferun.example.com \
//	    --addr 127.0.0.1:7443 [--name NAME] [--trust-domain feos.local] [--state-dir DIR] ...
//
// Every flag has an environment variable FERUN_TESTNODE_<NAME> (upper
// snake case of the flag); a flag wins over its variable. SIGINT and
// SIGTERM stop the node; SIGHUP restarts it gracefully, as an upgrade of
// feosd does (a "stopping" event first, then a new start).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ironcore-dev/feos/go/internal/testnode"
)

func main() {
	if err := run(os.Args[1:], os.Getenv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "ferun-testnode:", err)
		os.Exit(1)
	}
}

// addrs is a repeatable flag of addresses; a value may list several,
// separated by commas.
type addrs []string

func (a *addrs) String() string { return strings.Join(*a, ",") }

func (a *addrs) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*a = append(*a, s)
		}
	}
	return nil
}

// envName is the environment variable of a flag.
func envName(flagName string) string {
	return "FERUN_TESTNODE_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// byteSize parses an integer with an optional KiB, MiB, GiB or TiB suffix.
func byteSize(s string) (uint64, error) {
	units := []struct {
		suffix string
		shift  uint
	}{{"TiB", 40}, {"GiB", 30}, {"MiB", 20}, {"KiB", 10}}
	for _, u := range units {
		if v, ok := strings.CutSuffix(s, u.suffix); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return 0, err
			}
			return n << u.shift, nil
		}
	}
	return strconv.ParseUint(s, 10, 64)
}

// parse reads the flags and their environment variables into the node's
// configuration and the log level.
func parse(args []string, getenv func(string) string) (testnode.Config, slog.Level, error) {
	var level slog.Level
	host, _ := os.Hostname()
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	fs := flag.NewFlagSet("ferun-testnode", flag.ContinueOnError)
	name := fs.String("name", strings.ToLower(host), "the name the node proposes at its join (a DNS label)")
	token := fs.String("join-token", "", "the join token, feos-join-<secret>.<pin>; not needed when --state-dir holds an identity")
	serverName := fs.String("server-name", "", "the DNS name of the control plane's certificate on the nodes' port (required)")
	trustDomain := fs.String("trust-domain", "feos.local", "the SPIFFE trust domain")
	stateDir := fs.String("state-dir", "", "keeps the node's identity and its pods' directories; empty: a temporary directory")
	var bootstrap addrs
	fs.Var(&bootstrap, "addr", "host:port of the control plane's nodes' port, repeatable (required)")
	objectStore := fs.String("object-store", "", "the name of an object store in this process's memory, so that pods hibernate (no other process shares it); empty: none")
	objectStoreDir := fs.String("object-store-dir", "", "a directory that holds an object store, shared by every test node that names it; empty: none")
	pendingDelay := fs.Duration("pending-delay", 2*time.Second, "how long a pod's bring-up takes")
	abortOnCancel := fs.Bool("abort-on-cancel", false, "mutating pod calls stop where today's feosd does when their request is cancelled")
	maxStreams := fs.Uint("max-streams", 1024, "the HTTP/2 stream limit of the node connections")
	cpus := fs.Uint("cpus", 16, "the CPU cores the node reports")
	memory := fs.String("memory", "64GiB", "the memory the node reports")
	disk := fs.String("disk", "1TiB", "the disk the node reports")
	podPrefix := fs.String("pod-prefix", "", "the pods' IPv6 prefix; empty: derived from the name")
	onMesh := fs.Bool("mesh", false, "put the node on the pods' mesh: ask JoinMesh with a key of its own, and serve MeshService")
	meshEndpoint := fs.String("mesh-endpoint", "", "the node's endpoint on the mesh, address:port; empty: 127.0.0.1 and a port from the name")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")

	if err := fs.Parse(args); err != nil {
		return testnode.Config{}, level, err
	}
	if fs.NArg() > 0 {
		return testnode.Config{}, level, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	// The environment for the flags not given, so that a flag wins over its
	// variable, also a repeatable one.
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		if v := getenv(envName(f.Name)); v != "" && !given[f.Name] && envErr == nil {
			if err := fs.Set(f.Name, v); err != nil {
				envErr = fmt.Errorf("%s: %w", envName(f.Name), err)
			}
		}
	})
	if envErr != nil {
		return testnode.Config{}, level, envErr
	}
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return testnode.Config{}, level, fmt.Errorf("--log-level: %w", err)
	}
	memBytes, err := byteSize(*memory)
	if err != nil {
		return testnode.Config{}, level, fmt.Errorf("--memory: %w", err)
	}
	diskBytes, err := byteSize(*disk)
	if err != nil {
		return testnode.Config{}, level, fmt.Errorf("--disk: %w", err)
	}
	if *maxStreams > 1<<31 || *cpus > 1<<16 {
		return testnode.Config{}, level, errors.New("--max-streams or --cpus out of range")
	}
	cfg := testnode.Config{
		Name:          *name,
		JoinToken:     *token,
		ServerName:    *serverName,
		TrustDomain:   *trustDomain,
		StateDir:      *stateDir,
		Addrs:         bootstrap,
		PendingDelay:  *pendingDelay,
		AbortOnCancel: *abortOnCancel,
		MaxStreams:    uint32(*maxStreams),
		CPUCores:      uint32(*cpus),
		MemoryBytes:   memBytes,
		DiskBytes:     diskBytes,
		PodPrefix:     *podPrefix,
		Mesh:          *onMesh,
		MeshEndpoint:  *meshEndpoint,
	}
	switch {
	case *objectStore != "" && *objectStoreDir != "":
		return testnode.Config{}, level, errors.New("--object-store and --object-store-dir exclude each other")
	case *objectStore != "":
		cfg.ObjectStore = testnode.NewObjectStore(*objectStore)
	case *objectStoreDir != "":
		if cfg.ObjectStore, err = testnode.OpenObjectStore(*objectStoreDir); err != nil {
			return testnode.Config{}, level, err
		}
	}
	return cfg, level, nil
}

func run(args []string, getenv func(string) string) error {
	cfg, level, err := parse(args, getenv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	cfg.Log = log
	n, err := testnode.New(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for {
			select {
			case <-hup:
				log.Info("ferun-testnode: restarting gracefully")
				n.Restart(true)
			case <-ctx.Done():
				return
			}
		}
	}()
	log.Info("ferun-testnode: starting", "name", cfg.Name, "addrs", strings.Join(cfg.Addrs, ","), "server-name", cfg.ServerName)
	return n.Run(ctx)
}
