package out

import "testing"

func TestEscapeName(t *testing.T) {
	for in, want := range map[string]string{
		"plain.txt": "plain.txt",
		"dir/ü 日本":  "dir/ü 日本",
		`a\x41`:     `a\x41`,
		"a\xffb":    `a\xffb`,
		"a\x1b[2Jb": `a\x1b[2Jb`,
		"a\nb\\":    `a\x0ab\\`,
		"\u0085":    `\xc2\x85`,
		"x\x7f":     `x\x7f`,
		"\xe6\x97":  `\xe6\x97`,
	} {
		if got := EscapeName(in); got != want {
			t.Errorf("EscapeName(%q) = %q, want %q", in, got, want)
		}
	}
	if RawName("plain") != "" || RawName("a\xff") != "Yf8=" {
		t.Errorf("RawName: %q %q", RawName("plain"), RawName("a\xff"))
	}
	p := &Printer{ttyOut: true}
	if got := p.Name("a\xff"); got != `a\xff` {
		t.Errorf("terminal Name = %q", got)
	}
	p.ttyOut = false
	if got := p.Name("a\xff"); got != "a\xff" {
		t.Errorf("pipe Name = %q", got)
	}
}
