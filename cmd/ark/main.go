package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"syscall"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkuri"
	"github.com/alexjoedt/ark/cmd/ark/internal/config"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	"github.com/alexjoedt/ark/cmd/ark/internal/version"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

func main() {
	os.Exit(run(os.Args))
}

// run executes the CLI and returns the process exit code. Errors are
// printed here, once, as the JSON error shape or a plain line on stderr.
func run(args []string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Load --env-file before app.Run so the flag env sources see the values.
	err := config.PreloadEnvFile(args)
	if err == nil {
		err = newApp().Run(ctx, args)
	}
	if err == nil {
		return exitcode.OK
	}

	code, kind := exitcode.Of(err)
	switch {
	case exitcode.IsSilent(err):
	case jsonRequested(args):
		outputJSON(map[string]any{
			"error": map[string]string{"code": kind, "message": err.Error()},
		})
	default:
		fmt.Fprintf(os.Stderr, "ark: %v\n", err)
	}
	return code
}

func newApp() *cli.Command {
	// -v belongs to --verbose, the built-in version flag keeps only its long form.
	cli.VersionFlag = &cli.BoolFlag{Name: "version", Usage: "print the version"}
	cli.VersionPrinter = func(cmd *cli.Command) { fmt.Fprintln(cmd.Root().Writer, version.Line()) }
	app := &cli.Command{
		Name:    "ark",
		Usage:   "deduplicated backups for directories and Docker volumes",
		Version: version.GetVersion(),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "repo",
				Aliases: []string{"r"},
				Usage:   "repository path or `URI`, e.g. /srv/ark or sftp://user@host/srv/ark",
				Sources: cli.EnvVars("ARK_REPOSITORY"),
			},
			&cli.StringFlag{
				Name:    "password-file",
				Usage:   "read the repository password from `FILE`, else $ARK_PASSWORD, else a prompt",
				Sources: cli.EnvVars("ARK_PASSWORD_FILE"),
			},
			&cli.BoolFlag{
				Name:    "insecure-no-password",
				Usage:   "the repository has no password, anyone with backend access can read it",
				Sources: cli.EnvVars("ARK_INSECURE_NO_PASSWORD"),
			},
			&cli.BoolFlag{
				Name:    "json",
				Aliases: []string{"j"},
				Usage:   "machine readable output on stdout, logs on stderr",
			},
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"v"},
				Usage:   "debug logs and per file progress",
			},
			&cli.BoolFlag{
				Name:    "quiet",
				Aliases: []string{"q"},
				Usage:   "errors only, no progress",
			},
			&cli.StringFlag{
				Name:  "env-file",
				Usage: "load environment variables from `FILE` before parsing flags",
			},
			&cli.DurationFlag{
				Name:  "lock-wait",
				Usage: "wait up to `DURATION` for a repository lock",
			},
			&cli.IntFlag{
				Name:  "concurrency",
				Usage: "max concurrent repository operations",
				Value: 2 * runtime.NumCPU(),
			},
		},
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			if cmd.Bool("verbose") && cmd.Bool("quiet") {
				return ctx, exitcode.Usagef("--verbose and --quiet exclude each other")
			}
			slog.SetDefault(newLogger(cmd))
			return ctx, nil
		},
		Commands: []*cli.Command{
			initCommand(),
			backupCommand(),
			restoreCommand(),
			snapshotsCommand(),
			lsCommand(),
			inspectCommand(),
			forgetCommand(),
			pruneCommand(),
			verifyCommand(),
			checkCommand(),
			exportCommand(),
			importCommand(),
			unlockCommand(),
			dockerCommand(),
			keyCommand(),
			serveCommand(),
			versionCommand(),
		},
	}
	addRemoved(app)
	setUsageErrors(app)
	return app
}

// setUsageErrors turns flag and argument parse errors into exit 2 on every
// command. urfave/cli does not inherit OnUsageError, so the tree is walked.
func setUsageErrors(cmd *cli.Command) {
	if cmd.OnUsageError == nil {
		cmd.OnUsageError = func(_ context.Context, _ *cli.Command, err error, _ bool) error {
			return exitcode.Usagef("%v", err)
		}
	}
	for _, sub := range cmd.Commands {
		setUsageErrors(sub)
	}
}

// newLogger builds the stderr logger: debug with --verbose, errors only with
// --quiet, warnings otherwise; the command's own output reports progress and
// results. No timestamps, a CLI run is short.
func newLogger(cmd *cli.Command) *slog.Logger {
	lvl := slog.LevelWarn
	switch {
	case cmd.Bool("verbose"):
		lvl = slog.LevelDebug
	case cmd.Bool("quiet"):
		lvl = slog.LevelError
	}
	var w io.Writer = os.Stderr
	if term.IsTerminal(int(os.Stderr.Fd())) {
		w = clearLine{w}
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

// clearLine erases a spinner line before each log record.
type clearLine struct{ w io.Writer }

func (c clearLine) Write(b []byte) (int, error) {
	if _, err := io.WriteString(c.w, "\r\033[K"); err != nil {
		return 0, err
	}
	return c.w.Write(b)
}

// jsonRequested scans the raw arguments so an error before or during flag
// parsing still comes out in the requested shape.
func jsonRequested(args []string) bool {
	return slices.Contains(args, "--json") || slices.Contains(args, "-j")
}

// openRepo opens the repository named by --repo or ARK_REPOSITORY through
// arkuri. There is no default location.
//
//nolint:unparam // callers may pass options
func openRepo(ctx context.Context, cmd *cli.Command, repoOpts ...ark.Option) (*ark.Repository, error) {
	return openRepoAt(ctx, cmd, cmd.Root().String("repo"), repoOpts...)
}

// openRepoAt opens the repository at uri with the global password, lock and
// concurrency settings.
func openRepoAt(ctx context.Context, cmd *cli.Command, uri string, repoOpts ...ark.Option) (*ark.Repository, error) {
	root := cmd.Root()
	if uri == "" {
		return nil, exitcode.Usagef("no repository given, set --repo or ARK_REPOSITORY")
	}
	pw, err := repoPassword.read(root, false)
	if err != nil {
		return nil, err
	}
	b, err := arkuri.Open(ctx, uri)
	if err != nil {
		return nil, fmt.Errorf("open repository %s: %w", uri, err)
	}
	opts := []ark.Option{
		ark.WithLogger(slog.Default()),
		ark.WithLockWait(root.Duration("lock-wait")),
		ark.WithConcurrency(root.Int("concurrency")),
	}
	repo, err := ark.Open(ctx, b, pw, append(opts, repoOpts...)...)
	if err != nil {
		_ = b.Close()
		return nil, fmt.Errorf("open repository %s: %w", uri, err)
	}
	return repo, nil
}

// printer builds the output layer from the global flags plus the command's
// own --yes, where it has one.
func printer(cmd *cli.Command) *out.Printer {
	root := cmd.Root()
	return out.New(out.Mode{
		JSON:    root.Bool("json"),
		Verbose: root.Bool("verbose"),
		Quiet:   root.Bool("quiet"),
		Yes:     cmd.Bool("yes"),
	})
}

// outputJSON writes data as indented JSON to stdout.
func outputJSON(data any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(data)
}

func versionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "print version information",
		Action: func(_ context.Context, cmd *cli.Command) error {
			p := printer(cmd)
			if p.Mode().JSON {
				return p.JSON(version.Get())
			}
			p.Linef("%s", version.Line())
			return nil
		},
	}
}
