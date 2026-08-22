package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/app"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	"github.com/edu-agent/edu-agent/server/internal/platform/observability"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

const usage = "usage: edu-agentd [serve|pairing-code create|privacy-grant create --device <uuid>]"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, observability.RedactString(err.Error()))
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Fprintln(os.Stdout, usage)
		return nil
	}
	serve := len(args) == 0 || (len(args) == 1 && args[0] == "serve")
	createPairingCode := len(args) == 2 && args[0] == "pairing-code" && args[1] == "create"
	createPrivacyGrant := len(args) == 4 && args[0] == "privacy-grant" && args[1] == "create" && args[2] == "--device"
	if !serve && !createPairingCode && !createPrivacyGrant {
		return errors.New(usage)
	}
	if createPrivacyGrant && !privacy.CanonicalUUID(args[3]) {
		return errors.New("privacy-grant --device must be a canonical UUID")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration error: %w", err)
	}
	logger := observability.NewJSON(os.Stderr, slog.LevelInfo)
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if serve {
		return app.Run(ctx, cfg, logger)
	}
	if createPairingCode {
		code, expiresAt, err := app.CreatePairingCode(ctx, cfg)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, code)
		fmt.Fprintf(os.Stderr, "pairing code expires at %s\n", expiresAt.UTC().Format(time.RFC3339))
		return nil
	}
	grant, expiresAt, err := app.CreatePrivacyErasureGrant(ctx, cfg, args[3])
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, grant)
	fmt.Fprintf(os.Stderr, "privacy erasure grant expires at %s\n", expiresAt.UTC().Format(time.RFC3339))
	return nil
}
