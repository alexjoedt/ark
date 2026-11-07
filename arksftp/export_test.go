package arksftp

// DisablePosixRename sends b down the plain-rename path that a server without
// posix-rename@openssh.com gets.
func (b *Backend) DisablePosixRename() { b.posixRename = false }

// RemoveRoot deletes b's repository directory and everything in it.
func (b *Backend) RemoveRoot() error { return b.client.RemoveAll(b.basePath) }
