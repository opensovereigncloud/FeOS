// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package quickwit is the sink's client of Quickwit's REST API (ADR 0026
// §10): the log indexes, ingest that answers once a batch is published,
// search and delete tasks. Only the standard library: JSON over HTTP.
package quickwit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Doc is a line as an index holds it: its time (Unix ns), its stream's
// labels, and the line.
type Doc struct {
	TS          int64  `json:"ts"`
	Tenant      string `json:"tenant"`
	Stream      string `json:"stream"`
	Environment string `json:"environment,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Object      string `json:"object,omitempty"`
	Replica     int    `json:"replica"`
	Release     int    `json:"release"`
	Instance    string `json:"instance,omitempty"`
	Container   string `json:"container,omitempty"`
	Job         string `json:"job,omitempty"`
	Attempt     int    `json:"attempt"`
	Source      string `json:"source"`
	IOStream    string `json:"iostream,omitempty"`
	Level       int    `json:"level"`
	TraceID     string `json:"trace_id,omitempty"`
	Off         int64  `json:"off"`
	Partial     bool   `json:"partial"`
	Cont        bool   `json:"cont"`
	Text        string `json:"text"`
}

// Query selects a tenant's lines: the labels set (empty or nil: any), a
// time range in Unix ns (both ends included; 0: open), offsets after
// OffAfter up to OffTo (nil: any), levels (a mask; 0: any), a trace ID,
// words each in the text as a phrase, words none of which may be there,
// at most Max hits in the order Order. With OrPartial a partial line
// passes the words: they may be in its rest.
type Query struct {
	Tenant                                                      string
	Environment, Kind, Object, Instance, Container, Job, Stream string
	Source, IOStream                                            string
	Replica, Attempt                                            *int
	SinceNs, UntilNs                                            int64
	OffAfter, OffTo                                             *int64
	Levels                                                      int
	Trace                                                       string
	Words, NotWords                                             []string
	OrPartial                                                   bool
	Order                                                       Order
	Max                                                         int
}

// Order is a search's order of hits.
type Order int

const (
	// NewestFirst is by time, then offset, descending.
	NewestFirst Order = iota
	// OldestFirst is by time, then offset, ascending.
	OldestFirst
	// ByOffset is by offset ascending, for one stream's lines.
	ByOffset
)

// MaxHits is the most hits one search answers (Quickwit's bound).
const MaxHits = 10000

// levelBits are the level bits a mask may hold (the sink's LevelBits).
var levelBits = []int{1, 2, 4, 8, 16}

// String is the query in Quickwit's query language.
func (q Query) String() string {
	var c []string
	term := func(field, v string) {
		if v != "" {
			c = append(c, field+":"+quote(v))
		}
	}
	term("tenant", q.Tenant)
	term("environment", q.Environment)
	term("kind", q.Kind)
	term("object", q.Object)
	term("instance", q.Instance)
	term("container", q.Container)
	term("job", q.Job)
	term("stream", q.Stream)
	term("source", q.Source)
	term("iostream", q.IOStream)
	term("trace_id", q.Trace)
	if q.Replica != nil {
		c = append(c, "replica:"+strconv.Itoa(*q.Replica))
	}
	if q.Attempt != nil {
		c = append(c, "attempt:"+strconv.Itoa(*q.Attempt))
	}
	if q.SinceNs > 0 || q.UntilNs > 0 {
		lo, hi := "*", "*"
		if q.SinceNs > 0 {
			lo = rfc3339(q.SinceNs)
		}
		if q.UntilNs > 0 {
			hi = rfc3339(q.UntilNs)
		}
		c = append(c, "ts:["+lo+" TO "+hi+"]")
	}
	if q.OffAfter != nil || q.OffTo != nil {
		lo, hi, open := "*", "*", "["
		if q.OffAfter != nil {
			lo, open = strconv.FormatInt(*q.OffAfter, 10), "{"
		}
		if q.OffTo != nil {
			hi = strconv.FormatInt(*q.OffTo, 10)
		}
		c = append(c, "off:"+open+lo+" TO "+hi+"]")
	}
	if q.Levels != 0 {
		var in []string
		for _, b := range levelBits {
			if q.Levels&b != 0 {
				in = append(in, strconv.Itoa(b))
			}
		}
		if len(in) < len(levelBits) {
			c = append(c, "level:IN ["+strings.Join(in, " ")+"]")
		}
	}
	var words []string
	for _, w := range q.Words {
		words = append(words, "text:"+quote(w))
	}
	for _, w := range q.NotWords {
		words = append(words, "NOT text:"+quote(w))
	}
	switch {
	case len(words) > 0 && q.OrPartial:
		c = append(c, "(("+strings.Join(words, " AND ")+") OR partial:true)")
	case len(words) > 0:
		c = append(c, words...)
	}
	if len(c) == 0 {
		return "*"
	}
	return strings.Join(c, " AND ")
}

// quote is a phrase of the query language: s in double quotes, a quote
// or backslash in it escaped.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func rfc3339(ns int64) string {
	return time.Unix(0, ns).UTC().Format("2006-01-02T15:04:05.000000000Z")
}

// Client calls Quickwit: ingest at an indexer, everything else at a
// searcher (any node answers the index API).
type Client struct {
	ingest, search string
	http           *http.Client
}

// New returns a client of the indexers at ingestURL and the searchers at
// searchURL (http://host:7280).
func New(ingestURL, searchURL string, hc *http.Client) (*Client, error) {
	for _, u := range []string{ingestURL, searchURL} {
		p, err := url.Parse(u)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			return nil, fmt.Errorf("quickwit: %q is not an http(s) URL", u)
		}
	}
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{ingest: strings.TrimRight(ingestURL, "/"), search: strings.TrimRight(searchURL, "/"), http: hc}, nil
}

// Error is an answer of Quickwit that is not a success.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("quickwit: %d %s", e.Status, e.Message) }

// IsNotFound reports whether err is Quickwit's 404.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, url, ctype string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("quickwit: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return fmt.Errorf("quickwit: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &m)
		if m.Message == "" {
			m.Message = strings.TrimSpace(string(data[:min(len(data), 512)]))
		}
		return &Error{Status: resp.StatusCode, Message: m.Message}
	}
	if out == nil {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	return d.Decode(out)
}

// IndexConfig is the configuration of a log index id whose lines are
// kept retentionDays, committed every commitSecs.
func IndexConfig(id string, retentionDays, commitSecs int) []byte {
	raw := func(name string) map[string]any {
		return map[string]any{"name": name, "type": "text", "tokenizer": "raw"}
	}
	num := func(name string, fast bool) map[string]any {
		return map[string]any{"name": name, "type": "u64", "fast": fast}
	}
	cfg := map[string]any{
		"version":  "0.9",
		"index_id": id,
		"doc_mapping": map[string]any{
			"mode": "strict",
			"field_mappings": []map[string]any{
				{"name": "ts", "type": "datetime", "input_formats": []string{"unix_timestamp"},
					"output_format": "unix_timestamp_nanos", "fast": true, "fast_precision": "nanoseconds"},
				raw("tenant"), raw("stream"), raw("environment"), raw("kind"), raw("object"),
				num("replica", false), num("release", false), raw("instance"), raw("container"), raw("job"),
				num("attempt", false), raw("source"), raw("iostream"), num("level", true), raw("trace_id"),
				num("off", true),
				{"name": "partial", "type": "bool"},
				{"name": "cont", "type": "bool"},
				{"name": "text", "type": "text", "tokenizer": "default", "record": "position"},
			},
			"timestamp_field": "ts",
			"tag_fields":      []string{"tenant"},
		},
		"indexing_settings": map[string]any{"commit_timeout_secs": commitSecs},
		"search_settings":   map[string]any{"default_search_fields": []string{"text"}},
		"retention":         map[string]any{"period": fmt.Sprintf("%d days", retentionDays), "schedule": "hourly"},
	}
	b, _ := json.Marshal(cfg)
	return b
}

// EnsureIndex creates the log index id unless it exists.
func (c *Client) EnsureIndex(ctx context.Context, id string, retentionDays, commitSecs int) error {
	err := c.do(ctx, http.MethodGet, c.search+"/api/v1/indexes/"+url.PathEscape(id), "", nil, nil)
	if err == nil || !IsNotFound(err) {
		return err
	}
	err = c.do(ctx, http.MethodPost, c.search+"/api/v1/indexes", "application/json",
		IndexConfig(id, retentionDays, commitSecs), nil)
	var e *Error
	if errors.As(err, &e) && e.Status == http.StatusBadRequest && strings.Contains(e.Message, "already exist") {
		return nil
	}
	return err
}

// Indexes lists the ids of the indexes that start with prefix.
func (c *Client) Indexes(ctx context.Context, prefix string) ([]string, error) {
	var out []struct {
		IndexConfig struct {
			IndexID string `json:"index_id"`
		} `json:"index_config"`
	}
	if err := c.do(ctx, http.MethodGet, c.search+"/api/v1/indexes", "", nil, &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range out {
		if strings.HasPrefix(m.IndexConfig.IndexID, prefix) {
			ids = append(ids, m.IndexConfig.IndexID)
		}
	}
	return ids, nil
}

// Ingest indexes docs into the index id and answers once the split that
// holds them is published (commit=wait_for): only then are they in the
// object store. A rejected document fails the call.
func (c *Client) Ingest(ctx context.Context, id string, docs []Doc) error {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	for i := range docs {
		if err := enc.Encode(&docs[i]); err != nil {
			return err
		}
	}
	var out struct {
		NumIngested   int `json:"num_ingested_docs"`
		NumRejected   int `json:"num_rejected_docs"`
		ParseFailures []struct {
			Message string `json:"message"`
		} `json:"parse_failures"`
	}
	u := c.ingest + "/api/v1/" + url.PathEscape(id) + "/ingest?commit=wait_for&detailed_response=true"
	if err := c.do(ctx, http.MethodPost, u, "application/x-ndjson", body.Bytes(), &out); err != nil {
		return err
	}
	if out.NumRejected > 0 {
		msg := ""
		if len(out.ParseFailures) > 0 {
			msg = out.ParseFailures[0].Message
		}
		return fmt.Errorf("quickwit: %d of %d documents rejected: %s", out.NumRejected, len(docs), msg)
	}
	return nil
}

// Search answers the hits of q in the indexes pattern names
// (ferun-logs-*), at most q.Max (bounded by MaxHits).
func (c *Client) Search(ctx context.Context, pattern string, q Query) ([]Doc, error) {
	req := map[string]any{"query": q.String(), "max_hits": min(max(q.Max, 1), MaxHits), "count_all": false,
		"format": "json"}
	switch q.Order {
	case NewestFirst:
		req["sort_by"] = "ts,off" // Quickwit: a field alone is descending
	case OldestFirst:
		req["sort_by"] = "-ts,-off" // and with a minus ascending
	case ByOffset:
		req["sort_by"] = "-off"
	}
	if q.SinceNs > 0 {
		req["start_timestamp"] = q.SinceNs / 1e9
	}
	if q.UntilNs > 0 {
		req["end_timestamp"] = q.UntilNs/1e9 + 1
	}
	body, _ := json.Marshal(req)
	var out struct {
		Hits   []Doc    `json:"hits"`
		Errors []string `json:"errors"`
	}
	err := c.do(ctx, http.MethodPost, c.search+"/api/v1/"+pattern+"/search", "application/json", body, &out)
	if err != nil {
		return nil, err
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("quickwit: search: %s", strings.Join(out.Errors, "; "))
	}
	return out.Hits, nil
}

// Delete leaves a delete task of q's labels in the index id; Quickwit
// removes the documents when it rewrites their splits.
func (c *Client) Delete(ctx context.Context, id string, q Query) error {
	body, _ := json.Marshal(map[string]any{"query": q.String()})
	return c.do(ctx, http.MethodPost, c.search+"/api/v1/"+url.PathEscape(id)+"/delete-tasks", "application/json", body, nil)
}
