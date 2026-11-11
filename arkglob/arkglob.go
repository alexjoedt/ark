// Package arkglob matches slash-separated relative paths against exclude
// patterns with restic's rules.
//
// A pattern and a path are split on "/" and compared component by component
// with [path.Match], so matching is case-sensitive and byte-wise, "\" escapes
// the next character and "*" matches a leading dot:
//
//   - "*", "?" and "[...]" stay within one component: "b/*/e.log" does not
//     match "b/c/d/e.log".
//   - A component "**" matches zero or more components: "b/**/*.log" matches
//     "b/c/f.log" and "b/c/d/e.log", "a/**" matches "a".
//   - A pattern without a leading "/" matches any consecutive run of
//     components: "foo/bar" matches "a/foo/bar".
//   - A leading "/" anchors the pattern at the root: "/foo" matches "foo",
//     not "a/foo".
//   - A trailing "/" is ignored.
//   - A path below a matched path matches too: "foo" matches
//     "a/foo/bar/x.txt", so one call answers for a file inside an excluded
//     directory.
//
// Two differences from restic: a pattern starting with "!" is rejected
// instead of re-including what an earlier pattern excluded (write "\!" or
// "[!]" for a name that starts with "!"), and a leading "/" anchors at the
// root of the matched tree, such as the source directory of a backup, not at
// the root of the filesystem.
package arkglob

import (
	"fmt"
	"path"
	"strings"
)

// Matcher reports whether a path matches any of its patterns. It is safe for
// concurrent use.
type Matcher struct {
	patterns [][]string
}

// New compiles patterns into a Matcher. A Matcher built from no patterns
// matches nothing. An empty pattern, a pattern starting with "!" and a pattern
// with bad syntax return an error that wraps [path.ErrBadPattern] and names
// the pattern.
func New(patterns ...string) (*Matcher, error) {
	m := &Matcher{}
	for _, p := range patterns {
		parts, err := compile(p)
		if err != nil {
			return nil, fmt.Errorf("arkglob: pattern %q: %w", p, err)
		}
		m.patterns = append(m.patterns, parts)
	}
	return m, nil
}

func compile(p string) ([]string, error) {
	if strings.HasPrefix(p, "!") {
		return nil, fmt.Errorf("negation is not supported: %w", path.ErrBadPattern)
	}
	anchored := strings.HasPrefix(p, "/")
	clean := strings.TrimPrefix(path.Clean("/"+p), "/")
	if clean == "" {
		return nil, fmt.Errorf("no path component: %w", path.ErrBadPattern)
	}
	parts := strings.Split(clean, "/")
	for _, part := range parts {
		if _, err := path.Match(part, ""); err != nil {
			return nil, err
		}
	}
	if !anchored {
		parts = append([]string{"**"}, parts...)
	}
	return parts, nil
}

// Match reports whether p, a slash-separated path relative to the root of the
// matched tree, or a path below it matches any pattern.
func (m *Matcher) Match(p string) bool {
	var comps []string
	if clean := strings.TrimPrefix(path.Clean("/"+p), "/"); clean != "" {
		comps = strings.Split(clean, "/")
	}
	for _, pat := range m.patterns {
		if matchPrefix(pat, comps) {
			return true
		}
	}
	return false
}

// matchPrefix reports whether pat matches a leading run of comps.
// ponytail: backtracks on "**", exponential in the number of "**" per pattern;
// fine for hand-written excludes, add memoisation if patterns get generated.
func matchPrefix(pat, comps []string) bool {
	if len(pat) == 0 {
		return true
	}
	if pat[0] == "**" {
		for i := range len(comps) + 1 {
			if matchPrefix(pat[1:], comps[i:]) {
				return true
			}
		}
		return false
	}
	if len(comps) == 0 {
		return false
	}
	ok, _ := path.Match(pat[0], comps[0])
	return ok && matchPrefix(pat[1:], comps[1:])
}
