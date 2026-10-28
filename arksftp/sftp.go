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
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// tmpPrefix marks in-flight objects written by Put. List skips them.
const tmpPrefix = ".ark-tmp-"

// Compile-time check that *Backend satisfies ark.Backend.
var _ ark.Backend = (*Backend)(nil)

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

	return &Backend{
		client:      client,
		conn:        conn,
		basePath:    cfg.Path,
		posixRename: posixRename,
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
	if err := b.client.MkdirAll(dir); err != nil {
		return fmt.Errorf("arksftp: mkdirall %s: %w", dir, err)
	}

	f, tmp, err := b.createTemp(dir)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = b.client.Remove(tmp) // no-op once the rename succeeded
	}()

	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("arksftp: write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("arksftp: close %s: %w", tmp, err)
	}
	if err := b.rename(tmp, p); err != nil {
		return fmt.Errorf("arksftp: rename %s: %w", p, err)
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

// rename moves tmp onto p. It prefers posix-rename@openssh.com, which replaces
// an existing target atomically as the Backend contract requires. A server
// that does not announce the extension gets a plain rename, which has to
// remove the target first: a concurrent reader can then observe the object as
// missing for the width of one round trip. Put only ever writes identical
// content under a given name, so that window costs a retry, never wrong data.
func (b *Backend) rename(tmp, p string) error {
	if b.posixRename {
		return b.client.PosixRename(tmp, p)
	}
	if err := b.client.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return b.client.Rename(tmp, p)
}

// Get returns a ReadCloser for the object stored under h.
// Callers must close the returned reader.
func (b *Backend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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
// is rejected. Chunks are sharded two levels: chunks/aa/bb/<name>.
func (b *Backend) remotePath(h ark.Handle) (string, error) {
	dir, err := b.namespace(h.Type)
	if err != nil {
		return "", err
	}
	if h.Name == "" || h.Name == "." || h.Name == ".." ||
		strings.ContainsAny(h.Name, `/\`) || strings.HasPrefix(h.Name, tmpPrefix) {
		return "", fmt.Errorf("arksftp: invalid object name %q", h.Name)
	}
	if h.Type == ark.BlobFile && len(h.Name) >= 4 {
		return path.Join(dir, h.Name[:2], h.Name[2:4], h.Name), nil
	}
	return path.Join(dir, h.Name), nil
}
