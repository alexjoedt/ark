package out

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// unsafeRune reports whether r can change how a terminal shows a name:
// a control or format character (bidi overrides, zero-width characters) or
// a line or paragraph separator.
func unsafeRune(r rune) bool {
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)
}

// OddName reports whether a file name is not printable valid UTF-8: it holds
// bytes that are not UTF-8 or a character that unsafeRune rejects.
func OddName(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	return strings.ContainsFunc(s, unsafeRune)
}

// EscapeName returns s with every byte of an unsafe character or of invalid
// UTF-8 written as \xNN and every backslash doubled, so two different names
// never escape to the same text.
func EscapeName(s string) string {
	if !OddName(s) && !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == utf8.RuneError && n <= 1, unsafeRune(r):
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

// RawName is the base64 of a name that is not valid UTF-8, for the *_raw
// JSON fields, and empty for any other name: JSON carries a valid name
// without loss, control characters included.
func RawName(s string) string {
	if utf8.ValidString(s) {
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
