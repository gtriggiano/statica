// Package cli implements statica's command interface.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/gtriggiano/statica/internal/app"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/spf13/cobra"
)

// BuildInfo identifies the binary. Empty version, revision and GoVersion values
// use local development defaults; an empty BuildTime is omitted from output.
type BuildInfo struct {
	Version   string
	Revision  string
	GoVersion string
	BuildTime string
}

// Options supplies process dependencies. Nil writers discard output, and a nil
// LookupEnv reports every environment variable as unset.
type Options struct {
	Out           io.Writer
	Err           io.Writer
	LookupEnv     config.LookupEnv
	Build         BuildInfo
	OriginFactory OriginFactory
	// Signals delivers the process's shutdown signals. serve consumes them
	// itself: the first drains, another forces exit. Every other command is
	// cancelled by the first, like a signal-cancelled context. Nil delivers
	// none.
	Signals <-chan os.Signal
	// Serve runs serve's server; nil selects app.Run.
	Serve ServeFunc
}

// Run executes a new command tree. It returns 0 on success, 2 for usage or
// configuration errors, and 1 for runtime failures such as writing output.
func Run(ctx context.Context, args []string, opts Options) int {
	opts = defaults(opts)
	// Ends the signal watcher of a command, if any, when Run returns.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	logs := opts.Out
	output := &checkedWriter{writer: opts.Out}
	opts.Out = output
	opts.Err = &checkedWriter{writer: opts.Err}
	cmd := newCommand(opts, logs)
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		// Cobra's lookup errors include the argument. Avoid echoing arbitrary
		// user input, which may contain a mistakenly pasted credential.
		message := err.Error()
		if strings.HasPrefix(message, "unknown command ") || strings.HasPrefix(message, "unknown help topic ") {
			message = "unknown command; see statica --help"
		}
		if _, writeErr := fmt.Fprintf(opts.Err, "statica: %s\n", message); writeErr != nil {
			return 1
		}
		var failure runtimeError
		if errors.As(err, &failure) {
			return 1
		}
		return 2
	}
	// Cobra's built-in help renderer does not return write failures from
	// ExecuteContext, so inspect the output writer as well.
	if output.failed {
		fmt.Fprintln(opts.Err, "statica: cannot write output")
		return 1
	}
	return 0
}

// NewCommand constructs an independent Cobra command tree. It does not inspect
// the environment, load configuration, or access the network until execution.
// serve writes its logs to opts.Out.
func NewCommand(opts Options) *cobra.Command {
	opts = defaults(opts)
	return newCommand(opts, opts.Out)
}

// newCommand builds the tree for defaulted opts; serve logs to logs.
func newCommand(opts Options, logs io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:               "statica",
		Short:             "Serve and inspect configuration for the statica S3 web server",
		Long:              "statica serves one S3 bucket over HTTP and inspects its configuration.\nserve and config check-origin are online: they discover AWS credentials and\ncontact the origin (check-origin read-only). help, version, config validate and\nconfig print are offline: no credential discovery, DNS, or S3 access.",
		Args:              noArgs,
		SilenceErrors:     true,
		SilenceUsage:      true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return runtimeError{err}
			}
			cancelOnSignal(cmd, opts.Signals)
			return nil
		},
	}
	root.SetOut(opts.Out)
	root.SetErr(opts.Err)
	root.SetFlagErrorFunc(func(_ *cobra.Command, _ error) error {
		return errors.New("invalid command flags; see --help")
	})
	root.SetHelpCommand(&cobra.Command{
		Use:   "help [command]",
		Short: "Help about any command",
		RunE: func(_ *cobra.Command, args []string) error {
			target, remaining, err := root.Find(args)
			if err != nil || len(remaining) != 0 {
				return errors.New("unknown command; see statica --help")
			}
			return help(target, nil)
		},
	})
	root.RunE = help
	root.AddCommand(versionCommand(opts.Build), configCommand(opts), serveCommand(opts, logs))
	return root
}

// cancelOnSignal makes the first signal cancel cmd's context. The watcher
// ends with that context, which Run cancels when it returns.
func cancelOnSignal(cmd *cobra.Command, signals <-chan os.Signal) {
	if signals == nil {
		return
	}
	ctx, cancel := context.WithCancel(cmd.Context())
	go func() {
		defer cancel()
		select {
		case <-signals:
		case <-ctx.Done():
		}
	}()
	cmd.SetContext(ctx)
}

func defaults(opts Options) Options {
	if opts.OriginFactory == nil {
		opts.OriginFactory = defaultOriginFactory
	}
	if opts.Serve == nil {
		opts.Serve = app.Run
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.Err == nil {
		opts.Err = io.Discard
	}
	if opts.LookupEnv == nil {
		opts.LookupEnv = func(string) (string, bool) { return "", false }
	}
	if opts.Build.Version == "" {
		opts.Build.Version = "dev"
	}
	if opts.Build.Revision == "" {
		opts.Build.Revision = "unknown"
	}
	if opts.Build.GoVersion == "" {
		opts.Build.GoVersion = runtime.Version()
	}
	return opts
}

func versionCommand(build BuildInfo) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := fmt.Sprintf("statica %s (revision %s; %s", build.Version, build.Revision, build.GoVersion)
			if build.BuildTime != "" {
				info += "; built " + build.BuildTime
			}
			return write(cmd.OutOrStdout(), []byte(info+")\n"))
		},
	}
}

func configCommand(opts Options) *cobra.Command {
	lookup := opts.LookupEnv
	var path string
	parent := &cobra.Command{
		Use:   "config",
		Short: "Inspect configuration or check origin accessibility",
		Long:  "Validate and print operate without credential discovery, DNS, or S3 access.\nCheck-origin explicitly contacts the configured origin.\nSelect one file using --config, STATICA_CONFIG, or ./config.yaml (in that order).",
		Args:  noArgs,
		RunE:  help,
	}
	parent.PersistentFlags().StringVar(&path, "config", "", configFlagUsage)
	load := configLoader(lookup, &path)
	validate := &cobra.Command{
		Use:   "validate",
		Short: "Validate configuration and local TLS material offline",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load(cmd)
			if err != nil {
				return err
			}
			if err := cfg.ValidateTLS(); err != nil {
				return err
			}
			for _, warning := range cfg.CompiledRedirects().Warnings() {
				message := fmt.Sprintf("statica: warning: redirects[%d] may be shadowed by redirects[%d]\n", warning.Rule, warning.ShadowedBy)
				if err := write(cmd.ErrOrStderr(), []byte(message)); err != nil {
					return err
				}
			}
			message := "configuration valid\n"
			if cfg.S3.Region == "" {
				message += "S3 region deferred to AWS discovery at startup\n"
			}
			return write(cmd.OutOrStdout(), []byte(message))
		},
	}
	print := &cobra.Command{
		Use:   "print",
		Short: "Print normalized configuration with environment-derived values redacted",
		Long:  "Print normalized settings and defaults, redacting environment-derived values.\nThis diagnostic output is not guaranteed to round-trip. Local TLS files are not read.",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load(cmd)
			if err != nil {
				return err
			}
			data, err := cfg.MarshalRedacted()
			if err != nil {
				return runtimeError{errors.New("cannot render configuration")}
			}
			return write(cmd.OutOrStdout(), data)
		},
	}
	parent.AddCommand(validate, print, checkOriginCommand(load, opts.OriginFactory))
	return parent
}

const configFlagUsage = "configuration file (overrides STATICA_CONFIG and ./config.yaml)"

// configLoader selects one file using the --config flag bound to path, then
// STATICA_CONFIG, then ./config.yaml, and loads it.
func configLoader(lookup config.LookupEnv, path *string) func(*cobra.Command) (*config.Config, error) {
	return func(cmd *cobra.Command) (*config.Config, error) {
		selected := "./config.yaml"
		switch {
		case cmd.Flags().Changed("config"):
			selected = *path
		default:
			if value, ok := lookup("STATICA_CONFIG"); ok {
				selected = value
			}
		}
		if selected == "" {
			return nil, errors.New("configuration file path must not be empty")
		}
		return config.Load(selected, lookup)
	}
}

func noArgs(_ *cobra.Command, args []string) error {
	if len(args) != 0 {
		return errors.New("unexpected arguments; see --help")
	}
	return nil
}

func help(cmd *cobra.Command, _ []string) error {
	if err := cmd.Help(); err != nil {
		return runtimeError{err}
	}
	return nil
}

type runtimeError struct{ error }

type checkedWriter struct {
	writer io.Writer
	failed bool
}

func (w *checkedWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	if err != nil || n != len(data) {
		w.failed = true
		return n, errors.New("cannot write output")
	}
	return n, nil
}

func write(out io.Writer, data []byte) error {
	if n, err := out.Write(data); err != nil || n != len(data) {
		return runtimeError{errors.New("cannot write output")}
	}
	return nil
}
