package ark_test

import (
	"context"
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/alexjoedt/ark"
)

// endless reads n zero bytes without holding them.
type endless struct{ n int64 }

func (e *endless) Read(p []byte) (int, error) {
	if e.n == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), e.n)
	clear(p[:n])
	e.n -= n
	return int(n), nil
}

func (e *endless) Close() error { return nil }

// planted serves a 256 MiB object for every Get of its type, of the name when
// it has one.
type planted struct {
	ark.Backend
	typ  ark.HandleType
	name string
}

func (b planted) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	if h.Type == b.typ && (b.name == "" || h.Name == b.name) {
		return &endless{256 << 20}, nil
	}
	return b.Backend.Get(ctx, h)
}

func TestOversizedKeyFileConfigAndFeaturesAreRefusedWithoutAllocating(t *testing.T) {
	for name, p := range map[string]planted{
		"key file": {typ: ark.KeyFile},
		"config":   {typ: ark.ConfigFile, name: "config"},
		"features": {typ: ark.ConfigFile, name: "features"},
	} {
		t.Run(name, func(t *testing.T) {
			p.Backend = initRepo(t, ark.NoPassword())
			b := p
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			_, err := ark.Open(context.Background(), b, ark.NoPassword())
			runtime.ReadMemStats(&after)
			if !errors.Is(err, ark.ErrUnsupportedFormat) {
				t.Fatalf("Open = %v, want ark.ErrUnsupportedFormat", err)
			}
			if grown := after.TotalAlloc - before.TotalAlloc; grown > 10<<20 {
				t.Errorf("Open allocated %d MiB for a 256 MiB %s, want under 10", grown>>20, name)
			}
		})
	}
}

func TestWrittenKeyFileAndConfigStayUnderTheCap(t *testing.T) {
	b := initRepo(t, ark.NewPassword("a long enough password"))
	_, key := onlyKeyFile(t, b)
	rc, err := b.Get(context.Background(), ark.Handle{Type: ark.ConfigFile, Name: "config"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := io.ReadAll(rc)
	rc.Close()
	for what, body := range map[string][]byte{"key file": key, "config": cfg} {
		if len(body) == 0 || len(body) > 4<<10 {
			t.Errorf("%s is %d bytes, want 1 to 4096, far under the 64 KiB cap", what, len(body))
		}
	}
}
