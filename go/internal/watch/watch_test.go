// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	watchv1 "github.com/ironcore-dev/feos/go/gen/feos/watch/v1"
	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
	"github.com/ironcore-dev/feos/go/internal/tools/toolstest"
)

// A watch's life on one instance: the POST's answer from the guest's ack,
// events numbered from the epoch, the guest's stale events and unknown
// kinds, keepalives renewing the lease, a PUT, a DELETE.
func TestCreateEventsUpdateDelete(t *testing.T) {
	w := newWorld(t)
	a := w.addInstance("a", func(tm *timings) { tm.keepalive = 150 * time.Millisecond })
	code, body := w.call(a, w.sb, "POST", "/watches", pathsBody("/", "/src", "/missing"))
	if code != 201 {
		t.Fatalf("POST %d %s", code, body)
	}
	var res response
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(res.WatchID); err != nil {
		t.Fatalf("watch id %q", res.WatchID)
	}
	want := `{"watchId":"` + res.WatchID + `","cursor":4294967296,"watching":["/","/src"],` +
		`"failed":[{"path":"/missing","error":"file does not exist"}],"watches":2,"watchLimit":2048}` + "\n"
	if body != want {
		t.Errorf("POST answered %s, want %s", body, want)
	}
	row := w.row(res.WatchID)
	if row.Owner != "a" || row.Epoch != 1 || row.Gen != 1 || row.ResultGen != 1 || !row.OwnerLive {
		t.Errorf("row %+v", row)
	}
	if n := w.guestStreams(); n != 1 {
		t.Errorf("%d guest streams", n)
	}

	st := w.open(a, res.WatchID, res.Cursor)
	if n := w.guestEvent("/src", "a.txt"); n != 1 {
		t.Fatalf("%d guest streams got the event", n)
	}
	if ev := st.next(t, 5*time.Second); ev.id != epochID(1, 1) || ev.data != createdData(epochID(1, 1), "/src", "a.txt") {
		t.Errorf("event %d %s", ev.id, ev.data)
	}
	// An unknown kind is dropped; the guest's own stale passes.
	w.env.Node.Script().WatchEvents(w.sb.ID, &watchv1.Event{Kind: watchv1.EventKind(99), Dir: "/src", Name: "x"})
	w.env.Node.Script().WatchStale(w.sb.ID, "/src")
	if ev := st.next(t, 5*time.Second); ev.id != epochID(1, 2) || ev.data != staleData(epochID(1, 2), "/src") {
		t.Errorf("stale %d %s", ev.id, ev.data)
	}
	// Keepalives come and renew the lease.
	before := w.row(res.WatchID).ExpiresAt
	eventually(t, 5*time.Second, "keepalives", func() bool { return st.comments.Load() >= 2 })
	eventually(t, 5*time.Second, "the lease renewed", func() bool { return w.row(res.WatchID).ExpiresAt.After(before) })

	// A PUT on the owner places the new set; its cursor covers the events.
	up := w.put(a, res.WatchID, "/src")
	if up.Cursor != epochID(1, 2) || !slices.Equal(up.Watching, []string{"/src"}) || len(up.Failed) != 0 || up.WatchID != res.WatchID {
		t.Errorf("PUT %+v", up)
	}
	if row := w.row(res.WatchID); row.Gen != 2 || row.ResultGen != 2 {
		t.Errorf("row after PUT %+v", row)
	}
	// The empty set is legal.
	if up := w.put(a, res.WatchID); len(up.Watching) != 0 || len(up.Failed) != 0 || up.Watching == nil {
		t.Errorf("PUT [] %+v", up)
	}

	// DELETE: the stream ends cleanly, the guest stream goes.
	if code, body := w.call(a, w.sb, "DELETE", "/watches/"+res.WatchID, ""); code != 204 {
		t.Fatalf("DELETE %d %s", code, body)
	}
	if err := st.ended(t, 5*time.Second); err != nil {
		t.Errorf("the stream ended with %v, want a clean end", err)
	}
	eventually(t, 5*time.Second, "the guest stream gone", func() bool { return w.guestStreams() == 0 })
	if code, body := w.call(a, w.sb, "DELETE", "/watches/"+res.WatchID, ""); code != 404 || body != `{"error":"no such watch"}`+"\n" {
		t.Errorf("DELETE again %d %s", code, body)
	}
}

// The bodies and lookups: bad JSON, blank paths, unknown and foreign
// watches, a missing set.
func TestBodiesAndLookups(t *testing.T) {
	w := newWorld(t)
	a := w.addInstance("a")
	for _, c := range []struct{ body, want string }{
		{`[1]`, "request body must be a JSON object: "},
		{`{"paths":"x"}`, "invalid request body: "},
		{`{"paths":["/a",""]}`, `paths[1] must not be empty`},
		{`{"paths":["/a"," "]}`, `paths[1] must not be empty`},
	} {
		code, body := w.call(a, w.sb, "POST", "/watches", c.body)
		var e struct{ Error string }
		_ = json.Unmarshal([]byte(body), &e)
		if code != 400 || !strings.HasPrefix(e.Error, c.want) {
			t.Errorf("POST %s: %d %s", c.body, code, body)
		}
	}
	// No body and a null set are the empty set.
	for _, b := range []string{"", `{"paths":null}`} {
		code, body := w.call(a, w.sb, "POST", "/watches", b)
		if code != 201 || !strings.Contains(body, `"watching":[],"failed":[]`) {
			t.Errorf("POST %q: %d %s", b, code, body)
		}
	}
	res := w.create(a, "/")
	other := w.newSandbox()
	code, body := w.call(a, other, "POST", "/watches", pathsBody("/"))
	if code != 201 {
		t.Fatalf("%d %s", code, body)
	}
	var foreign response
	_ = json.Unmarshal([]byte(body), &foreign)
	noWatch := `{"error":"no such watch"}` + "\n"
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/watches/" + uuid.NewString(), "{}"},
		{"PUT", "/watches/not-a-uuid", "{}"},
		{"PUT", "/watches/" + foreign.WatchID, "{}"},
		{"GET", "/watches/" + foreign.WatchID + "/events", ""},
		{"DELETE", "/watches/" + foreign.WatchID, ""},
	} {
		if code, body := w.call(a, w.sb, c.method, c.path, c.body); code != 404 || body != noWatch {
			t.Errorf("%s %s: %d %s", c.method, c.path, code, body)
		}
	}
	// The foreign watch is untouched; the own one answers.
	if code, _ := w.call(a, other, "PUT", "/watches/"+foreign.WatchID, pathsBody("/")); code != 200 {
		t.Errorf("own PUT %d", code)
	}
	if up := w.put(a, res.WatchID, "/src"); !slices.Equal(up.Watching, []string{"/src"}) {
		t.Errorf("PUT %+v", up)
	}
}

// A GET of the events on another instance takes the watch over: the
// client's cursor of the old epoch gets one stale event, the old owner
// cuts its stream off and closes its guest stream, and the events go on
// in the new epoch.
func TestEventsTakeOwnership(t *testing.T) {
	w := newWorld(t)
	a, b := w.addInstance("a"), w.addInstance("b")
	res := w.create(a, "/", "/src")
	stA := w.open(a, res.WatchID, res.Cursor)
	w.guestEvent("/src", "one")
	evA := stA.next(t, 5*time.Second)
	if evA.id != epochID(1, 1) {
		t.Fatalf("event %d", evA.id)
	}

	stB := w.open(b, res.WatchID, evA.id)
	if ev := stB.next(t, 5*time.Second); ev.id != epochID(2, 0) || ev.data != staleData(epochID(2, 0), "/", "/src") {
		t.Errorf("the new owner's first event %d %s", ev.id, ev.data)
	}
	if err := stA.ended(t, 5*time.Second); err == nil {
		t.Error("the old owner's stream ended cleanly; it must be cut off")
	}
	if row := w.row(res.WatchID); row.Owner != "b" || row.Epoch != 2 || !row.OwnerLive {
		t.Errorf("row %+v", row)
	}
	eventually(t, 5*time.Second, "one guest stream", func() bool { return w.guestStreams() == 1 })
	w.guestEvent("/src", "two")
	if ev := stB.next(t, 5*time.Second); ev.id != epochID(2, 1) || ev.data != createdData(epochID(2, 1), "/src", "two") {
		t.Errorf("event %d %s", ev.id, ev.data)
	}
	stB.quiet(t, 200*time.Millisecond)

	// The client reconnects to the new owner with its cursor: caught up.
	stB.cancel()
	stB2 := w.open(b, res.WatchID, epochID(2, 1))
	w.guestEvent("/", "three")
	if ev := stB2.next(t, 5*time.Second); ev.id != epochID(2, 2) {
		t.Errorf("event %d %s", ev.id, ev.data)
	}
}

// A PUT on an instance that does not own the watch: with the owner live,
// the owner places the set and the PUT returns its answer; with the
// owner's lease gone, the PUT takes the watch over.
func TestPutOnNonOwner(t *testing.T) {
	w := newWorld(t)
	a := w.addInstance("a", func(tm *timings) { tm.ownerRenew = 300 * time.Millisecond })
	b := w.addInstance("b")
	res := w.create(a, "/")
	stA := w.open(a, res.WatchID, res.Cursor)
	w.guestEvent("/", "x")
	stA.next(t, 5*time.Second)

	up := w.put(b, res.WatchID, "/", "/src", "/missing")
	if up.WatchID != res.WatchID || up.Cursor != epochID(1, 1) || !slices.Equal(up.Watching, []string{"/", "/src"}) ||
		len(up.Failed) != 1 || up.Failed[0].Path != "/missing" || up.WatchLimit != 2048 {
		t.Errorf("PUT on the non-owner %+v", up)
	}
	row := w.row(res.WatchID)
	if row.Owner != "a" || row.Epoch != 1 || row.Gen != 2 || row.ResultGen != 2 ||
		!slices.Equal(row.Paths, []string{"/", "/src", "/missing"}) {
		t.Errorf("row %+v", row)
	}
	// The set is on the guest: an event of the new path arrives, on A.
	w.guestEvent("/src", "y")
	if ev := stA.next(t, 5*time.Second); ev.id != epochID(1, 2) {
		t.Errorf("event %d %s", ev.id, ev.data)
	}

	// The owner's lease ends (its instance died): the PUT takes over. The
	// old owner finds out at its next renewal (no ferun_watch tells it).
	if _, err := w.env.DB.Pool.Exec(context.Background(),
		"UPDATE watches SET owner = 'gone', owner_until = now() - interval '1 second' WHERE id = $1", row.ID); err != nil {
		t.Fatal(err)
	}
	if err := stA.ended(t, 5*time.Second); err == nil {
		t.Error("the old owner's stream ended cleanly")
	}
	up = w.put(b, res.WatchID, "/src")
	if !slices.Equal(up.Watching, []string{"/src"}) || up.Cursor != epochID(2, 0) {
		t.Errorf("PUT taking over %+v", up)
	}
	if row := w.row(res.WatchID); row.Owner != "b" || row.Epoch != 2 || !row.OwnerLive {
		t.Errorf("row %+v", row)
	}
	eventually(t, 5*time.Second, "one guest stream", func() bool { return w.guestStreams() == 1 })

	// An instance that stops lets go of its watches at once: the next PUT
	// elsewhere takes over without waiting for the lease.
	b.stop()
	if row := w.row(res.WatchID); row.OwnerLive {
		t.Errorf("the stopped owner's lease is live: %+v", row)
	}
	start := time.Now()
	up = w.put(a, res.WatchID, "/")
	if !slices.Equal(up.Watching, []string{"/"}) || time.Since(start) > 5*time.Second {
		t.Errorf("PUT after the owner stopped %+v after %v", up, time.Since(start))
	}
	if row := w.row(res.WatchID); row.Owner != "a" || row.Epoch != 3 {
		t.Errorf("row %+v", row)
	}
}

// A PUT whose owner does not answer in time gets 504; a deleted watch
// 404.
func TestPutWaitsForOwner(t *testing.T) {
	w := newWorld(t)
	a := w.addInstance("a")
	b := w.addInstance("b", func(tm *timings) { tm.ackTimeout = 500 * time.Millisecond })
	res := w.create(a, "/")
	// The owner is live in the row but does not act: its instance hangs.
	a.stop()
	if _, err := w.env.DB.Pool.Exec(context.Background(),
		"UPDATE watches SET owner = 'a', owner_until = now() + interval '1 minute' WHERE id = $1", res.WatchID); err != nil {
		t.Fatal(err)
	}
	code, body := w.call(b, w.sb, "PUT", "/watches/"+res.WatchID, pathsBody("/src"))
	if code != 504 || body != `{"error":"the sandbox did not acknowledge the path set within 500ms"}`+"\n" {
		t.Errorf("PUT %d %s", code, body)
	}
	if code, body := w.call(b, w.sb, "DELETE", "/watches/"+res.WatchID, ""); code != 204 {
		t.Fatalf("DELETE %d %s", code, body)
	}
	if code, _ := w.call(b, w.sb, "PUT", "/watches/"+res.WatchID, pathsBody("/src")); code != 404 {
		t.Errorf("PUT of a deleted watch %d", code)
	}
}

// Replay and stale: a cursor older than the ring gets one stale event at
// the head, a cursor within it the events after it, no cursor the first
// epoch from its start, a cursor past the head nothing.
func TestReplayAndStale(t *testing.T) {
	w := newWorld(t)
	a := w.addInstance("a", func(tm *timings) { tm.ringSize = 4 })
	res := w.create(a, "/", "/src")
	st := w.open(a, res.WatchID, res.Cursor)
	for i := range 6 {
		w.guestEvent("/", "f"+string(rune('a'+i)))
		st.next(t, 5*time.Second)
	}
	head := epochID(1, 6)

	old := w.open(a, res.WatchID, res.Cursor)
	if ev := old.next(t, 5*time.Second); ev.id != head || ev.data != staleData(head, "/", "/src") {
		t.Errorf("a cursor older than the ring: %d %s", ev.id, ev.data)
	}
	old.quiet(t, 200*time.Millisecond)

	within := w.open(a, res.WatchID, head-2)
	for _, n := range []uint64{5, 6} {
		if ev := within.next(t, 5*time.Second); ev.id != epochID(1, n) || ev.data != createdData(epochID(1, n), "/", "f"+string(rune('a'+n-1))) {
			t.Errorf("replay: %d %s", ev.id, ev.data)
		}
	}
	within.quiet(t, 200*time.Millisecond)

	none := w.open(a, res.WatchID, 0) // the ring lost the epoch's start
	if ev := none.next(t, 5*time.Second); ev.id != head || !strings.Contains(ev.data, `"kind":"stale"`) {
		t.Errorf("no cursor: %d %s", ev.id, ev.data)
	}
	// Past the head in the epoch: caught up. Of an epoch no owner issued:
	// stale.
	past := w.open(a, res.WatchID, head+5)
	past.quiet(t, 200*time.Millisecond)
	ahead := w.open(a, res.WatchID, epochID(7, 3))
	if ev := ahead.next(t, 5*time.Second); ev.id != head || !strings.Contains(ev.data, `"kind":"stale"`) {
		t.Errorf("a cursor of a later epoch: %d %s", ev.id, ev.data)
	}

	w.guestEvent("/src", "g")
	for _, s := range []*stream{st, old, within, none, ahead} {
		if ev := s.next(t, 5*time.Second); ev.id != head+1 {
			t.Errorf("the next event %d %s", ev.id, ev.data)
		}
	}
	past.quiet(t, 200*time.Millisecond)
}

// A dropped node connection detaches the guest stream (stale at once);
// the node's return places the set again (stale after the ack), and the
// events go on.
func TestRelinkAfterNodeReconnect(t *testing.T) {
	w := newWorld(t)
	a := w.addInstance("a")
	res := w.create(a, "/", "/src")
	st := w.open(a, res.WatchID, res.Cursor)

	w.env.Node.Partition(true)
	if ev := st.next(t, 10*time.Second); ev.id != epochID(1, 1) || ev.data != staleData(epochID(1, 1), "/", "/src") {
		t.Errorf("stale at the drop: %d %s", ev.id, ev.data)
	}
	// While the node is away, a PUT keeps the set and says why it is not
	// placed.
	up := w.put(a, res.WatchID, "/src")
	if len(up.Watching) != 0 || len(up.Failed) != 1 || up.Failed[0].Error != reasonNoNode {
		t.Errorf("PUT without the node %+v", up)
	}
	w.env.Node.Partition(false)
	if ev := st.next(t, 30*time.Second); ev.id != epochID(1, 2) || ev.data != staleData(epochID(1, 2), "/src") {
		t.Errorf("stale after the re-link: %d %s", ev.id, ev.data)
	}
	eventually(t, 5*time.Second, "one guest stream", func() bool { return w.guestStreams() == 1 })
	w.guestEvent("/src", "back")
	if ev := st.next(t, 5*time.Second); ev.id != epochID(1, 3) || ev.data != createdData(epochID(1, 3), "/src", "back") {
		t.Errorf("event %d %s", ev.id, ev.data)
	}
	row := w.row(res.WatchID)
	var stored response
	if err := json.Unmarshal(row.Result, &stored); err != nil || row.ResultGen != row.Gen ||
		!slices.Equal(stored.Watching, []string{"/src"}) || stored.Cursor != epochID(1, 1) {
		t.Errorf("row after the re-link %+v %s", row, row.Result)
	}
}

// A watch on a sandbox that does not run places nothing and wakes nothing;
// when the sandbox runs again (ferun_sandbox), the set is placed, with a
// stale event for the time it was not.
func TestRelinkAfterResume(t *testing.T) {
	w := newWorld(t)
	a := w.addInstance("a")
	ctx := context.Background()
	pods, err := w.env.Pods(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pods.SuspendPod(ctx, &podv1.SuspendPodRequest{PodId: w.sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	w.sbs.set(w.sb.ID, model.StatePaused, toolstest.NodeName)
	res := w.create(a, "/", "/src")
	if len(res.Watching) != 0 || len(res.Failed) != 2 || res.Failed[0].Error != reasonAsleep || res.WatchLimit != 0 {
		t.Errorf("POST while paused %+v", res)
	}
	if n := w.guestStreams(); n != 0 {
		t.Errorf("%d guest streams while paused", n)
	}
	st := w.open(a, res.WatchID, res.Cursor)
	// The owner looks again and places nothing, once: no answer written,
	// no ferun_watch that would wake it again.
	notes, unsubscribe := w.env.DB.Subscribe(db.ChanWatch)
	w.notifySandbox()
	st.quiet(t, 500*time.Millisecond)
	unsubscribe()
	if n := len(notes); n != 0 {
		t.Errorf("%d ferun_watch notifications while the sandbox sleeps", n)
	}
	if st, _ := w.env.PodState(ctx, w.sb.ID); st != podv1.PodState_POD_STATE_SUSPENDED {
		t.Errorf("the watch woke the pod: %v", st)
	}

	if _, err := pods.ResumePod(ctx, &podv1.ResumePodRequest{PodId: w.sb.ID.String()}); err != nil {
		t.Fatal(err)
	}
	w.sbs.set(w.sb.ID, model.StateRunning, toolstest.NodeName)
	w.notifySandbox()
	if ev := st.next(t, 10*time.Second); ev.id != epochID(1, 1) || ev.data != staleData(epochID(1, 1), "/", "/src") {
		t.Errorf("stale after the resume: %d %s", ev.id, ev.data)
	}
	if n := w.guestStreams(); n != 1 {
		t.Errorf("%d guest streams", n)
	}
	w.guestEvent("/src", "new")
	if ev := st.next(t, 5*time.Second); ev.id != epochID(1, 2) {
		t.Errorf("event %d %s", ev.id, ev.data)
	}
	var stored response
	if row := w.row(res.WatchID); json.Unmarshal(row.Result, &stored) != nil ||
		!slices.Equal(stored.Watching, []string{"/", "/src"}) {
		t.Errorf("row %s", row.Result)
	}
}

// The cap of guest streams per sandbox on an instance: past it every path
// fails with the reason; another instance has its own.
func TestStreamCap(t *testing.T) {
	w := newWorld(t)
	tune := func(tm *timings) { tm.perSandbox = 2 }
	a, b := w.addInstance("a", tune), w.addInstance("b", tune)
	w.create(a, "/")
	w.create(a, "/src")
	third := w.create(a, "/", "/src")
	if len(third.Watching) != 0 || len(third.Failed) != 2 || third.Failed[0].Error != reasonTooMany {
		t.Errorf("past the cap %+v", third)
	}
	if res := w.create(b, "/"); !slices.Equal(res.Watching, []string{"/"}) {
		t.Errorf("on another instance %+v", res)
	}
	if n := w.guestStreams(); n != 3 {
		t.Errorf("%d guest streams", n)
	}
}

// The leader's loop deletes the watches whose lease passed; the owner
// lets go: its stream ends cleanly, its guest stream closes.
func TestExpiry(t *testing.T) {
	w := newWorld(t)
	a := w.addInstance("a", func(tm *timings) { tm.lease = time.Second })
	res := w.create(a, "/")
	st := w.open(a, res.WatchID, res.Cursor)
	loop := a.s.Loops()[0]
	if loop.Name != "watch-expiry" || loop.Interval != time.Minute {
		t.Errorf("loop %s %v", loop.Name, loop.Interval)
	}
	if err := loop.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.New(w.env.DB).GetWatch(context.Background(), uuid.MustParse(res.WatchID)); err != nil {
		t.Fatalf("a live watch expired: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	if err := loop.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.New(w.env.DB).GetWatch(context.Background(), uuid.MustParse(res.WatchID)); !errors.Is(err, store.ErrNoRow) {
		t.Fatalf("the expired watch: %v", err)
	}
	if err := st.ended(t, 5*time.Second); err != nil {
		t.Errorf("the stream ended with %v", err)
	}
	eventually(t, 5*time.Second, "the guest stream gone", func() bool { return w.guestStreams() == 0 })
}

// A stopping instance cuts its event streams off and serves none any
// more; the client reconnects to another instance with its cursor.
func TestEndStreams(t *testing.T) {
	w := newWorld(t)
	a, b := w.addInstance("a"), w.addInstance("b")
	res := w.create(a, "/")
	st := w.open(a, res.WatchID, res.Cursor)
	a.s.EndStreams()
	if err := st.ended(t, 5*time.Second); err == nil {
		t.Error("the stream ended cleanly; it must be cut off")
	}
	code, body := w.call(a, w.sb, "GET", "/watches/"+res.WatchID+"/events", "")
	if code != 503 || body != `{"error":"this instance is stopping"}`+"\n" {
		t.Errorf("events on a stopping instance %d %s", code, body)
	}
	stB := w.open(b, res.WatchID, res.Cursor)
	if ev := stB.next(t, 5*time.Second); ev.id != epochID(2, 0) || !strings.Contains(ev.data, `"kind":"stale"`) {
		t.Errorf("event %d %s", ev.id, ev.data)
	}
}

// A DELETE on another instance than the owner: the owner ends its stream
// cleanly and closes its guest stream.
func TestDeleteOnNonOwner(t *testing.T) {
	w := newWorld(t)
	a, b := w.addInstance("a"), w.addInstance("b")
	res := w.create(a, "/")
	st := w.open(a, res.WatchID, res.Cursor)
	if code, body := w.call(b, w.sb, "DELETE", "/watches/"+res.WatchID, ""); code != 204 {
		t.Fatalf("DELETE %d %s", code, body)
	}
	if err := st.ended(t, 5*time.Second); err != nil {
		t.Errorf("the stream ended with %v", err)
	}
	eventually(t, 5*time.Second, "the guest stream gone", func() bool { return w.guestStreams() == 0 })
}

// The ring's arithmetic: dense ids from the epoch, overflow, replay.
func TestRing(t *testing.T) {
	r := newRing(3, 2)
	if evs, stale := r.since(epochID(2, 0)); evs != nil || stale {
		t.Errorf("empty ring %v %v", evs, stale)
	}
	for range 5 {
		r.push(event{Kind: "created"})
	}
	if r.last != epochID(2, 5) || r.count != 3 {
		t.Fatalf("last %d count %d", r.last, r.count)
	}
	if _, stale := r.since(epochID(2, 1)); !stale {
		t.Error("a cursor before the ring is not stale")
	}
	evs, stale := r.since(epochID(2, 2))
	if stale || len(evs) != 3 || evs[0].ID != epochID(2, 3) || evs[2].ID != epochID(2, 5) {
		t.Errorf("replay %v %v", evs, stale)
	}
	if evs, stale := r.since(epochID(2, 9)); evs != nil || stale {
		t.Errorf("past the head %v %v", evs, stale)
	}
}

// A deleted sandbox takes its watches with it (the rows cascade): the
// owner learns it from ferun_sandbox and lets go.
func TestSandboxDeleted(t *testing.T) {
	w := newWorld(t)
	a := w.addInstance("a")
	res := w.create(a, "/")
	st := w.open(a, res.WatchID, res.Cursor)
	deleted, err := store.New(w.env.DB).DeleteSandbox(context.Background(), w.sb.ID)
	if err != nil || !deleted {
		t.Fatalf("delete the sandbox: %v %v", deleted, err)
	}
	if err := st.ended(t, 5*time.Second); err != nil {
		t.Errorf("the stream ended with %v", err)
	}
	eventually(t, 5*time.Second, "the guest stream gone", func() bool { return w.guestStreams() == 0 })
}

// A guest stream that keeps ending right after its ack is opened again
// less and less often: the backoff doubles up to its longest wait, though
// each opening is acknowledged. Once a stream has stood for the longest
// wait, the backoff starts over.
func TestRelinkBackoffGrows(t *testing.T) {
	w := newWorld(t)
	f := newFaults(w.env.Port)
	a := w.addInstanceWith("a", instanceOptions{conns: f}, func(tm *timings) {
		tm.relinkMin = 50 * time.Millisecond
		tm.relinkMax = 800 * time.Millisecond
	})
	res := w.create(a, "/")
	st := w.open(a, res.WatchID, res.Cursor)

	// The stream ends at its next read, after this event, and so does every
	// one opened again.
	f.setEndAfterAck(true)
	start := time.Now()
	w.guestEvent("/", "x")
	if ev := st.next(t, 5*time.Second); ev.id != epochID(1, 1) {
		t.Errorf("event %d %s", ev.id, ev.data)
	}
	time.Sleep(2500 * time.Millisecond)
	var opens []time.Time
	for _, at := range f.opened() {
		if at.After(start) {
			opens = append(opens, at)
		}
	}
	// 50, 150, 350, 750, 1550 and 2350 ms with the backoff; every 50 ms
	// without it.
	if len(opens) < 3 || len(opens) > 8 {
		t.Fatalf("%d openings in 2.5 s", len(opens))
	}
	if gap := opens[len(opens)-1].Sub(opens[len(opens)-2]); gap < 400*time.Millisecond {
		t.Errorf("the last wait was %v; the backoff does not grow", gap)
	}

	// A stream that stands for the longest wait starts the backoff over.
	f.setEndAfterAck(false)
	eventually(t, 5*time.Second, "a stream that stands", func() bool {
		o := f.opened()
		return w.guestStreams() == 1 && time.Since(o[len(o)-1]) > 100*time.Millisecond
	})
	time.Sleep(time.Second)
	n := len(f.opened())
	f.setEndAfterAck(true)
	lost := time.Now()
	w.guestEvent("/", "y")
	eventually(t, 5*time.Second, "the stream opened again", func() bool { return len(f.opened()) > n })
	if d := f.opened()[n].Sub(lost); d > 400*time.Millisecond {
		t.Errorf("opened again after %v; the backoff did not start over", d)
	}
	f.setEndAfterAck(false)
}

// A guest stream that ends before its ack is opened again after a backoff
// that doubles: the placing's failure and the lost stream are one.
func TestRelinkBackoffDoubles(t *testing.T) {
	w := newWorld(t)
	f := newFaults(w.env.Port)
	a := w.addInstanceWith("a", instanceOptions{conns: f}, func(tm *timings) {
		tm.relinkMin = 100 * time.Millisecond
		tm.relinkMax = 3200 * time.Millisecond
	})
	res := w.create(a, "/")
	st := w.open(a, res.WatchID, res.Cursor)
	f.setEndAlways(true)
	start := time.Now()
	w.guestEvent("/", "x")
	if ev := st.next(t, 5*time.Second); ev.id != epochID(1, 1) {
		t.Errorf("event %d %s", ev.id, ev.data)
	}
	time.Sleep(2 * time.Second)
	f.setEndAlways(false)
	var opens []time.Time
	for _, at := range f.opened() {
		if at.After(start) {
			opens = append(opens, at)
		}
	}
	// 100, 300, 700 and 1500 ms when it doubles; 100, 500 and 2100 ms when
	// it grows fourfold.
	if len(opens) < 4 || len(opens) > 5 {
		t.Fatalf("%d openings in 2 s", len(opens))
	}
	if gap := opens[2].Sub(opens[1]); gap > 600*time.Millisecond {
		t.Errorf("the second wait was %v", gap)
	}
}

// A placing that gives up on its ack drops the guest stream: the late ack
// is not taken as the answer to the next set, which a new stream places.
func TestLateAck(t *testing.T) {
	w := newWorld(t)
	f := newFaults(w.env.Port)
	a := w.addInstanceWith("a", instanceOptions{conns: f}, func(tm *timings) { tm.ackTimeout = 300 * time.Millisecond })
	res := w.create(a, "/")
	st := w.open(a, res.WatchID, res.Cursor)

	f.setLateAck(450 * time.Millisecond)
	code, body := w.call(a, w.sb, "PUT", "/watches/"+res.WatchID, pathsBody("/src"))
	if code != 504 {
		t.Fatalf("PUT with a late ack %d %s", code, body)
	}
	up := w.put(a, res.WatchID, "/", "/missing")
	if !slices.Equal(up.Watching, []string{"/"}) || len(up.Failed) != 1 || up.Failed[0].Path != "/missing" {
		t.Errorf("the next PUT was answered %+v", up)
	}
	var stored response
	if row := w.row(res.WatchID); json.Unmarshal(row.Result, &stored) != nil || row.ResultGen != row.Gen ||
		!slices.Equal(stored.Watching, []string{"/"}) {
		t.Errorf("row %+v %s", row, row.Result)
	}
	// The dropped stream's readers re-read; the new one carries the events.
	if ev := st.next(t, 5*time.Second); !strings.Contains(ev.data, `"kind":"stale"`) {
		t.Errorf("event %d %s", ev.id, ev.data)
	}
	eventually(t, 5*time.Second, "one guest stream", func() bool { return w.guestStreams() == 1 })
}

// Two takes of a watch on one instance that adopt out of order: the later
// epoch stays and holds the watch; the earlier one gets it back.
func TestTakesAdoptOutOfOrder(t *testing.T) {
	w := newWorld(t)
	a := w.addInstance("a", func(tm *timings) { tm.ackTimeout = 3 * time.Second })
	b := w.addInstance("b")
	res := w.create(a, "/")
	ctx := context.Background()
	st := store.New(w.env.DB)
	wid := uuid.MustParse(res.WatchID)
	r2, err := st.TakeWatchOwnership(ctx, wid, "b", b.s.t.lease, b.s.t.ownerLease)
	if err != nil {
		t.Fatal(err)
	}
	r3, err := st.TakeWatchOwnership(ctx, wid, "b", b.s.t.lease, b.s.t.ownerLease)
	if err != nil {
		t.Fatal(err)
	}
	o3 := b.s.adopt(r3)
	if _, err := b.s.placeTaken(ctx, o3, r3); err != nil {
		t.Fatal(err)
	}
	if o := b.s.adopt(r2); o != o3 || o3.isClosed() {
		t.Fatalf("the earlier take adopted epoch %d; epoch 3 closed: %v", o.epoch, o3.isClosed())
	}
	// The owner in epoch 3 answers a PUT elsewhere, and serves the events.
	if up := w.put(a, res.WatchID, "/src"); !slices.Equal(up.Watching, []string{"/src"}) {
		t.Errorf("PUT %+v", up)
	}
	if row := w.row(res.WatchID); row.Owner != "b" || row.Epoch != 3 || row.ResultGen != row.Gen {
		t.Errorf("row %+v", row)
	}
	stB := w.open(b, res.WatchID, res.Cursor)
	if ev := stB.next(t, 5*time.Second); ev.id != epochID(3, 0) || !strings.Contains(ev.data, `"kind":"stale"`) {
		t.Errorf("event %d %s", ev.id, ev.data)
	}
	eventually(t, 5*time.Second, "one guest stream", func() bool { return w.guestStreams() == 1 })
}

// Over HTTP/2 an event stream that is cut off (its watch moved, its
// instance stops) is reset rather than ended, so that its client
// reconnects; a deleted watch still ends it cleanly.
func TestCutOffOverHTTP2(t *testing.T) {
	w := newWorld(t)
	h2 := instanceOptions{h2: true}
	a, b := w.addInstanceWith("a", h2), w.addInstanceWith("b", h2)
	res := w.create(a, "/")
	stA := w.open(a, res.WatchID, res.Cursor)
	stB := w.open(b, res.WatchID, res.Cursor)
	if err := stA.ended(t, 5*time.Second); err == nil {
		t.Error("the moved watch's stream ended cleanly")
	}
	if ev := stB.next(t, 5*time.Second); ev.id != epochID(2, 0) {
		t.Errorf("event %d %s", ev.id, ev.data)
	}
	b.s.EndStreams()
	if err := stB.ended(t, 5*time.Second); err == nil {
		t.Error("the stopping instance's stream ended cleanly")
	}
	stA = w.open(a, res.WatchID, epochID(2, 0))
	if ev := stA.next(t, 5*time.Second); ev.id != epochID(3, 0) {
		t.Errorf("event %d %s", ev.id, ev.data)
	}
	if code, body := w.call(a, w.sb, "DELETE", "/watches/"+res.WatchID, ""); code != 204 {
		t.Fatalf("DELETE %d %s", code, body)
	}
	if err := stA.ended(t, 5*time.Second); err != nil {
		t.Errorf("the deleted watch's stream ended with %v", err)
	}
}
