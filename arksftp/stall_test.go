package arksftp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// cancelWait is how long a call may take to return after its context ended.
const cancelWait = 5 * time.Second

// stallConn is one end of the link between a backend and an in-process
// server. Once it stalls nothing the other end sends arrives, like on a dead
// route.
type stallConn struct {
	net.Conn
	mu sync.Mutex
	// left is how many reads still arrive before the link stalls, negative
	// while nothing is planned.
	left    int
	held    chan struct{} // closed once the stalled link holds a read back
	resumed chan struct{} // closed by the resume that ends the stall
	closed  chan struct{}
	once    sync.Once
}

func newStallConn(conn net.Conn) *stallConn {
	return &stallConn{
		Conn: conn, left: -1,
		held: make(chan struct{}), resumed: make(chan struct{}), closed: make(chan struct{}),
	}
}

func (c *stallConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	for c.left == 0 {
		select {
		case <-c.held:
		default:
			close(c.held)
		}
		resumed := c.resumed
		c.mu.Unlock()
		select {
		case <-resumed:
		case <-c.closed:
			return 0, net.ErrClosed
		}
		c.mu.Lock()
	}
	if c.left > 0 {
		c.left--
	}
	c.mu.Unlock()
	return n, err
}

// stallAfter lets reads more reads arrive and stalls the link then.
func (c *stallConn) stallAfter(reads int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.left = reads
}

// resume ends a stall: the reads held back arrive.
func (c *stallConn) resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.left = -1
	close(c.resumed)
	c.held, c.resumed = make(chan struct{}), make(chan struct{})
}

// stalled returns a channel that closes once the link holds a read back.
func (c *stallConn) stalled() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held
}

func (c *stallConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// stallBackend serves t.TempDir() from an in-process SFTP server over a link
// the test can stall: the backend then hears nothing from the server, and
// since the pipe has no buffer the server soon stops reading too.
func stallBackend(t *testing.T) (*Backend, *stallConn) {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	srv, err := sftp.NewServer(srvConn)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return stallOver(t, srv, srvConn, cliConn, t.TempDir())
}

// sftpServer is what the two servers of pkg/sftp share.
type sftpServer interface {
	Serve() error
	Close() error
}

// stallOver returns a backend on root that talks to srv over a link the test
// can stall.
func stallOver(t *testing.T, srv sftpServer, srvConn, cliConn net.Conn, root string) (*Backend, *stallConn) {
	t.Helper()
	go func() { _ = srv.Serve() }()
	link := newStallConn(cliConn)
	client, err := sftp.NewClientPipe(link, link)
	if err != nil {
		t.Fatalf("NewClientPipe: %v", err)
	}
	b := &Backend{
		client: client, conn: link, basePath: root,
		lost: make(chan struct{}), closed: make(chan struct{}),
	}
	t.Cleanup(func() {
		_ = link.Close()
		_ = b.Close()
		_ = srv.Close()
		_ = srvConn.Close()
	})
	return b, link
}

var stalledObject = ark.Handle{Type: ark.DataPackFile, Name: "aa01"}

// calls is every call of the backend, on a repository stalledBackend made.
var calls = []struct {
	name string
	call func(ctx context.Context, b *Backend) error
}{
	{"Put", func(ctx context.Context, b *Backend) error {
		return b.Put(ctx, ark.Handle{Type: ark.DataPackFile, Name: "bb01"}, strings.NewReader("new"))
	}},
	{"Get, Read and Close", func(ctx context.Context, b *Backend) error {
		rc, err := b.Get(ctx, stalledObject)
		if err != nil {
			return err
		}
		if _, err := io.ReadAll(rc); err != nil {
			return err
		}
		return rc.Close()
	}},
	{"GetRange", func(ctx context.Context, b *Backend) error {
		_, err := b.GetRange(ctx, stalledObject, 0, 1)
		return err
	}},
	{"Stat", func(ctx context.Context, b *Backend) error {
		_, err := b.Stat(ctx, stalledObject)
		return err
	}},
	{"Exists", func(ctx context.Context, b *Backend) error {
		_, err := b.Exists(ctx, stalledObject)
		return err
	}},
	{"List", func(ctx context.Context, b *Backend) error {
		return b.List(ctx, ark.DataPackFile, func(ark.BlobInfo) error { return nil })
	}},
	{"List of a namespace never written", func(ctx context.Context, b *Backend) error {
		return b.List(ctx, ark.LockFile, func(ark.BlobInfo) error { return nil })
	}},
	{"Empty", func(ctx context.Context, b *Backend) error {
		_, err := b.Empty(ctx)
		return err
	}},
	{"Delete", func(ctx context.Context, b *Backend) error {
		return b.Delete(ctx, stalledObject)
	}},
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// stalledBackend returns a backend whose repository holds stalledObject, a
// second object in another shard and a pack behind a symlink.
func stalledBackend(t *testing.T) (*Backend, *stallConn) {
	t.Helper()
	b, link := stallBackend(t)
	linked := ark.Handle{Type: ark.DataPackFile, Name: "cc02"}
	putAll(t, b, stalledObject, ark.Handle{Type: ark.DataPackFile, Name: "cc01"}, linked)
	moveBehindLink(t, filepath.Join(b.basePath, "datapacks", "cc", linked.Name))
	return b, link
}

// cancelOnStall runs call and waits until it ends or the link holds a read
// back. It reports false for a call that ended, which must be without an
// error. A call the stall caught is cancelled and must return its context's
// error within cancelWait.
func cancelOnStall(t *testing.T, link *stallConn, call func(context.Context) error) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stalled := link.stalled()
	done := make(chan error, 1)
	go func() { done <- call(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("before the link stalled = %v", err)
		}
		return false
	case <-stalled:
	case <-time.After(cancelWait):
		t.Fatal("neither ended nor reached the stall")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("on a stalled link = %v, want context.Canceled", err)
		}
	case <-time.After(cancelWait):
		t.Fatalf("has not returned %v after its context ended", cancelWait)
	}
	return true
}

// TestCallsReturnWithTheirContextOnAStalledLink stalls the link at every
// point of every call, one read of the client further each time until the
// call gets through, and pins that the call never outlives its context.
func TestCallsReturnWithTheirContextOnAStalledLink(t *testing.T) {
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			for reads := 0; ; reads++ {
				b, link := stalledBackend(t)
				link.stallAfter(reads)
				if !cancelOnStall(t, link, func(ctx context.Context) error { return c.call(ctx, b) }) {
					// An sftp answer is two reads, its length and its body.
					if reads < 2 {
						t.Errorf("got through on %d reads: the link never stalled", reads)
					}
					return
				}
				if t.Failed() {
					t.Fatalf("stalled after %d reads", reads)
				}
			}
		})
	}
}

// TestPutAndGetMoveAnObjectOfSeveralPieces pins the piece loops of Put and
// of a Get reader, which a small object never enters.
func TestPutAndGetMoveAnObjectOfSeveralPieces(t *testing.T) {
	b, _ := stallBackend(t)
	want := make([]byte, 2*pieceSize+1)
	for i := range want {
		want[i] = byte(i / 251)
	}
	if err := b.Put(t.Context(), stalledObject, bytes.NewReader(want)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := b.Get(t.Context(), stalledObject)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got := make([]byte, len(want)+1)
	n, err := io.ReadFull(rc, got)
	if !errors.Is(err, io.ErrUnexpectedEOF) || !bytes.Equal(got[:n], want) {
		t.Errorf("read %d bytes, %v; want the %d bytes stored", n, err, len(want))
	}

	// io.Copy takes the reader's WriteTo, which reads whole pieces.
	rc, err = b.Get(t.Context(), stalledObject)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	var copied bytes.Buffer
	if _, ok := rc.(io.WriterTo); !ok {
		t.Error("the reader of a Get has no WriteTo")
	}
	if n, err := io.Copy(&copied, rc); err != nil || !bytes.Equal(copied.Bytes(), want) {
		t.Errorf("copied %d bytes, %v; want the %d bytes stored", n, err, len(want))
	}
}

// TestGetReaderReadToItsEndClosesWithoutErrorAfterItsContext pins that a
// context that ends after the last byte arrived does not fail the Close.
func TestGetReaderReadToItsEndClosesWithoutErrorAfterItsContext(t *testing.T) {
	b, _ := stalledBackend(t)
	for _, read := range []func(io.Reader) error{
		func(r io.Reader) error { _, err := io.ReadAll(r); return err },
		func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err },
	} {
		ctx, cancel := context.WithCancel(t.Context())
		rc, err := b.Get(ctx, stalledObject)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if err := read(rc); err != nil {
			t.Fatalf("read: %v", err)
		}
		cancel()
		if err := rc.Close(); err != nil {
			t.Errorf("Close after the context ended = %v, want nil", err)
		}
	}
}

// TestPutLeavesItsSourceAloneOnceItReturned pins that a Put cancelled while it
// uploads reads nothing from its source afterwards, also when the link comes
// back and the write it left behind completes, and that it stores nothing.
func TestPutLeavesItsSourceAloneOnceItReturned(t *testing.T) {
	b, link := stalledBackend(t)
	h := ark.Handle{Type: ark.DataPackFile, Name: "bb01"}
	var returned, late atomic.Bool
	stalled := cancelOnStall(t, link, func(ctx context.Context) error {
		defer returned.Store(true)
		return b.Put(ctx, h, io.LimitReader(readerFunc(func(p []byte) (int, error) {
			if returned.Load() {
				late.Store(true)
			}
			link.stallAfter(0)
			return len(p), nil
		}), 8*pieceSize))
	})
	if !stalled {
		t.Fatal("Put got through on a stalled link")
	}

	link.resume()
	// Every request left behind was sent before these and is answered first.
	for range 3 {
		if ok, err := b.Exists(t.Context(), h); err != nil || ok {
			t.Fatalf("after the cancelled Put Exists = %v, %v, want false, nil", ok, err)
		}
	}
	if late.Load() {
		t.Error("the source was read after Put returned")
	}
	if got := listNames(t, b, ark.DataPackFile); len(got) != 3 {
		t.Errorf("List = %v, want the three objects stored before", got)
	}
}

// sftpRecorder is an in-memory sftp server that tells the test which renames and
// removes it was asked for and whether it was sent a byte of marker.
type sftpRecorder struct {
	sftp.FileCmder
	sftp.FileWriter
	cmds   chan string
	marked atomic.Bool
}

const marker = 0xbb

func (r *sftpRecorder) Filecmd(req *sftp.Request) error {
	if req.Method == "Rename" || req.Method == "Remove" {
		r.cmds <- req.Method
	}
	return r.FileCmder.Filecmd(req)
}

func (r *sftpRecorder) Filewrite(req *sftp.Request) (io.WriterAt, error) {
	if _, err := r.FileWriter.Filewrite(req); err != nil {
		return nil, err
	}
	return markedWriter{r}, nil
}

// markedWriter drops what it is sent: the in-memory file of pkg/sftp sleeps
// a microsecond per byte, and no test here reads an object back.
type markedWriter struct{ r *sftpRecorder }

func (w markedWriter) WriteAt(p []byte, _ int64) (int, error) {
	if bytes.IndexByte(p, marker) >= 0 {
		w.r.marked.Store(true)
	}
	return len(p), nil
}

func recordedBackend(t *testing.T) (*Backend, *stallConn, *sftpRecorder) {
	t.Helper()
	srvConn, cliConn := net.Pipe()
	hs := sftp.InMemHandler()
	rec := &sftpRecorder{FileCmder: hs.FileCmd, FileWriter: hs.FilePut, cmds: make(chan string, 64)}
	hs.FileCmd, hs.FilePut = rec, rec
	b, link := stallOver(t, sftp.NewRequestServer(srvConn, hs), srvConn, cliConn, "/repo")
	return b, link, rec
}

// next returns the rename or remove the server was asked for, waiting up to
// wait for one, or "" when there is none.
func (r *sftpRecorder) next(wait time.Duration) string {
	select {
	case cmd := <-r.cmds:
		return cmd
	default:
	}
	select {
	case cmd := <-r.cmds:
		return cmd
	case <-time.After(wait):
		return ""
	}
}

// TestPutCancelledBeforeItsRenameStoresNothingLater stalls a Put at every
// point and cancels it there. Unless the rename was already with the server,
// the Put it left behind must not send one when the link comes back.
func TestPutCancelledBeforeItsRenameStoresNothingLater(t *testing.T) {
	h := ark.Handle{Type: ark.DataPackFile, Name: "bb01"}
	for reads := 0; ; reads++ {
		b, link, rec := recordedBackend(t)
		link.stallAfter(reads)
		stalled := cancelOnStall(t, link, func(ctx context.Context) error {
			return b.Put(ctx, h, strings.NewReader("new"))
		})
		if !stalled {
			if cmd := rec.next(0); cmd != "Rename" {
				t.Errorf("a Put that got through sent %q, want a rename", cmd)
			}
			return
		}
		if rec.next(0) == "Rename" {
			// The answer to the rename is what the link held back.
			continue
		}
		link.resume()
		// What was left behind removes the temp file it had created; it
		// had created none when the stall came before that.
		if cmd := rec.next(30 * time.Millisecond); cmd == "Rename" {
			t.Fatalf("stalled after %d reads: the cancelled Put renamed its object into place", reads)
		}
		if ok, err := b.Exists(t.Context(), h); err != nil || ok {
			t.Fatalf("stalled after %d reads: Exists = %v, %v, want false, nil", reads, ok, err)
		}
		// Also when the answer the link held back was the one that created it.
		for start := time.Now(); len(tempFiles(t, b, h)) > 0; time.Sleep(time.Millisecond) {
			if time.Since(start) > cancelWait {
				t.Fatalf("stalled after %d reads: the cancelled Put left %v", reads, tempFiles(t, b, h))
			}
		}
	}
}

// tempFiles returns the temp files of a Put in the directory of h.
func tempFiles(t *testing.T, b *Backend, h ark.Handle) []string {
	t.Helper()
	p, err := b.remotePath(h)
	if err != nil {
		t.Fatalf("remotePath: %v", err)
	}
	entries, err := b.client.ReadDir(path.Dir(p))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadDir: %v", err)
	}
	var temps []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tmpPrefix) {
			temps = append(temps, e.Name())
		}
	}
	return temps
}

// TestPutSendsNothingItsCallerWroteAfterItReturned pins that a Put cancelled
// in the middle of a piece sends the rest of that piece from its own copy:
// the caller owns its buffer again and may fill it with something else.
func TestPutSendsNothingItsCallerWroteAfterItReturned(t *testing.T) {
	b, link, rec := recordedBackend(t)
	data := make([]byte, pieceSize)
	stalled := cancelOnStall(t, link, func(ctx context.Context) error {
		return b.Put(ctx, ark.Handle{Type: ark.DataPackFile, Name: "bb01"}, io.MultiReader(
			readerFunc(func([]byte) (int, error) {
				link.stallAfter(0)
				return 0, io.EOF
			}),
			bytes.NewReader(data),
		))
	})
	if !stalled {
		t.Fatal("Put got through on a stalled link")
	}
	for i := range data {
		data[i] = marker
	}
	link.resume()
	if cmd := rec.next(cancelWait); cmd != "Remove" {
		t.Fatalf("the Put left behind sent %q, want the removal of its temp file", cmd)
	}
	if rec.marked.Load() {
		t.Error("the server received bytes written to the source after Put returned")
	}
}

// TestKeepaliveGivesUpAfterThreeMissedTicks drives the keepalive by hand: an
// answered ping keeps the connection, a ping unanswered three ticks later
// closes it, an answer or anything else from the server in between starts the
// count again, and every call fails from then on.
func TestKeepaliveGivesUpAfterThreeMissedTicks(t *testing.T) {
	b, link := stalledBackend(t)
	tick := make(chan time.Time)
	pinged := make(chan chan error)
	ended := make(chan struct{})
	// received is what the keepalive reads at a tick, once per tick.
	received := make(chan int64)
	go func() {
		defer close(ended)
		b.keepalive(func() error {
			answer := make(chan error)
			pinged <- answer
			return <-answer
		}, func() int64 { return <-received }, tick)
	}()
	// tickOnce ticks once, with or without bytes arrived from the server since
	// the last tick, and reports whether the keepalive took the tick.
	var count int64
	tickOnce := func(arrived bool) bool {
		select {
		case tick <- time.Time{}:
		case <-ended:
			return false
		}
		if arrived {
			count++
		}
		received <- count
		return true
	}
	send := func() bool { return tickOnce(false) }
	ping := func() chan error {
		t.Helper()
		send()
		select {
		case answer := <-pinged:
			return answer
		case <-time.After(cancelWait):
			t.Fatal("a tick with no ping in flight sent no ping")
			return nil
		}
	}
	answer := func(c chan error) {
		t.Helper()
		c <- nil
		for start := time.Now(); b.pinging.Load(); {
			if time.Since(start) > cancelWait {
				t.Fatal("the answer never reached the keepalive")
			}
			runtime.Gosched()
		}
	}

	for range 2 * keepaliveMisses {
		answer(ping())
	}
	late := ping()
	for range keepaliveMisses - 1 {
		if !send() {
			t.Fatal("gave up before the last missed tick")
		}
	}
	answer(late)
	if _, err := b.Stat(t.Context(), stalledObject); err != nil {
		t.Fatalf("Stat after answered pings = %v", err)
	}

	unanswered := ping()
	// A ping that waits behind an upload: the server's answers to the writes
	// still arrive.
	for range 2 * keepaliveMisses {
		for range keepaliveMisses - 1 {
			if !send() {
				t.Fatal("gave up before the last missed tick")
			}
		}
		if !tickOnce(true) {
			t.Fatal("gave up on a server it still hears from")
		}
	}
	misses := 0
	for send() {
		misses++
	}
	if misses != keepaliveMisses {
		t.Errorf("gave up after %d missed ticks, want %d", misses, keepaliveMisses)
	}
	select {
	case <-link.closed:
	default:
		t.Error("the transport is still open")
	}
	close(unanswered)

	for _, c := range calls {
		err := c.call(t.Context(), b)
		if !errors.Is(err, errLost) || !strings.Contains(err.Error(), "the server stopped answering") {
			t.Errorf("%s after the connection was given up = %v, want errLost", c.name, err)
		}
	}
	if err := b.Close(); err != nil {
		t.Errorf("Close after the connection was given up = %v, want nil", err)
	}
}

// TestKeepaliveGivesUpAtOnceWhenThePingFails pins that a connection that
// fails its ping is lost without the wait for missed ticks, and that the
// error names the failure, not a silent server.
func TestKeepaliveGivesUpAtOnceWhenThePingFails(t *testing.T) {
	b, link := stalledBackend(t)
	tick := make(chan time.Time)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		b.keepalive(func() error { return io.ErrClosedPipe }, func() int64 { return 0 }, tick)
	}()
	tick <- time.Time{}
	select {
	case <-ended:
	case <-time.After(cancelWait):
		t.Fatal("a failed ping did not end the keepalive")
	}
	select {
	case <-link.closed:
	default:
		t.Error("the transport is still open")
	}
	_, err := b.Stat(t.Context(), stalledObject)
	if !errors.Is(err, errLost) || !errors.Is(err, io.ErrClosedPipe) || errors.Is(err, errSilent) {
		t.Errorf("Stat after a failed ping = %v, want errLost with the ping's error", err)
	}
	if err := b.Close(); err != nil {
		t.Errorf("Close after the connection was given up = %v, want nil", err)
	}
}

// TestKeepaliveLeavesAClosedBackendAlone pins that the ping Close fails does
// not mark the backend lost.
func TestKeepaliveLeavesAClosedBackendAlone(t *testing.T) {
	b, _ := stalledBackend(t)
	tick := make(chan time.Time)
	pinged, failed := make(chan struct{}), make(chan struct{})
	go b.keepalive(func() error {
		close(pinged)
		<-b.closed
		defer close(failed)
		return io.ErrClosedPipe
	}, func() int64 { return 0 }, tick)
	tick <- time.Time{}
	<-pinged
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-failed
	// The ping's goroutine has nothing left to do but to give up or not.
	time.Sleep(10 * time.Millisecond)
	select {
	case <-b.lost:
		t.Error("the ping that Close failed marked the backend lost")
	default:
	}
}

// sshServer is an in-process ssh server with an sftp subsystem on the local
// filesystem, for one connection. It answers global requests, the client's
// keepalive among them, while answer is set, and counts those.
type sshServer struct {
	addr     string
	answer   atomic.Bool
	answered atomic.Int32
	// link is the server's end of the connection, once there is one.
	link chan *stallConn
	stop func()
}

func serveSFTP(t *testing.T) *sshServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg := acceptAnyKey()
	cfg.AddHostKey(newSigner(t))
	s := &sshServer{addr: ln.Addr().String(), link: make(chan *stallConn, 1)}
	s.answer.Store(true)
	var link atomic.Pointer[stallConn]
	s.stop = sync.OnceFunc(func() {
		_ = ln.Close()
		if l := link.Load(); l != nil {
			_ = l.Close()
		}
	})
	t.Cleanup(s.stop)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		l := newStallConn(conn)
		link.Store(l)
		s.link <- l
		defer l.Close()
		sc, chans, reqs, err := ssh.NewServerConn(l, cfg)
		if err != nil {
			return
		}
		defer sc.Close()
		go func() {
			for req := range reqs {
				if req.Type == "keepalive@openssh.com" && req.WantReply && s.answer.Load() {
					s.answered.Add(1)
					// OpenSSH refuses the request too: the refusal is the answer.
					_ = req.Reply(false, nil)
				}
			}
		}()
		for nc := range chans {
			ch, chReqs, err := nc.Accept()
			if err != nil {
				return
			}
			go func() {
				for req := range chReqs {
					_ = req.Reply(req.Type == "subsystem", nil)
				}
			}()
			srv, err := sftp.NewServer(ch)
			if err != nil {
				return
			}
			go func() {
				_ = srv.Serve()
				_ = ch.Close()
			}()
		}
	}()
	return s
}

// connectTo connects a backend on t.TempDir() to s.
func connectTo(t *testing.T, s *sshServer, keepalive time.Duration) *Backend {
	t.Helper()
	isolate(t)
	host, port := splitAddr(t, s.addr)
	b, err := connect(Config{
		Host: host, Port: port, User: "ark", Path: t.TempDir(),
		IdentityFile: writeIdentity(t), InsecureIgnoreHostKey: true,
	}, keepalive)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// TestBackendPingsTheServerAndGivesUpWhenItFallsSilent runs the keepalive of
// a connected backend over ssh: the pings the server answers keep the
// connection, and once the answers stop every call fails with the lost
// connection.
func TestBackendPingsTheServerAndGivesUpWhenItFallsSilent(t *testing.T) {
	s := serveSFTP(t)
	b := connectTo(t, s, 50*time.Millisecond)

	for start := time.Now(); s.answered.Load() < 2*keepaliveMisses; time.Sleep(time.Millisecond) {
		if time.Since(start) > cancelWait {
			t.Fatalf("the server saw %d keepalives in %v", s.answered.Load(), cancelWait)
		}
	}
	if empty, err := b.Empty(t.Context()); err != nil || !empty {
		t.Fatalf("Empty after answered keepalives = %v, %v, want true, nil", empty, err)
	}

	s.answer.Store(false)
	select {
	case <-b.lost:
	case <-time.After(cancelWait):
		t.Fatal("a server that answers no keepalive kept its connection")
	}
	if _, err := b.Empty(t.Context()); !errors.Is(err, errLost) {
		t.Errorf("Empty after the connection was given up = %v, want errLost", err)
	}
}

// TestCloseOnAStalledLinkLeavesNoGoroutine cancels every call on an ssh link
// that stalls, closes the backend, which the server never acknowledges, and
// counts: the requests left behind and the keepalive end with the connection.
func TestCloseOnAStalledLinkLeavesNoGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()

	s := serveSFTP(t)
	b := connectTo(t, s, time.Hour)
	link := <-s.link
	putAll(t, b, stalledObject)
	for _, c := range calls {
		// The server reads nothing more: the call hangs on its first request.
		link.stallAfter(0)
		if !cancelOnStall(t, link, func(ctx context.Context) error { return c.call(ctx, b) }) {
			t.Errorf("%s got through on a stalled link", c.name)
		}
		if t.Failed() {
			t.Fatalf("%s failed", c.name)
		}
		// Let the server answer what the call left behind, so the next one
		// finds the link held by nothing but its own request.
		link.resume()
	}
	link.stallAfter(0)
	left := link.stalled()
	go func() { _, _ = b.Stat(context.Background(), stalledObject) }()
	<-left

	closed := make(chan error, 1)
	start := time.Now()
	go func() { closed <- b.Close() }()
	select {
	case <-closed:
		if waited := time.Since(start); waited < closeWait {
			t.Errorf("Close returned after %v: the link was not stalled", waited)
		}
	case <-time.After(closeWait + cancelWait):
		t.Fatal("Close hangs on a stalled link")
	}
	s.stop()

	for start := time.Now(); runtime.NumGoroutine() > before; time.Sleep(10 * time.Millisecond) {
		if time.Since(start) > cancelWait {
			var stacks strings.Builder
			_ = pprof.Lookup("goroutine").WriteTo(&stacks, 1)
			t.Fatalf("%d goroutines before, %d after Close:\n%s", before, runtime.NumGoroutine(), stacks.String())
		}
	}
}
