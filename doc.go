// Package ark provides a general-purpose CDC (content-defined chunking) backup
// library. It offers restic-style deduplicating backups through a clean
// Source/Sink abstraction that any caller can implement.
//
// A repository is created once with Init and opened with Open. Open never
// creates one, so a mistyped path fails instead of starting a second
// repository.
//
// Basic usage:
//
//	b, _ := arklocal.New("/path/to/repo")
//	ark.Init(ctx, b, ark.WithPassword(pw)) // once, per repository
//
//	repo, _ := ark.Open(ctx, b, ark.WithPassword(pw))
//	snapshot, _ := repo.Backup(ctx, mySource)
//	repo.Restore(ctx, snapshot.ID, mySink)
//
// # Passwords
//
// Every repository is encrypted and authenticated. Init creates a random
// master key and stores it in a key file sealed under the password; the data
// is encrypted with the master key, never with the password. Open unseals the
// master key before it reads any other object, and a wrong password fails
// with ErrWrongPassword. There is no recovery: a lost password is lost data.
//
// A repository without a password is created and opened with WithoutPassword.
// It has the same format with the master key stored unprotected, so anyone
// with access to the backend can read and forge it; corruption is still
// detected. The choice is explicit on every Init and Open: WithoutPassword on
// a protected repository fails with ErrPasswordRequired, a password on an
// unprotected one with ErrNotPasswordProtected.
//
// ChangePassword seals the same master key under a new password. It does not
// revoke a master key that leaked, through the old password or a copy of the
// old key file; a leaked repository needs a new one. Nor does it revoke the old
// password against anyone with write access to the backend who kept the old
// key file: put back, it opens the repository under the old password again.
//
// Init, Open and ChangePassword derive the key with argon2id at 64 MiB of
// memory, so a container that opens a repository needs that much on top of
// the restore cache.
//
// What the encryption protects against, what it leaks and which risks are
// accepted is written down in docs/SECURITY.md
// (https://github.com/alexjoedt/ark/blob/main/docs/SECURITY.md).
//
// # Writing a Source or Sink
//
// A program that backs up something other than a directory implements Source
// and, to restore, Sink. The rules the core relies on:
//
// Source.Walk calls fn once per entry. Entries may come in any order, the core
// sorts every tree node by name; the one ordering rule is that a hardlink
// target comes before the FileTypeHardlink entries naming it. Walk should stop
// with ctx's error when ctx is cancelled.
//
// The open func passed to fn is nil for everything but a regular file. For a
// regular file it yields at most entry.Size bytes and does no work until it
// is called. The core calls it at most once, never when the parent snapshot
// already holds the file unchanged, and closes the reader it returns. A
// source that cannot serve an open after fn returned (a tar stream, a single
// connection) simply returns; the next fn call advances it past the entry.
//
// A source whose opens may run after fn returned, on any goroutine and
// concurrently, also implements ConcurrentSource with ConcurrentOpen
// returning true. Backup then reads its files in parallel. Implement it only
// when every open is independent of the walk's position; a wrapper that embeds
// a Source hides the method and must forward it.
//
// A zero FileInfo.ModTime means "unknown": the file never counts as unchanged
// and is read on every backup. Set ModTime, and ChangeTime and Inode where the
// medium has them, as soon as unchanged files should be skipped.
//
// A Sink sees Open once with the snapshot's SourceMeta, where it rejects a
// type it cannot restore with ErrIncompatibleSourceType; then Receive once per
// entry in walk order, parents before children, a hardlink after its target,
// r nil for everything but a regular file, which Receive reads to the end;
// then Close exactly once, also after a failed Receive or a cancelled restore.
//
// Prove both with the conformance suites in package arktest, which a foreign
// Source and Sink are expected to pass:
//
//	arktest.RunSourceSinkTests(t, myType, newSource, newSink)
//	arktest.RunStreamTests(t, newStreamSource, newStreamSink) // one-file streams only
//
// For a single byte stream, NewStreamSource and NewStreamSink already
// implement the contract.
//
// # Memory
//
// Restore and Export stream: they walk the snapshot in order and read the
// chunks of the file at hand, so their memory is the restore chunk cache
// (256 MiB by default, see WithRestoreCache) plus one chunk per worker in
// flight, where the workers are capped by WithConcurrency. Neither the size of
// the snapshot nor the size of a single file changes that.
//
// Verify and Check read pack by pack instead: they hold the names and places
// of the distinct chunks of one snapshot, about 150 bytes each, and one range
// of a pack at a time, which is at most a pack of about 20 MiB. Check reads a
// chunk several snapshots share once, and keeps what it found for every
// distinct chunk of the repository, about 60 bytes each.
package ark
