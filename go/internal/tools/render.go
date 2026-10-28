// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"fmt"
	"strings"
)

// RenderText is a call's answer as MCP's text renders it: the output (with
// read's line numbers), the truncation marker, the read continuation line,
// then either "\nerror: <err>" (isError) or "(no output)" / "\n(exit code
// N)". The picture is not part of it.
//
// MCP's tool results are this text, and the record of a call with an id
// carries it (REST's GET …/calls/{key}), so that a client that recovers a
// call renders it exactly as the live answer.
func RenderText(res Result, err error) (text string, isError bool) {
	text = res.Text
	if res.FirstLine > 0 {
		text = numbered(text, res.FirstLine)
	}
	if res.Truncated {
		text += TruncatedMarker
	}
	if res.FirstLine > 0 && (res.More || res.Truncated) {
		// The first line not shown whole: a cut may end inside one.
		next := res.FirstLine + strings.Count(res.Text, "\n")
		text += fmt.Sprintf("... [the file goes on: read with offset=%d for more]\n", next)
	}
	if err != nil {
		if text != "" {
			text += "\n"
		}
		return text + "error: " + err.Error(), true
	}
	if strings.TrimSpace(text) == "" {
		text = "(no output)"
	}
	if res.ExitCode != 0 {
		text += fmt.Sprintf("\n(exit code %d)", res.ExitCode)
	}
	return text, res.IsError
}

// numbered puts each line's number in front of it, as cat -n does: right
// aligned in six columns, then a tab. An agent finds its place by them and
// edits by the text after the tab.
func numbered(text string, first int) string {
	var b strings.Builder
	b.Grow(len(text) + 8*(strings.Count(text, "\n")+1))
	for n := first; text != ""; n++ {
		line, rest, nl := strings.Cut(text, "\n")
		fmt.Fprintf(&b, "%6d\t%s", n, line)
		if nl {
			b.WriteByte('\n')
		}
		text = rest
	}
	return b.String()
}
