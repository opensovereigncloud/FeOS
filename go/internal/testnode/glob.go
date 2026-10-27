// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"errors"
	"regexp"
	"strings"
)

// globRegexp translates a glob into an anchored regular expression with
// the semantics the guest's tools use (globset with a literal separator):
// "*" and "?" never match "/", "**" as a whole component matches any
// number of components, "[...]" and "[!...]" are classes, "{a,b}" are
// alternatives and "\" escapes.
func globRegexp(glob string, caseInsensitive bool) (*regexp.Regexp, error) {
	var b strings.Builder
	if caseInsensitive {
		b.WriteString("(?i)")
	}
	b.WriteString("^")
	inAlt := false
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				start := i == 0 || glob[i-1] == '/'
				j := i + 2
				end := j == len(glob) || glob[j] == '/'
				if start && end {
					switch {
					case j == len(glob):
						b.WriteString(".*")
					default:
						b.WriteString("(?:.*/)?")
						j++ // the "/" after "**"
					}
					i = j - 1
					continue
				}
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '[':
			j := i + 1
			if j < len(glob) && (glob[j] == '!' || glob[j] == '^') {
				j++
			}
			if j < len(glob) && glob[j] == ']' {
				j++
			}
			for j < len(glob) && glob[j] != ']' {
				j++
			}
			if j >= len(glob) {
				return nil, errors.New("unclosed character class; missing ']'")
			}
			class := glob[i+1 : j]
			b.WriteString("[")
			if strings.HasPrefix(class, "!") || strings.HasPrefix(class, "^") {
				b.WriteString("^")
				class = class[1:]
			}
			for _, r := range class {
				if r == '\\' || r == '[' || r == ']' || r == '^' {
					b.WriteByte('\\')
				}
				b.WriteRune(r)
			}
			b.WriteString("]")
			i = j
		case '{':
			if inAlt {
				return nil, errors.New("nested alternate groups are not allowed")
			}
			inAlt = true
			b.WriteString("(?:")
		case '}':
			if !inAlt {
				b.WriteString(regexp.QuoteMeta("}"))
				continue
			}
			inAlt = false
			b.WriteString(")")
		case ',':
			if inAlt {
				b.WriteString("|")
			} else {
				b.WriteString(",")
			}
		case '\\':
			if i+1 >= len(glob) {
				return nil, errors.New("dangling '\\'")
			}
			i++
			b.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		default:
			b.WriteString(regexp.QuoteMeta(glob[i : i+1]))
		}
	}
	if inAlt {
		return nil, errors.New("unclosed alternate group; missing '}'")
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
