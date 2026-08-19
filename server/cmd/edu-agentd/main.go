package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/edu-agent/edu-agent/server/internal/app"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	"github.com/edu-agent/edu-agent/server/internal/platform/observability"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, observability.RedactString(err.Error()))
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Fprintln(os.Stdout, "usage: edu-agentd [serve|pairing-code create]")
		return nil
	}
	serve := len(args) == 0 || (len(args) == 1 && args[0] == "serve")
	createPairingCode := len(args) == 2 && args[0] == "pairing-code" && args[1] == "create"
	if !serve && !createPairingCode {
		return fmt.Errorf("usage: edu-agentd [serve|pairing-code create]")
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
	code, expiresAt, err := app.CreatePairingCode(ctx, cfg)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, code)
	fmt.Fprintf(os.Stderr, "pairing code expires at %s\n", expiresAt.Format("2006-01-02T15:04:05Z07:00"))
	return nil
}
