package ark

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/crypto/chacha20poly1305"
)

// formatSealed is the format byte of every data object: a nonce and an
// XChaCha20-Poly1305 sealed zstd frame follow it. The first byte names the
// codec of everything after it, so a later codec gets a new byte.
const formatSealed byte = 0x02

// sealOverhead is what seal adds to the zstd frame: format byte, nonce, tag.
const sealOverhead = 1 + chacha20poly1305.NonceSizeX + chacha20poly1305.Overhead

var (
	// errSealBroken is returned by open when the tag does not verify.
	errSealBroken = errors.New("ark: sealed object does not authenticate")
	// errNameMismatch is returned by a verifying store when an object opens
	// but its content does not hash to its name.
	errNameMismatch = errors.New("ark: object content does not match its name")
)

// objectCodec seals and opens data objects and key files.
// The zstd encoder and decoder are safe for concurrent use, so one codec
// serves every objectStore of a repository.
type objectCodec struct {
	enc *zstd.Encoder
	dec *zstd.Decoder
}

func newObjectCodec() *objectCodec {
	// These allocations never fail with the default options.
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	dec, _ := zstd.NewReader(nil)
	return &objectCodec{enc: enc, dec: dec}
}

// Close releases the pooled zstd encoder and decoder.
func (c *objectCodec) Close() {
	_ = c.enc.Close()
	c.dec.Close()
}

// seal returns the sealed form of data: formatSealed, a random nonce, then
// XChaCha20-Poly1305 over the zstd frame of data with aad bound to the tag.
func (c *objectCodec) seal(key *[32]byte, aad string, data []byte) []byte {
	aead, _ := chacha20poly1305.NewX(key[:]) // fails only on a wrong key size
	const hdr = 1 + chacha20poly1305.NonceSizeX
	out := make([]byte, hdr, sealOverhead+len(data)/2)
	out[0] = formatSealed
	rand.Read(out[1:hdr])
	// Compress behind the header and seal in place, one buffer for both.
	out = slices.Grow(c.enc.EncodeAll(data, out), chacha20poly1305.Overhead)
	return aead.Seal(out[:hdr], out[1:hdr], out[hdr:], []byte(aad))
}

// open reverses seal. A changed byte, an unknown format byte, a different key
// or a different aad all return errSealBroken.
func (c *objectCodec) open(key *[32]byte, aad string, obj []byte) ([]byte, error) {
	if len(obj) == 0 {
		return nil, fmt.Errorf("%w: object is empty", errSealBroken)
	}
	if obj[0] != formatSealed {
		return nil, fmt.Errorf("%w: unknown object format byte 0x%02x", errSealBroken, obj[0])
	}
	if len(obj) < sealOverhead {
		return nil, errSealBroken
	}
	aead, _ := chacha20poly1305.NewX(key[:])
	nonce := obj[1 : 1+chacha20poly1305.NonceSizeX]
	frame, err := aead.Open(nil, nonce, obj[1+chacha20poly1305.NonceSizeX:], []byte(aad))
	if err != nil {
		return nil, errSealBroken
	}
	return c.dec.DecodeAll(frame, nil)
}

// objectAAD is the associated data an object is sealed with, so an object
// moved to another name or namespace does not open.
func objectAAD(h Handle) string { return h.Type.String() + "/" + h.Name }

// putSealed seals data with the data key and writes it under h, replacing
// what is there. Manifests and locks go through it: their names are chosen,
// not derived from content.
func (r *Repository) putSealed(ctx context.Context, h Handle, data []byte) error {
	obj := r.codec.seal((*[32]byte)(&r.key.DataKey), objectAAD(h), data)
	if err := r.backend.Put(ctx, h, bytes.NewReader(obj)); err != nil {
		return fmt.Errorf("ark: failed to store %s object %s: %w", h.Type, h.Name, err)
	}
	return nil
}

// getSealed reads and opens the object under h with the data key.
func (r *Repository) getSealed(ctx context.Context, h Handle) ([]byte, error) {
	return readSealed(ctx, r.backend, r.codec, (*[32]byte)(&r.key.DataKey), h)
}

// readSealed reads and opens the object under h. Errors wrap ErrNotFound from
// the backend, and errSealBroken for an object that was changed or moved.
func readSealed(ctx context.Context, b Backend, c *objectCodec, key *[32]byte, h Handle) ([]byte, error) {
	rc, err := b.Get(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("ark: failed to read %s object %s: %w", h.Type, h.Name, err)
	}
	defer rc.Close()

	obj, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("ark: failed to read %s object %s: %w", h.Type, h.Name, err)
	}
	data, err := c.open(key, objectAAD(h), obj)
	if err != nil {
		return nil, fmt.Errorf("ark: failed to open %s object %s: %w", h.Type, h.Name, err)
	}
	return data, nil
}

// objectStore is a content-addressable store over one backend namespace.
// An object is named by the HMAC-SHA256 of its raw content under the ID key
// and sealed with the data key, so its name reveals nothing without the keys
// and does not depend on how it is encoded.
type objectStore struct {
	backend Backend
	typ     HandleType
	codec   *objectCodec
	dataKey *[32]byte
	idKey   []byte
	// verify makes Get recompute the name of every object it reads.
	verify bool
}

func newObjectStore(b Backend, t HandleType, c *objectCodec, key *masterKey) *objectStore {
	return &objectStore{
		backend: b,
		typ:     t,
		codec:   c,
		dataKey: (*[32]byte)(&key.DataKey),
		idKey:   key.IDKey[:],
	}
}

// verifying returns a copy of s whose Get fails with errNameMismatch when an
// object does not hash to its name.
func (s *objectStore) verifying() *objectStore {
	v := *s
	v.verify = true
	return &v
}

func (s *objectStore) handle(name string) Handle { return Handle{Type: s.typ, Name: name} }

// name returns the object name of data: its HMAC-SHA256 under the ID key, hex.
func (s *objectStore) name(data []byte) string {
	mac := hmac.New(sha256.New, s.idKey)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// Put stores data under its name and returns that name with the number of
// bytes written. A stored size of 0 means the object was already present and
// nothing was written.
func (s *objectStore) Put(ctx context.Context, data []byte) (string, int, error) {
	name := s.name(data)
	size, err := s.putNamed(ctx, name, data)
	return name, size, err
}

// putNamed is Put for a caller that already holds the name of data.
func (s *objectStore) putNamed(ctx context.Context, name string, data []byte) (int, error) {
	h := s.handle(name)
	exists, err := s.backend.Exists(ctx, h)
	if err != nil {
		return 0, fmt.Errorf("ark: failed to check %s object %s: %w", s.typ, name, err)
	}
	if exists {
		return 0, nil
	}

	obj := s.codec.seal(s.dataKey, objectAAD(h), data)
	if err := s.backend.Put(ctx, h, bytes.NewReader(obj)); err != nil {
		return 0, fmt.Errorf("ark: failed to store %s object %s: %w", s.typ, name, err)
	}
	return len(obj), nil
}

// Get returns the raw content of the object stored under name. An object that
// was changed or moved from another name fails to open.
func (s *objectStore) Get(ctx context.Context, name string) ([]byte, error) {
	data, err := readSealed(ctx, s.backend, s.codec, s.dataKey, s.handle(name))
	if err != nil {
		return nil, err
	}
	if s.verify && s.name(data) != name {
		return nil, fmt.Errorf("ark: %s object %s: %w", s.typ, name, errNameMismatch)
	}
	return data, nil
}

// Exists reports whether the object named name is present.
func (s *objectStore) Exists(ctx context.Context, name string) (bool, error) {
	return s.backend.Exists(ctx, s.handle(name))
}

// Delete removes the object named name.
func (s *objectStore) Delete(ctx context.Context, name string) error {
	return s.backend.Delete(ctx, s.handle(name))
}

// StoredSize returns the size of the object as stored, header byte included.
func (s *objectStore) StoredSize(ctx context.Context, name string) (int64, error) {
	info, err := s.backend.Stat(ctx, s.handle(name))
	if err != nil {
		return 0, fmt.Errorf("ark: failed to stat %s object %s: %w", s.typ, name, err)
	}
	return info.Size, nil
}

// Names returns the name of every object currently in the namespace.
func (s *objectStore) Names(ctx context.Context) ([]string, error) {
	var names []string
	err := s.backend.List(ctx, s.typ, func(info BlobInfo) error {
		names = append(names, info.Name)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ark: failed to list %s objects: %w", s.typ, err)
	}
	return names, nil
}
