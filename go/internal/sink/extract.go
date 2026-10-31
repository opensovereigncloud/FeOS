// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"encoding/json"
	"strconv"
	"strings"
)

// A line's level, as a bit: a hot line keeps its own (log_lines.level),
// a document of the index too (its field level).
const (
	LevelError   = 1
	LevelWarn    = 2
	LevelInfo    = 4
	LevelDebug   = 8
	LevelUnknown = 16
	// LevelAll is every bit.
	LevelAll = LevelError | LevelWarn | LevelInfo | LevelDebug | LevelUnknown
)

// LevelName is the API's name of a level bit: "" for unknown.
func LevelName(bit int) string {
	switch bit {
	case LevelError:
		return "error"
	case LevelWarn:
		return "warn"
	case LevelInfo:
		return "info"
	case LevelDebug:
		return "debug"
	}
	return ""
}

// levelBit parses a level filter's name: error, warn, info, debug or
// unknown; 0 for anything else.
func levelBit(name string) int {
	switch name {
	case "error":
		return LevelError
	case "warn":
		return LevelWarn
	case "info":
		return LevelInfo
	case "debug":
		return LevelDebug
	case "unknown":
		return LevelUnknown
	}
	return 0
}

// levelOfWord maps a level's word, case insensitive, to its bit; 0 when it
// is none.
func levelOfWord(w string) int {
	switch strings.ToLower(w) {
	case "error", "fatal", "panic", "critical":
		return LevelError
	case "warn", "warning":
		return LevelWarn
	case "info":
		return LevelInfo
	case "debug", "trace":
		return LevelDebug
	}
	return 0
}

// jsonFields are the fields of a JSON line the sink reads (sink.md, §6).
type jsonFields struct {
	Level       json.RawMessage `json:"level"`
	Lvl         json.RawMessage `json:"lvl"`
	Severity    json.RawMessage `json:"severity"`
	TraceID     *string         `json:"trace_id"`
	TraceIDCC   *string         `json:"traceId"`
	Traceparent *string         `json:"traceparent"`
}

// Extract reads a line's level (a bit, LevelUnknown when it names none)
// and its W3C trace ID ("" when none). A JSON object gives its level,
// lvl or severity, as a word or as a number of pino's scale (10, 20 debug,
// 30 info, 40 warn, 50 and above error), and its trace_id, traceId or
// traceparent's second field. Any other line gives a level when it starts
// with one: bare, in brackets or as level=…, after a leading time= or ts=
// field (logfmt, as Go's slog writes it).
func Extract(text string) (level int, trace string) {
	s := strings.TrimLeft(text, " \t")
	if strings.HasPrefix(s, "{") {
		var f jsonFields
		if json.Unmarshal([]byte(s), &f) == nil {
			level = LevelUnknown
			for _, raw := range []json.RawMessage{f.Level, f.Lvl, f.Severity} {
				if b := jsonLevel(raw); b != 0 {
					level = b
					break
				}
			}
			switch {
			case f.TraceID != nil && isTraceID(*f.TraceID):
				trace = *f.TraceID
			case f.TraceIDCC != nil && isTraceID(*f.TraceIDCC):
				trace = *f.TraceIDCC
			case f.Traceparent != nil:
				if p := strings.Split(*f.Traceparent, "-"); len(p) == 4 && isTraceID(p[1]) {
					trace = p[1]
				}
			}
			return level, trace
		}
	}
	if b := textLevel(s); b != 0 {
		return b, ""
	}
	return LevelUnknown, ""
}

// jsonLevel is the bit of a JSON level value, a string or a number.
func jsonLevel(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var w string
	if json.Unmarshal(raw, &w) == nil {
		return levelOfWord(w)
	}
	n, err := strconv.ParseFloat(string(raw), 64)
	if err != nil {
		return 0
	}
	switch {
	case n >= 50:
		return LevelError
	case n >= 40:
		return LevelWarn
	case n >= 30:
		return LevelInfo
	case n >= 10:
		return LevelDebug
	}
	return 0
}

// textLevel is the level a line of text starts with: ERROR, [ERROR] or
// level=error (quoted or not), the word ending the line or followed by
// something other than a letter; a leading time= or ts= field is skipped.
func textLevel(s string) int {
	for _, k := range []string{"time=", "ts="} {
		if strings.HasPrefix(s, k) {
			_, rest, ok := strings.Cut(s, " ")
			if !ok {
				return 0
			}
			s = strings.TrimLeft(rest, " ")
			break
		}
	}
	if len(s) >= 6 && strings.EqualFold(s[:6], "level=") {
		s = strings.TrimPrefix(s[6:], `"`)
	} else if strings.HasPrefix(s, "[") {
		s = s[1:]
	}
	end := 0
	for end < len(s) && end < 10 && isLetter(s[end]) {
		end++
	}
	if end == 0 || (end < len(s) && isLetter(s[end])) {
		return 0
	}
	return levelOfWord(s[:end])
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// isTraceID reports whether s is a W3C trace ID: 32 lowercase hex digits,
// not all zero.
func isTraceID(s string) bool {
	if len(s) != 32 {
		return false
	}
	zero := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
		if c != '0' {
			zero = false
		}
	}
	return !zero
}
