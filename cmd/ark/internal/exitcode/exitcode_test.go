package exitcode

import (
	"errors"
	"fmt"
	"testing"

	"github.com/alexjoedt/ark"
)

func TestOf(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code int
		kind string
	}{
		{"nil", nil, OK, ""},
		{"plain", errors.New("boom"), Failure, KindError},
		{"usage", Usagef("missing %s", "arg"), Usage, KindUsage},
		{"verify", Verify(errors.New("bad chunk")), VerifyFailed, KindVerifyFailed},
		{"locked", fmt.Errorf("open: %w", ark.ErrRepoLocked), Locked, KindLocked},
		{"not found", fmt.Errorf("snapshot: %w", ark.ErrNotFound), NotFound, KindNotFound},
		{"ambiguous", fmt.Errorf("snapshot: %w", ark.ErrAmbiguousID), NotFound, KindAmbiguousID},
		{"empty policy", ark.ErrEmptyPolicy, Usage, KindUsage},
		{"pinned wins", &Error{Code: Usage, Kind: KindUsage, Err: ark.ErrRepoLocked}, Usage, KindUsage},
		{"pinned wrapped", fmt.Errorf("ctx: %w", Usagef("x")), Usage, KindUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, kind := Of(tt.err)
			if code != tt.code || kind != tt.kind {
				t.Fatalf("Of(%v) = %d %q, want %d %q", tt.err, code, kind, tt.code, tt.kind)
			}
		})
	}
}

func TestErrorUnwrap(t *testing.T) {
	err := Verify(fmt.Errorf("wrap: %w", ark.ErrCorruptChunk))
	if !errors.Is(err, ark.ErrCorruptChunk) {
		t.Fatal("errors.Is lost the cause")
	}
	if err.Error() != "wrap: ark: corrupt chunk" {
		t.Fatalf("message %q", err.Error())
	}
}
