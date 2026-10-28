package arksftp

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Secrets are read from the environment, never from Config or a URI.
const (
	// EnvPassword holds the SSH password, used when neither the agent nor an
	// identity file authenticates.
	EnvPassword = "ARK_SFTP_PASSWORD" //nolint:gosec // env var name, not a credential
	// EnvKeyPassphrase unlocks an encrypted IdentityFile.
	EnvKeyPassphrase = "ARK_SFTP_KEY_PASSPHRASE" //nolint:gosec // env var name, not a credential
)

// defaultIdentityFiles are the keys in ~/.ssh tried when no IdentityFile is
// set, in the order OpenSSH tries them.
var defaultIdentityFiles = []string{"id_rsa", "id_ecdsa", "id_ed25519"}

// authChain is the ordered set of auth methods offered to the server, plus a
// record of what was offered and what was left out. The server picks; when it
// accepts none of them the record explains why the chain looked the way it did.
type authChain struct {
	methods []ssh.AuthMethod
	offered []string
	skipped []string
	// agent and identities feed one publickey method. The ssh client tries
	// each method name once, so two publickey entries would leave the second
	// untried whenever the first is refused.
	agent      agent.ExtendedAgent
	identities []ssh.Signer
	// closers holds resources the methods borrow, currently the agent socket.
	// They are only needed until the handshake completes.
	closers []io.Closer
}

// close releases everything the chain borrowed. Safe after a failed dial.
func (c *authChain) close() {
	for _, cl := range c.closers {
		_ = cl.Close()
	}
	c.closers = nil
}

// describe renders the chain for an error message.
func (c *authChain) describe() string {
	var b strings.Builder
	b.WriteString("tried " + strings.Join(c.offered, ", "))
	if len(c.skipped) > 0 {
		b.WriteString("; skipped " + strings.Join(c.skipped, ", "))
	}
	return b.String()
}

// buildAuthChain assembles the auth methods in a fixed order:
// ssh-agent when SSH_AUTH_SOCK is set, then IdentityFile or the default keys in
// ~/.ssh, then a password from EnvPassword. A method that is not configured is skipped; a method that is
// configured but broken, such as an unreadable key, is an error.
func buildAuthChain(cfg Config) (*authChain, error) {
	c := &authChain{}

	c.addAgent()

	if err := c.addIdentity(cfg.IdentityFile); err != nil {
		c.close()
		return nil, err
	}

	if c.agent != nil || len(c.identities) > 0 {
		c.methods = append(c.methods, ssh.PublicKeysCallback(c.publicKeys))
	}

	c.addPassword()

	if len(c.methods) == 0 {
		return nil, fmt.Errorf("arksftp: no usable auth method (%s); "+
			"start an ssh-agent, set Config.IdentityFile, or set %s",
			c.describe(), EnvPassword)
	}

	return c, nil
}

// addAgent offers the keys held by the running ssh-agent. An agent that is not
// running or not reachable is skipped, not an error: the later methods may
// still work.
func (c *authChain) addAgent() {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		c.skipped = append(c.skipped, "ssh-agent (SSH_AUTH_SOCK not set)")
		return
	}

	conn, err := net.Dial("unix", sock) //nolint:gosec // SSH_AUTH_SOCK is the agent socket by definition
	if err != nil {
		c.skipped = append(c.skipped, fmt.Sprintf("ssh-agent (%v)", err))
		return
	}

	c.closers = append(c.closers, conn)
	c.agent = agent.NewClient(conn)
	c.offered = append(c.offered, "ssh-agent")
}

// publicKeys yields the agent keys followed by the identity file keys, in the
// order of buildAuthChain. An agent that fails to list its keys is skipped so
// the identity files still get their turn.
func (c *authChain) publicKeys() ([]ssh.Signer, error) {
	var signers []ssh.Signer
	if c.agent != nil {
		if s, err := c.agent.Signers(); err == nil {
			signers = append(signers, s...)
		}
	}
	signers = append(signers, c.identities...)
	return signers, nil
}

// addIdentity offers the private key in file. An encrypted key is unlocked
// with EnvKeyPassphrase. An explicitly configured key that cannot be loaded
// is an error, because the caller asked for that key by name. Without a file
// the default keys in ~/.ssh are tried instead.
func (c *authChain) addIdentity(file string) error {
	if file == "" {
		c.addDefaultIdentities()
		return nil
	}

	signer, err := loadIdentity(file)
	if err != nil {
		return err
	}

	c.identities = append(c.identities, signer)
	c.offered = append(c.offered, "identity file "+file)
	return nil
}

// addDefaultIdentities offers the default keys in ~/.ssh, as OpenSSH does. A
// missing key is skipped silently, an unusable one is skipped with a note:
// the caller did not ask for any of them by name.
func (c *authChain) addDefaultIdentities() {
	home, err := os.UserHomeDir()
	if err != nil {
		c.skipped = append(c.skipped, fmt.Sprintf("identity file (Config.IdentityFile not set, %v)", err))
		return
	}

	found := false
	for _, name := range defaultIdentityFiles {
		file := filepath.Join(home, ".ssh", name)
		signer, err := loadIdentity(file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		found = true
		if err != nil {
			c.skipped = append(c.skipped, err.Error())
			continue
		}
		c.identities = append(c.identities, signer)
		c.offered = append(c.offered, "identity file "+file)
	}

	if !found {
		c.skipped = append(c.skipped, "identity file (Config.IdentityFile not set, no default key in ~/.ssh)")
	}
}

// loadIdentity parses the private key in file, unlocking it with
// EnvKeyPassphrase when it is encrypted.
func loadIdentity(file string) (ssh.Signer, error) {
	pem, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("arksftp: read identity file %s: %w", file, err)
	}

	signer, err := ssh.ParsePrivateKey(pem)
	if _, ok := errors.AsType[*ssh.PassphraseMissingError](err); ok {
		passphrase := os.Getenv(EnvKeyPassphrase)
		if passphrase == "" {
			return nil, fmt.Errorf("arksftp: identity file %s is encrypted, set %s", file, EnvKeyPassphrase)
		}
		signer, err = ssh.ParsePrivateKeyWithPassphrase(pem, []byte(passphrase))
		if err != nil {
			return nil, fmt.Errorf("arksftp: unlock identity file %s with %s: %w", file, EnvKeyPassphrase, err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("arksftp: parse identity file %s: %w", file, err)
	}

	return signer, nil
}

// addPassword offers the password from EnvPassword.
func (c *authChain) addPassword() {
	password := os.Getenv(EnvPassword)
	if password == "" {
		c.skipped = append(c.skipped, EnvPassword+" (not set)")
		return
	}

	c.methods = append(c.methods, ssh.Password(password))
	c.offered = append(c.offered, EnvPassword)
}

// buildSSHConfig constructs an ssh.ClientConfig from cfg. The host key is
// verified unless cfg opts out; see [hostKeys]. The returned chain is closed
// by the caller once the handshake is done.
func buildSSHConfig(cfg Config) (*ssh.ClientConfig, *authChain, error) {
	chain, err := buildAuthChain(cfg)
	if err != nil {
		return nil, nil, err
	}

	hostKeyCallback, err := hostKeys(cfg)
	if err != nil {
		chain.close()
		return nil, nil, err
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}

	return &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            chain.methods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         timeout,
	}, chain, nil
}
