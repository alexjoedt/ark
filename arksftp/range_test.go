package arksftp_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arksftp"
)

// TestGetRangeReadsOnlyTheRange pins that a range at the end of an object
// costs the range on the wire, not the bytes before it, against the whole-object
// Get as the control. The range spans many sftp packets, as a pack read does.
func TestGetRangeReadsOnlyTheRange(t *testing.T) {
	ctx := t.Context()
	b := openTestBackend(t, testRoot(t)).(*arksftp.Backend)
	received, err := b.CountReceived()
	if err != nil {
		t.Fatalf("CountReceived: %v", err)
	}

	const size, length = 8 << 20, 512 << 10
	payload := bytes.Repeat([]byte("0123456789abcdef"), size/16)
	h := ark.Handle{Type: ark.BlobFile, Name: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff0001"}
	if err := b.Put(ctx, h, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	received.Store(0)
	got, err := b.GetRange(ctx, h, size-length, length)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}
	if !bytes.Equal(got, payload[size-length:]) {
		t.Fatal("GetRange returned the wrong bytes")
	}
	rangeBytes := received.Load()

	received.Store(0)
	rc, err := b.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := io.Copy(io.Discard, rc); err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = rc.Close()
	wholeBytes := received.Load()

	// The slack covers the header of every data packet and the open and
	// close replies of one handle.
	if rangeBytes < length || rangeBytes > length+length/100+1<<10 {
		t.Errorf("GetRange of %d bytes received %d bytes on the wire", length, rangeBytes)
	}
	if wholeBytes < size {
		t.Errorf("control Get of %d bytes received %d bytes, the counter misses traffic", size, wholeBytes)
	}
}

// TestGetRangeWaitEndsWithTheContext pins that a cancelled context ends the
// wait on a read that the sftp client cannot abort.
func TestGetRangeWaitEndsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	reading, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	returned := make(chan error, 1)
	go func() {
		_, err := arksftp.AwaitRead(ctx, func() ([]byte, error) {
			close(reading)
			<-release
			return nil, nil
		})
		returned <- err
	}()

	<-reading
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("wait on a blocked read = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("the wait on a blocked read outlived its context")
	}
}
