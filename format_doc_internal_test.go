package ark

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// documentedObjects maps each JSON object section of docs/format.md to the
// type whose JSON the code writes for it.
var documentedObjects = map[string]any{
	"Config":     repositoryConfig{},
	"Chunker":    chunkerParams{},
	"Key file":   keyFile{},
	"Master key": masterKey{},
	"Manifest":   manifest{},
	"Statistics": manifestStats{},
	"Lock":       lockObject{},
	"Features":   repositoryFeatures{},
}

var docKeyRow = regexp.MustCompile("^\\| `([a-z_]+)` \\| [^|]+ \\| ([^|]+) \\|")

// docKeys returns, per "### " section of the format page, the keys its table
// names and whether each is marked optional.
func docKeys(t *testing.T) map[string]map[string]bool {
	t.Helper()
	data, err := os.ReadFile("docs/format.md")
	if err != nil {
		t.Fatalf("read docs/format.md: %v", err)
	}
	sections := map[string]map[string]bool{}
	var section string
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "#") {
			section = strings.TrimSpace(strings.TrimLeft(line, "#"))
			continue
		}
		if m := docKeyRow.FindStringSubmatch(line); m != nil {
			if sections[section] == nil {
				sections[section] = map[string]bool{}
			}
			sections[section][m[1]] = strings.HasPrefix(strings.TrimSpace(m[2]), "yes")
		}
	}
	return sections
}

// TestFormatPageNamesEveryJSONKey fails when a stored JSON object has a key
// docs/format.md does not name, names a key the code does not have, or marks
// a key optional that the code always writes, or the other way round.
func TestFormatPageNamesEveryJSONKey(t *testing.T) {
	documented := docKeys(t)
	known := map[reflect.Type]bool{}
	for _, v := range documentedObjects {
		known[reflect.TypeOf(v)] = true
	}
	for section, v := range documentedObjects {
		typ := reflect.TypeOf(v)
		page := documented[section]
		if page == nil {
			t.Errorf("docs/format.md has no key table under %q for %s", section, typ.Name())
			continue
		}
		code := map[string]bool{}
		for f := range typ.Fields() {
			name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				t.Errorf("%s.%s has no JSON name", typ.Name(), f.Name)
				continue
			}
			code[name] = slices.Contains(strings.Split(opts, ","), "omitempty")
			ft := f.Type
			if ft.Kind() == reflect.Struct && ft != reflect.TypeFor[time.Time]() && !known[ft] {
				t.Errorf("%s.%s is the object %s, which no section of docs/format.md describes",
					typ.Name(), f.Name, ft.Name())
			}
		}
		for name, optional := range code {
			pageOptional, ok := page[name]
			switch {
			case !ok:
				t.Errorf("%s: the code writes %q, docs/format.md does not name it", section, name)
			case pageOptional != optional:
				t.Errorf(
					"%s: %q is optional %v in the code, %v in docs/format.md",
					section,
					name,
					optional,
					pageOptional,
				)
			}
		}
		for name := range page {
			if _, ok := code[name]; !ok {
				t.Errorf("%s: docs/format.md names %q, the code has no such key", section, name)
			}
		}
	}
}

// TestJSONObjectsStoreTimesInUTC pins the writer rule of docs/format.md: every
// time in a stored JSON object is written in UTC, whatever the local zone.
func TestJSONObjectsStoreTimesInUTC(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("UTC+5", 5*60*60)
	t.Cleanup(func() { time.Local = local })

	ctx := context.Background()
	b := newStubBackend()
	if err := Init(ctx, b, NoPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := Open(ctx, b, NoPassword())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	snap, err := repo.Backup(ctx, NewStreamSource("f", strings.NewReader("content")))
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	_, lock, err := repo.acquire(ctx, false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.release(ctx)

	config, err := readRepositoryConfig(ctx, b)
	if err != nil {
		t.Fatalf("readRepositoryConfig: %v", err)
	}
	kf, err := readKeyFile(ctx, b, repo.keyName)
	if err != nil {
		t.Fatalf("readKeyFile: %v", err)
	}
	sealedKey, err := hex.DecodeString(kf.Key)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	var wrap [32]byte
	masterKey, err := repo.codec.open(&wrap, objectAAD(Handle{Type: KeyFile, Name: repo.keyName}), sealedKey)
	if err != nil {
		t.Fatalf("open the master key: %v", err)
	}
	manifest, err := repo.getSealed(ctx, Handle{Type: SnapshotFile, Name: string(snap.ID)})
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}
	var lockData []byte
	err = b.List(ctx, LockFile, func(i BlobInfo) error {
		lockData, err = repo.getSealed(ctx, Handle{Type: LockFile, Name: i.Name})
		return err
	})
	if err != nil || lockData == nil {
		t.Fatalf("read the lock: %v", err)
	}

	for _, tc := range []struct {
		object, key string
		data        []byte
	}{
		{"config", "created_at", config},
		{"manifest", "created_at", manifest},
		{"master key", "created", masterKey},
		{"lock", "time", lockData},
	} {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(tc.data, &fields); err != nil {
			t.Fatalf("%s: %v", tc.object, err)
		}
		var at string
		if err := json.Unmarshal(fields[tc.key], &at); err != nil {
			t.Fatalf("%s %s: %v", tc.object, tc.key, err)
		}
		if !strings.HasSuffix(at, "Z") {
			t.Errorf("%s %s is %s, want a time in UTC", tc.object, tc.key, at)
		}
	}
}
