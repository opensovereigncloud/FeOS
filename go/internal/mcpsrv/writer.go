// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package mcpsrv

import (
	"mime"
	"net/http"
	"sync"
	"time"
)

// keepaliveComment is the SSE comment that keeps a quiet response alive;
// clients ignore it.
const keepaliveComment = ": keepalive\n\n"

// keepaliveWriter is the response writer the SDK gets. Every Write,
// WriteHeader and Flush holds one mutex, so that the keepalive comments it
// writes while a tool call runs never land inside the SDK's events. It
// also sets the session id on a successful initialize answer.
type keepaliveWriter struct {
	w        http.ResponseWriter
	interval time.Duration

	mu          sync.Mutex
	onOK        func(http.Header) // called before a 200's header is written
	wroteHeader bool
	sse         bool // the response is text/event-stream
	started     bool
	stopped     bool
	quit        chan struct{}
	done        chan struct{}
}

func newKeepaliveWriter(w http.ResponseWriter, interval time.Duration) *keepaliveWriter {
	return &keepaliveWriter{w: w, interval: interval, quit: make(chan struct{}), done: make(chan struct{})}
}

func (k *keepaliveWriter) Header() http.Header { return k.w.Header() }

func (k *keepaliveWriter) WriteHeader(code int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.writeHeaderLocked(code)
}

func (k *keepaliveWriter) writeHeaderLocked(code int) {
	if k.wroteHeader {
		return
	}
	k.wroteHeader = true
	if code == http.StatusOK && k.onOK != nil {
		k.onOK(k.w.Header())
	}
	k.sse = isEventStream(k.w.Header())
	k.w.WriteHeader(code)
}

func (k *keepaliveWriter) Write(p []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.writeHeaderLocked(http.StatusOK)
	return k.w.Write(p)
}

// FlushError is what http.ResponseController.Flush calls.
func (k *keepaliveWriter) FlushError() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.writeHeaderLocked(http.StatusOK)
	return http.NewResponseController(k.w).Flush()
}

func (k *keepaliveWriter) Flush() { _ = k.FlushError() }

// Unwrap gives http.ResponseController the underlying writer for what this
// one does not do itself.
func (k *keepaliveWriter) Unwrap() http.ResponseWriter { return k.w }

// start begins the keepalive comments, once. It is called by a tool
// handler: by then the SDK has set the response's headers, before it
// handed the call over.
func (k *keepaliveWriter) start() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.started || k.stopped || k.interval <= 0 {
		return
	}
	k.started = true
	go k.loop()
}

func (k *keepaliveWriter) loop() {
	defer close(k.done)
	t := time.NewTicker(k.interval)
	defer t.Stop()
	for {
		select {
		case <-k.quit:
			return
		case <-t.C:
			k.beat()
		}
	}
}

// beat writes one keepalive comment, if the response is an event stream;
// before the SDK wrote anything, it writes the stream's header first.
func (k *keepaliveWriter) beat() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return
	}
	if !k.wroteHeader {
		if !isEventStream(k.w.Header()) {
			return
		}
		k.writeHeaderLocked(http.StatusOK)
	}
	if !k.sse {
		return
	}
	if _, err := k.w.Write([]byte(keepaliveComment)); err != nil {
		return
	}
	_ = http.NewResponseController(k.w).Flush()
}

// stop ends the keepalive comments when the handler returns, and waits for
// them: nothing is written after the handler.
func (k *keepaliveWriter) stop() {
	k.mu.Lock()
	k.stopped = true
	started := k.started
	k.mu.Unlock()
	if started {
		close(k.quit)
		<-k.done
	}
}

func isEventStream(h http.Header) bool {
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && mt == "text/event-stream"
}
