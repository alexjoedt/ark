package ark

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"
)

// DebugNode returns the stored tree node of the directory dir ("." for the
// snapshot root) decoded as indented JSON, for people who inspect a
// repository. A name, link target or xattr name that is not valid UTF-8 keeps
// its bytes in a sibling *_raw field as base64. The shape may change in any
// release.
func (s *SnapshotFS) DebugNode(dir string) ([]byte, error) {
	e, err := s.resolve("debug node", dir)
	if err != nil {
		return nil, err
	}
	if e.te.Type != FileTypeDirectory {
		return nil, &fs.PathError{Op: "debug node", Path: dir, Err: fmt.Errorf("not a directory: %w", fs.ErrInvalid)}
	}
	if err := s.rd.enter(); err != nil {
		return nil, &fs.PathError{Op: "debug node", Path: dir, Err: err}
	}
	defer s.rd.leave()
	node, err := s.node(e.te.TreeHash)
	if err != nil {
		return nil, &fs.PathError{Op: "debug node", Path: dir, Err: err}
	}
	entries := make([]debugEntry, 0, len(node.Entries))
	for _, te := range node.Entries {
		entries = append(entries, newDebugEntry(te))
	}
	return json.MarshalIndent(struct {
		Entries []debugEntry `json:"entries"`
	}{entries}, "", "  ")
}

type debugEntry struct {
	Name      string       `json:"name"`
	NameRaw   string       `json:"name_raw,omitempty"`
	Type      string       `json:"type"`
	Mode      string       `json:"mode"`
	UID       int          `json:"uid"`
	GID       int          `json:"gid"`
	MtimeNs   int64        `json:"mtime_ns"`
	Mtime     string       `json:"mtime"`
	CtimeNs   *int64       `json:"ctime_ns,omitempty"`
	Inode     uint64       `json:"inode,omitempty"`
	Chunks    []debugChunk `json:"chunks,omitempty"`
	Tree      string       `json:"tree,omitempty"`
	Target    string       `json:"target,omitempty"`
	TargetRaw string       `json:"target_raw,omitempty"`
	DevMajor  *uint32      `json:"dev_major,omitempty"`
	DevMinor  *uint32      `json:"dev_minor,omitempty"`
	Xattrs    []debugXattr `json:"xattrs,omitempty"`
}

type debugChunk struct {
	ID     string `json:"id"`
	Length int64  `json:"length"`
}

type debugXattr struct {
	Name    string `json:"name"`
	NameRaw string `json:"name_raw,omitempty"`
	Value   string `json:"value"`
}

func newDebugEntry(te treeEntry) debugEntry {
	d := debugEntry{
		Name: te.Name, NameRaw: rawIfInvalid(te.Name), Type: string(te.Type),
		Mode: "0" + strconv.FormatUint(uint64(te.Mode), 8), UID: te.UID, GID: te.GID,
		MtimeNs: te.ModTime.UnixNano(), Mtime: te.ModTime.UTC().Format(time.RFC3339Nano), Inode: te.Inode,
		Tree: te.TreeHash, Target: te.LinkDest, TargetRaw: rawIfInvalid(te.LinkDest),
	}
	if !te.ChangeTime.IsZero() {
		ns := te.ChangeTime.UnixNano()
		d.CtimeNs = &ns
	}
	for _, c := range te.Chunks {
		d.Chunks = append(d.Chunks, debugChunk{ID: c.ID, Length: c.Length})
	}
	if te.Type == FileTypeCharDevice || te.Type == FileTypeBlockDevice {
		d.DevMajor, d.DevMinor = &te.DevMajor, &te.DevMinor
	}
	names := make([]string, 0, len(te.Xattrs))
	for n := range te.Xattrs {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		d.Xattrs = append(d.Xattrs, debugXattr{
			Name: n, NameRaw: rawIfInvalid(n), Value: base64.StdEncoding.EncodeToString(te.Xattrs[n]),
		})
	}
	return d
}

func rawIfInvalid(s string) string {
	if utf8.ValidString(s) {
		return ""
	}
	return base64.StdEncoding.EncodeToString([]byte(s))
}
