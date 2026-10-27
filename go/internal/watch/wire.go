// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	watchv1 "github.com/gardenlinux/feos/go/gen/feos/watch/v1"
	"github.com/gardenlinux/feos/go/internal/apierr"
)

// pathError is one path that could not be watched, and why.
type pathError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// response is what POST and PUT answer, the same body for both: they are
// the same operation, the complete path set placed and acknowledged. The
// owner stores it in the row for PUTs that other instances answer.
type response struct {
	WatchID    string      `json:"watchId"`
	Cursor     uint64      `json:"cursor"`
	Watching   []string    `json:"watching"`
	Failed     []pathError `json:"failed"`
	Watches    uint32      `json:"watches"`
	WatchLimit uint32      `json:"watchLimit"`
}

// The reasons a path set is not placed, per path.
const (
	// reasonAsleep: the sandbox does not run; the set is placed when it
	// runs again. The sandbox system's text, without its citation of a
	// design record of its own.
	reasonAsleep = "the sandbox is not running: the paths are placed as soon " +
		"as it runs again (a watch does not hold a sandbox awake)"
	// reasonEnded: the guest stream ended before its ack.
	reasonEnded = "the sandbox's watch stream ended before the " +
		"paths were placed; the next PUT re-places them"
	// reasonNoNode: the sandbox's node has no connection to the owner.
	reasonNoNode = "the sandbox's node is not connected to this instance: " +
		"the paths are placed as soon as it is"
	// reasonTooMany: the owner's cap of guest streams per sandbox.
	reasonTooMany = "too many watches on this instance"
)

// notPlaced is the answer for a set the watch keeps but could not place:
// every path in failed with the reason.
func notPlaced(id uuid.UUID, cursor uint64, paths []string, reason string) response {
	failed := make([]pathError, 0, len(paths))
	for _, p := range paths {
		failed = append(failed, pathError{Path: p, Error: reason})
	}
	return response{WatchID: id.String(), Cursor: cursor, Watching: []string{}, Failed: failed}
}

// ackResponse is the answer for a set the guest acknowledged, with the
// cursor of the owner's ring at the ack.
func ackResponse(id uuid.UUID, cursor uint64, a *watchv1.Ack) response {
	res := response{WatchID: id.String(), Cursor: cursor, Watching: a.GetWatching(), Failed: []pathError{},
		Watches: a.GetWatches(), WatchLimit: a.GetWatchLimit()}
	if res.Watching == nil {
		res.Watching = []string{}
	}
	for _, f := range a.GetFailed() {
		res.Failed = append(res.Failed, pathError{Path: f.GetPath(), Error: f.GetError()})
	}
	return res
}

// event is one filesystem event as the API numbers it.
type event struct {
	ID         uint64
	Kind       string
	Dir        string
	Name       string
	IsDir      bool
	Cookie     uint32
	StalePaths []string
}

const kindStale = "stale"

// kindName is the API's name of a guest event kind; "" for one it has no
// name for, which is dropped (the API numbers what it forwards, so the
// cursor does not suffer).
func kindName(k watchv1.EventKind) string {
	switch k {
	case watchv1.EventKind_EVENT_KIND_CREATED:
		return "created"
	case watchv1.EventKind_EVENT_KIND_DELETED:
		return "deleted"
	case watchv1.EventKind_EVENT_KIND_MODIFIED:
		return "modified"
	case watchv1.EventKind_EVENT_KIND_ATTRIB:
		return "attrib"
	case watchv1.EventKind_EVENT_KIND_MOVED_FROM:
		return "moved_from"
	case watchv1.EventKind_EVENT_KIND_MOVED_TO:
		return "moved_to"
	case watchv1.EventKind_EVENT_KIND_STALE:
		return kindStale
	}
	return ""
}

// eventsOf maps a guest batch; the guest's ids are ignored.
func eventsOf(b *watchv1.EventBatch) []event {
	out := make([]event, 0, len(b.GetEvents()))
	for _, ev := range b.GetEvents() {
		kind := kindName(ev.GetKind())
		if kind == "" {
			continue
		}
		out = append(out, event{Kind: kind, Dir: ev.GetDir(), Name: ev.GetName(), IsDir: ev.GetIsDir(),
			Cookie: ev.GetCookie(), StalePaths: ev.GetStalePaths()})
	}
	return out
}

// marshalEvent renders the two shapes of the contract, with every field
// present: a stale event carries stalePaths where an ordinary one carries
// name and isDir ("isDir": false is an answer, not an absence).
func marshalEvent(ev event) ([]byte, error) {
	if ev.Kind == kindStale {
		paths := ev.StalePaths
		if paths == nil {
			paths = []string{}
		}
		return json.Marshal(struct {
			ID         uint64   `json:"id"`
			Kind       string   `json:"kind"`
			Dir        string   `json:"dir"`
			Cookie     uint32   `json:"cookie"`
			StalePaths []string `json:"stalePaths"`
		}{ev.ID, ev.Kind, ev.Dir, ev.Cookie, paths})
	}
	return json.Marshal(struct {
		ID     uint64 `json:"id"`
		Kind   string `json:"kind"`
		Dir    string `json:"dir"`
		Name   string `json:"name"`
		IsDir  bool   `json:"isDir"`
		Cookie uint32 `json:"cookie"`
	}{ev.ID, ev.Kind, ev.Dir, ev.Name, ev.IsDir, ev.Cookie})
}

// writeEvent frames one event: "id:" is the cursor the client sends back
// as Last-Event-ID, "data:" one JSON object.
func writeEvent(w io.Writer, ev event) error {
	payload, err := marshalEvent(ev)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.ID, payload)
	return err
}

// lastEventID reads the client's cursor; per the SSE rules an unparsable
// one is ignored (0).
func lastEventID(r *http.Request) uint64 {
	n, err := strconv.ParseUint(strings.TrimSpace(r.Header.Get("Last-Event-ID")), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// maxBody bounds a request body, as the REST API does.
const maxBody = 32 << 20

// readPaths reads the one field the body carries: the complete set. A
// missing or null set is the empty one, which is a legal subscription; a
// blank path is not.
func readPaths(r *http.Request) ([]string, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, apierr.New(http.StatusBadRequest, "read body: %v", err)
	}
	if len(raw) > maxBody {
		return nil, apierr.New(http.StatusRequestEntityTooLarge, "request body larger than %d bytes", maxBody)
	}
	if strings.TrimSpace(string(raw)) == "" {
		raw = []byte("{}")
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sent); err != nil {
		return nil, apierr.New(http.StatusBadRequest, "request body must be a JSON object: %v", err)
	}
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, apierr.New(http.StatusBadRequest, "invalid request body: %v", err)
	}
	if body.Paths == nil {
		body.Paths = []string{}
	}
	for i, p := range body.Paths {
		if strings.TrimSpace(p) == "" {
			return nil, apierr.New(http.StatusBadRequest, "paths[%d] must not be empty", i)
		}
	}
	return body.Paths, nil
}
