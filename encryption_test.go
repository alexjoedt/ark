package ark_test

// The encryption criteria of the v1 format. Where a criterion already had a home
// the list points at it instead of repeating it:
//
//	no plaintext in any object name or body   TestBackendHoldsNoPlaintext
//	key file plaintext is exactly its fields   TestInitWritesOneKeyFile (keys_test.go)
//	config plaintext is exactly its fields     TestConfigHoldsOnlyItsPlaintextFields
//	keyed chunk, tree node and index names,   TestChunksAndTreesAreSealedUnderKeyedNames,
//	no object name shared across repos        TestSameFilesInTwoRepositoriesShareNoNames (format_test.go)
//	snapshot names carry no time               TestSnapshotIDsCarryNoTimestamp
//	wrong password fails before data reads    TestWrongPasswordReadsOnlyConfigAndKeys (keys_test.go)
//	every sealed namespace x every mutation   TestTamperedObjectsAreRejected (tamper_test.go)
//	tampered chunk, tree, manifest            TestScenarioVerifyFindsACorruptChunk, TestScenarioVerifyFindsACorruptTree,
//	                                          TestScenarioTamperedManifestIsRefused (scenario_test.go)
//	tampered key file                         TestEveryFlippedKeyFileByteFailsOpen
//	tampered lock                             TestTamperedLockIsIgnoredUntilForced
//	key file from another repository          TestKeyFileDoesNotOpenElsewhere (keys_test.go)
//
// The accepted leaks, pinned so a widening fails:
//
//	pack and index counts and sizes           TestListingRevealsCountsAndSizes
//	pack size = sealed chunks + sealed table  TestListingRevealsCountsAndSizes
//	sealed chunk size = zstd frame + overhead TestListingRevealsCountsAndSizes
//	KDF parameters and config in plaintext    TestInitWritesOneKeyFile, TestConfigHoldsOnlyItsPlaintextFields
//	ms timestamp in lock names                TestLockNamesCarryTheirTime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkmem"
	"github.com/alexjoedt/ark/chunker"
	"golang.org/x/sys/unix"
)

// marker is planted in everything a backup records.
const marker = "arkmarker7c1d"

var allHandleTypes = []ark.HandleType{
	ark.ConfigFile,
	ark.KeyFile,
	ark.BlobFile,
	ark.TreeFile,
	ark.SnapshotFile,
	ark.LockFile,
	ark.DataPackFile,
	ark.TreePackFile,
	ark.DataIndexFile,
	ark.TreeIndexFile,
	ark.LostPackFile,
}

// scanForMarker fails when an object name, Stat field or body in b contains
// marker and returns the number of objects per namespace.
func scanForMarker(t *testing.T, b ark.Backend) map[ark.HandleType]int {
	t.Helper()
	seen := make(map[ark.HandleType]int)
	for _, typ := range allHandleTypes {
		for _, name := range objectNames(t, b, typ) {
			seen[typ]++
			if strings.Contains(name, marker) {
				t.Errorf("object name %s/%s contains the marker", typ, name)
			}
			info, err := b.Stat(context.Background(), ark.Handle{Type: typ, Name: name})
			if err != nil {
				t.Fatalf("stat %s/%s: %v", typ, name, err)
			}
			if strings.Contains(fmt.Sprint(info), marker) {
				t.Errorf("stat of %s/%s shows the marker: %+v", typ, name, info)
			}
			if bytes.Contains(readObject(t, b, typ, name), []byte(marker)) {
				t.Errorf("object %s/%s contains the marker", typ, name)
			}
		}
	}
	return seen
}

type excludingMetaSource struct {
	ark.Source
	excludes []string
}

func (s excludingMetaSource) Meta() ark.SourceMeta {
	m := s.Source.Meta()
	m.Excludes = s.excludes
	return m
}

func TestBackendHoldsNoPlaintext(t *testing.T) {
	for _, tt := range []struct {
		name string
		pw   ark.Passphrase
	}{
		{"password", testPassword},
		{"without password", ark.WithoutPassword()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), marker+"-source")
			dir := filepath.Join(root, marker+"-dir")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			// the large file spans several chunks, each with the marker in it
			large := bytes.Repeat([]byte(marker+" large file line\n"), 4*scenarioMaxChunk/30)
			for name, data := range map[string][]byte{
				marker + ".txt": []byte("small file " + marker),
				"large.txt":     large,
			} {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(marker+".txt", filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			xattrs := true
			for _, p := range []string{dir, filepath.Join(dir, marker+".txt")} {
				if err := unix.Setxattr(p, "user."+marker, []byte("value "+marker), 0); err != nil {
					t.Logf("xattrs unsupported here, not scanned: %v", err)
					xattrs = false
				}
			}

			b := arkmem.New()
			cfg := chunker.Config{MinSize: 4 << 10, AvgSize: 8 << 10, MaxSize: scenarioMaxChunk}
			if err := ark.Init(ctx, b, tt.pw, ark.WithChunkerConfig(cfg)); err != nil {
				t.Fatalf("Init: %v", err)
			}
			repo, err := ark.Open(ctx, b, tt.pw)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer repo.Close()

			src, err := arkfs.NewSource(root)
			if err != nil {
				t.Fatal(err)
			}
			src = excludingMetaSource{Source: src, excludes: []string{marker + "-skip/**"}}
			// the first progress event arrives under the backup's lock
			var during map[ark.HandleType]int
			var once sync.Once
			snap, err := repo.Backup(ctx, src,
				ark.WithHost(marker+"-host"),
				ark.WithTags(marker+"-tag"),
				ark.WithComment("comment "+marker),
				ark.WithBackupProgress(func(ark.ProgressEvent) {
					once.Do(func() { during = scanForMarker(t, b) })
				}),
			)
			if err != nil {
				t.Fatalf("Backup: %v", err)
			}
			if during[ark.LockFile] == 0 {
				t.Error("no lock seen during the backup, the scan did not cover it")
			}
			if len(snap.Excludes) != 1 || !strings.Contains(snap.Excludes[0], marker) {
				t.Fatalf("the exclude pattern did not reach the snapshot: %q", snap.Excludes)
			}
			if snap.Host != marker+"-host" || !strings.Contains(snap.SourceName, marker) {
				t.Fatalf("the marker did not reach the snapshot: %+v", snap)
			}
			if xattrs {
				found := 0
				err := repo.Walk(ctx, snap.ID, func(fi ark.FileInfo) error {
					if strings.Contains(string(fi.Xattrs["user."+marker]), marker) {
						found++
					}
					return nil
				})
				if err != nil || found != 2 {
					t.Fatalf("Walk found the marker xattr on %d entries, want 2: %v", found, err)
				}
			}

			after := scanForMarker(t, b)
			for _, typ := range []ark.HandleType{
				ark.ConfigFile, ark.KeyFile, ark.SnapshotFile,
				ark.DataPackFile, ark.TreePackFile, ark.DataIndexFile, ark.TreeIndexFile,
			} {
				if after[typ] == 0 {
					t.Errorf("no %s object scanned", typ)
				}
			}
			if n := len(blobNames(t, repo, ark.BlobFile)); n < 3 {
				t.Errorf("%d chunks in the scanned packs, the large file should span several", n)
			}
		})
	}
}

// TestEveryFlippedKeyFileByteFailsOpen flips each byte of the key file in
// turn. A byte of the sealed key fails as ErrWrongPassword, a header byte as
// ErrWrongPassword (salt, KDF parameters), ErrUnsupportedFormat or a JSON
// error. Open never succeeds and never panics.
func TestEveryFlippedKeyFileByteFailsOpen(t *testing.T) {
	ctx := context.Background()
	for _, pw := range []ark.Passphrase{testPassword, ark.WithoutPassword()} {
		b := initRepo(t, pw)
		name, body := onlyKeyFile(t, b)
		var kf struct{ Key string }
		if err := json.Unmarshal(body, &kf); err != nil {
			t.Fatal(err)
		}
		keyStart := bytes.Index(body, []byte(kf.Key))

		for i := range body {
			tampered := bytes.Clone(body)
			tampered[i] ^= 0x01
			putObject(t, b, ark.KeyFile, name, tampered)

			repo, err := ark.Open(ctx, b, pw)
			if err == nil {
				repo.Close()
				t.Errorf("%v: Open succeeded with byte %d of the key file flipped", pw, i)
				continue
			}
			var syntax *json.SyntaxError
			var typ *json.UnmarshalTypeError
			switch inKey := i >= keyStart && i < keyStart+len(kf.Key); {
			case inKey && !errors.Is(err, ark.ErrWrongPassword):
				t.Errorf("%v: key byte %d flipped: Open = %v, want ark.ErrWrongPassword", pw, i, err)
			case !inKey && !errors.Is(err, ark.ErrWrongPassword) && !errors.Is(err, ark.ErrUnsupportedFormat) &&
				!errors.As(err, &syntax) && !errors.As(err, &typ):
				t.Errorf(
					"%v: header byte %d (%q) flipped: Open = %v, want a password, format or JSON error",
					pw,
					i,
					body[i],
					err,
				)
			}
		}
	}
}

// TestTamperedLockIsIgnoredUntilForced: a lock that does not open blocks
// nothing, since whoever can change it can also delete it. It is never read
// as a lock, every acquire warns about it, and Unlock removes it only
// WithForce.
func TestTamperedLockIsIgnoredUntilForced(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, testPassword); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var logs bytes.Buffer
	repo, err := ark.Open(ctx, b, testPassword, ark.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	putForeignLock(t, repo, true)
	name := objectNames(t, b, ark.LockFile)[0]
	obj := readObject(t, b, ark.LockFile, name)
	obj[len(obj)-1] ^= 0x01
	if err := b.Put(ctx, ark.Handle{Type: ark.LockFile, Name: name}, bytes.NewReader(obj)); err != nil {
		t.Fatalf("overwrite lock: %v", err)
	}

	if _, err := repo.Backup(ctx, stubSource{name: "app", content: "contents"}); err != nil {
		t.Fatalf("Backup under a tampered exclusive lock: %v", err)
	}
	if !strings.Contains(logs.String(), "lock="+name) {
		t.Errorf("no warning names the tampered lock, logs:\n%s", logs.String())
	}
	if locks, err := repo.Locks(ctx); err != nil || len(locks) != 0 {
		t.Errorf("Locks = %v, %v, want none", locks, err)
	}

	if n, err := repo.Unlock(ctx); err != nil || n != 0 {
		t.Errorf("Unlock = %d, %v, want 0 removed", n, err)
	}
	if n, err := repo.Unlock(ctx, ark.WithForce()); err != nil || n != 1 {
		t.Errorf("Unlock WithForce = %d, %v, want 1 removed", n, err)
	}
	if names := objectNames(t, b, ark.LockFile); len(names) != 0 {
		t.Errorf("locks left after Unlock WithForce: %v", names)
	}
}

// TestConfigHoldsOnlyItsPlaintextFields pins the config, which is read before
// any key: a new plaintext field fails here.
func TestConfigHoldsOnlyItsPlaintextFields(t *testing.T) {
	b := initRepo(t, testPassword)
	names := objectNames(t, b, ark.ConfigFile)
	if len(names) != 1 {
		t.Fatalf("config objects %v, want one", names)
	}
	body := readObject(t, b, ark.ConfigFile, names[0])
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("config is not plain JSON: %v", err)
	}
	var chunkerFields map[string]json.RawMessage
	if err := json.Unmarshal(cfg["chunker"], &chunkerFields); err != nil {
		t.Fatalf("config chunker: %v", err)
	}
	for _, tc := range []struct {
		fields map[string]json.RawMessage
		want   []string
	}{
		{cfg, []string{"chunker", "compression", "created_at", "id", "version"}},
		{chunkerFields, []string{"avg", "max", "min"}},
	} {
		if got := slices.Sorted(maps.Keys(tc.fields)); !slices.Equal(got, tc.want) {
			t.Errorf("config fields %v, want exactly %v: %s", got, tc.want, body)
		}
	}
}

// idPrefixTime decodes the first 10 Crockford base32 characters of id as
// milliseconds since the epoch, the layout of a timestamped ID.
func idPrefixTime(t *testing.T, id string) time.Time {
	t.Helper()
	const chars = "0123456789abcdefghjkmnpqrstvwxyz"
	if len(id) != 26 {
		t.Fatalf("id %q has %d characters, want 26", id, len(id))
	}
	var ms int64
	for i := range 26 {
		v := strings.IndexByte(chars, id[i])
		if v < 0 {
			t.Fatalf("id %q is not Crockford base32", id)
		}
		if i < 10 {
			ms = ms*32 + int64(v)
		}
	}
	return time.UnixMilli(ms)
}

// TestSnapshotIDsCarryNoTimestamp: a snapshot ID is random, so the listing
// does not date a snapshot; prefixes still resolve and the parent is still the
// newest snapshot of the source.
func TestSnapshotIDsCarryNoTimestamp(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())
	var prev string
	for i := range 5 {
		snap, err := repo.Backup(ctx, stubSource{name: "app", content: fmt.Sprint("contents ", i)})
		if err != nil {
			t.Fatalf("Backup: %v", err)
		}
		if d := idPrefixTime(t, snap.ID).Sub(snap.CreatedAt).Abs(); d < 24*time.Hour {
			t.Errorf("snapshot %s decodes to %s off its CreatedAt, want no timestamp in it", snap.ID, d)
		}
		if snap.ParentID != prev {
			t.Errorf("backup %d has parent %q, want the newest snapshot %q", i, snap.ParentID, prev)
		}
		got, err := repo.Snapshot(ctx, snap.ID[:8])
		if err != nil || got.ID != snap.ID {
			t.Errorf("Snapshot(%q) = %v, %v, want %s", snap.ID[:8], got, err, snap.ID)
		}
		prev = snap.ID
	}
}

// TestLockNamesCarryTheirTime pins an accepted leak: a lock name starts with
// the millisecond it was taken, so a zero-byte lock can age out without a
// key.
func TestLockNamesCarryTheirTime(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := openRepo(t, b)
	var locks []string
	var once sync.Once
	start := time.Now().Truncate(time.Millisecond)
	_, err := repo.Backup(ctx, stubSource{name: "app", content: "contents"},
		ark.WithBackupProgress(func(ark.ProgressEvent) {
			once.Do(func() { locks = objectNames(t, b, ark.LockFile) })
		}))
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	end := time.Now()
	if len(locks) != 1 {
		t.Fatalf("locks %v during the backup, want one", locks)
	}
	if at := idPrefixTime(t, locks[0]); at.Before(start) || at.After(end) {
		t.Errorf("lock %s decodes to %s, want a time between %s and %s", locks[0], at, start, end)
	}
}

// TestListingRevealsCountsAndSizes pins the accepted leaks of a listing: one
// data pack, one tree pack and one index object each for a small backup, one
// object per snapshot and key file, every size as stored. No chunk and no
// tree node is listed: a pack is its sealed chunks, each only a fixed
// overhead plus its zstd frame larger than its content, and a sealed table
// of one entry per chunk, so its size shows the bytes it holds and not where
// one chunk ends. Padding would narrow this; anything that widens it fails.
func TestListingRevealsCountsAndSizes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for name, size := range map[string]int{"top.bin": 3 << 10, "a/one.bin": 20 << 10, "b/two.bin": 50 << 10} {
		data := make([]byte, size)
		_, _ = rand.Read(data)
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b := arkmem.New()
	cfg := chunker.Config{MinSize: 4 << 10, AvgSize: 8 << 10, MaxSize: scenarioMaxChunk}
	if err := ark.Init(ctx, b, testPassword, ark.WithChunkerConfig(cfg)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()
	src, err := arkfs.NewSource(root)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	want := map[ark.HandleType]int{
		ark.ConfigFile: 1, ark.KeyFile: 1, ark.SnapshotFile: 1, ark.LockFile: 0,
		ark.BlobFile: 0, ark.TreeFile: 0,
		ark.DataPackFile: 1, ark.TreePackFile: 1, ark.DataIndexFile: 1, ark.TreeIndexFile: 1,
	}
	sizes := make(map[ark.HandleType]int64)
	for _, typ := range allHandleTypes {
		var infos []ark.BlobInfo
		if err := b.List(ctx, typ, func(info ark.BlobInfo) error {
			infos = append(infos, info)
			return nil
		}); err != nil {
			t.Fatalf("list %s: %v", typ, err)
		}
		if len(infos) != want[typ] {
			t.Errorf("%s holds %d objects, want %d", typ, len(infos), want[typ])
		}
		for _, info := range infos {
			if n := int64(len(readObject(t, b, typ, info.Name))); info.Size != n {
				t.Errorf("%s/%s listed at %d bytes, stored %d", typ, info.Name, info.Size, n)
			}
			sizes[typ] += info.Size
		}
	}

	// zstd stores random data in raw blocks behind a frame header and
	// checksum, a few bytes per chunk.
	const zstdFrameSlack = 24
	chunks := blobNames(t, repo, ark.BlobFile)
	if len(chunks) != snap.Stats.ChunksNew {
		t.Errorf("the data index lists %d chunks, the backup stored %d", len(chunks), snap.Stats.ChunksNew)
	}
	if n := len(blobNames(t, repo, ark.TreeFile)); n != 3 {
		t.Errorf("the tree index lists %d nodes, want one per directory, 3", n)
	}
	var plain, sealed int64
	for _, name := range chunks {
		size := int64(len(readBlob(t, b, repo, ark.BlobFile, name)))
		p := int64(len(openObject(t, repo, ark.BlobFile, name)))
		plain += p
		sealed += size
		if extra := size - ark.SealOverhead - p; extra < 0 || extra > zstdFrameSlack {
			t.Errorf("chunk of %d bytes sealed at %d, want %d plus at most %d",
				p, size, p+ark.SealOverhead, zstdFrameSlack)
		}
	}
	if plain != snap.Stats.TotalSize {
		t.Errorf("chunks hold %d bytes, the snapshot %d", plain, snap.Stats.TotalSize)
	}
	if sealed != snap.Stats.StoredSize {
		t.Errorf("chunks are sealed to %d bytes, the snapshot counts %d stored", sealed, snap.Stats.StoredSize)
	}
	// The rest of the pack is the sealed table, 36 bytes per chunk before
	// compression, and its 4 byte length.
	const tableEntry = 36
	table := sizes[ark.DataPackFile] - sealed
	if limit := int64(len(chunks)*tableEntry + ark.SealOverhead + zstdFrameSlack + 4); table <= 4 || table > limit {
		t.Errorf("the data pack holds %d bytes besides its chunks, want a table of at most %d", table, limit)
	}
}
