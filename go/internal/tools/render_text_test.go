// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// oldRender is mcpsrv's render before its text moved here (RenderText),
// reduced to its text content and isError: the reference RenderText must
// equal byte for byte.
func oldRender(res Result, err error) (string, bool) {
	text := res.Text
	if res.FirstLine > 0 {
		text = numbered(text, res.FirstLine)
	}
	if res.Truncated {
		text += TruncatedMarker
	}
	if res.FirstLine > 0 && (res.More || res.Truncated) {
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

// RenderText renders as MCP's render did: the cases of its tests with
// their old texts, and every combination of the fields it reads compared
// with the old function.
func TestRenderText(t *testing.T) {
	for _, tc := range []struct {
		res     Result
		err     error
		text    string
		isError bool
	}{
		{Result{Text: "x", ExitCode: 0}, nil, "x", false},
		{Result{Text: "", ExitCode: 0}, nil, "(no output)", false},
		{Result{Text: " \n", ExitCode: 2, IsError: true}, nil, "(no output)\n(exit code 2)", true},
		{Result{Text: "abc", Truncated: true}, nil, "abc\n... [truncated]\n", false},
		{Result{Text: "abc", Truncated: true}, fmt.Errorf("boom"), "abc\n... [truncated]\n\nerror: boom", true},
		{Result{Text: "partial"}, fmt.Errorf("node n1: gone"), "partial\nerror: node n1: gone", true},
		{Result{}, fmt.Errorf("x"), "error: x", true},
		// A read of text: numbered as cat -n numbers, and where to go on.
		{Result{Text: "a\n\tb\n", FirstLine: 1}, nil, "     1\ta\n     2\t\tb\n", false},
		{Result{Text: "no newline", FirstLine: 1}, nil, "     1\tno newline", false},
		{Result{Text: "x\ny\n", FirstLine: 7, More: true}, nil,
			"     7\tx\n     8\ty\n... [the file goes on: read with offset=9 for more]\n", false},
		{Result{Text: "ab\ncd", FirstLine: 1, Truncated: true}, nil,
			"     1\tab\n     2\tcd\n... [truncated]\n... [the file goes on: read with offset=2 for more]\n", false},
		// MCP's end-to-end cases: a failure before the guest, and a timeout
		// with the output so far.
		{Result{}, errors.New("ensure sandbox: sandbox lost"), "error: ensure sandbox: sandbox lost", true},
		{Result{Text: "begun\n"}, &TimeoutError{Seconds: 1}, "begun\n\nerror: timeout after 1s", true},
		// The picture is not part of the text; a failed call is isError
		// whatever the result says.
		{Result{Text: "/s.png: image/png, 3x4 px, 1 KiB\n", Image: &Image{MediaType: "image/png", Data: []byte("p")}}, nil,
			"/s.png: image/png, 3x4 px, 1 KiB\n", false},
		{Result{Text: "out", ExitCode: 3}, errors.New("e"), "out\nerror: e", true},
	} {
		text, isError := RenderText(tc.res, tc.err)
		if text != tc.text || isError != tc.isError {
			t.Errorf("RenderText %+v %v: %q %v, want %q %v", tc.res, tc.err, text, isError, tc.text, tc.isError)
		}
		if old, oldIsError := oldRender(tc.res, tc.err); text != old || isError != oldIsError {
			t.Errorf("RenderText %+v %v: %q %v, the old render %q %v", tc.res, tc.err, text, isError, old, oldIsError)
		}
	}

	n := 0
	for _, text := range []string{"", " ", "\n", "a", "a\n", "a\nb", "a\nb\n", "\t\n \n", "é\x00\xff\n"} {
		for _, first := range []int{0, 1, 42} {
			for _, flags := range []int{0, 1, 2, 3, 4, 5, 6, 7} {
				for _, code := range []int{0, 1, -1, 137} {
					for _, err := range []error{nil, errors.New("boom"), errors.New("")} {
						res := Result{Text: text, FirstLine: first, ExitCode: code,
							Truncated: flags&1 != 0, More: flags&2 != 0, IsError: flags&4 != 0}
						text, isError := RenderText(res, err)
						if old, oldIsError := oldRender(res, err); text != old || isError != oldIsError {
							t.Errorf("RenderText %+v %v: %q %v, the old render %q %v", res, err, text, isError, old, oldIsError)
						}
						n++
					}
				}
			}
		}
	}
	if n != 9*3*8*4*3 {
		t.Fatalf("%d combinations", n)
	}
}
