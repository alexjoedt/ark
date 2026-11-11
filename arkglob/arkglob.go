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
// Differences from restic: a pattern starting with "!" is rejected instead of
// re-including what an earlier pattern excluded (write "\!" or "[!]" for a
// name that starts with "!"); a pattern with a "." or ".." component is
// rejected instead of cleaned, so "./foo" is an error, not an unanchored
// "foo"; a leading "/" anchors at the root of the matched tree, such as the
// source directory of a backup, not at the root of the filesystem; and the
// root itself never matches, so "**" excludes everything below the root but
// not the root.
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
// matches nothing. An empty pattern, a pattern starting with "!", a pattern
// with a "." or ".." component and a pattern with bad syntax return an error that wraps [path.ErrBadPattern] and names
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
	var parts []string
	for part := range strings.SplitSeq(p, "/") {
		switch {
		case part == "":
			continue
		case part == "." || part == "..":
			return nil, fmt.Errorf("%q component: %w", part, path.ErrBadPattern)
		case part == "**" && len(parts) > 0 && parts[len(parts)-1] == "**":
			continue
		}
		if _, err := path.Match(part, ""); err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("no path component: %w", path.ErrBadPattern)
	}
	if !strings.HasPrefix(p, "/") && parts[0] != "**" {
		parts = append([]string{"**"}, parts...)
	}
	return parts, nil
}

// Match reports whether p, a slash-separated path relative to the root of the
// matched tree, or a path below it matches any pattern. The root itself ("",
// ".", "/") never matches.
func (m *Matcher) Match(p string) bool {
	clean := strings.TrimPrefix(path.Clean("/"+p), "/")
	if clean == "" {
		return false
	}
	comps := strings.Split(clean, "/")
	buf := make([]bool, 2*(len(comps)+1))
	at, next := buf[:len(comps)+1], buf[len(comps)+1:]
	for _, pat := range m.patterns {
		if matchPrefix(pat, comps, at, next) {
			return true
		}
	}
	return false
}

// matchPrefix reports whether pat matches a leading run of comps. It tracks
// the set of component positions reachable after each pattern part, so it
// runs in O(len(pat) * len(comps)) without backtracking. at and next are
// scratch space of len(comps)+1 each.
func matchPrefix(pat, comps []string, at, next []bool) bool {
	clear(at)
	at[0] = true
	for _, part := range pat {
		clear(next)
		reached := false
		for i, ok := range at {
			if !ok {
				continue
			}
			if part == "**" {
				for j := i; j <= len(comps); j++ {
					next[j] = true
				}
				reached = true
				break
			}
			// compile validated part, so path.Match cannot return an error here.
			if i < len(comps) {
				if hit, _ := path.Match(part, comps[i]); hit {
					next[i+1] = true
					reached = true
				}
			}
		}
		if !reached {
			return false
		}
		at, next = next, at
	}
	return true
}
