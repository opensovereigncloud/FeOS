// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"strings"

	"github.com/ironcore-dev/feos/go/internal/activity"
)

// Image is a picture a tool returned beside its text: a read of a PNG,
// JPEG, GIF or WebP, typically a screenshot. The text cap does not apply to
// it; the guest caps its size.
type Image struct {
	MediaType string
	Data      []byte
	Width     int
	Height    int
}

// Result is what a tool call produced.
//
// Text is the tool's output, stdout and stderr interleaved as they arrived,
// cut at MaxToolOutput bytes; Truncated says it was cut. ExitCode is the
// tool's verdict (bash: the command's; rg and fd: 1 for no match, 2 for an
// error). IsError is the verdict as MCP reports it: rg and fd fail above 1,
// every other tool on anything but 0. Each door renders a Result its own
// way: REST as exitCode, output and truncated, MCP as one text with the
// marker, "(no output)" and "(exit code N)".
//
// A Call that fails returns the output gathered until then in Text.
//
// FirstLine is set by a read of text: Text is the file's lines from that
// one on, and More says the file goes on past them. REST answers the text
// as it is; MCP, whose client is an agent, numbers the lines and names the
// offset to go on from.
type Result struct {
	Text      string
	ExitCode  int
	Truncated bool
	Image     *Image
	IsError   bool
	FirstLine int
	More      bool
}

// TruncatedMarker follows a cut output in an MCP answer.
const TruncatedMarker = "\n... [truncated]\n"

// streamMarker is written once into a stream of an exec whose output passed
// MaxToolOutput, as the guest's answers were capped per stream.
const streamMarker = "\n... [truncated]\n"

// isToolError is the MCP verdict of an exit code: rg and fd exit 1 when
// nothing matched, which is an answer, not a failure.
func isToolError(tool string, code int) bool {
	if code == 0 {
		return false
	}
	if tool == ToolRg || tool == ToolFd {
		return code > 1
	}
	return true
}

// output gathers what a call emits: the text the client gets, the audit
// copy for the activity log, and the picture. Every chunk also beats the
// progress callback.
type output struct {
	text     strings.Builder
	audit    strings.Builder
	auditCut bool
	image    *Image
	progress func(chunk string)
	// The lines of a read (Result.FirstLine, Result.More).
	firstLine int
	more      bool
}

// emit takes one chunk of text output. The client's text takes chunks
// while it holds less than MaxToolOutput bytes, so that it overshoots by at
// most one chunk, which result then cuts; the audit copy is cut at the
// activity log's output cap.
func (o *output) emit(chunk string) {
	if o.progress != nil {
		o.progress(chunk)
	}
	if o.text.Len() < MaxToolOutput {
		o.text.WriteString(chunk)
	}
	if rem := activity.OutputCap - o.audit.Len(); rem > 0 {
		if len(chunk) > rem {
			o.audit.WriteString(chunk[:rem])
			o.auditCut = true
		} else {
			o.audit.WriteString(chunk)
		}
	} else {
		o.auditCut = true
	}
}

// emitImage takes the picture of a read. It beats the progress callback
// like a chunk, and is no output of the audit copy: the line emitted beside
// it describes it.
func (o *output) emitImage(img *Image) {
	if o.progress != nil {
		o.progress("")
	}
	o.image = img
}

// lines says the text emitted is a file's lines from line first on, and
// whether the file goes on past them.
func (o *output) lines(first int, more bool) {
	o.firstLine, o.more = first, more
}

// result is the Result of what was emitted, for exit code code.
func (o *output) result(tool string, code int) Result {
	text := o.text.String()
	r := Result{Text: text, ExitCode: code, Image: o.image, IsError: isToolError(tool, code),
		FirstLine: o.firstLine, More: o.more}
	if len(text) > MaxToolOutput {
		r.Text, r.Truncated = text[:MaxToolOutput], true
	}
	return r
}

// partial is the Result of a call that failed: the output so far.
func (o *output) partial() Result {
	r := o.result("", -1)
	r.Image = nil
	return r
}

// streamCap caps one output stream of an exec at MaxToolOutput bytes: past
// it, the stream's chunks are dropped and the marker is emitted once.
type streamCap struct {
	n   int
	cut bool
}

// take returns what of a chunk of n bytes to emit: the chunk, the marker,
// or nothing.
func (c *streamCap) take(chunk []byte) (string, bool) {
	c.n += len(chunk)
	if c.n <= MaxToolOutput {
		return string(chunk), true
	}
	if !c.cut {
		c.cut = true
		return streamMarker, true
	}
	return "", false
}
