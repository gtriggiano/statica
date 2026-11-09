package cli

import (
	"context"
	"io"

	"github.com/gtriggiano/statica/internal/app"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/metrics"
	"github.com/spf13/cobra"
)

// ServeFunc runs the server for a loaded configuration whose TLS material
// was validated, until shutdown. A nil result exits 0 and an error exits 1,
// as app.Run documents.
type ServeFunc func(context.Context, *config.Config, app.Options) error

func serveCommand(opts Options, logs io.Writer) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the configured bucket (online)",
		Long: "Serve the configured bucket on the public listener, with liveness, readiness and\n" +
			"metrics on the admin listener. Logs are logfmt events on stdout. Contacts AWS\n" +
			"credential providers and the origin.\n" +
			"Select one file using --config, STATICA_CONFIG, or ./config.yaml (in that order).\n" +
			"SIGINT or SIGTERM drains within server.shutdown_timeout; another signal forces exit.\n" +
			"Exit status: 0 after a signal, even one that forced the drain or stopped the startup;\n" +
			"1 for runtime failures (origin client, bind or listener failure); 2 for configuration errors.",
		Args: noArgs,
		// serve receives the raw signals itself, so a second one can force
		// exit; only the root's cancellation check applies.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := cmd.Context().Err(); err != nil {
				return runtimeError{err}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := configLoader(opts.LookupEnv, &path)(cmd)
			if err != nil {
				return err
			}
			if err := cfg.ValidateTLS(); err != nil {
				return err
			}
			build := metrics.BuildInfo{Version: opts.Build.Version, Revision: opts.Build.Revision, GoVersion: opts.Build.GoVersion}
			if err := opts.Serve(cmd.Context(), cfg, app.Options{Logs: logs, Build: build, Signals: opts.Signals}); err != nil {
				return runtimeError{err}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "config", "", configFlagUsage)
	return cmd
}
