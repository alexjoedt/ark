package ark_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// readBackend records every object read through Get.
type readBackend struct {
	ark.Backend
	mu    sync.Mutex
	reads []ark.Handle
}

func (b *readBackend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	b.mu.Lock()
	b.reads = append(b.reads, h)
	b.mu.Unlock()
	return b.Backend.Get(ctx, h)
}

// keyFiles returns the name and body of every key file in b.
func keyFiles(t *testing.T, b ark.Backend) map[string][]byte {
	t.Helper()
	ctx := context.Background()
	files := make(map[string][]byte)
	err := b.List(ctx, ark.KeyFile, func(info ark.BlobInfo) error {
		rc, err := b.Get(ctx, ark.Handle{Type: ark.KeyFile, Name: info.Name})
		if err != nil {
			return err
		}
		defer rc.Close()
		files[info.Name], err = io.ReadAll(rc)
		return err
	})
	if err != nil {
		t.Fatalf("list key files: %v", err)
	}
	return files
}

// onlyKeyFile returns the single key file of b.
func onlyKeyFile(t *testing.T, b ark.Backend) (string, []byte) {
	t.Helper()
	files := keyFiles(t, b)
	if len(files) != 1 {
		t.Fatalf("repository has %d key files, want 1", len(files))
	}
	for name, body := range files {
		return name, body
	}
	panic("unreachable")
}

func putKeyFile(t *testing.T, b ark.Backend, name string, body []byte) {
	t.Helper()
	if err := b.Put(
		context.Background(),
		ark.Handle{Type: ark.KeyFile, Name: name},
		bytes.NewReader(body),
	); err != nil {
		t.Fatalf("put key file: %v", err)
	}
}

func deleteKeyFiles(t *testing.T, b ark.Backend) {
	t.Helper()
	for name := range keyFiles(t, b) {
		if err := b.Delete(context.Background(), ark.Handle{Type: ark.KeyFile, Name: name}); err != nil {
			t.Fatalf("delete key file: %v", err)
		}
	}
}

func initRepo(t *testing.T, pw ark.Passphrase) *arkmem.Backend {
	t.Helper()
	b := arkmem.New()
	if err := ark.Init(context.Background(), b, pw); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return b
}

func TestInitWritesOneKeyFile(t *testing.T) {
	cases := map[string]struct {
		pw         ark.Passphrase
		kdf        string
		wantFields []string
	}{
		"password": {
			ark.WithPassword("secret"),
			"argon2id",
			[]string{"version", "kdf", "time", "memory", "threads", "salt", "key"},
		},
		"no password": {ark.WithoutPassword(), "none", []string{"version", "kdf", "key"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := initRepo(t, tc.pw)
			keyName, body := onlyKeyFile(t, b)
			if raw, err := hex.DecodeString(keyName); err != nil || len(raw) != 16 {
				t.Errorf("key file name %q, want 16 random bytes in hex", keyName)
			}

			var fields map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Fatalf("key file is not JSON: %v", err)
			}
			if len(fields) != len(tc.wantFields) {
				t.Errorf("key file fields %s, want exactly %v", body, tc.wantFields)
			}
			for _, f := range tc.wantFields {
				if _, ok := fields[f]; !ok {
					t.Errorf("key file has no %q field: %s", f, body)
				}
			}

			var kf struct {
				Version int    `json:"version"`
				KDF     string `json:"kdf"`
				Salt    string `json:"salt"`
				Key     string `json:"key"`
			}
			if err := json.Unmarshal(body, &kf); err != nil {
				t.Fatalf("key file: %v", err)
			}
			if kf.Version != 1 || kf.KDF != tc.kdf {
				t.Errorf("version %d kdf %q, want 1 and %q", kf.Version, kf.KDF, tc.kdf)
			}
			if salt, _ := hex.DecodeString(kf.Salt); tc.kdf == "argon2id" && len(salt) != 16 {
				t.Errorf("salt %q, want 16 bytes in hex", kf.Salt)
			}
			sealed, err := hex.DecodeString(kf.Key)
			if err != nil || len(sealed) < 41 || sealed[0] != 0x02 {
				t.Errorf("key %q, want a sealed object in hex starting with format byte 0x02", kf.Key)
			}
			for _, leak := range []string{"data_key", "id_key", "chunker_seed", "repository_id", "secret"} {
				if strings.Contains(string(body), leak) {
					t.Errorf("key file shows %q in plaintext: %s", leak, body)
				}
			}
		})
	}
}

func TestOpenChecksThePassphrase(t *testing.T) {
	protected := initRepo(t, ark.WithPassword("secret"))
	unprotected := initRepo(t, ark.WithoutPassword())
	anyErr := errors.New("any error")

	// Repositories stay open: Close would drop the shared arkmem objects.
	cases := map[string]struct {
		b    ark.Backend
		pw   ark.Passphrase
		want error
	}{
		"right password":                 {protected, ark.WithPassword("secret"), nil},
		"wrong password":                 {protected, ark.WithPassword("Secret"), ark.ErrWrongPassword},
		"no password on protected":       {protected, ark.WithoutPassword(), ark.ErrPasswordRequired},
		"no password on unprotected":     {unprotected, ark.WithoutPassword(), nil},
		"password on unprotected":        {unprotected, ark.WithPassword("secret"), ark.ErrNotPasswordProtected},
		"empty password on protected":    {protected, ark.WithPassword(""), anyErr},
		"zero passphrase on unprotected": {unprotected, ark.Passphrase{}, anyErr},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ark.Open(context.Background(), tc.b, tc.pw)
			switch {
			case tc.want == nil && err != nil:
				t.Errorf("Open: %v", err)
			case errors.Is(tc.want, anyErr) && err == nil:
				t.Error("Open succeeded, want an error for an empty passphrase")
			case tc.want != nil && !errors.Is(tc.want, anyErr) && !errors.Is(err, tc.want):
				t.Errorf("Open = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestInitRejectsAnEmptyPassphrase(t *testing.T) {
	for name, pw := range map[string]ark.Passphrase{"empty": ark.WithPassword(""), "zero": {}} {
		t.Run(name, func(t *testing.T) {
			b := arkmem.New()
			if err := ark.Init(context.Background(), b, pw); err == nil {
				t.Fatal("Init succeeded")
			}
			if ok, _ := b.Exists(context.Background(), ark.Handle{Type: ark.ConfigFile, Name: "config"}); ok {
				t.Error("a rejected Init wrote a config")
			}
			if n := len(keyFiles(t, b)); n != 0 {
				t.Errorf("a rejected Init wrote %d key files", n)
			}
		})
	}
}

func TestWrongPasswordReadsOnlyConfigAndKeys(t *testing.T) {
	b := &readBackend{Backend: initRepo(t, ark.WithPassword("secret"))}
	if _, err := ark.Open(context.Background(), b, ark.WithPassword("wrong")); !errors.Is(err, ark.ErrWrongPassword) {
		t.Fatalf("Open = %v, want ark.ErrWrongPassword", err)
	}
	for _, h := range b.reads {
		if h.Type != ark.ConfigFile && h.Type != ark.KeyFile {
			t.Errorf("Open read %s/%s before failing on the password", h.Type, h.Name)
		}
	}
}

func TestKeyFileDoesNotOpenElsewhere(t *testing.T) {
	ctx := context.Background()
	pw := ark.WithPassword("secret")

	t.Run("other repository", func(t *testing.T) {
		donor := initRepo(t, pw)
		name, body := onlyKeyFile(t, donor)
		b := initRepo(t, pw)
		deleteKeyFiles(t, b)
		putKeyFile(t, b, name, body)
		if _, err := ark.Open(ctx, b, pw); !errors.Is(err, ark.ErrWrongPassword) {
			t.Errorf("Open with a transplanted key file = %v, want ark.ErrWrongPassword", err)
		}
	})
	t.Run("renamed", func(t *testing.T) {
		b := initRepo(t, pw)
		_, body := onlyKeyFile(t, b)
		deleteKeyFiles(t, b)
		putKeyFile(t, b, strings.Repeat("ab", 16), body)
		if _, err := ark.Open(ctx, b, pw); !errors.Is(err, ark.ErrWrongPassword) {
			t.Errorf("Open with a renamed key file = %v, want ark.ErrWrongPassword", err)
		}
	})
	t.Run("tampered", func(t *testing.T) {
		for _, pw := range []ark.Passphrase{pw, ark.WithoutPassword()} {
			b := initRepo(t, pw)
			name, body := onlyKeyFile(t, b)
			var kf map[string]any
			if err := json.Unmarshal(body, &kf); err != nil {
				t.Fatal(err)
			}
			key, err := hex.DecodeString(kf["key"].(string))
			if err != nil {
				t.Fatal(err)
			}
			key[len(key)-1] ^= 0x01
			kf["key"] = hex.EncodeToString(key)
			body, _ = json.Marshal(kf)
			putKeyFile(t, b, name, body)
			if _, err := ark.Open(ctx, b, pw); !errors.Is(err, ark.ErrWrongPassword) {
				t.Errorf("%v: Open with a tampered key = %v, want ark.ErrWrongPassword", pw, err)
			}
		}
	})
}

func TestOpenRejectsUnknownKeyFiles(t *testing.T) {
	cases := map[string]string{
		"future version":  `{"version":2,"kdf":"none","key":"00"}`,
		"unknown kdf":     `{"version":1,"kdf":"scrypt","key":"00"}`,
		"zero time":       `{"version":1,"kdf":"argon2id","time":0,"memory":64,"threads":1,"salt":"00","key":"00"}`,
		"huge memory":     `{"version":1,"kdf":"argon2id","time":1,"memory":4294967295,"threads":1,"salt":"00","key":"00"}`,
		"too much time":   `{"version":1,"kdf":"argon2id","time":17,"memory":64,"threads":1,"salt":"00","key":"00"}`,
		"too much memory": `{"version":1,"kdf":"argon2id","time":1,"memory":1048577,"threads":1,"salt":"00","key":"00"}`,
		"not json":        `not a key file`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			b := initRepo(t, ark.WithoutPassword())
			deleteKeyFiles(t, b)
			putKeyFile(t, b, "k", []byte(body))
			if _, err := ark.Open(
				context.Background(),
				b,
				ark.WithoutPassword(),
			); !errors.Is(
				err,
				ark.ErrUnsupportedFormat,
			) {
				t.Errorf("Open = %v, want ark.ErrUnsupportedFormat", err)
			}
		})
	}

	t.Run("no key file", func(t *testing.T) {
		b := initRepo(t, ark.WithoutPassword())
		deleteKeyFiles(t, b)
		if _, err := ark.Open(
			context.Background(),
			b,
			ark.WithoutPassword(),
		); !errors.Is(
			err,
			ark.ErrUnsupportedFormat,
		) {
			t.Errorf("Open = %v, want ark.ErrUnsupportedFormat", err)
		}
	})
}

func TestOpenTriesAtMostTwentyKeyFiles(t *testing.T) {
	pw := ark.WithPassword("secret")
	_, foreign := onlyKeyFile(t, initRepo(t, pw))
	mem := initRepo(t, pw)
	deleteKeyFiles(t, mem)
	for i := range 30 {
		putKeyFile(t, mem, fmt.Sprintf("k%02d", i), foreign)
	}

	b := &readBackend{Backend: mem}
	if _, err := ark.Open(context.Background(), b, pw); !errors.Is(err, ark.ErrWrongPassword) {
		t.Fatalf("Open = %v, want ark.ErrWrongPassword", err)
	}
	var keyReads int
	for _, h := range b.reads {
		if h.Type == ark.KeyFile {
			keyReads++
		}
	}
	if keyReads != 20 {
		t.Errorf("Open read %d key files, want 20", keyReads)
	}
}

func TestPassphraseHidesThePassword(t *testing.T) {
	pw := ark.WithPassword("hunter2")
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		if got := fmt.Sprintf(format, pw); strings.Contains(got, "hunter2") {
			t.Errorf("Sprintf(%q) = %q, shows the password", format, got)
		}
	}
}

// keyFaults fails key file writes or deletes once armed.
type keyFaults struct {
	ark.Backend
	failPut, failDelete bool
}

func (b *keyFaults) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	if b.failPut && h.Type == ark.KeyFile {
		return errors.New("backend is down")
	}
	return b.Backend.Put(ctx, h, r)
}

func (b *keyFaults) Delete(ctx context.Context, h ark.Handle) error {
	if b.failDelete && h.Type == ark.KeyFile {
		return errors.New("backend is down")
	}
	return b.Backend.Delete(ctx, h)
}

func TestChangePassword(t *testing.T) {
	cases := map[string]struct{ from, to, wrong ark.Passphrase }{
		"new password":      {ark.WithPassword("old"), ark.WithPassword("new"), ark.WithPassword("old")},
		"drop the password": {ark.WithPassword("old"), ark.WithoutPassword(), ark.WithPassword("old")},
		"add a password":    {ark.WithoutPassword(), ark.WithPassword("new"), ark.WithoutPassword()},
	}
	wantWrong := map[string]error{
		"new password":      ark.ErrWrongPassword,
		"drop the password": ark.ErrNotPasswordProtected,
		"add a password":    ark.ErrPasswordRequired,
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			b := initRepo(t, tc.from)
			oldName, _ := onlyKeyFile(t, b)

			repo, err := ark.Open(ctx, b, tc.from)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			snap, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
			if err != nil {
				t.Fatalf("Backup: %v", err)
			}
			if err := repo.ChangePassword(ctx, tc.to); err != nil {
				t.Fatalf("ChangePassword: %v", err)
			}
			if newName, _ := onlyKeyFile(t, b); newName == oldName {
				t.Error("ChangePassword kept the old key file")
			}
			// The open repository keeps working on the same master key.
			if _, err := repo.Snapshot(ctx, snap.ID); err != nil {
				t.Errorf("Snapshot after the change: %v", err)
			}

			if _, err := ark.Open(ctx, b, tc.wrong); !errors.Is(err, wantWrong[name]) {
				t.Errorf("Open with the old passphrase returned %v, want %v", err, wantWrong[name])
			}
			reopened, err := ark.Open(ctx, b, tc.to)
			if err != nil {
				t.Fatalf("Open with the new passphrase: %v", err)
			}
			if _, err := reopened.Snapshot(ctx, snap.ID); err != nil {
				t.Errorf("Snapshot through the new passphrase: %v", err)
			}
		})
	}
}

func TestChangePasswordRejectsAnEmptyPassword(t *testing.T) {
	ctx := context.Background()
	b := initRepo(t, ark.WithPassword("old"))
	before, _ := onlyKeyFile(t, b)
	repo, err := ark.Open(ctx, b, ark.WithPassword("old"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := repo.ChangePassword(ctx, ark.WithPassword("")); err == nil {
		t.Fatal("ChangePassword accepted an empty password")
	}
	if after, _ := onlyKeyFile(t, b); after != before {
		t.Error("a rejected change replaced the key file")
	}
}

func TestChangePasswordKeepsTheOldKeyFileOnFailure(t *testing.T) {
	ctx := context.Background()
	b := &keyFaults{Backend: initRepo(t, ark.WithPassword("old"))}
	repo, err := ark.Open(ctx, b, ark.WithPassword("old"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	b.failPut = true
	if err := repo.ChangePassword(ctx, ark.WithPassword("new")); err == nil {
		t.Fatal("ChangePassword with a failing write returned no error")
	}
	onlyKeyFile(t, b)
	if _, err := ark.Open(ctx, b, ark.WithPassword("old")); err != nil {
		t.Errorf("Open with the old password after a failed write: %v", err)
	}

	// A failed delete leaves both key files, and Open tries each of them.
	b.failPut, b.failDelete = false, true
	if err := repo.ChangePassword(ctx, ark.WithPassword("new")); err == nil {
		t.Fatal("ChangePassword with a failing delete returned no error")
	}
	if n := len(keyFiles(t, b)); n != 2 {
		t.Fatalf("repository has %d key files after a failed delete, want 2", n)
	}
	for _, pw := range []ark.Passphrase{ark.WithPassword("old"), ark.WithPassword("new")} {
		if _, err := ark.Open(ctx, b, pw); err != nil {
			t.Errorf("Open %v with two key files: %v", pw, err)
		}
	}
}
