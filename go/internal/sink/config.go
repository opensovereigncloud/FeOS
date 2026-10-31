// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package sink is ferun-sink, the platform's log store (ADR 0026,
// docs/architecture/sink.md): the nodes' OTLP log ingest over mTLS, the
// hot stage in its PostgreSQL, the cold stage in Quickwit's indexes (§10),
// the read API for tenants, and retention.
//
// Every flag has an environment variable FERUN_SINK_<NAME> (the flag's
// name in upper snake case); a flag given wins over its variable. The
// secrets come from variables only: SINK_SERVICE_TOKEN, the sink's
// service token at ferund, and SINK_BUCKET_ACCESS_KEY and
// SINK_BUCKET_SECRET_KEY, the keys of the bucket of the chunks made
// before the index, while there are some.
package sink

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/pki"
)

// EnvPrefix starts every environment variable of a flag.
const EnvPrefix = "FERUN_SINK_"

// The variables of the secrets.
const (
	EnvServiceToken    = "SINK_SERVICE_TOKEN"
	EnvBucketAccessKey = "SINK_BUCKET_ACCESS_KEY"
	EnvBucketSecretKey = "SINK_BUCKET_SECRET_KEY"
)

// The log formats (--log-format).
const (
	LogFormatJSON = "json"
	LogFormatText = "text"
)

// Config is the configuration of `ferun-sink serve`: its flags, its
// secrets, and the tunables tests change, which are not flags.
type Config struct {
	// Name is the instance's name, a DNS label ($(POD_NAME)); its
	// certificate's URI is spiffe://<TrustDomain>/sink/<Name>.
	Name string
	// Listen is the HTTP API's address, IngestListen the ingest's (gRPC
	// over TLS).
	Listen, IngestListen string
	// DatabaseURL is the sink's own database.
	DatabaseURL string
	TrustDomain string
	// FerunURL is ferund's API inside the cluster: GET /api/v1/tenant and
	// POST /api/v1/service-certificates.
	FerunURL string
	// ServiceToken is the sink's service token at ferund.
	ServiceToken string
	// The index (ADR 0026 §10): Quickwit's indexers for ingest, its
	// searchers for everything else, and the prefix of the log indexes'
	// names (<prefix>-<days>d).
	IndexIngestURL, IndexSearchURL, IndexPrefix string
	// The bucket of the chunks made before the index, until they are
	// moved: its endpoint, region, name and keys, and the prefix of every
	// object key. No endpoint: none.
	BucketEndpoint, BucketRegion, Bucket, BucketPrefix string
	BucketAccessKey, BucketSecretKey                   string
	// Retention is how long lines are kept, unless a stream asks for
	// longer (ferun.retention_days); RetentionMax bounds what it asks.
	Retention, RetentionMax time.Duration
	// Quota is the bytes of text a tenant may store a day; 0: no quota.
	Quota     int64
	LogLevel  slog.Level
	LogFormat string

	// CommitTimeout is the indexes' commit interval: a line is searchable
	// at most this long after it is stored (Quickwit's
	// commit_timeout_secs, set when an index is made).
	CommitTimeout time.Duration
	// Tunables, not flags: the cold stage looks for streams with hot lines
	// every ChunkInterval, the sweep runs every SweepInterval; a follow
	// polls every FollowPoll and sends {} after Heartbeat without a line; a
	// read scans at most ScanLimit bytes of lines.
	ChunkInterval time.Duration
	SweepInterval time.Duration
	FollowPoll    time.Duration
	Heartbeat     time.Duration
	ScanLimit     int64
}

// Default returns the defaults of serve, without the required settings.
func Default() Config {
	return Config{
		Listen:        ":8080",
		IngestListen:  ":4317",
		Retention:     720 * time.Hour,
		RetentionMax:  9600 * time.Hour,
		LogLevel:      slog.LevelInfo,
		LogFormat:     LogFormatJSON,
		IndexPrefix:   "ferun-logs",
		CommitTimeout: 10 * time.Second,
		ChunkInterval: time.Second,
		SweepInterval: 10 * time.Minute,
		FollowPoll:    500 * time.Millisecond,
		Heartbeat:     15 * time.Second,
		ScanLimit:     256 << 20,
	}
}

// EnvName is the environment variable of a flag.
func EnvName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// Parse reads the flags of serve from args, the variables of the flags not
// given and of the secrets through getenv, over Default, and validates the
// result. --help yields flag.ErrHelp.
func Parse(args []string, getenv func(string) string) (Config, error) {
	c := Default()
	fs := newFlagSet("serve")
	level := register(fs, &c)
	if err := parseFlags(fs, args, getenv); err != nil {
		return Config{}, err
	}
	if err := c.LogLevel.UnmarshalText([]byte(*level)); err != nil {
		return Config{}, fmt.Errorf("--log-level: %w", err)
	}
	if getenv != nil {
		c.ServiceToken = getenv(EnvServiceToken)
		c.BucketAccessKey = getenv(EnvBucketAccessKey)
		c.BucketSecretKey = getenv(EnvBucketSecretKey)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func register(fs *flag.FlagSet, c *Config) *string {
	fs.StringVar(&c.Name, "name", c.Name, "the instance's name, a DNS label: $(POD_NAME) on Kubernetes (required)")
	fs.StringVar(&c.Listen, "listen", c.Listen, "the HTTP API's address")
	fs.StringVar(&c.IngestListen, "ingest-listen", c.IngestListen, "the ingest's address (OTLP and SinkService over gRPC and TLS, nodes only)")
	fs.StringVar(&c.DatabaseURL, "database-url", c.DatabaseURL, "the sink's PostgreSQL database (required); PGPASSWORD may hold the password")
	fs.StringVar(&c.TrustDomain, "trust-domain", c.TrustDomain, "the SPIFFE trust domain (required)")
	fs.StringVar(&c.FerunURL, "ferun-url", c.FerunURL, "ferund's API inside the cluster, e.g. http://ferund.ferun.svc:8100 (required)")
	fs.StringVar(&c.IndexIngestURL, "index-ingest-url", c.IndexIngestURL, "Quickwit's indexers, e.g. http://quickwit-indexer:7280 (required)")
	fs.StringVar(&c.IndexSearchURL, "index-search-url", c.IndexSearchURL, "Quickwit's searchers, e.g. http://quickwit-searcher:7280 (required)")
	fs.StringVar(&c.IndexPrefix, "index-prefix", c.IndexPrefix, "the prefix of the log indexes' names, <prefix>-<days>d")
	fs.DurationVar(&c.CommitTimeout, "index-commit-timeout", c.CommitTimeout, "how soon a line is searchable in the index, whole seconds (a new index's commit_timeout_secs)")
	fs.StringVar(&c.BucketEndpoint, "bucket-endpoint", c.BucketEndpoint, "the S3 API's base URL, path-style, of the chunks made before the index; empty: none")
	fs.StringVar(&c.BucketRegion, "bucket-region", c.BucketRegion, "the bucket's signing region: auto for Cloudflare R2")
	fs.StringVar(&c.Bucket, "bucket", c.Bucket, "the bucket of the chunks made before the index")
	fs.StringVar(&c.BucketPrefix, "bucket-prefix", c.BucketPrefix, "the prefix of every object key, e.g. sink/; empty: none")
	fs.DurationVar(&c.Retention, "log-retention", c.Retention, "how long lines are kept, unless a stream asks for longer")
	fs.DurationVar(&c.RetentionMax, "log-retention-max", c.RetentionMax, "the longest a stream may ask for (ferun.retention_days)")
	fs.Var((*byteSize)(&c.Quota), "log-quota", "the bytes a tenant may store a day (bytes, or with a KiB, MiB or GiB suffix); 0: no quota")
	level := strings.ToLower(c.LogLevel.String())
	fs.StringVar(&level, "log-level", level, "debug, info, warn or error")
	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat, "json or text")
	return &level
}

// Validate checks a configuration.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if c.Name == "" {
		bad("--name is required")
	} else if !pki.IsNodeName(c.Name) {
		bad("--name %q is not a DNS label", c.Name)
	}
	if c.Listen == "" || c.IngestListen == "" {
		bad("--listen and --ingest-listen must not be empty")
	}
	if c.DatabaseURL == "" {
		bad("--database-url is required")
	}
	if c.TrustDomain == "" {
		bad("--trust-domain is required")
	} else if err := pki.ValidateTrustDomain(c.TrustDomain); err != nil {
		bad("--trust-domain: %w", err)
	}
	if u, err := url.Parse(c.FerunURL); c.FerunURL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		bad("--ferun-url must be an http(s) URL")
	}
	if c.ServiceToken == "" {
		bad("%s is required", EnvServiceToken)
	}
	for _, u := range []struct{ flag, v string }{{"--index-ingest-url", c.IndexIngestURL}, {"--index-search-url", c.IndexSearchURL}} {
		if p, err := url.Parse(u.v); u.v == "" || err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			bad("%s must be an http(s) URL", u.flag)
		}
	}
	if c.IndexPrefix == "" || strings.ContainsAny(c.IndexPrefix, "*/ ") {
		bad("--index-prefix must be a name")
	}
	if c.CommitTimeout < time.Second || c.CommitTimeout%time.Second != 0 {
		bad("--index-commit-timeout must be whole seconds, at least one")
	}
	if c.BucketEndpoint != "" {
		if c.BucketRegion == "" || c.Bucket == "" {
			bad("--bucket-region and --bucket are required with --bucket-endpoint")
		}
		if c.BucketAccessKey == "" || c.BucketSecretKey == "" {
			bad("%s and %s are required with --bucket-endpoint", EnvBucketAccessKey, EnvBucketSecretKey)
		}
	}
	if strings.HasPrefix(c.BucketPrefix, "/") {
		bad("--bucket-prefix must not start with a slash")
	}
	if c.Retention <= 0 || c.RetentionMax < c.Retention {
		bad("--log-retention must be positive and at most --log-retention-max")
	}
	if c.Quota < 0 {
		bad("--log-quota must not be negative")
	}
	if c.LogFormat != LogFormatJSON && c.LogFormat != LogFormatText {
		bad("--log-format must be %s or %s, not %q", LogFormatJSON, LogFormatText, c.LogFormat)
	}
	return errors.Join(errs...)
}

// keyPrefix is the bucket prefix as keys start with it: empty, or ending
// in a slash.
func (c *Config) keyPrefix() string {
	if c.BucketPrefix == "" || strings.HasSuffix(c.BucketPrefix, "/") {
		return c.BucketPrefix
	}
	return c.BucketPrefix + "/"
}

// NewLogger returns the logger of a log format and level, writing to w.
func NewLogger(w io.Writer, format string, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == LogFormatText {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

// Usage prints the help of serve.
func Usage(w io.Writer) {
	fs := newFlagSet("serve")
	c := Default()
	register(fs, &c)
	fmt.Fprintln(w, "Usage: ferun-sink [serve] [flags]\n\nRuns the sink until SIGTERM or SIGINT. Every flag has an environment variable "+
		EnvPrefix+"<NAME>;\nthe secrets come from "+EnvServiceToken+", "+EnvBucketAccessKey+" and "+EnvBucketSecretKey+".\n\nFlags:")
	fs.SetOutput(w)
	fs.PrintDefaults()
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parseFlags parses args into fs, then sets every flag not given from its
// environment variable; an empty variable counts as unset.
func parseFlags(fs *flag.FlagSet, args []string, getenv func(string) string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var errs []error
	fs.VisitAll(func(f *flag.Flag) {
		if given[f.Name] || getenv == nil {
			return
		}
		if v := getenv(EnvName(f.Name)); v != "" {
			if err := fs.Set(f.Name, v); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", EnvName(f.Name), err))
			}
		}
	})
	return errors.Join(errs...)
}

// byteSize is a flag of bytes: an integer with an optional KiB, MiB or
// GiB suffix, as ferun-ingress writes them.
type byteSize int64

func (b *byteSize) String() string { return strconv.FormatInt(int64(*b), 10) }

func (b *byteSize) Set(v string) error {
	s := strings.TrimSpace(v)
	num, shift := s, 0
	for _, u := range []struct {
		suffix string
		shift  int
	}{{"KiB", 10}, {"MiB", 20}, {"GiB", 30}} {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			num, shift = strings.TrimSpace(n), u.shift
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 || n > (1<<62)>>shift {
		return fmt.Errorf("%q is not a byte size (an integer with an optional KiB, MiB or GiB suffix)", v)
	}
	*b = byteSize(n << shift)
	return nil
}
