// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gardenlinux/feos/go/internal/auth"
)

// sinkPage is the most lines sinkLog asks a page of the sink's GET
// /api/v1/logs for (the sink's own default; at most 5,000,
// docs/architecture/sink.md): of lines of up to 64 KiB, 32 MiB.
const sinkPage = 500

// sinkLog reads the last lines lines of a stream's main container from
// the sink (ADR 0026), oldest first, within answerLog's budget: sel names
// the stream (instance=<sandbox ID>, or job=<ID>), since its start. It
// asks with the caller's own token (auth.Tenant.Token), so that the sink
// answers the caller's streams only. ok is false without --sink-url or a
// token, when the sink fails, and when it has no line: the caller reads
// the node then.
func (c *Controller) sinkLog(ctx context.Context, key, value string, since time.Time, lines int) ([]string, bool) {
	t, _ := auth.TenantFrom(ctx)
	if c.d.SinkURL == "" || t.Token == "" || lines <= 0 {
		return nil, false
	}
	q := url.Values{key: {value}, "container": {"main"}, "direction": {"backward"}}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339Nano))
	}
	var pages [][]string
	got, bytes := 0, 0
	for got < lines && bytes <= answerLog.total {
		q.Set("limit", strconv.Itoa(min(lines-got, sinkPage)))
		page, cursor, err := c.sinkPage(ctx, t.Token, q)
		if err != nil {
			c.rl.warn("sink-logs", "cannot read a log from the sink; reading the node", key, value, "err", err)
			return nil, false
		}
		for i, l := range page {
			page[i] = answerLog.cut(l)
			bytes += len(page[i])
		}
		pages = append(pages, page)
		got += len(page)
		if cursor == "" || len(page) == 0 {
			break
		}
		q.Set("cursor", cursor)
	}
	if got == 0 {
		return nil, false
	}
	tail := logTail{b: answerLog, max: lines}
	for i := len(pages) - 1; i >= 0; i-- {
		for _, l := range pages[i] {
			tail.add(l)
		}
	}
	return tail.result(), true
}

// sinkPage asks the sink for one page: its lines' texts, oldest first,
// and the cursor to the older page ("": none).
func (c *Controller) sinkPage(ctx context.Context, token string, q url.Values) ([]string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.d.SinkURL, "/")+"/api/v1/logs?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.d.SinkClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", fmt.Errorf("the sink answered %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var page struct {
		Lines []struct {
			Text string `json:"text"`
		} `json:"lines"`
		Cursor string `json:"cursor"`
	}
	// sinkPage lines of the sink's longest part (64 KiB), twice for their
	// escapes and fields.
	if err := json.NewDecoder(io.LimitReader(resp.Body, (2*sinkPage)<<16)).Decode(&page); err != nil {
		return nil, "", fmt.Errorf("the sink's answer: %w", err)
	}
	out := make([]string, len(page.Lines))
	for i, l := range page.Lines {
		out[i] = strings.TrimRight(l.Text, "\r\n")
	}
	return out, page.Cursor, nil
}
