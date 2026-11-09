package cli

import (
	"context"
	"errors"
	"io"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/storage"
	origins3 "github.com/gtriggiano/statica/internal/storage/s3"
	"github.com/spf13/cobra"
)

// Origin is the read-only process resource created for an online origin check.
// Offline commands never call its factory.
type Origin interface {
	storage.Reader
	io.Closer
}

type OriginFactory func(context.Context, config.S3) (Origin, error)

func defaultOriginFactory(ctx context.Context, cfg config.S3) (Origin, error) {
	origin, err := origins3.New(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return origin, nil
}

func checkOriginCommand(load func(*cobra.Command) (*config.Config, error), factory OriginFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "check-origin",
		Short: "Check one known origin object's accessibility (online, read-only)",
		Long:  "Check one known object using HEAD, with a metadata-only GET if absence is ambiguous.\nUses admin.origin_health.key when configured, otherwise index.\nThis checks accessibility; it does not certify optional provider capabilities.",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load(cmd)
			if err != nil {
				return err
			}
			if cfg.S3.InsecureSkipVerify {
				if err := write(cmd.ErrOrStderr(), []byte("statica: warning: origin TLS certificate verification is disabled\n")); err != nil {
					return err
				}
			}
			key := cfg.Index
			if cfg.Admin.OriginHealth != nil && cfg.Admin.OriginHealth.Key != "" {
				key = cfg.Admin.OriginHealth.Key
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), cfg.Server.ResponseHeaderTimeout)
			defer cancel()
			if err := probeOrigin(ctx, cfg.S3, cfg.S3.Prefix+key, factory); err != nil {
				return runtimeError{err}
			}
			return write(cmd.OutOrStdout(), []byte("origin object accessible\n"))
		},
	}
}

func probeOrigin(ctx context.Context, cfg config.S3, key string, factory OriginFactory) (err error) {
	// Keep every public failure fixed text: neither injected factories nor SDK
	// errors may contribute endpoint, object, credential, or provider response text.
	if ctx.Err() != nil {
		return originCheckFailure(ctx, nil)
	}
	origin, createErr := factory(ctx, cfg)
	if origin != nil {
		defer func() {
			if closeErr := origin.Close(); closeErr != nil && err == nil {
				err = originCheckFailure(ctx, closeErr)
			}
		}()
	}
	if createErr != nil || origin == nil {
		return originCheckFailure(ctx, createErr)
	}
	if ctx.Err() != nil {
		return originCheckFailure(ctx, nil)
	}
	_, readErr := origin.Head(ctx, key)
	if ctx.Err() != nil {
		return originCheckFailure(ctx, readErr)
	}
	if storage.IsKind(readErr, storage.AmbiguousNotFound) {
		object, getErr := origin.Get(ctx, key, storage.ReadOptions{})
		var closeErr error
		if object != nil && object.Body != nil {
			closeErr = object.Body.Close()
		}
		if getErr != nil {
			return originCheckFailure(ctx, getErr)
		}
		if closeErr != nil {
			return originCheckFailure(ctx, closeErr)
		}
		if object == nil || object.Body == nil {
			return originCheckFailure(ctx, &storage.Error{Kind: storage.InvalidResponse, Size: -1})
		}
		readErr = nil
	}
	if readErr != nil || ctx.Err() != nil {
		return originCheckFailure(ctx, readErr)
	}
	return nil
}

// Only known, finite diagnostics may appear in command output. In particular,
// never format an arbitrary Kind value, which injected readers could populate
// with a provider message or a secret.
func originCheckFailure(ctx context.Context, cause error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(cause, context.DeadlineExceeded) {
		return errors.New("origin check timed out")
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(cause, context.Canceled) {
		return errors.New("origin check canceled")
	}
	var failure *storage.Error
	if errors.As(cause, &failure) {
		switch failure.Kind {
		case storage.Denied:
			return errors.New("origin check failed: access denied")
		case storage.NotFound:
			return errors.New("origin check failed: object not found")
		case storage.Configuration:
			return errors.New("origin check failed: origin configuration")
		case storage.Unavailable:
			return errors.New("origin check failed: origin unavailable")
		case storage.Throttled:
			return errors.New("origin check failed: origin throttled")
		case storage.Timeout:
			return errors.New("origin check timed out")
		case storage.Canceled:
			return errors.New("origin check canceled")
		case storage.Unsupported:
			return errors.New("origin check failed: unsupported origin operation")
		case storage.InvalidResponse, storage.Precondition, storage.NotModified, storage.InvalidRange:
			return errors.New("origin check failed: invalid origin response")
		case storage.AmbiguousNotFound:
			return errors.New("origin check failed: ambiguous object absence")
		case storage.ChecksumMismatch:
			return errors.New("origin check failed: checksum mismatch")
		case storage.InvalidKey:
			return errors.New("origin check failed: invalid origin key")
		}
	}
	return errors.New("origin check failed")
}
