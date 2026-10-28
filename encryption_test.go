package ark_test

// The encryption criteria of the v1 format. Where a criterion already had a home
// the list points at it instead of repeating it:
//
//	no plaintext in any object name or body   TestBackendHoldsNoPlaintext
//	wrong password fails before data reads    TestWrongPasswordReadsOnlyConfigAndKeys (keys_test.go)
//	tampered chunk, tree, manifest            TestScenarioVerifyFindsACorruptChunk, TestScenarioVerifyFindsACorruptTree,
//	                                          TestScenarioTamperedManifestIsRefused (scenario_test.go)
//	tampered key file                         TestEveryFlippedKeyFileByteFailsOpen
//	tampered lock                             TestTamperedLockIsIgnoredUntilForced
//	key file from another repository          TestKeyFileDoesNotOpenElsewhere (keys_test.go)

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkmem"
	"github.com/alexjoedt/ark/chunker"
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
}

// scanForMarker fails when an object name or body in b contains marker and
// returns the number of objects per namespace.
func scanForMarker(t *testing.T, b ark.Backend) map[ark.HandleType]int {
	t.Helper()
	seen := make(map[ark.HandleType]int)
	for _, typ := range allHandleTypes {
		for _, name := range objectNames(t, b, typ) {
			seen[typ]++
			if strings.Contains(name, marker) {
				t.Errorf("object name %s/%s contains the marker", typ, name)
			}
			if bytes.Contains(readObject(t, b, typ, name), []byte(marker)) {
				t.Errorf("object %s/%s contains the marker", typ, name)
			}
		}
	}
	return seen
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
				t.Error("no lock seen during the backup, the scan did not cover locks")
			}
			if snap.Host != marker+"-host" || !strings.Contains(snap.SourceName, marker) {
				t.Fatalf("the marker did not reach the snapshot: %+v", snap)
			}

			after := scanForMarker(t, b)
			for _, typ := range []ark.HandleType{ark.ConfigFile, ark.KeyFile, ark.BlobFile, ark.TreeFile, ark.SnapshotFile} {
				if after[typ] == 0 {
					t.Errorf("no %s object scanned", typ)
				}
			}
			if after[ark.BlobFile] < 3 {
				t.Errorf("%d chunks scanned, the large file should span several", after[ark.BlobFile])
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
			putKeyFile(t, b, name, tampered)

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
