// Command statica serves an S3-backed website and validates and inspects its
// configuration.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/gtriggiano/statica/internal/cli"
)

// These values may be set at build time with -ldflags -X.
var (
	version   = "dev"
	revision  = "unknown"
	buildTime = ""
)

func main() {
	// Deliver SIGINT and SIGTERM to the command rather than terminating:
	// serve drains on the first and forces exit on another, and every other
	// command is cancelled by the first. The buffer holds both signals.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	// A closed standard output or error must not end the process: writes
	// then fail with EPIPE. serve counts its lost log lines as drops; other
	// commands fail the write and exit through their normal error path.
	signal.Ignore(syscall.SIGPIPE)
	os.Exit(cli.Run(context.Background(), os.Args[1:], cli.Options{
		Out:       os.Stdout,
		Err:       os.Stderr,
		LookupEnv: os.LookupEnv,
		Build:     cli.BuildInfo{Version: version, Revision: revision, BuildTime: buildTime},
		Signals:   signals,
	}))
}
