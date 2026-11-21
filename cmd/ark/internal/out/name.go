package out

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// OddName reports whether a file name is not printable valid UTF-8: it holds
// bytes that are not UTF-8 or a control character.
func OddName(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	return strings.ContainsFunc(s, unicode.IsControl)
}

// EscapeName returns s with every byte of a control character or of invalid
// UTF-8 written as \xNN and every backslash doubled. A name that is not odd
// comes back unchanged.
func EscapeName(s string) string {
	if !OddName(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == utf8.RuneError && n <= 1, unicode.IsControl(r):
			for _, c := range []byte(s[i : i+n]) {
				fmt.Fprintf(&b, `\x%02x`, c)
			}
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// RawName is the base64 of an odd name for the *_raw JSON fields, empty for
// any other name.
func RawName(s string) string {
	if !OddName(s) {
		return ""
	}
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// Name is s as the human output shows it: escaped on a terminal, the raw
// bytes in a pipe or file.
func (p *Printer) Name(s string) string {
	if p.ttyOut {
		return EscapeName(s)
	}
	return s
}
