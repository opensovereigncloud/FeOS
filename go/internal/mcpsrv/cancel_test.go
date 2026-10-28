// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package mcpsrv

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
	"github.com/gardenlinux/feos/go/internal/tools"
	"github.com/gardenlinux/feos/go/internal/tools/toolstest"
)

// clock is a door's clock that a test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// bareServer is a door without a database or sandboxes, on a clock of the
// test's: enough for its memory of calls with an id and of cancels, whose
// cancels of calls find none.
func bareServer() (*Server, *clock) {
	log := slog.New(slog.DiscardHandler)
	s := New(Config{Log: log}, tools.NewRunner(nil, nil, nil, nil, "i1", log), nil, nil)
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	s.now = c.now
	return s, c
}

// held is how many entries the door's memories hold now, after the
// expiry.
func (s *Server) held() (keyed, recent int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.keyed.expire(now)
	s.recent.expire(now)
	return len(s.keyed.entries), len(s.recent.entries)
}

// One client's flood of requests with an id and of cancels, on this door
// and through ferun_cancel, evicts only its own entries: another client's
// lingering call and early cancel survive it, and the late cancel still
// reaches the call. The flood passes the door-wide bound the memories had
// before they were kept per client.
func TestCancelMemoryPerClient(t *testing.T) {
	s, clk := bareServer()
	id := podid.ID("sbx")
	victim, attacker := clientOf(store.HashToken("victim")), clientOf(store.HashToken("attacker"))
	if victim == attacker || len(victim) != 16 {
		t.Fatalf("clients %q and %q", victim, attacker)
	}

	end, err := s.registerKeyed(victim, "v:1", id, "vk")
	if err != nil {
		t.Fatal(err)
	}
	end() // the go-sdk hangs up first; its cancel comes later
	s.cancelKey(victim, "v:2")

	const flood = 5000
	for i := range flood {
		clk.add(time.Millisecond)
		end, err := s.registerKeyed(attacker, fmt.Sprintf("a:%d", i), id, "ak")
		if err != nil {
			t.Fatalf("attacker request %d: %v", i, err)
		}
		end() // a conflict or a replay, answered at once
		s.cancelKey(attacker, fmt.Sprintf("a:c%d", i))
		s.remoteCancel(fmt.Sprintf(`{"kind":"mcp","key":"a:r%d","client":%q}`, i, attacker))
	}

	if !s.recentlyCancelled("v:2") {
		t.Error("the victim's early cancel was forgotten")
	}
	if n := s.cancelKey(victim, "v:1"); n != 1 {
		t.Errorf("the victim's late cancel found %d calls", n)
	}
	s.mu.Lock()
	ak, ar := s.keyed.clients[attacker].n, s.recent.clients[attacker].n
	_, newest := s.keyed.entries[fmt.Sprintf("a:%d", flood-1)]
	_, oldest := s.keyed.entries["a:0"]
	_, newestRemote := s.recent.entries[fmt.Sprintf("a:r%d", flood-1)]
	s.mu.Unlock()
	if ak != maxPerClient || ar != maxPerClient || !newest || oldest || !newestRemote {
		t.Errorf("the attacker holds %d entries and %d cancels (newest %v, oldest %v, newest remote %v)",
			ak, ar, newest, oldest, newestRemote)
	}
	if nk, nr := s.held(); nk != maxPerClient+1 || nr != maxPerClient+1 {
		t.Errorf("%d entries, %d cancels", nk, nr)
	}
}

// A ferun_cancel of kind mcp goes into the part of the client it names;
// an older instance's names none and goes into its session's part. Other
// payloads are ignored.
func TestRemoteCancelClient(t *testing.T) {
	s, _ := bareServer()
	for _, p := range []string{
		`{"kind":"mcp","key":"sess:7","client":"c1"}`,
		`{"kind":"mcp","key":"old:8"}`,
		`{"kind":"call","sandbox":"sbx","key":"k"}`,
		`{"kind":"mcp"}`,
		`not json`,
		``,
	} {
		if n := s.remoteCancel(p); n != 0 {
			t.Errorf("%s cancelled %d", p, n)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.recent.entries) != 2 || s.recent.entries["sess:7"] == nil || s.recent.entries["sess:7"].client != "c1" ||
		s.recent.entries["old:8"] == nil || s.recent.entries["old:8"].client != "session:old" {
		t.Errorf("remembered %v", s.recent.entries)
	}
	b, _ := json.Marshal(cancelPayload{Kind: "mcp", Key: "sess:7", Client: "c1"})
	if string(b) != `{"kind":"mcp","key":"sess:7","client":"c1"}` {
		t.Errorf("payload %s", b)
	}
}

// Registering a request with an id, ending it and remembering a cancel
// cost the same whatever the door holds: nothing is scanned under s.mu.
// Before, each registration walked every entry, about 1.6 ms at 50,000.
func TestKeyedRegistrationCost(t *testing.T) {
	const reps = 4096
	id := podid.ID("sbx")
	cost := func(held int) time.Duration {
		s, clk := bareServer()
		for i := range held {
			client := fmt.Sprintf("c%d", i/maxPerClient)
			end, err := s.registerKeyed(client, fmt.Sprintf("%s:%d", client, i), id, "k")
			if err != nil {
				t.Fatal(err)
			}
			if i%2 == 0 {
				end() // half ended and lingering, half still running
			}
			s.cancelKey(client, fmt.Sprintf("%s:c%d", client, i))
		}
		best := time.Duration(math.MaxInt64)
		for round := range 3 {
			start := time.Now()
			for i := range reps {
				clk.add(time.Microsecond)
				end, err := s.registerKeyed("fresh", fmt.Sprintf("f%d:%d", round, i), id, "k")
				if err != nil {
					t.Fatal(err)
				}
				end()
				s.cancelKey("fresh", fmt.Sprintf("f%d:c%d", round, i))
			}
			best = min(best, time.Since(start)/reps)
		}
		if nk, nr := s.held(); nk != held+maxPerClient || nr != held+maxPerClient {
			t.Errorf("holding %d: %d entries, %d cancels", held, nk, nr)
		}
		return best
	}
	small, large := cost(512), cost(64*1024)
	t.Logf("per registration: %v holding 512 entries, %v holding 65536", small, large)
	if large > 4*small+50*time.Microsecond {
		t.Errorf("a registration costs %v holding 65536 entries, %v holding 512", large, small)
	}
}

// A session client with maxPerClient requests with an id running on the
// instance gets an isError result for the next, before its call is looked
// up: no _meta, nothing runs. Another token of the sandbox, a client
// without a session and the same client once a request ended are served.
func TestKeyedClientCap(t *testing.T) {
	h := newHarness(t, toolstest.Options{}, Config{})
	h.runCalls()
	client := clientOf(store.HashToken(h.token))
	var ends []func()
	for i := range maxPerClient {
		end, err := h.srv.registerKeyed(client, fmt.Sprintf("held:%d", i), h.sb.ID, "held")
		if err != nil {
			t.Fatal(err)
		}
		ends = append(ends, end)
	}
	defer func() {
		for _, end := range ends[1:] {
			end()
		}
	}()

	cs := h.session(legacy)
	res, err := keyedCall(bg, cs, "bash", map[string]any{"command": "echo refused"}, "refused")
	if err != nil || !res.IsError || textOf(res) != "error: "+errClientFull.Error() {
		t.Fatalf("past the cap: %+v %v", res, err)
	}
	noCallMeta(t, res)
	h.noRow("refused")

	other := h.tokens.mint(h.sb)
	ocs, err := h.connect(h.http.URL, other, nil, legacy)
	if err != nil {
		t.Fatal(err)
	}
	defer ocs.Close()
	for name, s := range map[string]*mcp.ClientSession{"another token": ocs, "no session": h.session(nil)} {
		key := "served-" + name[:2]
		res, err := keyedCall(bg, s, "bash", map[string]any{"command": "echo served"}, key)
		if err != nil || textOf(res) != "served\n" {
			t.Errorf("%s: %+v %v", name, res, err)
			continue
		}
		callMeta(t, res, key, "done", false)
	}

	ends[0]()
	res, err = keyedCall(bg, cs, "bash", map[string]any{"command": "echo admitted"}, "admitted")
	if err != nil || textOf(res) != "admitted\n" {
		t.Fatalf("after a request ended: %+v %v", res, err)
	}
	callMeta(t, res, "admitted", "done", false)
}
