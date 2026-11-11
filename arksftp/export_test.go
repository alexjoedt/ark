package arksftp

import (
	"fmt"
	"io"
	"sync/atomic"

	"github.com/pkg/sftp"
)

// DisablePosixRename sends b down the plain-rename path that a server without
// posix-rename@openssh.com gets.
func (b *Backend) DisablePosixRename() { b.posixRename = false }

// RemoveRoot deletes b's repository directory and everything in it.
func (b *Backend) RemoveRoot() error { return b.client.RemoveAll(b.basePath) }

// AwaitRead is the wait of GetRange on its read.
var AwaitRead = awaitRead

// CountReceived swaps b's sftp client for one on a new session of the same
// SSH connection and returns the count of sftp bytes the server sends on it.
// Call it before anything else uses b: the swap is not synchronized.
func (b *Backend) CountReceived() (_ *atomic.Int64, err error) {
	s, err := b.conn.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	if err := s.RequestSubsystem("sftp"); err != nil {
		return nil, fmt.Errorf("request sftp: %w", err)
	}
	w, err := s.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin: %w", err)
	}
	r, err := s.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout: %w", err)
	}
	n := new(atomic.Int64)
	client, err := sftp.NewClientPipe(countingReader{r, n}, w)
	if err != nil {
		return nil, fmt.Errorf("sftp client: %w", err)
	}
	_ = b.client.Close()
	b.client = client
	return n, nil
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
