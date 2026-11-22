package chunker

import (
	"encoding/hex"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/chacha20"
)

type docVector struct {
	seed     [32]byte
	sizes    [3]int
	input    []byte
	lengths  []int
	inputDoc string
}

// docVectors reads the vectors of the section "Chunker test vectors" of
// docs/format.md.
func docVectors(t *testing.T) []docVector {
	t.Helper()
	data, err := os.ReadFile("../docs/format.md")
	if err != nil {
		t.Fatalf("read docs/format.md: %v", err)
	}
	_, section, ok := strings.Cut(string(data), "#### Chunker test vectors\n")
	if !ok {
		t.Fatal("docs/format.md has no section Chunker test vectors")
	}
	if loc := regexp.MustCompile(`(?m)^#{1,4} `).FindStringIndex(section); loc != nil {
		section = section[:loc[0]]
	}

	var vs []docVector
	var last string
	for line := range strings.SplitSeq(section, "\n") {
		key, rest, _ := strings.Cut(line, " ")
		if strings.HasPrefix(line, " ") {
			switch last {
			case "lengths":
				key, rest = last, line
			case "":
			default:
				t.Fatalf("docs/format.md: indented line %q after %s, only lengths may wrap", line, last)
			}
		}
		cur := func() *docVector {
			if len(vs) == 0 {
				t.Fatalf("docs/format.md: chunker test vector line %q comes before its seed", line)
			}
			return &vs[len(vs)-1]
		}
		f := strings.Fields(rest)
		switch key {
		case "seed":
			vs = append(vs, docVector{seed: [32]byte(hexKey(t, rest))})
		case "min", "avg", "max":
			cur().sizes[slices.Index([]string{"min", "avg", "max"}, key)] = atoi(t, rest)
		case "input":
			v := cur()
			v.inputDoc = strings.TrimSpace(rest)
			switch {
			case len(f) == 3 && f[0] == "chacha20":
				v.input = keystream(t, hexKey(t, f[1]), atoi(t, f[2]))
			case len(f) == 2 && f[0] == "zero":
				v.input = make([]byte, atoi(t, f[1]))
			default:
				t.Fatalf("input %q: want chacha20 <key> <n> or zero <n>", rest)
			}
		case "lengths":
			v := cur()
			for _, s := range f {
				v.lengths = append(v.lengths, atoi(t, s))
			}
		default:
			key = ""
		}
		last = key
	}
	return vs
}

func hexKey(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 32 {
		t.Fatalf("key %q: want 64 hex characters", s)
	}
	return b
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func keystream(t *testing.T, key []byte, n int) []byte {
	t.Helper()
	var nonce [chacha20.NonceSize]byte
	c, err := chacha20.NewUnauthenticatedCipher(key, nonce[:])
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, n)
	c.XORKeyStream(b, b)
	return b
}

// TestChunkerMatchesTheFormatPageVectors pins the chunker to the test vectors
// docs/format.md gives for other implementations.
func TestChunkerMatchesTheFormatPageVectors(t *testing.T) {
	vs := docVectors(t)
	if len(vs) < 2 {
		t.Fatalf("docs/format.md gives %d chunker test vectors, want at least 2", len(vs))
	}
	for _, v := range vs {
		if len(v.input) == 0 || len(v.lengths) == 0 || slices.Contains(v.sizes[:], 0) {
			t.Fatalf("docs/format.md has a chunker test vector without sizes, input or lengths: %+v", v)
		}
		cfg := Config{MinSize: v.sizes[0], AvgSize: v.sizes[1], MaxSize: v.sizes[2], Seed: v.seed}
		ends, _ := cuts(t, cfg, v.input)
		var got []int
		var prev uint64
		for _, e := range ends {
			got = append(got, int(e-prev))
			prev = e
		}
		if !slices.Equal(got, v.lengths) {
			t.Errorf("sizes %v, input %s: chunk lengths %v, docs/format.md gives %v",
				v.sizes, v.inputDoc, got, v.lengths)
		}
	}
}
