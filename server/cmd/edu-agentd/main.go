package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

const usage = "usage: edu-agentd [serve|pairing-code create|privacy-grant create --device <uuid>|nocturne-backup restore --artifact <relative-path> --output <tmpfs-path>]"

var (
	loadConfiguration     = config.Load
	restoreNocturneBackup = app.RestoreNocturneBackup
)

type commandKind int

const (
	commandServe commandKind = iota
	commandPairingCode
	commandPrivacyGrant
	commandNocturneBackupRestore
)

type command struct {
	kind         commandKind
	deviceID     string
	artifactPath string
	output       string
}

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
	parsed, err := parseCommand(args)
	if err != nil {
		return err
	}

	cfg, err := loadConfiguration()
	if err != nil {
		return fmt.Errorf("configuration error: %w", err)
	}
	logger := observability.NewJSON(os.Stderr, slog.LevelInfo)
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if parsed.kind == commandServe {
		return app.Run(ctx, cfg, logger)
	}
	if parsed.kind == commandPairingCode {
		code, expiresAt, err := app.CreatePairingCode(ctx, cfg)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, code)
		fmt.Fprintf(os.Stderr, "pairing code expires at %s\n", expiresAt.UTC().Format(time.RFC3339))
		return nil
	}
	if parsed.kind == commandPrivacyGrant {
		grant, expiresAt, err := app.CreatePrivacyErasureGrant(ctx, cfg, parsed.deviceID)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, grant)
		fmt.Fprintf(os.Stderr, "privacy erasure grant expires at %s\n", expiresAt.UTC().Format(time.RFC3339))
		return nil
	}
	return restoreNocturneBackup(ctx, cfg, parsed.artifactPath, parsed.output)
}

func parseCommand(args []string) (command, error) {
	if len(args) == 0 || len(args) == 1 && args[0] == "serve" {
		return command{kind: commandServe}, nil
	}
	if len(args) == 2 && args[0] == "pairing-code" && args[1] == "create" {
		return command{kind: commandPairingCode}, nil
	}
	if len(args) == 4 && args[0] == "privacy-grant" && args[1] == "create" && args[2] == "--device" {
		if !privacy.CanonicalUUID(args[3]) {
			return command{}, errors.New("privacy-grant --device must be a canonical UUID")
		}
		return command{kind: commandPrivacyGrant, deviceID: args[3]}, nil
	}
	if len(args) >= 2 && args[0] == "nocturne-backup" && args[1] == "restore" {
		flags := flag.NewFlagSet("nocturne-backup restore", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		artifact := flags.String("artifact", "", "managed backup artifact path")
		output := flags.String("output", "", "tmpfs output path")
		if err := flags.Parse(args[2:]); err != nil || flags.NArg() != 0 || *artifact == "" || *output == "" {
			return command{}, errors.New(usage)
		}
		return command{kind: commandNocturneBackupRestore, artifactPath: *artifact, output: *output}, nil
	}
	return command{}, errors.New(usage)
}
