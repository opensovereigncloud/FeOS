// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package watch is ferund's filesystem watch API (§14 of the design), on
// the sandbox class of the REST API:
//
//	POST   /api/v1/sandboxes/{id}/watches          {"paths":[…]} → 201
//	PUT    /api/v1/sandboxes/{id}/watches/{wid}    {"paths":[…]} → 200
//	GET    /api/v1/sandboxes/{id}/watches/{wid}/events           → SSE
//	DELETE /api/v1/sandboxes/{id}/watches/{wid}                  → 204
//
// The bodies, texts and the event stream are those of the sandbox system,
// which FeCode's backend was built against. A PUT carries the complete
// path set, never a delta, and doubles as the heartbeat; "stale" means
// "re-read these paths" and is the answer whenever events may have been
// lost. A watch never wakes a sandbox and never holds one awake: a watch
// on a sandbox that does not run has nothing placed and says so per path,
// and is placed when the sandbox runs again.
//
// A watch is a row of the watches table, so that any instance answers its
// requests, and it has one owner: the instance that holds the guest's
// WatchService/Watch stream on its node connection and the replay ring of
// the last 1024 events. Event ids are epoch<<32 | n, where the epoch grows
// with every change of owner, so ids keep increasing across owners. The
// requests that land elsewhere:
//
//   - A GET of the events on another instance takes the watch over in the
//     next epoch; the old owner learns it from ferun_watch, closes its
//     guest stream and ends its event streams. A Last-Event-ID of an older
//     epoch, or older than the ring, gets one stale event with the whole
//     set.
//   - A PUT on another instance, while the owner's lease is live, writes
//     the set to the row; the owner places it and stores its answer, which
//     the PUT waits for and returns. With the owner's lease gone, the PUT
//     takes the watch over.
//   - A DELETE deletes the row; the owner lets go of everything.
//
// An owner keeps its guest streams placed: when one ends (a pause, a
// dropped node connection, a feosd restart) it pushes a stale event for
// the whole set and places the set again, with backoff, for as long as
// the sandbox runs, at once when the node connects again or the sandbox
// runs again, followed by another stale event: the time in between was
// not watched.
package watch
