// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	watchv1 "github.com/gardenlinux/feos/go/gen/feos/watch/v1"
)

// guestApp is a web server in a sandbox's guest, as a dev server is: it
// records the Host and Authorization of every request it gets.
type guestApp struct {
	srv *httptest.Server

	mu    sync.Mutex
	hosts []string
	auths []string
}

func newGuestApp(t *testing.T) *guestApp {
	g := &guestApp{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.hosts = append(g.hosts, r.Host)
		g.auths = append(g.auths, r.Header.Get("Authorization"))
		g.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		fmt.Fprintf(w, "path=%s query=%s", r.URL.Path, r.URL.RawQuery)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *guestApp) seen() (hosts, auths []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.hosts), slices.Clone(g.auths)
}

// forward sends a GET through the port door of in (nil: the balancer)
// with the preview host FeCode sets.
func (c *cluster) forward(in *instance, sb sandbox, port int, path, host string) (int, string, error) {
	if in == nil {
		if in = c.pick(); in == nil {
			return 0, "", errors.New("no instance runs")
		}
	}
	req, err := http.NewRequest("GET", in.url(fmt.Sprintf("/fwd/%s/%d%s", sb.id, port, path)), nil)
	if err != nil {
		return 0, "", err
	}
	req.Host = host
	req.Header.Set("Authorization", sb.auth())
	resp, err := plainClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// Scenario 9: the port door through every instance: the guest sees the
// original Host and no Authorization, and 100 concurrent GETs to one port
// all succeed.
func TestPorts(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	c.start("a")
	c.start("b")
	c.start("c")
	n1 := c.join("n1")
	sb := c.create(nil)
	c.run(nil, sb, "true")
	app := newGuestApp(t)
	n1.Script().SetPortAddr(5173, app.srv.Listener.Addr().String())

	const host = "5173--wt1.preview.example.test"
	for _, in := range c.live() {
		code, body, err := c.forward(in, sb, 5173, "/src/main.ts?x=1", host)
		if err != nil || code != http.StatusOK || body != "path=/src/main.ts query=x=1" {
			t.Fatalf("through %s: %d %q %v", in.name, code, body, err)
		}
	}
	hosts, auths := app.seen()
	for i := range hosts {
		if hosts[i] != host || auths[i] != "" {
			t.Fatalf("the guest saw Host %q, Authorization %q", hosts[i], auths[i])
		}
	}

	var wg sync.WaitGroup
	errs := make(chan string, 100)
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body, err := c.forward(nil, sb, 5173, fmt.Sprintf("/m%d.js", i), host)
			if err != nil || code != http.StatusOK || body != fmt.Sprintf("path=/m%d.js query=", i) {
				errs <- fmt.Sprintf("GET %d: %d %q %v", i, code, body, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	// Every request held the sandbox while it ran; the holds are gone.
	eventually(t, 10*time.Second, "the forward holds released", func() bool {
		return c.count("SELECT count(*) FROM sandbox_holds WHERE sandbox_id = $1", sb.id) == 0
	})
}

// watchAnswer is the answer of a watch's POST and PUT.
type watchAnswer struct {
	WatchID  string   `json:"watchId"`
	Cursor   uint64   `json:"cursor"`
	Watching []string `json:"watching"`
	Failed   []struct {
		Path  string `json:"path"`
		Error string `json:"error"`
	} `json:"failed"`
}

// watchEvent is one event of a watch's stream.
type watchEvent struct {
	ID         uint64   `json:"id"`
	Kind       string   `json:"kind"`
	Dir        string   `json:"dir"`
	Name       string   `json:"name"`
	StalePaths []string `json:"stalePaths"`
}

// eventStream is a watch's open event stream (SSE).
type eventStream struct {
	events chan watchEvent
	ended  chan error
	last   uint64
}

// openEvents opens the event stream of watch wid through in, from
// Last-Event-ID last, as FeCode does.
func (c *cluster) openEvents(in *instance, sb sandbox, wid string, last uint64) *eventStream {
	c.t.Helper()
	ctx, cancel := context.WithCancel(bg)
	c.t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", in.url(sb.path("/watches/"+wid+"/events")), nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", sb.auth())
	req.Header.Set("Accept", "text/event-stream")
	if last > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(last, 10))
	}
	resp, err := (&http.Client{Transport: newTransport()}).Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		c.t.Fatalf("events: %d %s", resp.StatusCode, b)
	}
	st := &eventStream{events: make(chan watchEvent, 256), ended: make(chan error, 1)}
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var id uint64
		var data string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if data != "" {
					var ev watchEvent
					if err := json.Unmarshal([]byte(data), &ev); err == nil && ev.ID == id {
						st.events <- ev
					} else {
						st.events <- watchEvent{Kind: "malformed: " + data}
					}
				}
				id, data = 0, ""
			case strings.HasPrefix(line, "id: "):
				id, _ = strconv.ParseUint(line[len("id: "):], 10, 64)
			case strings.HasPrefix(line, "data: "):
				data = line[len("data: "):]
			}
		}
		st.ended <- sc.Err()
	}()
	return st
}

// next returns the stream's next event; the ids grow.
func (st *eventStream) next(t *testing.T, within time.Duration) watchEvent {
	t.Helper()
	select {
	case ev := <-st.events:
		if strings.HasPrefix(ev.Kind, "malformed") || ev.ID <= st.last {
			t.Fatalf("event %+v after id %d", ev, st.last)
		}
		st.last = ev.ID
		return ev
	case err := <-st.ended:
		t.Fatalf("the event stream ended: %v", err)
	case <-time.After(within):
		t.Fatal("no event")
	}
	return watchEvent{}
}

// until skips events until one of kind (and name, if given) arrives.
func (st *eventStream) until(t *testing.T, within time.Duration, kind, name string) watchEvent {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		ev := st.next(t, time.Until(deadline))
		if ev.Kind == kind && (name == "" || ev.Name == name) {
			return ev
		}
	}
}

// Scenario 9: a watch's requests spread over the instances (POST on one,
// the events on another, PUT on a third) and the guest's events reach the
// stream; when the node reconnects, the stream says stale and the events
// flow again.
func TestWatches(t *testing.T) {
	t.Parallel()
	c := newCluster(t, nil)
	a := c.start("a")
	b := c.start("b")
	d := c.start("c")
	n1 := c.join("n1")
	sb := c.create(nil)
	c.run(nil, sb, "mkdir -p src")

	r := c.call(a, "POST", sb.path("/watches"), sb.auth(), `{"paths":["/","/src"]}`)
	var created watchAnswer
	if err := json.Unmarshal([]byte(r.body), &created); err != nil || r.code != http.StatusCreated || created.WatchID == "" ||
		!slices.Equal(created.Watching, []string{"/", "/src"}) {
		t.Fatalf("POST: %d %s", r.code, r.body)
	}
	st := c.openEvents(d, sb, created.WatchID, created.Cursor)
	r = c.call(b, "PUT", sb.path("/watches/"+created.WatchID), sb.auth(), `{"paths":["/src"]}`)
	var put watchAnswer
	if err := json.Unmarshal([]byte(r.body), &put); err != nil || r.code != http.StatusOK || put.WatchID != created.WatchID ||
		!slices.Equal(put.Watching, []string{"/src"}) {
		t.Fatalf("PUT: %d %s", r.code, r.body)
	}
	guestEvent := func(name string) {
		n1.Script().WatchEvents(sb.id, &watchv1.Event{Kind: watchv1.EventKind_EVENT_KIND_CREATED, Dir: "/src", Name: name})
	}
	eventually(t, 15*time.Second, "one guest stream", func() bool { return n1.Script().Watches(sb.id) == 1 })
	guestEvent("a.txt")
	if ev := st.until(t, 15*time.Second, "created", "a.txt"); ev.Dir != "/src" {
		t.Fatalf("event %+v", ev)
	}

	n1.Restart(false)
	st.until(t, 30*time.Second, "stale", "")
	c.waitNode(n1)
	eventually(t, 30*time.Second, "the guest stream again", func() bool { return n1.Script().Watches(sb.id) == 1 })
	// The re-linked stream may still be settling: the guest sends until
	// its event comes through.
	eventually(t, 30*time.Second, "an event after the node reconnected", func() bool {
		guestEvent("back.txt")
		select {
		case ev := <-st.events:
			if ev.ID <= st.last {
				t.Fatalf("event %+v after id %d", ev, st.last)
			}
			st.last = ev.ID
			return ev.Kind == "created" && ev.Name == "back.txt"
		case err := <-st.ended:
			t.Fatalf("the event stream ended: %v", err)
		case <-time.After(time.Second):
		}
		return false
	})
	if r := c.call(a, "DELETE", sb.path("/watches/"+created.WatchID), sb.auth(), ""); r.code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", r.code, r.body)
	}
	if n := c.count("SELECT count(*) FROM watches WHERE sandbox_id = $1", sb.id); n != 0 {
		t.Fatalf("%d watches left", n)
	}
}
