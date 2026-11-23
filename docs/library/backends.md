# Backends

A backend is where a repository lives. It stores opaque, encrypted objects and knows nothing about
snapshots or files. This page shows the backends that come with ark and how to write one for
storage that ark doesn't cover, such as S3 or the API of a NAS.

## Built-in backends

Create a backend and hand it to `ark.Init` and `ark.Open`:

```go
b, err := arklocal.New("/srv/ark")

b, err := arksftp.New(arksftp.Config{Host: "nas", User: "backup", Path: "/srv/ark"})

b := arkmem.New()
```

| Package | Stores objects in | Notes |
|---|---|---|
| `arklocal` | a local directory | writes through a temporary file and a rename, so no reader sees a partial object |
| `arksftp` | a directory over SFTP | checks the host key against `known_hosts` by default |
| `arkmem` | memory | for tests; its objects are gone on `Close` |

## Open a backend from a string

`arkuri.Open` picks the backend from a location string, the same strings the CLI accepts for
`--repo`:

```go
b, err := arkuri.Open(ctx, "sftp://backup@nas/srv/ark") // or "/srv/ark", "local:///srv/ark"
```

- In an `sftp://` URI, user and port are optional, and the path after the host is absolute on the
  server.
- Options that aren't secret are query parameters: `known_hosts`, `identity`, and `insecure`.
- The scp form `sftp:host:/path` is rejected.

[Locations](../cli/index.md#locations) lists the forms.

## SFTP authentication

`arksftp` reads `~/.ssh/config`, so a `Host` alias works. It tries these in order:

1. the keys of a running ssh-agent
2. the `IdentityFile`, or without one the default keys in `~/.ssh`
3. the password in `ARK_SFTP_PASSWORD`

`ARK_SFTP_KEY_PASSPHRASE` unlocks an encrypted key file. Secrets never go into `arksftp.Config` or
a URI.

## SFTP durability

`arksftp` needs the `fsync@openssh.com` extension to flush files and directories on the server.
OpenSSH has it. A server without it has no durability barrier: `Sync` returns nil, and a power loss
on the server can drop objects that `Put` returned. `arksftp.New` logs a warning once when the
extension is missing.

## SFTP on a link that stops answering

Every `arksftp` call returns when its context ends, also when the server no longer answers. The
request it waited on stays behind until the server answers it or the connection closes, so a
cancelled `Delete` may still remove its object and a cancelled `Put` may leave a temporary
`.ark-tmp-` object, which no listing shows.

The backend pings the server every 15 seconds. It closes the connection after three intervals in
a row with a ping unanswered and nothing else arriving from the server. A ping that fails because
the connection is gone closes it at once. Every call fails from then on with an error that says
the connection is lost and why. `Close` then returns no error. `arksftp` does not reconnect: call
`arksftp.New` again.

## Write your own backend

Anything that can store, read, list, and delete named blobs can hold a repository. Implement
`ark.Backend`, which has nine methods:

```go
type Backend interface {
	Put(ctx context.Context, h Handle, r io.Reader) error
	Get(ctx context.Context, h Handle) (io.ReadCloser, error)
	GetRange(ctx context.Context, h Handle, offset, length int64) ([]byte, error)
	Exists(ctx context.Context, h Handle) (bool, error)
	Delete(ctx context.Context, h Handle) error
	Sync(ctx context.Context) error
	List(ctx context.Context, t HandleType, fn func(BlobInfo) error) error
	Stat(ctx context.Context, h Handle) (BlobInfo, error)
	Close() error
}
```

An `ark.Handle` names one object by its type and its name. The example below keeps each object
under the key `<namespace>/<name>`, the layout an object store uses:

```go
// key maps a handle to its key. The namespace comes from Type.String() and not
// from a switch on the constants, so a handle type that a later version of ark
// adds needs no change here. ark.CheckName refuses a name that is no object
// name, such as one with a "/" that would reach into another key.
func key(h ark.Handle) (string, error) {
	ns := h.Type.String()
	if ns == ark.UnknownNamespace {
		return "", fmt.Errorf("kv: unknown handle type %d", h.Type)
	}
	if err := ark.CheckName(h.Name); err != nil {
		return "", fmt.Errorf("kv: %s: %w", ns, err)
	}
	return ns + "/" + h.Name, nil
}

// Put replaces the object in one step: a reader sees the old object or the
// new one, never a part of it.
func (b *kvBackend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k, err := key(h)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("kv: read %s: %w", k, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[k] = data
	return nil
}

// GetRange returns exactly length bytes from offset. A range that ends past
// the object is an error wrapping [io.ErrUnexpectedEOF], never a short result.
func (b *kvBackend) GetRange(ctx context.Context, h ark.Handle, offset, length int64) ([]byte, error) {
	if offset < 0 || length < 0 {
		return nil, fmt.Errorf("kv: invalid range %d+%d of %s", offset, length, h.Name)
	}
	data, err := b.load(ctx, h)
	if err != nil {
		return nil, err
	}
	if offset > int64(len(data)) || length > int64(len(data))-offset {
		return nil, fmt.Errorf("kv: range %d+%d of %s (%d bytes): %w",
			offset, length, h.Name, len(data), io.ErrUnexpectedEOF)
	}
	return bytes.Clone(data[offset : offset+length]), nil
}
```

The complete backend, with all nine methods and a program that uses it, is
[`examples/custombackend`](https://github.com/alexjoedt/ark/tree/main/examples/custombackend).

### The contract

The core relies on these rules. The documentation of
[`ark.Backend`](https://pkg.go.dev/github.com/alexjoedt/ark#Backend) states them in full.

| Method | Rule |
|---|---|
| all | safe for concurrent use |
| all with a handle | refuse a name that `ark.CheckName` refuses (empty, starting with `.`, or holding `/` or `\`) with an error that wraps `ark.ErrInvalidName` |
| all with a context | return an error that wraps `ctx.Err()` when called with a done context |
| `Put` | atomic: a reader never sees a partial object; an existing object is replaced; `r` isn't read after `Put` returns |
| `Get`, `Stat` | a missing object returns an error that wraps `ark.ErrNotFound` |
| `GetRange` | returns exactly `length` bytes without reading the bytes before `offset`; a range past the end wraps `io.ErrUnexpectedEOF`; a missing object wraps `ark.ErrNotFound`; a negative offset or length is an error |
| `Delete` | removing a missing object isn't an error |
| `Sync` | when it returns, a power loss can't undo any `Put` or `Delete` that returned before the call |
| `List` | calls `fn` once per object of the type and never with a name `ark.CheckName` refuses, such as a temporary file or a `.DS_Store`; stops and returns the error when `fn` returns one |

Three more points:

- **Namespaces.** Map `Handle.Type` to a directory or key prefix by its `String()` value. Don't
  switch on the constants, because later versions may add types. An unknown type has the namespace
  `ark.UnknownNamespace`; a backend may reject it.
- **Sharding.** A backend that stores objects as files shards `ark.DataPackFile` and
  `ark.TreePackFile` one level deep by the first two characters of the name, and nothing else.
- **Concurrency.** The core runs at most `ark.WithConcurrency` operations at a time, twice the
  CPU count by default. A remote backend can size its connection pool from that number.

`Put` doesn't have to make the object durable. `Sync` does that. A store that makes every write
durable on its own returns nil from `Sync` without doing anything.

### Test it

`arktest.RunBackendTests` runs the suite that the built-in backends pass. Give it a function that
returns an empty backend:

```go
func TestConformance(t *testing.T) {
	arktest.RunBackendTests(t, func(*testing.T) ark.Backend {
		return newKVBackend()
	})
}
```

Every subtest checks one clause of the contract, so a failure names the rule that broke.

### Measure it

`ark.Instrument` wraps any backend and counts calls, errors, durations, and bytes per method. See
[Progress and logging](observing.md#backend-statistics).

## Next steps

- [Sources and sinks](sources-and-sinks.md) covers the other two extension points.
- [Repository format](../format.md) describes the objects a backend stores.
