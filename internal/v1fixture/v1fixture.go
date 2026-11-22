// Package v1fixture is the synthetic source of the stored repository under
// testdata/repo-v1: scripts/v1fixture backed it up once, in format version 1,
// and the tests that read that repository or write it again compare against
// it. Every entry, time, owner and inode is fixed here, so the same input can
// be backed up again on any host and without privileges.
//
// Nothing here may change after the fixture is stored: a change is a new
// fixture, which is allowed only before the format freeze.
package v1fixture

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/chunker"
)

// Dir is where the fixture repository lives, relative to the module root.
const Dir = "testdata/repo-v1"

// Password unlocks the fixture repository. It guards nothing.
const Password = "ark fixture, format version 1" //nolint:gosec // the fixture's password guards nothing

// Host is the host both snapshots are recorded under.
const Host = "fixture-host"

// Chunker gives the fixture small chunks, so a multi-chunk file stays small.
var Chunker = chunker.Config{MinSize: 4 << 10, AvgSize: 8 << 10, MaxSize: 16 << 10}

// Labels are the labels both snapshots carry.
func Labels() map[string]string {
	return map[string]string{"env": "fixture", "example.com/team": "storage"}
}

// meta is the source of both snapshots, so they form one series.
var meta = ark.SourceMeta{Name: "/srv/fixture", Type: ark.SourceTypeFile}

// File is one entry of a snapshot and, for a regular file, its content.
type File struct {
	ark.FileInfo
	Content []byte
}

// Snapshot is one backup of the fixture: what the source yields and how the
// backup is recorded.
type Snapshot struct {
	Time  time.Time
	Tags  []string
	Files []File
}

// Source yields the files of s in their order.
func (s Snapshot) Source() ark.Source { return source(s.Files) }

// Options are the backup options s was recorded with.
func (s Snapshot) Options() []ark.BackupOption {
	return []ark.BackupOption{
		ark.WithHost(Host),
		ark.WithTime(s.Time),
		ark.WithTags(s.Tags...),
		ark.WithLabels(Labels()),
	}
}

type source []File

func (source) Meta() ark.SourceMeta { return meta }

func (s source) Walk(ctx context.Context, fn ark.WalkFunc) error {
	for _, f := range s {
		if err := ctx.Err(); err != nil {
			return err
		}
		var open func() (io.ReadCloser, error)
		if f.Type == ark.FileTypeRegular {
			open = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(f.Content)), nil }
		}
		if err := fn(f.FileInfo, open); err != nil {
			return err
		}
	}
	return nil
}

var base = time.Date(2026, 11, 1, 9, 30, 15, 123456789, time.UTC)

func at(d time.Duration) time.Time { return base.Add(d) }

// noise is incompressible content from a fixed seed: its chunks are stored raw.
func noise(n int) []byte {
	r := rand.New(rand.NewPCG(29, 13)) //nolint:gosec // fixed content, no secret
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// Snapshots returns the two snapshots of the fixture, oldest first. The second
// changes one file, adds one and drops one; everything else it keeps with the
// same ctime and inode, so its backup takes those files from the first.
func Snapshots() []Snapshot {
	const user, group = 1000, 100
	readme := []byte(strings.Repeat("ark keeps this line once, the rest is a zstd frame.\n", 40))
	big := noise(40 << 10)

	dir := func(path string, mode os.FileMode, uid, gid int, d time.Duration) File {
		return File{Path: path, Type: ark.FileTypeDirectory, Mode: mode, UID: uid, GID: gid, ModTime: at(d)}
	}
	file := func(path string, mode os.FileMode, content []byte, d time.Duration, inode uint64) File {
		return File{
			Content: content, Path: path, Type: ark.FileTypeRegular, Mode: mode, Size: int64(len(content)),
			UID: user, GID: group, ModTime: at(d), ChangeTime: at(d + time.Second), Inode: inode,
		}
	}

	old := withOwner(file("old.txt", 0o444, []byte("written before 1970\n"), 0, 51), 65534, 65534)
	old.ModTime = time.Date(1969, 7, 20, 20, 17, 40, 1, time.UTC)

	first := []File{
		dir("bin", 0o775|os.ModeSetgid, 0, 50, -48*time.Hour),
		file("bin/tool", 0o755|os.ModeSetuid, []byte("#!/bin/sh\necho fixture\n"), -47*time.Hour, 11),
		dir("data", 0o750, user, group, -40*time.Hour),
		file("data/big.bin", 0o640, big, -39*time.Hour, 21),
		{
			Path: "data/big.hard", Type: ark.FileTypeHardlink, LinkDest: "data/big.bin",
			Mode: 0o640, UID: user, GID: group, ModTime: at(-39 * time.Hour),
		},
		{
			Path: "dev", Type: ark.FileTypeDirectory, Mode: 0o755, ModTime: at(-72 * time.Hour),
			Xattrs: map[string][]byte{"security.selinux": []byte("system_u:object_r:device_t:s0\x00")},
		},
		{
			Path: "dev/fifo", Type: ark.FileTypeFifo, Mode: 0o620, UID: user, GID: 5, ModTime: at(-71 * time.Hour),
		},
		{
			Path: "dev/null", Type: ark.FileTypeCharDevice, Mode: 0o666, ModTime: at(-71 * time.Hour),
			DevMajor: 1, DevMinor: 3,
		},
		{
			Path: "dev/nvme0n1", Type: ark.FileTypeBlockDevice, Mode: 0o660, GID: 6, ModTime: at(-71 * time.Hour),
			DevMajor: 259, DevMinor: 1 << 19,
		},
		dir("docs", 0o755, user, group, -30*time.Hour),
		file("docs/empty", 0o600, nil, -29*time.Hour, 31),
		{
			Path: "docs/latest", Type: ark.FileTypeSymlink, LinkDest: "readme.txt", Mode: 0o777,
			UID: user, GID: group, ModTime: at(-29 * time.Hour),
		},
		withXattrs(file("docs/readme.txt", 0o644, readme, -28*time.Hour, 32), map[string][]byte{
			"user.comment": []byte("fixture"),
			"user.bytes":   {0x00, 0x01, 0xfe, 0xff},
			"user.\xffraw": []byte("a name that is not UTF-8"),
		}),
		dir("names", 0o700, 100000, 100001, -20*time.Hour),
		withOwner(file("names/caf\xe9.txt", 0o644, []byte("latin-1 name\n"), -19*time.Hour, 41), 100000, 100001),
		{
			Path: "names/\xff\xfe", Type: ark.FileTypeSymlink, LinkDest: "caf\xe9.txt", Mode: 0o777,
			UID: 100000, GID: 100001, ModTime: at(-19 * time.Hour),
		},
		old,
		dir("tmp", 0o777|os.ModeSticky, 0, 0, -10*time.Hour),
	}

	second := make([]File, 0, len(first))
	for _, f := range first {
		switch f.Path {
		case "docs/empty":
			continue
		case "docs/readme.txt":
			f.Content = append(bytes.Clone(readme), "and one more line in the second snapshot.\n"...)
			f.Size = int64(len(f.Content))
			f.ModTime, f.ChangeTime = at(2*time.Hour), at(2*time.Hour+time.Millisecond)
		case "tmp":
			added := file("tmp/new.txt", 0o644, []byte("added in the second snapshot\n"), 3*time.Hour, 61)
			second = append(second, f, added)
			continue
		}
		second = append(second, f)
	}

	return []Snapshot{
		{Time: at(0), Tags: []string{"daily"}, Files: first},
		{Time: at(24 * time.Hour), Tags: []string{"daily", "weekly"}, Files: second},
	}
}

func withXattrs(f File, x map[string][]byte) File {
	f.Xattrs = x
	return f
}

func withOwner(f File, uid, gid int) File {
	f.UID, f.GID = uid, gid
	return f
}
