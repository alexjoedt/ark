package arksftp

import (
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/kevinburke/ssh_config"
)

// ssh_config files, in the order OpenSSH reads them. The user file wins.
var sshConfigFiles = []string{
	filepath.Join("~", ".ssh", "config"),
	filepath.Join("/etc", "ssh", "ssh_config"),
}

// sshConfig looks up ssh_config keywords for a Host alias.
type sshConfig interface {
	Get(alias, key string) string
}

// sshFiles queries several parsed ssh_config files in order, so the user file
// shadows the system one, matching OpenSSH.
type sshFiles []*ssh_config.Config

// Get returns the first non-empty value for key under alias.
func (f sshFiles) Get(alias, key string) string {
	for _, c := range f {
		if v, err := c.Get(alias, key); err == nil && v != "" {
			return v
		}
	}
	return ""
}

// maxSSHConfigSize is the largest ssh_config file that is read; a larger one
// is skipped.
const maxSSHConfigSize = 1 << 20

// maxIncludeDepth bounds nested Include files, as the decoder does.
const maxIncludeDepth = 5

// warned holds the warnings already logged, so a process that connects often
// reports each ssh_config problem once per file.
var warned sync.Map

func warnOnce(msg, path string, args ...any) {
	if _, dup := warned.LoadOrStore(msg+"\x00"+path, struct{}{}); !dup {
		slog.Warn(msg, append([]any{"path", path}, args...)...)
	}
}

// loadSSHConfig parses the user and system ssh_config files. A file that is
// missing or unparsable is skipped: ssh_config is a convenience, and failing
// the connect over a stray line in an unrelated stanza would be wrong.
func loadSSHConfig() sshConfig {
	var files sshFiles
	for _, name := range sshConfigFiles {
		path := expandHome(name)
		text, ok := readSSHConfig(path)
		if !ok {
			continue
		}
		text = flattenSSHConfig(text, path, filepath.Dir(path), 0, false)
		cfg, err := ssh_config.Decode(strings.NewReader(text))
		if err != nil {
			warnOnce("arksftp: skipping unparsable ssh_config", path, "err", err)
			continue
		}
		files = append(files, cfg)
	}
	return files
}

// readSSHConfig returns the text of the ssh_config file at path, or false for
// a file to skip: missing, unreadable or larger than maxSSHConfigSize, since
// a file cut off mid-stanza would be parsed as if it were whole.
func readSSHConfig(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Debug("arksftp: skipping ssh_config", "path", path, "err", err)
		}
		return "", false
	}
	text, err := io.ReadAll(io.LimitReader(f, maxSSHConfigSize+1))
	_ = f.Close()
	switch {
	case err != nil:
		warnOnce("arksftp: skipping unreadable ssh_config", path, "err", err)
	case len(text) > maxSSHConfigSize:
		warnOnce("arksftp: skipping ssh_config larger than 1 MiB", path)
	default:
		return string(text), true
	}
	return "", false
}

// resolve fills the blanks in cfg from ssh_config, then from the defaults.
// Precedence is explicit Config fields, then ssh_config, then the defaults:
// port 22 and the current OS user. cfg.Host is treated as the Host alias, so
// a HostName in the matching stanza replaces it.
func resolve(cfg Config, sc sshConfig) Config {
	alias := cfg.Host

	//nolint:nestif // flat field defaults
	if sc != nil {
		if cfg.User == "" {
			cfg.User = sc.Get(alias, "User")
		}
		if cfg.Port == 0 {
			if port, err := strconv.Atoi(sc.Get(alias, "Port")); err == nil {
				cfg.Port = port
			}
		}
		if cfg.IdentityFile == "" {
			cfg.IdentityFile = sc.Get(alias, "IdentityFile")
		}
		if hostName := sc.Get(alias, "HostName"); hostName != "" {
			cfg.Host = hostName
		}
	}

	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	if cfg.User == "" {
		cfg.User = currentUser()
	}
	if cfg.IdentityFile != "" {
		cfg.IdentityFile = expandTokens(cfg.IdentityFile, cfg)
	}

	return cfg
}

// currentUser returns the OS user name, the default ssh uses when no user is
// given. An empty result lets the ssh handshake report the missing user.
func currentUser() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}

// expandTokens expands the ssh_config percent tokens arksftp can resolve, plus
// a leading ~. Unknown tokens are left alone.
func expandTokens(s string, cfg Config) string {
	s = strings.NewReplacer(
		"%h", cfg.Host,
		"%p", strconv.Itoa(cfg.Port),
		"%r", cfg.User,
		"%%", "%",
	).Replace(s)
	return expandHome(s)
}

// expandHome replaces a leading ~ with the home directory.
func expandHome(s string) string {
	if s != "~" && !strings.HasPrefix(s, "~"+string(filepath.Separator)) {
		return s
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return s
	}
	return filepath.Join(home, strings.TrimPrefix(s, "~"))
}

// flattenSSHConfig returns text as the decoder can take it: without its Match
// blocks, and with each Include line replaced by the flattened files it names.
// The decoder rejects most Match criteria and reads "Match host" as a Host
// line, against the alias instead of the resolved host name, so every Match
// block goes. It would read included files itself and fail the whole file on
// one such block in any of them, so they are inlined here.
//
// path names the file in warnings; relative Include names resolve against
// dir. With head set, the text is a file included from inside a Host block and
// only the lines before its first Host line are returned, the ones that belong
// to that block. OpenSSH applies its own Host stanzas only where both patterns
// match, which flat text cannot say, so they are dropped.
func flattenSSHConfig(text, path, dir string, depth int, head bool) string {
	text, stripped := stripMatchBlocks(text)
	if stripped {
		warnOnce("arksftp: ignoring Match blocks in ssh_config", path)
	}
	var out strings.Builder
	inHost := head
	for _, line := range strings.SplitAfter(text, "\n") {
		switch firstKeyword(line) {
		case "host":
			if head {
				return out.String()
			}
			inHost = true
		case "include":
			if depth == maxIncludeDepth {
				warnOnce("arksftp: ignoring ssh_config Include nested too deep", path)
				continue
			}
			for _, inc := range includePaths(line, dir) {
				if sub, ok := readSSHConfig(inc); ok {
					out.WriteString(flattenSSHConfig(sub, inc, dir, depth+1, inHost))
					out.WriteString("\n")
				}
			}
			if !inHost {
				// The included Host stanzas end here: what follows
				// applies to every host again.
				out.WriteString("Host *\n")
			}
			continue
		}
		out.WriteString(line)
	}
	return out.String()
}

// includePaths returns the files an Include line names, globs expanded. A
// relative name resolves against dir, as OpenSSH resolves it against ~/.ssh
// for the user file and /etc/ssh for the system one.
func includePaths(line, dir string) []string {
	args := strings.TrimLeft(line, " \t")[len("include"):]
	var paths []string
	for arg := range strings.FieldsSeq(strings.TrimLeft(args, " \t=")) {
		if strings.HasPrefix(arg, "#") {
			break
		}
		arg = expandHome(strings.Trim(arg, `"`))
		if !filepath.IsAbs(arg) {
			arg = filepath.Join(dir, arg)
		}
		// Glob fails only on a malformed pattern, which names no file.
		matches, _ := filepath.Glob(arg)
		paths = append(paths, matches...)
	}
	return paths
}

// stripMatchBlocks removes every Match line and the lines after it up to the
// next Host or Match keyword. Only a line whose first word is Match starts a
// block, so a Match inside a comment or a value does not.
func stripMatchBlocks(text string) (string, bool) {
	var out strings.Builder
	inMatch, stripped := false, false
	for _, line := range strings.SplitAfter(text, "\n") {
		switch firstKeyword(line) {
		case "match":
			inMatch, stripped = true, true
		case "host":
			inMatch = false
		}
		if !inMatch {
			out.WriteString(line)
		}
	}
	return out.String(), stripped
}

// firstKeyword returns the lowercased first word of an ssh_config line, the
// keyword, or "" for blank and comment lines. A word ends at white space or =.
func firstKeyword(line string) string {
	line = strings.TrimLeft(line, " \t")
	if strings.HasPrefix(line, "#") {
		return ""
	}
	if i := strings.IndexAny(line, " \t=\r\n"); i >= 0 {
		line = line[:i]
	}
	return strings.ToLower(line)
}

// decodeSSHConfig parses one ssh_config from r. Used by tests to stay off the
// developer's own ~/.ssh/config.
func decodeSSHConfig(r io.Reader) (sshConfig, error) {
	text, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	flat := flattenSSHConfig(string(text), "", "", 0, false)
	cfg, err := ssh_config.Decode(strings.NewReader(flat))
	if err != nil {
		return nil, err
	}
	return sshFiles{cfg}, nil
}
