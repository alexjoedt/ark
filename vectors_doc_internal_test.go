package ark

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const vectorsPage = "docs/format/vectors.md"

// pageVector is the text block under one heading of the vectors page. A field
// the test never reads fails it, so no field goes unused; reading a field does
// not compare it with ark, the test does that with equal and equalText.
type pageVector struct {
	t       *testing.T
	heading string
	fields  map[string]string
	read    map[string]bool
}

// pageVectors reads every text block of the vectors page by the heading it
// stands under.
func pageVectors(t *testing.T) map[string]*pageVector {
	t.Helper()
	data, err := os.ReadFile(vectorsPage)
	if err != nil {
		t.Fatalf("read %s: %v", vectorsPage, err)
	}
	vs := map[string]*pageVector{}
	var heading, last string
	var cur *pageVector
	var fenced bool
	for line := range strings.SplitSeq(string(data), "\n") {
		switch {
		case fenced:
			fenced = !strings.HasPrefix(line, "```")
		case cur == nil && strings.HasPrefix(line, "#"):
			heading = strings.TrimSpace(strings.TrimLeft(line, "#"))
		case cur == nil && line == "```text":
			if vs[heading] != nil {
				t.Fatalf("%s: second text block under %q", vectorsPage, heading)
			}
			cur = &pageVector{t: t, heading: heading, fields: map[string]string{}, read: map[string]bool{}}
			vs[heading], last = cur, ""
		case cur == nil:
			fenced = strings.HasPrefix(line, "```")
		case line == "```":
			cur = nil
		case strings.HasPrefix(line, " "):
			if last == "" {
				t.Fatalf("%s: %q: indented line %q has no field", vectorsPage, heading, line)
			}
			cur.fields[last] += strings.TrimSpace(line)
		default:
			key, value, ok := strings.Cut(line, " ")
			if !ok || cur.fields[key] != "" {
				t.Fatalf("%s: %q: line %q is no new field", vectorsPage, heading, line)
			}
			cur.fields[key], last = strings.TrimSpace(value), key
		}
	}
	if cur != nil {
		t.Fatalf("%s: text block under %q is not closed", vectorsPage, heading)
	}
	t.Cleanup(func() {
		if t.Failed() {
			return
		}
		for _, v := range vs {
			for k := range v.fields {
				if !v.read[k] {
					t.Errorf("%s: %q: field %s is not checked", vectorsPage, v.heading, k)
				}
			}
		}
	})
	return vs
}

func (v *pageVector) text(key string) string {
	v.t.Helper()
	s, ok := v.fields[key]
	if !ok {
		v.t.Fatalf("%s: %q has no field %s", vectorsPage, v.heading, key)
	}
	v.read[key] = true
	return s
}

func (v *pageVector) bytes(key string) []byte {
	v.t.Helper()
	b, err := hex.DecodeString(v.text(key))
	if err != nil {
		v.t.Fatalf("%s: %q: field %s: %v", vectorsPage, v.heading, key, err)
	}
	return b
}

func (v *pageVector) number(key string) int {
	v.t.Helper()
	n, err := strconv.Atoi(v.text(key))
	if err != nil {
		v.t.Fatalf("%s: %q: field %s: %v", vectorsPage, v.heading, key, err)
	}
	return n
}

func (v *pageVector) format() byte {
	v.t.Helper()
	b := v.bytes("format")
	if len(b) != 1 {
		v.t.Fatalf("%s: %q: format is %d bytes", vectorsPage, v.heading, len(b))
	}
	return b[0]
}

// seal seals payload under format and the vector's nonce and aad.
func (v *pageVector) seal(key *[32]byte, format byte, payload []byte) []byte {
	v.t.Helper()
	nonce := v.bytes("nonce")
	if len(nonce) != 24 {
		v.t.Fatalf("%s: %q: nonce is %d bytes", vectorsPage, v.heading, len(nonce))
	}
	return sealPayload(key, v.text("aad"), slices.Concat([]byte{format}, nonce, payload))
}

func (v *pageVector) equal(what string, got, want []byte) {
	v.t.Helper()
	if !bytes.Equal(got, want) {
		v.t.Errorf("%q: %s:\n got %x\nwant %x", v.heading, what, got, want)
	}
}

func (v *pageVector) equalText(what, got, want string) {
	v.t.Helper()
	if got != want {
		v.t.Errorf("%q: %s: got %s, want %s", v.heading, what, got, want)
	}
}

// TestFormatVectorsPageMatchesArk recomputes every vector of the vectors page
// with the code ark reads and writes a repository with.
func TestFormatVectorsPageMatchesArk(t *testing.T) {
	vs := pageVectors(t)
	vector := func(heading string) *pageVector {
		t.Helper()
		v, ok := vs[heading]
		if !ok {
			t.Fatalf("%s has no vector %q", vectorsPage, heading)
		}
		return v
	}
	codec := newObjectCodec()
	defer codec.Close()

	keys := vector("Keys")
	mk := &masterKey{DataKey: secretKey(keys.bytes("data_key")), IDKey: secretKey(keys.bytes("id_key"))}
	dataKey, idKey := (*[32]byte)(&mk.DataKey), mk.IDKey[:]

	{
		v := vector("Wrap key")
		p := kdfParams{
			Time:    uint32(v.number("time")),
			Memory:  uint32(v.number("memory")),
			Threads: uint8(v.number("threads")),
		}
		got := p.derive(string(v.bytes("password")), v.bytes("salt"))
		v.equal("wrap key", got[:], v.bytes("wrap_key"))
	}

	for _, c := range []struct {
		heading string
		format  byte
		payload string
	}{
		{"Raw", formatRaw, "content"},
		{"Zstd", formatZstd, "payload"},
	} {
		v := vector(c.heading)
		if f := v.format(); f != c.format {
			t.Errorf("%s: format byte %#02x, want %#02x", c.heading, f, c.format)
			continue
		}
		content, sealed := v.bytes("content"), v.bytes("sealed")
		payload := v.bytes(c.payload)
		if c.format == formatZstd {
			v.equal("ark's frame", codec.enc.EncodeAll(content, nil), payload)
		}
		v.equal("sealed", v.seal(dataKey, c.format, payload), sealed)
		opened, err := codec.open(dataKey, v.text("aad"), sealed)
		if err != nil {
			t.Errorf("%s: open: %v", c.heading, err)
			continue
		}
		v.equal("opened", opened, content)
		if f := codec.seal(dataKey, v.text("aad"), content)[0]; f != c.format {
			t.Errorf("ark seals the content under %#02x, the vector shows %#02x", f, c.format)
		}
	}

	{
		v := vector("Names")
		v.equalText("chunk name", keyedName(idKey, v.bytes("chunk")), v.text("chunk_name"))
		node := v.bytes("node")
		if _, err := decodeTreeNode(node); err != nil {
			t.Errorf("node does not decode: %v", err)
		}
		v.equalText("node name", keyedName(idKey, node), v.text("node_name"))
		v.equal("marker input", v.bytes("marker"), []byte(LostPackFile.String()))
		r := &Repository{key: mk}
		v.equalText("marker name", r.lostPacksMarker().Name, v.text("marker_name"))
	}

	{
		space := newPackSpace(codec, mk, BlobFile)
		var section []byte
		var entries []packEntry
		for _, heading := range []string{"First blob", "Second blob"} {
			v := vector(heading)
			content, name, sealed := v.bytes("content"), v.text("name"), v.bytes("sealed")
			v.equalText("name", keyedName(idKey, content), name)
			v.equalText("aad", v.text("aad"), objectAAD(Handle{Type: space.blobs, Name: name}))
			v.equal("sealed", v.seal(dataKey, formatRaw, content), sealed)
			if f := codec.seal(dataKey, v.text("aad"), content)[0]; f != formatRaw {
				t.Errorf("%s: ark seals the content under %#02x, the vector shows raw", heading, f)
			}
			n, ok := parseName(name)
			if !ok {
				t.Fatalf("%s: %q is no blob name", heading, name)
			}
			entries = append(entries, packEntry{name: n, length: uint32(len(sealed))})
			section = append(section, sealed...)
		}

		table := vector("Table")
		tail, packName := space.finish(entries, section)
		if sum := sha256.Sum256(slices.Concat(section, tail)); packName != hex.EncodeToString(sum[:]) {
			t.Errorf("ark names a pack %s, not the SHA-256 of its bytes", packName)
		}
		content, err := codec.open(dataKey, space.tableAAD(), tail[:len(tail)-packTrailerSize])
		if err != nil {
			t.Fatalf("open the table ark wrote: %v", err)
		}
		table.equal("content", content, table.bytes("content"))
		table.equalText("aad", table.text("aad"), space.tableAAD())
		sealedTable := table.bytes("sealed")
		table.equal("sealed", table.seal(dataKey, formatRaw, content), sealedTable)

		whole := vector("Whole pack")
		pack := whole.bytes("pack")
		want := binary.LittleEndian.AppendUint32(slices.Concat(section, sealedTable), uint32(len(sealedTable)))
		whole.equal("pack", pack, want)
		sum := sha256.Sum256(pack)
		whole.equalText("name", hex.EncodeToString(sum[:]), whole.text("name"))
		parsed, err := space.parse(pack)
		if err != nil {
			t.Fatalf("parse the pack: %v", err)
		}
		if !slices.Equal(parsed, entries) {
			t.Errorf("pack table %v, want %v", parsed, entries)
		}

		index := vector("Index object")
		it, err := newIndexTable([]indexedPack{{name: whole.text("name"), entries: entries}})
		if err != nil {
			t.Fatal(err)
		}
		content = it.encode()
		index.equal("content", content, index.bytes("content"))
		name := keyedName(idKey, content)
		index.equalText("name", name, index.text("name"))
		index.equalText("aad", index.text("aad"), objectAAD(Handle{Type: space.index, Name: name}))
		sealed := index.bytes("sealed")
		index.equal("sealed", index.seal(dataKey, formatRaw, content), sealed)
		opened, err := codec.open(dataKey, index.text("aad"), sealed)
		if err != nil {
			t.Fatalf("open the index object: %v", err)
		}
		var back indexTable
		if err := back.parse(opened); err != nil {
			t.Fatalf("parse the index object: %v", err)
		}
		back.sort()
		if !slices.Equal(allRecords(&back), allRecords(it)) {
			t.Errorf("index records %v, want %v", allRecords(&back), allRecords(it))
		}
	}
}
