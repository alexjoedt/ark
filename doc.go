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
// old key file; a leaked repository needs a new one.
//
// Init, Open and ChangePassword derive the key with argon2id at 64 MiB of
// memory, so a container that opens a repository needs that much on top of
// the restore cache.
//
// # Memory
//
// Restore, Export and Verify stream: they walk the snapshot in order and read
// the chunks of the file at hand, so their memory is the restore chunk cache
// (256 MiB by default, see WithRestoreCache) plus one chunk per worker in
// flight, where the workers are capped by WithConcurrency. Neither the size of
// the snapshot nor the size of a single file changes that.
package ark
