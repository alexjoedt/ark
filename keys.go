package ark

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/argon2"
)

// Passphrase is the password of a repository, or the explicit statement that
// it has none. WithPassword and WithoutPassword are its only constructors; the
// zero value is rejected by Init and Open. The package documentation, section
// Passwords, describes what the password protects and what it does not.
type Passphrase struct {
	password string
	none     bool
}

// WithPassword returns the passphrase for a password protected repository.
// An empty password is rejected by Init and Open; use WithoutPassword.
func WithPassword(password string) Passphrase {
	return Passphrase{password: password}
}

// WithoutPassword returns the passphrase for a repository without a password.
// Its master key is stored unprotected: anyone with access to the backend can
// read and forge its contents, only corruption is still detected.
func WithoutPassword() Passphrase {
	return Passphrase{none: true}
}

// String never reveals the password.
func (p Passphrase) String() string {
	if p.none {
		return "ark.WithoutPassword()"
	}
	return "ark.WithPassword(redacted)"
}

// GoString keeps %#v from printing the password.
func (p Passphrase) GoString() string { return p.String() }

func (p Passphrase) validate() error {
	if !p.none && p.password == "" {
		return errors.New("ark: empty password, use ark.WithoutPassword() for a repository without one")
	}
	return nil
}

// kdfParams are the argon2id work parameters of a key file.
type kdfParams struct {
	Time    uint32
	Memory  uint32 // KiB
	Threads uint8
}

// defaultKDF is what Init writes, RFC 9106 option 2. Root package tests lower
// it; Open always uses the parameters stored in the key file.
var defaultKDF = kdfParams{Time: 3, Memory: 64 * 1024, Threads: 4}

// maxKDFMemory and maxKDFTime cap the work a key file may ask for, so a
// forged one cannot make Open allocate or spin without bound. Memory in KiB.
const (
	maxKDFMemory = 1 << 20
	maxKDFTime   = 16
)

func (p kdfParams) derive(password string, salt []byte) [32]byte {
	var k [32]byte
	copy(k[:], argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(k))))
	return k
}

const (
	keyFileVersion   = 1
	kdfArgon2id      = "argon2id"
	kdfNone          = "none"
	keyFileNameBytes = 16
	saltBytes        = 16
	// maxKeyFiles is how many key files Open tries before giving up.
	maxKeyFiles = 20
)

// keyFile is the JSON body of an object in the keys namespace. Only the kdf
// parameters are plaintext; Key is the sealed masterKey in hex.
type keyFile struct {
	Version int    `json:"version"`
	KDF     string `json:"kdf"`
	Time    uint32 `json:"time,omitempty"`
	Memory  uint32 `json:"memory,omitempty"`
	Threads uint8  `json:"threads,omitempty"`
	Salt    string `json:"salt,omitempty"`
	Key     string `json:"key"`
}

// masterKey is the key material of a repository. Every key file of a
// repository seals the same masterKey.
type masterKey struct {
	DataKey      secretKey `json:"data_key"`
	IDKey        secretKey `json:"id_key"`
	ChunkerSeed  secretKey `json:"chunker_seed"`
	RepositoryID string    `json:"repository_id"`
	Created      time.Time `json:"created"`
	Label        string    `json:"label,omitempty"`
}

// secretKey is 32 bytes of key material, hex in JSON.
type secretKey [32]byte

func (k secretKey) MarshalText() ([]byte, error) {
	return hex.AppendEncode(nil, k[:]), nil
}

func (k *secretKey) UnmarshalText(b []byte) error {
	if hex.DecodedLen(len(b)) != len(k) {
		return fmt.Errorf("ark: key is %d hex characters, want %d", len(b), hex.EncodedLen(len(k)))
	}
	_, err := hex.Decode(k[:], b)
	return err
}

// newMasterKey returns fresh random keys for the repository repoID.
func newMasterKey(repoID string) *masterKey {
	mk := &masterKey{RepositoryID: repoID, Created: time.Now().UTC()}
	rand.Read(mk.DataKey[:])
	rand.Read(mk.IDKey[:])
	rand.Read(mk.ChunkerSeed[:])
	return mk
}

// putKeyFile seals mk under pw into a new key file with a random name and
// returns that name.
func putKeyFile(ctx context.Context, b Backend, codec *objectCodec, mk *masterKey, pw Passphrase) (string, error) {
	h := Handle{Type: KeyFile, Name: randomHex(keyFileNameBytes)}

	kf := keyFile{Version: keyFileVersion, KDF: kdfNone}
	var wrap [32]byte
	if !pw.none {
		salt := make([]byte, saltBytes)
		rand.Read(salt)
		p := defaultKDF
		kf.KDF, kf.Time, kf.Memory, kf.Threads = kdfArgon2id, p.Time, p.Memory, p.Threads
		kf.Salt = hex.EncodeToString(salt)
		wrap = p.derive(pw.password, salt)
	}

	blob, err := json.Marshal(mk)
	if err != nil {
		return "", fmt.Errorf("ark: failed to encode the master key: %w", err)
	}
	kf.Key = hex.EncodeToString(codec.seal(&wrap, objectAAD(h), blob))
	data, err := json.Marshal(kf)
	if err != nil {
		return "", fmt.Errorf("ark: failed to encode the key file: %w", err)
	}
	if err := b.Put(ctx, h, bytes.NewReader(data)); err != nil {
		return "", fmt.Errorf("ark: failed to write key file %s: %w", h.Name, err)
	}
	return h.Name, nil
}

func randomHex(n int) string {
	buf := make([]byte, n)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

// errStopList ends a List walk early.
var errStopList = errors.New("stop listing")

// loadMasterKey tries the first maxKeyFiles key files in listing order and
// returns the master key of the first that opens under pw and belongs to the
// repository repoID, with the name of that key file. Nothing but the key
// files is read.
func loadMasterKey(
	ctx context.Context,
	b Backend,
	codec *objectCodec,
	repoID string,
	pw Passphrase,
) (*masterKey, string, error) {
	var names []string
	err := b.List(ctx, KeyFile, func(info BlobInfo) error {
		names = append(names, info.Name)
		if len(names) == maxKeyFiles {
			return errStopList
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopList) {
		return nil, "", fmt.Errorf("ark: failed to list key files: %w", err)
	}

	// Precedence when nothing opens: a file that was tried with a matching
	// kind of passphrase, then a password or no-password mismatch, then an
	// unreadable header.
	var tried bool
	var mismatch, unsupported error
	for _, name := range names {
		kf, err := readKeyFile(ctx, b, name)
		if errors.Is(err, ErrNotFound) {
			continue // replaced by ChangePassword since the listing
		}
		if errors.Is(err, ErrUnsupportedFormat) {
			unsupported = err
			continue
		}
		if err != nil {
			return nil, "", err
		}
		switch {
		case kf.KDF == kdfNone && !pw.none:
			mismatch = ErrNotPasswordProtected
			continue
		case kf.KDF != kdfNone && pw.none:
			mismatch = ErrPasswordRequired
			continue
		}
		tried = true
		if mk := kf.open(codec, name, pw, repoID); mk != nil {
			return mk, name, nil
		}
	}

	switch {
	case tried:
		return nil, "", ErrWrongPassword
	case mismatch != nil:
		return nil, "", mismatch
	case unsupported != nil:
		return nil, "", unsupported
	default:
		return nil, "", fmt.Errorf("%w: repository has no key file", ErrUnsupportedFormat)
	}
}

// readKeyFile reads and validates the header of the key file name. A header
// this build cannot use is ErrUnsupportedFormat.
func readKeyFile(ctx context.Context, b Backend, name string) (*keyFile, error) {
	rc, err := b.Get(ctx, Handle{Type: KeyFile, Name: name})
	if err != nil {
		return nil, fmt.Errorf("ark: failed to read key file %s: %w", name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("ark: failed to read key file %s: %w", name, err)
	}

	var kf keyFile
	if err := json.Unmarshal(data, &kf); err != nil {
		return nil, fmt.Errorf("%w: key file %s is not valid JSON: %w", ErrUnsupportedFormat, name, err)
	}
	if kf.Version != keyFileVersion {
		return nil, fmt.Errorf("%w: key file %s has version %d, this build reads version %d",
			ErrUnsupportedFormat, name, kf.Version, keyFileVersion)
	}
	switch kf.KDF {
	case kdfNone:
	case kdfArgon2id:
		if kf.Time < 1 || kf.Time > maxKDFTime || kf.Threads < 1 ||
			kf.Memory < 8*uint32(kf.Threads) || kf.Memory > maxKDFMemory {
			return nil, fmt.Errorf("%w: key file %s has invalid argon2id parameters time %d memory %d threads %d",
				ErrUnsupportedFormat, name, kf.Time, kf.Memory, kf.Threads)
		}
		if salt, err := hex.DecodeString(kf.Salt); err != nil || len(salt) == 0 {
			return nil, fmt.Errorf("%w: key file %s has an invalid salt", ErrUnsupportedFormat, name)
		}
	default:
		return nil, fmt.Errorf("%w: key file %s uses unknown kdf %q", ErrUnsupportedFormat, name, kf.KDF)
	}
	return &kf, nil
}

// open unseals the master key of kf, the key file name, under pw. It returns
// nil when the key does not open or belongs to another repository; a wrong
// password and a tampered key are indistinguishable by design.
func (kf *keyFile) open(codec *objectCodec, name string, pw Passphrase, repoID string) *masterKey {
	sealed, err := hex.DecodeString(kf.Key)
	if err != nil {
		return nil
	}
	var wrap [32]byte
	if kf.KDF == kdfArgon2id {
		salt, _ := hex.DecodeString(kf.Salt) // validated by readKeyFile
		p := kdfParams{Time: kf.Time, Memory: kf.Memory, Threads: kf.Threads}
		wrap = p.derive(pw.password, salt)
	}
	blob, err := codec.open(&wrap, objectAAD(Handle{Type: KeyFile, Name: name}), sealed)
	if err != nil {
		return nil
	}
	var mk masterKey
	if err := json.Unmarshal(blob, &mk); err != nil || mk.RepositoryID != repoID {
		return nil
	}
	return &mk
}
