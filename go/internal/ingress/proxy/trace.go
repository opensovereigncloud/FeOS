// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// The headers of W3C Trace Context (https://www.w3.org/TR/trace-context/).
const (
	headerTraceparent = "Traceparent"
	headerTracestate  = "Tracestate"
)

// maxTracestate is the longest tracestate passed on: the length the
// specification asks every vendor to propagate. A longer one is dropped,
// not cut at an entry.
const maxTracestate = 512

// flagSampled is traceparent's sampled flag. The ingress records every
// request, so a trace it starts is sampled.
const flagSampled = 0x01

// trace is a request's server span at the ingress: the trace it belongs
// to, its own span ID, the client's span when the request brought a
// valid traceparent, the flags, and the tracestate passed on.
type trace struct {
	id        [16]byte
	span      [8]byte
	parent    [8]byte
	hasParent bool
	flags     byte
	state     string
}

// newTrace continues the trace of the request's traceparent, as W3C
// Trace Context asks, or starts one when it brings none or an invalid
// one; the span is always the ingress's own. tracestate is passed on
// only with a traceparent continued.
func newTrace(h http.Header) trace {
	var t trace
	if id, parent, flags, ok := parseTraceparent(h.Values(headerTraceparent)); ok {
		t.id, t.parent, t.hasParent, t.flags = id, parent, true, flags
		t.state = tracestate(h.Values(headerTracestate))
	} else {
		for t.id == ([16]byte{}) {
			_, _ = rand.Read(t.id[:])
		}
		t.flags = flagSampled
	}
	for t.span == ([8]byte{}) {
		_, _ = rand.Read(t.span[:])
	}
	return t
}

// traceparent is the header the instance gets: this trace, with the
// ingress's span as the parent.
func (t trace) traceparent() string {
	var b [55]byte
	copy(b[:], "00-")
	hex.Encode(b[3:35], t.id[:])
	b[35] = '-'
	hex.Encode(b[36:52], t.span[:])
	b[52] = '-'
	hex.Encode(b[53:55], []byte{t.flags})
	return string(b[:])
}

// set replaces the trace headers of an outbound request with t's.
func (t trace) set(h http.Header) {
	h.Set(headerTraceparent, t.traceparent())
	if t.state != "" {
		h.Set(headerTracestate, t.state)
	} else {
		h.Del(headerTracestate)
	}
}

// ids are the trace's, the span's and the parent span's IDs in lower
// case hex; parent is "" without one.
func (t trace) ids() (id, span, parent string) {
	id, span = hex.EncodeToString(t.id[:]), hex.EncodeToString(t.span[:])
	if t.hasParent {
		parent = hex.EncodeToString(t.parent[:])
	}
	return id, span, parent
}

// parseTraceparent reads a traceparent header: exactly one field,
// version 00 at its exact length, or a later version at least as long
// with a dash after the flags; lower case hex; neither ID all zero;
// version ff is invalid.
func parseTraceparent(vv []string) (id [16]byte, parent [8]byte, flags byte, ok bool) {
	if len(vv) != 1 {
		return id, parent, 0, false
	}
	s := strings.TrimSpace(vv[0])
	if len(s) < 55 || s[2] != '-' || s[35] != '-' || s[52] != '-' {
		return id, parent, 0, false
	}
	var version [1]byte
	if !lowerHex(s[0:2], version[:]) || version[0] == 0xff {
		return id, parent, 0, false
	}
	if version[0] == 0 && len(s) != 55 {
		return id, parent, 0, false
	}
	if version[0] > 0 && len(s) > 55 && s[55] != '-' {
		return id, parent, 0, false
	}
	var f [1]byte
	if !lowerHex(s[3:35], id[:]) || !lowerHex(s[36:52], parent[:]) || !lowerHex(s[53:55], f[:]) {
		return id, parent, 0, false
	}
	if id == ([16]byte{}) || parent == ([8]byte{}) {
		return id, parent, 0, false
	}
	return id, parent, f[0], true
}

// lowerHex decodes s, lower case hex of exactly len(dst) bytes, into dst.
func lowerHex(s string, dst []byte) bool {
	if len(s) != 2*len(dst) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	_, err := hex.Decode(dst, []byte(s))
	return err == nil
}

// tracestate joins the request's tracestate fields, as one list, when
// they are printable ASCII and within maxTracestate; else "".
func tracestate(vv []string) string {
	s := strings.Join(vv, ",")
	if len(s) > maxTracestate {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return ""
		}
	}
	return strings.TrimSpace(s)
}
