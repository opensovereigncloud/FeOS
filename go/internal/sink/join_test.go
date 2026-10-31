// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

// part is a line of a stream for the joining tests.
func part(off int64, ms int, source, text string, partial bool) Line {
	lvl, _ := Extract(text)
	return Line{Off: off, TS: base + int64(ms)*int64(time.Millisecond), Source: source, Level: lvl, Partial: partial, Text: text}
}

func TestPartialLinesJoined(t *testing.T) {
	cfg := testConfig()
	s, idx, lg := newTestStore(t, cfg)
	ctx := context.Background()
	key, l := StreamKey{"p", "c", "1"}, envLabels("acme", "e", 0)
	big := strings.Repeat("x", 600<<10)
	// The first batch goes into the index, the rest stays in the topic's
	// tail, so that a line's parts lie in both; stderr runs between the
	// parts; a re-sent batch changes nothing; the last line's rest comes
	// in the next batch.
	first := []Line{
		part(0, 0, "stdout", "INFO a", false),
		part(10, 1, "stdout", "ERROR b1 ", true),
		part(20, 2, "stderr", "e", false),
		part(30, 3, "stdout", "b2 ", true),
	}
	if err := s.Ingest(ctx, batch(key, l, first)); err != nil {
		t.Fatal(err)
	}
	idx.sync(lg)
	second := []Line{
		part(40, 4, "stdout", "b3 needle", false),
		part(50, 5, "stdout", "c", false),
		part(60, 6, "stderr", big, true), part(70, 7, "stderr", big, true), part(80, 8, "stderr", "", false),
		part(90, 9, "stdout", "d1 open", true),
	}
	for range 2 {
		if err := s.Ingest(ctx, batch(key, l, second)); err != nil {
			t.Fatal(err)
		}
	}
	// The rest of the last line in a batch of its own, in the tail as
	// well.
	if err := s.Ingest(ctx, batch(key, l, []Line{part(100, 10, "stdout", " d2", false)})); err != nil {
		t.Fatal(err)
	}
	if n, recs := idx.count("acme"), lg.count(logTopic); n != 4 || recs != 11 {
		t.Fatalf("%d of %d records indexed", n, recs)
	}
	type want struct {
		text    string
		off     string
		partial bool
		level   string
	}
	all := []want{{"INFO a", "0", false, "info"}, {"ERROR b1 b2 b3 needle", "10", false, "error"}, {"e", "20", false, ""},
		{"c", "50", false, ""}, {"<1 MiB>", "60", true, ""}, {"d1 open d2", "90", false, ""}}
	check := func(name string, got []LineJSON, w []want) {
		t.Helper()
		if len(got) != len(w) {
			t.Fatalf("%s: %d lines, want %d", name, len(got), len(w))
		}
		for i, g := range got {
			text := g.Text
			if len(text) == maxJoined {
				text = "<1 MiB>"
			}
			if text != w[i].text || fmt.Sprint(g.Offset) != w[i].off || g.Partial != w[i].partial || g.Level != w[i].level {
				t.Errorf("%s line %d: %q off %d partial %v level %q; want %+v", name, i, short(g.Text), g.Offset, g.Partial, g.Level, w[i])
			}
		}
	}
	reads := func(when string) {
		t.Helper()
		check(when+", backward", mustRead(t, s, "acme", url.Values{"stream": {sidOf(t, s)}}).page(false).Lines, all)
		// Page by page, either way: each joined line once, never a part.
		for _, dir := range []string{"backward", "forward"} {
			var got []LineJSON
			v := url.Values{"stream": {sidOf(t, s)}, "limit": {"1"}, "direction": {dir}, "since": {fmt.Sprint(base - 1)}}
			for range 20 {
				r := mustRead(t, s, "acme", v)
				p := r.page(dir == "forward")
				if dir == "forward" {
					got = append(got, p.Lines...)
				} else {
					got = append(p.Lines, got...)
				}
				if p.Cursor == "" {
					break
				}
				v.Set("cursor", p.Cursor)
			}
			check(when+", "+dir, got, all)
		}
		// Filters see the joined line.
		check(when+", q", mustRead(t, s, "acme", url.Values{"stream": {sidOf(t, s)}, "q": {"needle"}}).page(false).Lines, all[1:2])
		check(when+", level", mustRead(t, s, "acme", url.Values{"stream": {sidOf(t, s)}, "level": {"error"}}).page(false).Lines, all[1:2])
	}
	reads("index and tail")
	// The parts in the topic twice, and then all in the index.
	produceAgain(t, lg, logTopic, sidOf(t, s))
	reads("twice")
	idx.sync(lg)
	reads("index")
}

func sidOf(t *testing.T, s *Store) string {
	t.Helper()
	var sid string
	if err := s.db.Pool.QueryRow(context.Background(), `SELECT sid FROM log_streams`).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	return sid
}

func short(s string) string {
	if len(s) > 40 {
		return s[:40] + fmt.Sprintf("…(%d)", len(s))
	}
	return s
}

func TestFollowJoins(t *testing.T) {
	f := followJoins{open: map[joinKey]*followJoin{}, cut: map[joinKey]bool{}}
	now := time.Now()
	ln := func(off int64, src, text string, partial, cont bool) Line {
		return Line{Stream: 1, Off: off, TS: off, Source: src, Partial: partial, Cont: cont, Text: text}
	}
	texts := func(ls []Line) string {
		var out []string
		for _, l := range ls {
			out = append(out, fmt.Sprintf("%s/%v", short(l.Text), l.Partial))
		}
		return strings.Join(out, "|")
	}
	got := f.take([]Line{ln(0, "stdout", "a", true, false), ln(1, "stderr", "e", false, false)}, now)
	if texts(got) != "e/false" {
		t.Errorf("poll 1: %s", texts(got))
	}
	got = f.take([]Line{ln(2, "stdout", "b", true, true), ln(3, "stdout", "c", false, true), ln(4, "stdout", "x", false, true)}, now)
	if texts(got) != "abc/false|x/false" {
		t.Errorf("poll 2: %s", texts(got))
	}
	big := strings.Repeat("y", 700<<10)
	got = f.take([]Line{ln(5, "stdout", big, true, false), ln(6, "stdout", big, true, true), ln(7, "stdout", "rest", true, true),
		ln(8, "stdout", "end", false, true), ln(9, "stdout", "next", true, false)}, now)
	if len(got) != 1 || len(got[0].Text) != maxJoined || !got[0].Partial {
		t.Errorf("poll 3: %s", texts(got))
	}
	if got = f.take(nil, now.Add(joinWait)); texts(got) != "next/true" {
		t.Errorf("held past the wait: %s", texts(got))
	}
}
