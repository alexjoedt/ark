// Package arksftp implements [ark.Backend] over an SFTP connection.
//
// One Backend owns one SSH connection and one SFTP client. Requests are
// multiplexed over that single connection; there is no pool. The Repository
// concurrency bound (ark.WithConcurrency, [runtime.NumCPU] times two by
// default) caps how many requests are in flight, so the caller sizes the load
// on that one connection.
//
// Secrets never live in Config or in a URI. The password comes from
// ARK_SFTP_PASSWORD and a key passphrase from ARK_SFTP_KEY_PASSPHRASE; see
// [EnvPassword] and [EnvKeyPassphrase].
package arksftp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/internal/rangeread"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// tmpPrefix marks in-flight objects written by Put. List skips them.
const tmpPrefix = ".ark-tmp-"

// Compile-time check that *Backend satisfies ark.Backend.
var _ ark.Backend = (*Backend)(nil)

// syncFile flushes f on the server. Tests swap it to observe the calls.
var syncFile = (*sftp.File).Sync

// Config describes how to reach an SFTP server. Zero values fall back to the
// documented defaults. It holds no secrets.
type Config struct {
	// Host is the remote hostname, IP address, or an ssh_config Host alias
	// whose HostName replaces it.
	Host string
	// Port is the SSH port. Falls back to ssh_config, then to 22.
	Port int
	// User is the SSH username. Falls back to ssh_config, then to the
	// current OS user.
	User string
	// Path is the repository root directory on the remote server.
	Path string
	// IdentityFile is the path to a PEM-encoded private key file.
	// Falls back to ssh_config, then to ~/.ssh/id_rsa, id_ecdsa and
	// id_ed25519.
	IdentityFile string
	// KnownHostsFile is the known_hosts file used to verify the host key.
	// Defaults to ~/.ssh/known_hosts.
	KnownHostsFile string
	// InsecureIgnoreHostKey skips host key verification. It exposes the
	// connection to man-in-the-middle attacks and is meant for tests and
	// throwaway environments only.
	InsecureIgnoreHostKey bool
	// Timeout bounds the TCP and SSH handshake. Defaults to 30s when zero.
	Timeout time.Duration
}

const (
	// defaultTimeout bounds dial and handshake when Config.Timeout is zero.
	defaultTimeout = 30 * time.Second
	// defaultPort is the SSH port used when neither Config nor ssh_config
	// names one.
	defaultPort = 22
)

// Backend implements ark.Backend backed by an SFTP server.
// All methods are safe for concurrent use via the underlying SSH/SFTP
// multiplexing.
type Backend struct {
	client   *sftp.Client
	conn     *ssh.Client
	basePath string
	// posixRename records whether the server announced
	// posix-rename@openssh.com. See [Backend.rename].
	posixRename bool
	// fsync records whether the server announced fsync@openssh.com, which
	// Put needs to flush a temp file before renaming it into place and
	// Put and Delete need to flush the directories they change.
	fsync bool
	// durable holds directories whose entries up to the root this process
	// has fsynced, so Put pays the parent syncs once per directory.
	durable sync.Map
}

// New connects to the SFTP server described by cfg and returns a ready Backend.
// The caller closes it with [Backend.Close].
func New(cfg Config) (*Backend, error) {
	cfg = resolve(cfg, loadSSHConfig())

	sshCfg, chain, err := buildSSHConfig(cfg)
	if err != nil {
		return nil, err
	}
	// The agent socket is only needed for the handshake, which ssh.Dial
	// completes before it returns.
	defer chain.close()

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	conn, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("arksftp: dial %s (%s): %w", addr, chain.describe(), err)
	}

	// Concurrent writes let a single Put stream ahead instead of waiting for
	// one round trip per packet. Safe here because Put owns its temp file.
	client, err := sftp.NewClient(conn, sftp.UseConcurrentWrites(true))
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("arksftp: open sftp subsystem: %w", err)
	}

	_, posixRename := client.HasExtension("posix-rename@openssh.com")
	_, fsync := client.HasExtension("fsync@openssh.com")

	return &Backend{
		client:      client,
		conn:        conn,
		basePath:    cfg.Path,
		posixRename: posixRename,
		fsync:       fsync,
	}, nil
}

// Close closes the SFTP client and the underlying SSH connection.
func (b *Backend) Close() error {
	clientErr := b.client.Close()
	connErr := b.conn.Close()
	if clientErr != nil {
		return fmt.Errorf("arksftp: close client: %w", clientErr)
	}
	if connErr != nil {
		return fmt.Errorf("arksftp: close conn: %w", connErr)
	}
	return nil
}

// Put writes r to a temporary name in the target directory and renames it into
// place with posix-rename, so a reader never sees a partial object.
// It is idempotent: an existing object is replaced.
func (b *Backend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	p, err := b.remotePath(h)
	if err != nil {
		return err
	}

	dir := path.Dir(p)
	if err := b.mkdirDurable(dir); err != nil {
		return err
	}

	f, tmp, err := b.createTemp(dir)
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		_ = f.Close()
		if !renamed {
			_ = b.client.Remove(tmp)
		}
	}()

	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("arksftp: write %s: %w", tmp, err)
	}
	// Without fsync@openssh.com there is no way to flush the file or the
	// directories; the object then only survives a process crash, not a power
	// loss on the server.
	if b.fsync {
		if err := syncFile(f); err != nil {
			return fmt.Errorf("arksftp: sync %s: %w", tmp, err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("arksftp: close %s: %w", tmp, err)
	}
	if err := b.rename(tmp, p); err != nil {
		return fmt.Errorf("arksftp: rename %s: %w", p, err)
	}
	renamed = true

	return b.syncDir(dir)
}

// mkdirDurable creates dir and, with fsync@openssh.com, fsyncs the parent of
// every directory from dir up to the root once per process, so a server power
// loss cannot drop a directory that a renamed object lives in.
func (b *Backend) mkdirDurable(dir string) error {
	if _, ok := b.durable.Load(dir); ok {
		if _, err := b.client.Stat(dir); err == nil {
			return nil
		}
		b.durable.Clear() // removed behind our back; its parents may be gone too
	}
	if err := b.client.MkdirAll(dir); err != nil {
		return fmt.Errorf("arksftp: mkdirall %s: %w", dir, err)
	}
	if !b.fsync {
		return nil
	}
	var synced []string
	for d := dir; d != path.Dir(d); d = path.Dir(d) {
		if _, ok := b.durable.Load(d); ok {
			break
		}
		if err := b.syncDir(path.Dir(d)); err != nil {
			return err
		}
		synced = append(synced, d)
	}
	for _, d := range synced {
		b.durable.Store(d, struct{}{})
	}
	return nil
}

// syncDir fsyncs dir on the server so a rename, create or remove inside it
// survives a server power loss. It is a no-op without fsync@openssh.com. A
// server that will not open or flush a directory, or an ancestor the user may
// not read, answers with a status; that counts as unsupported, not as a
// failure. Transport errors still fail.
func (b *Backend) syncDir(dir string) error {
	if !b.fsync {
		return nil
	}
	d, err := b.client.Open(dir)
	if err == nil {
		err = syncFile(d)
		_ = d.Close()
	}
	var status *sftp.StatusError
	if err != nil && !errors.As(err, &status) && !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("arksftp: sync dir %s: %w", dir, err)
	}
	return nil
}

// createTemp opens a fresh temporary file in dir and returns it with its path.
// The name is random rather than derived from the object name: two concurrent
// Puts of the same object would otherwise share a temp file, and the second
// rename would find it already moved away.
func (b *Backend) createTemp(dir string) (*sftp.File, string, error) {
	var name [8]byte
	for range 10 {
		if _, err := rand.Read(name[:]); err != nil {
			return nil, "", fmt.Errorf("arksftp: temp name: %w", err)
		}
		p := path.Join(dir, tmpPrefix+hex.EncodeToString(name[:]))
		f, err := b.client.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err == nil {
			return f, p, nil
		}
		if !os.IsExist(err) {
			return nil, "", fmt.Errorf("arksftp: create %s: %w", p, err)
		}
	}
	return nil, "", fmt.Errorf("arksftp: no free temporary name in %s", dir)
}

// renameAttempts bounds the remove-and-rename retries of [Backend.rename]. A
// retry is only needed when another Put of the same name landed in between,
// and each such Put lands once, so this is the number of concurrent writers of
// one name the plain-rename path tolerates.
const renameAttempts = 16

// rename moves tmp onto p. It prefers posix-rename@openssh.com, which replaces
// an existing target atomically as the Backend contract requires. A server
// that does not announce the extension gets a plain SFTP v3 rename, which
// fails with SSH_FX_FAILURE when the target exists. The rename is tried first,
// so storing a new object never removes anything; only that failure removes p
// and tries again, and every nil return means our bytes landed at p.
//
// ponytail: replacing an existing object without the extension removes it
// first, so a reader sees it missing for one round trip, and a crash or a
// dropped link in that round trip loses it. SFTP v3 has no atomic replace;
// a server with posix-rename closes the window.
func (b *Backend) rename(tmp, p string) error {
	if b.posixRename {
		return b.client.PosixRename(tmp, p)
	}
	err := b.client.Rename(tmp, p)
	for range renameAttempts {
		var status *sftp.StatusError
		if err == nil || !errors.As(err, &status) || status.FxCode() != sftp.ErrSSHFxFailure {
			return err
		}
		if rerr := b.client.Remove(p); rerr != nil && !os.IsNotExist(rerr) {
			return rerr
		}
		err = b.client.Rename(tmp, p)
	}
	return err
}

// Get returns a ReadCloser for the object stored under h.
// Callers must close the returned reader.
func (b *Backend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := b.open(h)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// GetRange reads length bytes of the object stored under h at offset with
// ReadAt on one handle, so the bytes before offset never cross the wire. It
// returns as soon as ctx ends, see [awaitRead].
func (b *Backend) GetRange(ctx context.Context, h ark.Handle, offset, length int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := b.remotePath(h); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 {
		return nil, fmt.Errorf("arksftp: invalid range %d+%d of %s", offset, length, h.Name)
	}
	return awaitRead(ctx, func() ([]byte, error) {
		f, err := b.open(h)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		buf, err := rangeread.Read(f, offset, length)
		if err != nil {
			return nil, fmt.Errorf("arksftp: range %d+%d of %s: %w", offset, length, h.Name, err)
		}
		return buf, nil
	})
}

// awaitRead runs read and returns its result, or ctx's error as soon as ctx
// ends. The sftp client cannot abort a request in flight, so a read left
// behind that way ends on its own or with the connection.
func awaitRead(ctx context.Context, read func() ([]byte, error)) ([]byte, error) {
	type result struct {
		buf []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		buf, err := read()
		done <- result{buf, err}
	}()
	select {
	case r := <-done:
		return r.buf, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// open opens the object stored under h for reading.
func (b *Backend) open(h ark.Handle) (*sftp.File, error) {
	p, err := b.remotePath(h)
	if err != nil {
		return nil, err
	}
	f, err := b.client.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ark.ErrNotFound, h.Name)
		}
		return nil, fmt.Errorf("arksftp: open %s: %w", p, err)
	}
	return f, nil
}

// Exists reports whether the object identified by h is present.
func (b *Backend) Exists(ctx context.Context, h ark.Handle) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	p, err := b.remotePath(h)
	if err != nil {
		return false, err
	}

	if _, err := b.client.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("arksftp: stat %s: %w", h.Name, err)
	}

	return true, nil
}

// Sync is a no-op: Put and Delete already fsync every object and directory
// they touch on the server (with fsync@openssh.com). A deferred flush would
// cost an open, fsync and close round trip per object, more than it saves.
func (b *Backend) Sync(ctx context.Context) error { return ctx.Err() }

// Delete removes the object identified by h. Removing a non-existent object
// is not an error.
func (b *Backend) Delete(ctx context.Context, h ark.Handle) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	p, err := b.remotePath(h)
	if err != nil {
		return err
	}

	if err := b.client.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("arksftp: remove %s: %w", h.Name, err)
	}
	// Sync even when the object was already gone: an earlier Delete may have
	// died between its remove and its sync. Forget relies on a removed
	// snapshot staying removed before prune deletes its chunks.
	if err := b.syncDir(path.Dir(p)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// List walks the namespace of t and calls fn for every object it holds.
// An absent namespace is empty, not an error.
func (b *Backend) List(ctx context.Context, t ark.HandleType, fn func(ark.BlobInfo) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	dir, err := b.namespace(t)
	if err != nil {
		return err
	}

	if _, err := b.client.Stat(dir); os.IsNotExist(err) {
		return nil
	}

	walker := b.client.Walk(dir)
	for walker.Step() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := walker.Err(); err != nil {
			return fmt.Errorf("arksftp: walk %s: %w", dir, err)
		}
		st := walker.Stat()
		if st.IsDir() || strings.HasPrefix(st.Name(), tmpPrefix) {
			continue
		}
		if err := fn(ark.BlobInfo{Name: st.Name(), Size: st.Size()}); err != nil {
			return err
		}
	}

	return nil
}

// Stat returns metadata for the object stored under h.
func (b *Backend) Stat(ctx context.Context, h ark.Handle) (ark.BlobInfo, error) {
	if err := ctx.Err(); err != nil {
		return ark.BlobInfo{}, err
	}

	p, err := b.remotePath(h)
	if err != nil {
		return ark.BlobInfo{}, err
	}

	fi, err := b.client.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return ark.BlobInfo{}, fmt.Errorf("%w: %s", ark.ErrNotFound, h.Name)
		}
		return ark.BlobInfo{}, fmt.Errorf("arksftp: stat %s: %w", h.Name, err)
	}

	return ark.BlobInfo{Name: h.Name, Size: fi.Size()}, nil
}

// --- path helpers ---

// namespace returns the remote directory holding objects of type t.
// [path.Join] (not [filepath.Join]) is used because SFTP remote paths are always
// forward-slash regardless of the client OS.
func (b *Backend) namespace(t ark.HandleType) (string, error) {
	ns := t.String()
	if ns == ark.UnknownNamespace {
		return "", fmt.Errorf("arksftp: unknown handle type %d", t)
	}
	return path.Join(b.basePath, ns), nil
}

// remotePath maps a Handle to its absolute path on the remote server. Names are
// opaque object names, never paths, so anything that could escape the namespace
// is rejected. Packs are sharded one level: datapacks/aa/<name>.
func (b *Backend) remotePath(h ark.Handle) (string, error) {
	dir, err := b.namespace(h.Type)
	if err != nil {
		return "", err
	}
	if h.Name == "" || h.Name == "." || h.Name == ".." ||
		strings.ContainsAny(h.Name, `/\`) || strings.HasPrefix(h.Name, tmpPrefix) {
		return "", fmt.Errorf("arksftp: invalid object name %q", h.Name)
	}
	if (h.Type == ark.DataPackFile || h.Type == ark.TreePackFile) && len(h.Name) >= 2 {
		return path.Join(dir, h.Name[:2], h.Name), nil
	}
	return path.Join(dir, h.Name), nil
}
