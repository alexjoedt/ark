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
	"log/slog"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/internal/rangeread"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// tmpPrefix marks in-flight objects written by Put. List skips them, as it
// skips every name ark.CheckName refuses.
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
	// noDirSync is set when the server refused to open a directory of the
	// repository before it opened any; directory syncs are then skipped for
	// the rest of the backend's life.
	noDirSync atomic.Bool
	// dirOpened is set once a directory opened for fsync: the server
	// supports that, and a later refusal is a failure.
	dirOpened atomic.Bool
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

	return newBackend(client, conn, cfg), nil
}

// newBackend wraps a connected client, recording the extensions the server
// announced and warning once when it has no fsync.
func newBackend(client *sftp.Client, conn *ssh.Client, cfg Config) *Backend {
	_, posixRename := client.HasExtension("posix-rename@openssh.com")
	_, fsync := client.HasExtension("fsync@openssh.com")
	if !fsync {
		slog.Warn("arksftp: the server has no fsync@openssh.com, Sync is no durability barrier "+
			"and a power loss on the server can drop objects Put returned", "host", cfg.Host)
	}

	return &Backend{
		client:      client,
		conn:        conn,
		basePath:    cfg.Path,
		posixRename: posixRename,
		fsync:       fsync,
	}
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
// server that refuses to open a directory of the repository with a status,
// before it opened any, does not support directory syncs: that warns once and
// turns them off for this backend. Once a directory has opened, such a
// refusal is an error. An ancestor of the repository the user may not read is
// skipped by itself. A directory that is gone has nothing to flush. A failed
// fsync on a directory that opened is an error.
func (b *Backend) syncDir(dir string) error {
	if !b.fsync || b.noDirSync.Load() {
		return nil
	}
	d, err := b.client.Open(dir)
	if err != nil {
		var status *sftp.StatusError
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil
		case !errors.As(err, &status) && !errors.Is(err, os.ErrPermission):
			// not a refusal by the server: the transport failed
		case !b.inRepo(dir):
			return nil
		case !b.dirOpened.Load():
			if b.noDirSync.CompareAndSwap(false, true) {
				slog.Warn("arksftp: the server refuses to open a directory for fsync, "+
					"directory syncs are off and renames are not durable on a power loss",
					"dir", dir, "err", err)
			}
			return nil
		}
		return fmt.Errorf("arksftp: sync dir %s: %w", dir, err)
	}
	b.dirOpened.Store(true)
	err = syncFile(d)
	_ = d.Close()
	if err != nil {
		return fmt.Errorf("arksftp: sync dir %s: %w", dir, err)
	}
	return nil
}

// inRepo reports whether dir is the repository root or lies below it.
func (b *Backend) inRepo(dir string) bool {
	base := path.Clean(b.basePath)
	return dir == base || base == "." || strings.HasPrefix(dir, strings.TrimSuffix(base, "/")+"/")
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
// Without the extension it still returns nil; New warns about that once.
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

// List reads the namespace of t and calls fn for every object it holds: the
// files of a flat namespace, or the files in each shard dir of a pack
// namespace. Symlinks to a namespace, a shard or an object are followed.
// Entries outside that layout are skipped. An absent namespace is empty, as is
// an absent root, which the first Put creates, unless a dangling link is in the
// way (see absent); a dir or an entry that cannot be read fails the listing,
// so it is never partial.
func (b *Backend) List(ctx context.Context, t ark.HandleType, fn func(ark.BlobInfo) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	dir, err := b.namespace(t)
	if err != nil {
		return err
	}

	entries, err := b.client.ReadDir(dir)
	switch {
	case err == nil:
		err = b.listDir(ctx, dir, entries, t == ark.DataPackFile || t == ark.TreePackFile, "", fn)
	case os.IsNotExist(err):
		// The namespace was never written, nor the root before the first Put.
		if gone, aerr := b.absent(dir); aerr != nil {
			err = aerr
		} else if gone {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("arksftp: list %s: %w", t, err)
	}
	return nil
}

// listDir calls fn for every object among the entries of dir. With sharded,
// dir is a pack namespace: its two-character dirs are shards, listed in turn,
// and only a name shorter than a shard is an object there (see remotePath). An
// object in a shard starts with the shard's name.
func (b *Backend) listDir(ctx context.Context, dir string, entries []os.FileInfo, sharded bool, shard string,
	fn func(ark.BlobInfo) error,
) error {
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		// Temporary objects and files another tool leaves, such as a
		// .DS_Store, are no objects: a caller could not read or delete them.
		if ark.CheckName(name) != nil {
			skip(dir, name)
			continue
		}
		info, err := b.follow(path.Join(dir, name), e)
		switch {
		case err != nil:
			return err
		case info == nil && sharded && len(name) == 2:
			// A shard that vanished mid-listing leaves it partial: a caller
			// would take the objects it held as not stored.
			return fmt.Errorf("%s vanished while listing: %w", path.Join(dir, name), os.ErrNotExist)
		case info == nil:
			// deleted since the dir was read
		case info.IsDir() && sharded && len(name) == 2:
			sub := path.Join(dir, name)
			objects, err := b.client.ReadDir(sub)
			if err != nil {
				return fmt.Errorf("read %s: %w", sub, err)
			}
			if err := b.listDir(ctx, sub, objects, false, name, fn); err != nil {
				return err
			}
		case info.Mode().IsRegular() && strings.HasPrefix(name, shard) && (!sharded || len(name) < 2):
			if err := fn(ark.BlobInfo{Name: name, Size: info.Size()}); err != nil {
				return err
			}
		default:
			skip(dir, name)
		}
	}
	return nil
}

// follow returns the file info of e at p, through a symlink, or nil when the
// entry is gone. A link whose target is gone fails: it may lead to a disk that
// is not mounted, whose objects are stored all the same.
func (b *Backend) follow(p string, e os.FileInfo) (os.FileInfo, error) {
	if e.Mode()&os.ModeSymlink == 0 {
		return e, nil
	}
	info, err := b.client.Stat(p)
	if os.IsNotExist(err) {
		gone, err := b.absent(p)
		if err != nil || gone {
			return nil, err
		}
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", p, err)
	}
	return info, nil
}

// absent reports whether p, which a call just found missing, is plainly not
// there: the deepest part of it that is there, p or an ancestor, is no link
// whose target is gone. Such a link may lead to a disk that is not mounted,
// whose objects are stored all the same. Any other error is returned.
func (b *Backend) absent(p string) (bool, error) {
	for {
		fi, err := b.client.Lstat(p)
		switch {
		case err == nil && fi.Mode()&os.ModeSymlink == 0:
			return true, nil
		case err == nil:
			if _, err := b.client.Stat(p); os.IsNotExist(err) {
				return false, nil
			} else if err != nil {
				return false, fmt.Errorf("stat %s: %w", p, err)
			}
			return true, nil
		case !os.IsNotExist(err):
			return false, fmt.Errorf("lstat %s: %w", p, err)
		case path.Dir(p) == p:
			return true, nil
		}
		p = path.Dir(p)
	}
}

// skip logs an entry List passes over because it is outside the layout.
func skip(dir, name string) {
	slog.Debug("arksftp: skipping an entry outside the repository layout", "dir", dir, "name", name)
}

// Empty reports whether the repository directory is missing or holds no
// entries at all, so a caller can refuse to initialize over existing data. A
// root behind a dangling link is not missing (see absent): Empty fails.
func (b *Backend) Empty(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	entries, err := b.client.ReadDir(b.basePath)
	if os.IsNotExist(err) {
		if gone, aerr := b.absent(b.basePath); aerr != nil {
			err = aerr
		} else if gone {
			return true, nil
		}
	}
	if err != nil {
		return false, fmt.Errorf("arksftp: read %s: %w", b.basePath, err)
	}
	return len(entries) == 0, nil
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
	if err := ark.CheckName(h.Name); err != nil {
		return "", fmt.Errorf("arksftp: %s: %w", h.Type, err)
	}
	if (h.Type == ark.DataPackFile || h.Type == ark.TreePackFile) && len(h.Name) >= 2 {
		return path.Join(dir, h.Name[:2], h.Name), nil
	}
	return path.Join(dir, h.Name), nil
}
