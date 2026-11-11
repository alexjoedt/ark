package arkglob_test

import (
	"errors"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark/arkglob"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"b/*/e.log", "b/c/d/e.log", false},
		{"b/*/e.log", "b/c/e.log", true},
		{"?.log", "b/c/f.log", true},
		{"?.log", "b/c/ff.log", false},
		{"*", ".hidden", true},
		{"b/**/*.log", "b/c/f.log", true},
		{"b/**/*.log", "b/c/d/e.log", true},
		{"b/**/*.log", "b/f.log", true},
		{"a/**", "a", true},
		{"**/bar", "a/foo/bar", true},
		{"**", "a/b", true},
		{"/foo", "foo", true},
		{"/foo", "a/foo", false},
		{"/a/foo", "a/foo", true},
		{"foo", "a/foo/bar", true},
		{"foo/bar", "a/foo/bar", true},
		{"foo/bar", "a/foo/baz/bar", false},
		{"foo/", "a/foo", true},
		{"/foo/", "foo", true},
		{"foo", "a/foo/bar/x.txt", true},
		{"/foo", "foo/z.txt", true},
		{"foo", "a/foobar", false},
		{"Foo", "foo", false},
		{`\!x`, "!x", true},
		{`[!]x`, "!x", true},
		{`[!]x`, "yx", false},
		{`\*`, "a", false},
		{`\*`, "*", true},
		{"**", "", false},
		{"/**", ".", false},
		{"**/", "/", false},
		{"**/x", "", false},
		{"a/**/**/b", "a/b", true},
		{"**/**/x", "a/b/x/c", true},
	}
	for _, tt := range tests {
		m, err := arkglob.New(tt.pattern)
		if err != nil {
			t.Fatalf("New(%q): %v", tt.pattern, err)
		}
		if got := m.Match(tt.path); got != tt.want {
			t.Errorf("pattern %q, path %q: Match = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestMatchAnyPattern(t *testing.T) {
	m, err := arkglob.New("*.tmp", "/cache")
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{"a/x.tmp": true, "cache/y": true, "a/cache": false, "x.txt": false} {
		if got := m.Match(p); got != want {
			t.Errorf("Match(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestNoPatternsMatchNothing(t *testing.T) {
	m, err := arkglob.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "a", "a/b"} {
		if m.Match(p) {
			t.Errorf("Match(%q) = true for no patterns", p)
		}
	}
}

func TestNewRejectsBadPatterns(t *testing.T) {
	for _, p := range []string{"", "/", "//", "[", "a/[/b", "!x", "!", "./foo", "a/../b", "../secret", "a/.", ".."} {
		_, err := arkglob.New("ok", p)
		if !errors.Is(err, path.ErrBadPattern) {
			t.Errorf("New(%q): err = %v, want path.ErrBadPattern", p, err)
			continue
		}
		if !strings.Contains(err.Error(), `"`+p+`"`) {
			t.Errorf("New(%q): error %q does not name the pattern", p, err)
		}
	}
}

func TestMatchManyDoubleStarsIsFast(t *testing.T) {
	m, err := arkglob.New("**/a/**/a/**/a/**/a/**/a/**/a/**/x")
	if err != nil {
		t.Fatal(err)
	}
	deep := strings.Repeat("a/", 200) + "y"
	start := time.Now()
	if m.Match(deep) {
		t.Fatal("Match = true, want false")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Match took %v on a 201-component path", d)
	}
}
