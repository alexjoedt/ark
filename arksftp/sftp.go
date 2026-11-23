// Package arksftp implements [ark.Backend] over an SFTP connection.
//
// One Backend owns one SSH connection and one SFTP client. Requests are
// multiplexed over that single connection; there is no pool. The Repository
// concurrency bound (ark.WithConcurrency, [runtime.NumCPU] times two by
// default) caps how many requests are in flight, so the caller sizes the load
// on that one connection.
//
// Every call returns when its context ends, also on a link that stopped
// answering, and a keepalive closes a connection nothing arrives on any more.
// See [Backend].
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
	"slices"
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
	// keepaliveInterval is how often the backend pings the server.
	keepaliveInterval = 15 * time.Second
	// keepaliveMisses is how many intervals in a row a ping may stay
	// unanswered, with nothing else arriving from the server, before the
	// connection counts as lost.
	keepaliveMisses = 3
	// closeWait bounds how long Close waits for the server to end the session.
	closeWait = 2 * time.Second
	// pieceSize is how much of an object one request of a Put or of a Get
	// reader moves: 64 packets of 32 KiB, what the sftp client keeps in flight.
	pieceSize = 2 << 20
)

// errLost is in the error of every call once the keepalive has closed the
// connection, with the reason behind it.
var errLost = errors.New("arksftp: connection lost")

// errSilent is the reason of a connection given up after keepaliveMisses.
var errSilent = fmt.Errorf("%w: the server stopped answering", errLost)

// Backend implements ark.Backend backed by an SFTP server.
// All methods are safe for concurrent use via the underlying SSH/SFTP
// multiplexing.
//
// A call returns its context's error as soon as the context ends. The sftp
// client cannot abort a request in flight, so the one request the call was
// waiting on stays behind until the server answers it or the connection
// closes: a Delete may still remove its object, and a Put may leave a
// temporary object, which no listing shows. A Put that returned the context's
// error stores nothing later, unless the context ended while it renamed the
// object into place.
//
// The backend pings the server every 15 s. Three intervals in a row with a
// ping unanswered and nothing else arriving from the server close the
// connection, and so does a ping the connection fails: the calls in flight and
// every later call fail with an error that names the lost connection. The
// backend never reconnects.
type Backend struct {
	client *sftp.Client
	// conn is the transport under client, an *ssh.Client outside tests.
	conn     io.Closer
	basePath string
	// lost is closed when the keepalive gave the connection up, with lostErr
	// set before.
	lost     chan struct{}
	lostErr  error
	lostOnce sync.Once
	// closed is closed by Close and ends the keepalive.
	closed    chan struct{}
	closeOnce sync.Once
	// pinging is set while a keepalive ping waits for its answer.
	pinging atomic.Bool
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
	// noChmod is set once the server refused to set a directory mode, so
	// the warning shows once.
	noChmod atomic.Bool
	// dirOpened is set once a directory opened for fsync: the server
	// supports that, and a later refusal is a failure.
	dirOpened atomic.Bool
	// durable holds directories whose entries up to the root this process
	// has fsynced, so Put pays the parent syncs once per directory.
	durable sync.Map
}

// New connects to the SFTP server described by cfg and returns a ready Backend.
// The caller closes it with [Backend.Close].
func New(cfg Config) (*Backend, error) { return connect(cfg, keepaliveInterval) }

// connect is New with the keepalive interval of its choice.
func connect(cfg Config, interval time.Duration) (*Backend, error) {
	cfg = resolve(cfg, loadSSHConfig())

	sshCfg, chain, err := buildSSHConfig(cfg)
	if err != nil {
		return nil, err
	}
	// The agent socket is only needed for the handshake, which ssh.Dial
	// completes before it returns.
	defer chain.close()

	// ssh.Dial, with the bytes that arrive counted for the keepalive.
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	tcp, err := net.DialTimeout("tcp", addr, sshCfg.Timeout)
	if err != nil {
		return nil, fmt.Errorf("arksftp: dial %s (%s): %w", addr, chain.describe(), err)
	}
	link := &countConn{Conn: tcp}
	sshConn, chans, reqs, err := ssh.NewClientConn(link, addr, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("arksftp: dial %s (%s): %w", addr, chain.describe(), err)
	}
	conn := ssh.NewClient(sshConn, chans, reqs)

	// Concurrent writes let a single Put stream ahead instead of waiting for
	// one round trip per packet. Safe here because Put owns its temp file.
	client, err := sftp.NewClient(conn, sftp.UseConcurrentWrites(true))
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("arksftp: open sftp subsystem: %w", err)
	}

	b := newBackend(client, conn, cfg)
	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		b.keepalive(func() error {
			// A server that does not know the request refuses it, which is
			// an answer all the same.
			_, _, err := conn.SendRequest("keepalive@openssh.com", true, nil)
			return err
		}, link.n.Load, tick.C)
	}()
	return b, nil
}

// countConn counts the bytes that arrive on a connection.
type countConn struct {
	net.Conn
	n atomic.Int64
}

func (c *countConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// newBackend wraps a connected client, recording the extensions the server
// announced and warning once when it has no fsync.
func newBackend(client *sftp.Client, conn io.Closer, cfg Config) *Backend {
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
		lost:        make(chan struct{}),
		closed:      make(chan struct{}),
	}
}

// keepalive pings the server at every tick, one ping at a time, and gives the
// connection up after keepaliveMisses ticks in a row at which a ping was still
// unanswered and received had not moved. received is the count of bytes that
// arrived from the server: the ping waits behind every write queued on the
// connection, which on a slow uplink takes longer than the misses allow, while
// the server's answers to those writes show that it is there. An answer of
// any kind counts; ping returns an error only when the connection is gone,
// which gives it up at once. It returns when the backend closes or is lost.
func (b *Backend) keepalive(ping func() error, received func() int64, tick <-chan time.Time) {
	var seen int64
	missed := 0
	for {
		select {
		case <-b.closed:
			return
		case <-b.lost:
			return
		case <-tick:
		}
		n := received()
		arrived := n != seen
		seen = n
		switch {
		case b.pinging.CompareAndSwap(false, true):
			missed = 0
			go func() {
				err := ping()
				if err == nil {
					b.pinging.Store(false)
					return
				}
				select {
				case <-b.closed:
					// Close took the connection away.
				default:
					b.giveUp(fmt.Errorf("%w: the connection closed: %w", errLost, err))
				}
			}()
		case arrived:
			missed = 0
		default:
			if missed++; missed == keepaliveMisses {
				b.giveUp(errSilent)
				return
			}
		}
	}
}

// giveUp marks the backend lost for the reason cause, which wraps errLost, and
// closes the transport, which ends every request in flight.
func (b *Backend) giveUp(cause error) {
	b.lostOnce.Do(func() {
		b.lostErr = cause
		close(b.lost)
		_ = b.conn.Close()
	})
}

// await runs call and returns its result, or ctx's error as soon as ctx ends,
// or the reason once the keepalive gave the connection up. The sftp client cannot
// abort a request in flight, so a call left behind that way ends on its own or
// with the connection, and whatever it opened stays open until then. call may
// outlive the wait, so it must not touch what the caller of the backend owns,
// such as the buffer of a Read or the source of a Put.
func await[T any](ctx context.Context, b *Backend, call func() (T, error)) (T, error) {
	var zero T
	// Checked first: on a connection given up call fails at once, and the
	// select below might return that failure in place of the reason.
	select {
	case <-b.lost:
		return zero, b.lostErr
	default:
	}
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := call()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-b.lost:
		return zero, b.lostErr
	}
}

// do is await for a call that returns only an error.
func (b *Backend) do(ctx context.Context, call func() error) error {
	_, err := await(ctx, b, func() (struct{}, error) { return struct{}{}, call() })
	return err
}

// Close closes the SFTP client and the underlying SSH connection. The client
// waits for the server to end the session; on a link that stopped answering
// that wait ends after closeWait, when the connection closes under it. A
// backend whose connection the keepalive gave up closes without an error:
// every call has reported the loss.
func (b *Backend) Close() error {
	b.closeOnce.Do(func() {
		if b.closed != nil {
			close(b.closed)
		}
	})
	select {
	case <-b.lost:
		// The transport is closed, so the client ends without the server.
		_ = b.client.Close()
		_ = b.conn.Close()
		return nil
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeWait)
	defer cancel()
	clientErr := b.do(ctx, b.client.Close)
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
// It is idempotent: an existing object is replaced. r is read on the calling
// goroutine only, and not after Put returned.
func (b *Backend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	p, err := b.remotePath(h)
	if err != nil {
		return err
	}

	dir := path.Dir(p)
	type temp struct {
		f    *sftp.File
		path string
	}
	t, err := await(ctx, b, func() (temp, error) {
		if err := b.mkdirDurable(dir); err != nil {
			return temp{}, err
		}
		f, tmp, err := b.createTemp(dir)
		if err == nil && ctx.Err() != nil {
			// Put has returned and nothing else would take the file away.
			_ = f.Close()
			_ = b.client.Remove(tmp)
			return temp{}, ctx.Err()
		}
		return temp{f, tmp}, err
	})
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(&upload{ctx: ctx, b: b, f: t.f}, r)
	// One call for the rest, so the removal of the temp file after a failure
	// can never overtake the rename of a commit left behind.
	return b.do(ctx, func() error {
		err := copyErr
		if err != nil {
			err = fmt.Errorf("arksftp: write %s: %w", t.path, err)
		} else {
			err = b.commit(ctx, t.f, t.path, p)
		}
		if err != nil {
			_ = t.f.Close()
			_ = b.client.Remove(t.path)
			return err
		}
		return b.syncDir(dir)
	})
}

// commit flushes and closes the temp file f at tmp and renames it onto p.
func (b *Backend) commit(ctx context.Context, f *sftp.File, tmp, p string) error {
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
	// Put has returned when ctx is over and told its caller that the object
	// is not stored: it must not appear after that.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.rename(tmp, p); err != nil {
		return fmt.Errorf("arksftp: rename %s: %w", p, err)
	}
	return nil
}

// upload writes to the temp file of a Put, a piece at a time from a buffer of
// its own: a write that await left behind may still be sending when Put has
// returned and its caller owns the bytes of p again.
//
// ponytail: a source without WriteTo arrives in the 32 KiB reads of [io.Copy],
// one round trip each. Every source ark passes writes itself; a ReadFrom that
// fills whole pieces is the upgrade when another kind shows up.
type upload struct {
	ctx context.Context
	b   *Backend
	f   *sftp.File
	buf []byte
}

func (u *upload) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		// No write was left behind so far, or this one had not been reached:
		// buf is free.
		n := min(len(p)-written, pieceSize)
		if cap(u.buf) < n {
			u.buf = make([]byte, n)
		}
		buf := u.buf[:n]
		copy(buf, p[written:])
		n, err := await(u.ctx, u.b, func() (int, error) { return u.f.Write(buf) })
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
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
	if err := b.mkdirPrivate(dir); err != nil {
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

// mkdirPrivate creates dir and its missing parents like MkdirAll, walking up
// past any Stat error and leaving the real failure to Mkdir. A directory of
// the repository is made private with [Backend.mkdirRepo], because the server
// applies its umask to the plain Mkdir and other users of the server could
// read the key files. One that a concurrent Put created first is made private
// too, since its creator may be an older ark or have had its chmod refused.
// Parents above the repository and directories that already exist keep their
// mode.
func (b *Backend) mkdirPrivate(dir string) error {
	var missing []string
	for d := dir; ; d = path.Dir(d) {
		fi, err := b.client.Stat(d)
		if err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", d)
			}
			break
		}
		missing = append(missing, d)
		if d == path.Dir(d) {
			break
		}
	}
	for _, d := range slices.Backward(missing) {
		mkdir := b.client.Mkdir
		if b.inRepo(d) {
			mkdir = b.mkdirRepo
		}
		if err := mkdir(d); err != nil {
			if fi, serr := b.client.Stat(d); serr != nil || !fi.IsDir() {
				return err
			}
			if b.inRepo(d) {
				if err := b.chmodPrivate(d); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// mkdirRepo creates the repository directory d with mode 0700. It makes the
// directory under a temporary name and renames it into place once private,
// so d never exists with the server's default mode, not even when the link
// or the process dies between the calls. A temporary directory left behind
// is skipped by List like a temporary object.
func (b *Backend) mkdirRepo(d string) error {
	tmp, err := tempName(path.Dir(d))
	if err != nil {
		return err
	}
	if err := b.client.Mkdir(tmp); err != nil {
		return err
	}
	err = b.chmodPrivate(tmp)
	if err == nil {
		err = b.client.Rename(tmp, d)
	}
	if err != nil {
		_ = b.client.RemoveDirectory(tmp)
	}
	return err
}

// chmodPrivate sets mode 0700 on d. A server that refuses with a status, which
// pkg/sftp hands over as [os.ErrPermission] for PERMISSION_DENIED, warns once
// and keeps its default mode. A directory that is gone or a failed transport
// is an error.
func (b *Backend) chmodPrivate(d string) error {
	err := b.client.Chmod(d, 0o700)
	var status *sftp.StatusError
	switch {
	case err == nil:
	case errors.As(err, &status) || errors.Is(err, os.ErrPermission):
		if b.noChmod.CompareAndSwap(false, true) {
			slog.Warn("arksftp: the server refuses to set a directory mode, "+
				"so new directories keep its default mode; chmod go-rwx the repository",
				"dir", d, "err", err)
		}
	default:
		return fmt.Errorf("chmod %s: %w", d, err)
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
	for range 10 {
		p, err := tempName(dir)
		if err != nil {
			return nil, "", err
		}
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

// tempName returns a random temporary path in dir.
func tempName(dir string) (string, error) {
	var name [8]byte
	if _, err := rand.Read(name[:]); err != nil {
		return "", fmt.Errorf("arksftp: temp name: %w", err)
	}
	return path.Join(dir, tmpPrefix+hex.EncodeToString(name[:])), nil
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
// Callers must close the returned reader. Its Read and Close end with ctx like
// every call.
func (b *Backend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := await(ctx, b, func() (*sftp.File, error) { return b.open(h) })
	if err != nil {
		return nil, err
	}
	return &reader{ctx: ctx, b: b, f: f}, nil
}

// reader reads the object of a Get, a piece at a time into a buffer of its
// own: a read that await left behind may still be filling it when Read has
// returned and its caller owns p again.
type reader struct {
	ctx context.Context
	b   *Backend
	f   *sftp.File
	buf []byte
	// eof is set once the object was read to its end.
	eof bool
}

func (r *reader) Read(p []byte) (int, error) {
	// After a read was left behind ctx is over: buf is never used again.
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n := min(len(p), pieceSize)
	if cap(r.buf) < n {
		r.buf = make([]byte, n)
	}
	buf := r.buf[:n]
	n, err := await(r.ctx, r.b, func() (int, error) { return r.f.Read(buf) })
	r.eof = errors.Is(err, io.EOF)
	return copy(p, buf[:n]), err
}

// WriteTo is the path of [io.Copy], which would otherwise Read 32 KiB a round
// trip. It reads pieces that double up to pieceSize, which the sftp client
// fetches with that many requests in flight, so a small object stays at one
// request.
func (r *reader) WriteTo(w io.Writer) (int64, error) {
	var total int64
	for size := 32 << 10; ; size = min(2*size, pieceSize) {
		// As in Read: after a read was left behind buf is never used again.
		if err := r.ctx.Err(); err != nil {
			return total, err
		}
		if cap(r.buf) < size {
			r.buf = make([]byte, size)
		}
		buf := r.buf[:size]
		n, err := await(r.ctx, r.b, func() (int, error) { return r.f.Read(buf) })
		if n > 0 {
			m, werr := w.Write(buf[:n])
			total += int64(m)
			if werr != nil {
				return total, werr
			}
		}
		if errors.Is(err, io.EOF) {
			r.eof = true
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// Close closes the handle on the server. It ends with ctx like every call,
// with one exception: a reader that was read to its end waits up to closeWait
// for the server when ctx is over, so it does not fail because its context
// ended after the last byte arrived.
func (r *reader) Close() error {
	ctx := r.ctx
	if r.eof && ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), closeWait)
		defer cancel()
	}
	return r.b.do(ctx, r.f.Close)
}

// GetRange reads length bytes of the object stored under h at offset with
// ReadAt on one handle, so the bytes before offset never cross the wire.
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
	return await(ctx, b, func() ([]byte, error) {
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

	if _, err := b.stat(ctx, p); err != nil {
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

	return b.do(ctx, func() error {
		if err := b.client.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("arksftp: remove %s: %w", h.Name, err)
		}
		// Sync even when the object was already gone: an earlier Delete may
		// have died between its remove and its sync. Forget relies on a
		// removed snapshot staying removed before prune deletes its chunks.
		if err := b.syncDir(path.Dir(p)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
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

	entries, err := b.readDir(ctx, dir)
	switch {
	case err == nil:
		err = b.listDir(ctx, dir, entries, t == ark.DataPackFile || t == ark.TreePackFile, "", fn)
	case os.IsNotExist(err):
		// The namespace was never written, nor the root before the first Put.
		if gone, aerr := b.absent(ctx, dir); aerr != nil {
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
		info, err := b.follow(ctx, path.Join(dir, name), e)
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
			objects, err := b.readDir(ctx, sub)
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
func (b *Backend) follow(ctx context.Context, p string, e os.FileInfo) (os.FileInfo, error) {
	if e.Mode()&os.ModeSymlink == 0 {
		return e, nil
	}
	info, err := b.stat(ctx, p)
	if os.IsNotExist(err) {
		gone, err := b.absent(ctx, p)
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
func (b *Backend) absent(ctx context.Context, p string) (bool, error) {
	for {
		fi, err := await(ctx, b, func() (os.FileInfo, error) { return b.client.Lstat(p) })
		switch {
		case err == nil && fi.Mode()&os.ModeSymlink == 0:
			return true, nil
		case err == nil:
			if _, err := b.stat(ctx, p); os.IsNotExist(err) {
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

// readDir is the client's ReadDir behind await.
func (b *Backend) readDir(ctx context.Context, dir string) ([]os.FileInfo, error) {
	return await(ctx, b, func() ([]os.FileInfo, error) { return b.client.ReadDir(dir) })
}

// stat is the client's Stat behind await.
func (b *Backend) stat(ctx context.Context, p string) (os.FileInfo, error) {
	return await(ctx, b, func() (os.FileInfo, error) { return b.client.Stat(p) })
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
	entries, err := b.readDir(ctx, b.basePath)
	if os.IsNotExist(err) {
		if gone, aerr := b.absent(ctx, b.basePath); aerr != nil {
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

	fi, err := b.stat(ctx, p)
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

// asidePrefix marks an object a Put renamed aside to replace it (see
// ark.Leftovers).
const asidePrefix = ".ark-old-"

var _ ark.Leftovers = (*Backend)(nil)

// RemoveTemp deletes every temporary file a Put left in a namespace or a
// shard dir, except in the locks namespace (see [ark.Leftovers]), and every
// temporary directory mkdirRepo left in the root or a namespace dir. An entry
// the server refuses to remove, such as another user's in a shared repository
// or a directory that is not empty, is logged and kept.
func (b *Backend) RemoveTemp(ctx context.Context) error {
	locks := ark.LockFile.String() + "/"
	return b.leftovers(ctx, tmpPrefix, func(rel string, dir bool) error {
		if strings.HasPrefix(rel, locks) {
			return nil
		}
		p := path.Join(b.basePath, rel)
		remove := b.client.Remove
		if dir {
			remove = b.client.RemoveDirectory
		}
		err := b.do(ctx, func() error { return remove(p) })
		var status *sftp.StatusError
		switch {
		case err == nil || os.IsNotExist(err):
		case errors.As(err, &status) || errors.Is(err, os.ErrPermission):
			slog.WarnContext(ctx, "arksftp: cannot remove a temporary entry", "path", rel, "err", err)
		default:
			return fmt.Errorf("remove %s: %w", p, err)
		}
		return nil
	})
}

// ListAside calls fn with the path, relative to the root, of every aside file
// in a namespace or a shard dir.
func (b *Backend) ListAside(ctx context.Context, fn func(path string) error) error {
	return b.leftovers(ctx, asidePrefix, func(rel string, dir bool) error {
		if dir {
			return nil
		}
		return fn(rel)
	})
}

// leftovers calls fn with the relative path of every entry whose name starts
// with prefix in the root, a namespace dir or a shard dir, and whether it is
// a directory.
func (b *Backend) leftovers(ctx context.Context, prefix string, fn func(rel string, dir bool) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.leftoversIn(ctx, "", prefix, false, fn); err != nil {
		return fmt.Errorf("arksftp: root: %w", err)
	}
	for t := ark.ConfigFile; t.String() != ark.UnknownNamespace; t++ {
		sharded := t == ark.DataPackFile || t == ark.TreePackFile
		if err := b.leftoversIn(ctx, t.String(), prefix, sharded, fn); err != nil {
			return fmt.Errorf("arksftp: %s: %w", t, err)
		}
	}
	return nil
}

func (b *Backend) leftoversIn(
	ctx context.Context, rel, prefix string, sharded bool, fn func(string, bool) error,
) error {
	dir := path.Join(b.basePath, rel)
	entries, err := b.readDir(ctx, dir)
	if os.IsNotExist(err) {
		if gone, aerr := b.absent(ctx, dir); aerr != nil {
			return aerr
		} else if gone {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		switch {
		case strings.HasPrefix(name, prefix):
			if err := fn(path.Join(rel, name), e.IsDir()); err != nil {
				return err
			}
		case sharded && len(name) == 2:
			info, err := b.follow(ctx, path.Join(dir, name), e)
			if err != nil {
				return err
			}
			if info != nil && info.IsDir() {
				if err := b.leftoversIn(ctx, rel+"/"+name, prefix, false, fn); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
