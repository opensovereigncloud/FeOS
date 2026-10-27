// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Command feos-k8s keeps a Kubernetes cluster's FeOS nodes and FeRun in
// step (ADR 0020, internal/feosk8s): join tokens for new nodes, guard pods
// that hold a cordoned node's drain until FeRun has drained it, undrains on
// uncordon, and the removal of nodes that are gone. It runs in the cluster,
// one replica, with the service account's token.
//
//	feos-k8s --ferun-url http://ferund.ferun:8100 \
//	    --ferun-credentials-file /etc/feos-k8s/ferun/credentials \
//	    --node-name-prefix shoot--myproject--myshoot- [flags]
//
// Every flag has an environment variable FEOS_K8S_<NAME> (upper snake case
// of the flag); a flag wins over its variable. SIGTERM and SIGINT stop it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gardenlinux/feos/go/internal/feosk8s"
	"github.com/gardenlinux/feos/go/internal/kube"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr))
}

// options are the parsed flags.
type options struct {
	ctl        feosk8s.Config
	ferunURL   string
	ferunCreds string
	healthAddr string
	logLevel   slog.Level
	logJSON    bool
	kubeServer string // development: the API without the in-cluster files
}

// envName is the environment variable of a flag.
func envName(flagName string) string {
	return "FEOS_K8S_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// parse reads the flags and their environment variables.
func parse(args []string, getenv func(string) string, out io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("feos-k8s", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&o.ferunURL, "ferun-url", "", "FeRun's front door, http(s)://host[:port] (required)")
	fs.StringVar(&o.ferunCreds, "ferun-credentials-file", "",
		"a file holding user:password of one of ferund's --admin-users, read at every request (required)")
	fs.StringVar(&o.ctl.NodeSelector, "node-selector", "feos.fecode.dev/node=true", "the label selector of the Kubernetes nodes that run FeOS")
	fs.StringVar(&o.ctl.NodeNamePrefix, "node-name-prefix", "",
		"only FeRun nodes whose name begins with it are removed when their Kubernetes node is gone (required)")
	fs.StringVar(&o.ctl.Namespace, "namespace", "feos", "the namespace of the guards and the join secret")
	fs.StringVar(&o.ctl.JoinSecret, "join-secret", "feosd-join", "the secret with a join token per node name")
	fs.StringVar(&o.ctl.GuardImage, "guard-image", "registry.k8s.io/pause:3.10", "the guards' image")
	fs.StringVar(&o.ctl.GuardPriorityClass, "guard-priority-class", "system-node-critical", "the guards' priority class")
	fs.DurationVar(&o.ctl.Interval, "interval", 5*time.Second, "the time between passes")
	fs.DurationVar(&o.ctl.JoinTokenTTL, "join-token-ttl", time.Hour, "a join token's lifetime; one that expires unused is replaced")
	fs.DurationVar(&o.ctl.RemoveAfter, "remove-after", time.Minute,
		"how long a FeRun node is down without a Kubernetes node before it is removed")
	fs.StringVar(&o.healthAddr, "health-listen", ":8080", "the address of /healthz; empty: none")
	level := fs.String("log-level", "info", "debug, info, warn or error")
	fs.BoolVar(&o.logJSON, "log-json", false, "log JSON lines instead of text")
	fs.StringVar(&o.kubeServer, "kube-server", "", "the Kubernetes API's URL instead of the in-cluster one (development)")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments %q", fs.Args())
	}
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
		return o, envErr
	}
	if err := o.logLevel.UnmarshalText([]byte(*level)); err != nil {
		return o, fmt.Errorf("--log-level: %w", err)
	}
	switch {
	case o.ferunURL == "":
		return o, errors.New("--ferun-url is required")
	case o.ferunCreds == "":
		return o, errors.New("--ferun-credentials-file is required")
	case o.ctl.NodeNamePrefix == "":
		return o, errors.New("--node-name-prefix is required: it keeps the controller off other clusters' nodes")
	case o.ctl.Interval <= 0 || o.ctl.JoinTokenTTL <= 0 || o.ctl.RemoveAfter < 0:
		return o, errors.New("--interval and --join-token-ttl must be positive, --remove-after not negative")
	}
	return o, nil
}

func run(args []string, getenv func(string) string, stderr io.Writer) int {
	o, err := parse(args, getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "feos-k8s:", err)
		return 2
	}
	if err := serve(o, getenv, stderr); err != nil {
		fmt.Fprintln(stderr, "feos-k8s:", err)
		return 1
	}
	return 0
}

func serve(o options, getenv func(string) string, stderr io.Writer) error {
	hopts := &slog.HandlerOptions{Level: o.logLevel}
	var log *slog.Logger
	if o.logJSON {
		log = slog.New(slog.NewJSONHandler(stderr, hopts))
	} else {
		log = slog.New(slog.NewTextHandler(stderr, hopts))
	}
	kcfg := kube.Config{Server: o.kubeServer}
	if o.kubeServer == "" {
		var err error
		if kcfg, err = kube.InCluster(getenv); err != nil {
			return err
		}
	}
	kc, err := kube.New(kcfg)
	if err != nil {
		return err
	}
	fr, err := feosk8s.NewFeRun(o.ferunURL, o.ferunCreds, nil)
	if err != nil {
		return err
	}
	ctl, err := feosk8s.New(o.ctl, kc, fr, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if o.healthAddr != "" {
		if err := serveHealth(ctx, o.healthAddr, ctl, max(time.Minute, 6*o.ctl.Interval), log); err != nil {
			return err
		}
	}
	log.Info("feos-k8s: starting", "ferun", o.ferunURL, "kubernetes", kcfg.Server, "selector", o.ctl.NodeSelector,
		"prefix", o.ctl.NodeNamePrefix, "namespace", o.ctl.Namespace, "secret", o.ctl.JoinSecret)
	err = ctl.Run(ctx)
	log.Info("feos-k8s: stopped")
	return err
}

// serveHealth serves /healthz until ctx ends: 200 while passes end, 503
// when none ended within maxAge (a pass that hangs).
func serveHealth(ctx context.Context, addr string, ctl *feosk8s.Controller, maxAge time.Duration, log *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen for /healthz: %w", err)
	}
	start := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if time.Since(start) > maxAge && !ctl.Healthy(maxAge) {
			http.Error(w, "no pass ended within "+maxAge.String(), http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("serve /healthz", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	return nil
}
