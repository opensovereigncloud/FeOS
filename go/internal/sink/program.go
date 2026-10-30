// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/sink/s3"
)

// tenantTTL is how long a token's tenant is kept.
const tenantTTL = time.Minute

// Program is a running sink: its database, bucket, identity, the two
// listeners and the loops of the cold stage and the sweep.
type Program struct {
	cfg     Config
	log     *slog.Logger
	db      *db.DB
	store   *Store
	ident   *Identity
	tenants *Tenants
	apiLis  net.Listener

	mu        sync.Mutex
	ingestLis net.Listener
	ingestUp  chan struct{}
}

// Deps are what tests replace: the bucket (nil: the S3 client of the
// configuration) and the HTTP client of the calls to ferund (nil: the
// default).
type Deps struct {
	Bucket Bucket
	HTTP   *http.Client
}

// New opens and migrates the database and binds the HTTP API's listener;
// the ingest listens once the first certificate is there (Run).
func New(ctx context.Context, cfg Config, deps Deps, log *slog.Logger) (*Program, error) {
	if log == nil {
		log = slog.Default()
	}
	d, err := db.Open(ctx, withAppName(cfg.DatabaseURL), log)
	if err != nil {
		return nil, err
	}
	if err := Migrate(ctx, d); err != nil {
		d.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	bucket := deps.Bucket
	if bucket == nil {
		c, err := s3.New(cfg.BucketEndpoint, cfg.BucketRegion, cfg.Bucket, cfg.BucketAccessKey, cfg.BucketSecretKey)
		if err != nil {
			d.Close()
			return nil, err
		}
		bucket = c
	}
	lis, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		d.Close()
		return nil, fmt.Errorf("API listener: %w", err)
	}
	return &Program{
		cfg:      cfg,
		log:      log,
		db:       d,
		store:    NewStore(d, bucket, cfg, log),
		ident:    NewIdentity(cfg.FerunURL, cfg.ServiceToken, cfg.TrustDomain, cfg.Name, deps.HTTP, log),
		tenants:  NewTenants(cfg.FerunURL, deps.HTTP, tenantTTL),
		apiLis:   lis,
		ingestUp: make(chan struct{}),
	}, nil
}

// withAppName names the sink's connections unless the URL does.
func withAppName(u string) string {
	if strings.Contains(u, "application_name") {
		return u
	}
	if strings.HasPrefix(u, "postgres://") || strings.HasPrefix(u, "postgresql://") {
		if strings.Contains(u, "?") {
			return u + "&application_name=ferun-sink"
		}
		return u + "?application_name=ferun-sink"
	}
	return u + " application_name=ferun-sink"
}

// APIAddr is the HTTP API's address.
func (p *Program) APIAddr() net.Addr { return p.apiLis.Addr() }

// IngestAddr is the ingest's address once it listens: it waits for that
// or for ctx.
func (p *Program) IngestAddr(ctx context.Context) (net.Addr, error) {
	select {
	case <-p.ingestUp:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ingestLis.Addr(), nil
}

// Run serves until ctx ends, then stops the listeners (requests get 10 s)
// and closes the database.
func (p *Program) Run(ctx context.Context) error {
	defer p.db.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	errc := make(chan error, 4)
	run := func(name string, f func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(ctx); err != nil {
				errc <- fmt.Errorf("%s: %w", name, err)
				cancel()
			}
		}()
	}
	run("identity", p.ident.Run)
	run("cold stage", p.store.RunChunks)
	run("sweep", p.store.RunSweep)
	run("ingest", p.serveIngest)
	run("api", p.serveAPI)
	wg.Wait()
	close(errc)
	var errs []error
	for err := range errc {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (p *Program) serveAPI(ctx context.Context) error {
	srv := &http.Server{
		Handler:           newAPI(p.store, p.tenants, p.ingestUp, p.log),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(p.apiLis) }()
	p.log.Info("API listening", "addr", p.apiLis.Addr().String())
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	<-done
	return nil
}

// serveIngest waits for the first certificate, then listens and serves.
func (p *Program) serveIngest(ctx context.Context) error {
	select {
	case <-p.ident.Ready():
	case <-ctx.Done():
		return nil
	}
	lis, err := net.Listen("tcp", p.cfg.IngestListen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	p.mu.Lock()
	p.ingestLis = lis
	p.mu.Unlock()
	close(p.ingestUp)
	srv := newIngestServer(p.store, p.ident, p.log)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(lis) }()
	p.log.Info("ingest listening", "addr", lis.Addr().String())
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		srv.Stop()
		<-stopped
	}
	<-done
	return nil
}
