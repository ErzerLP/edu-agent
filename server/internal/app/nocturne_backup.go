package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/edu-agent/edu-agent/server/internal/integrations/nocturne"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	platformpostgres "github.com/edu-agent/edu-agent/server/internal/platform/postgres"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacypostgres "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
)

const linuxTmpfsMagic = 0x01021994

type managedBackupRestorer interface {
	RestoreToFile(context.Context, privacy.ManagedBackupArtifact, string) error
}

// RestoreNocturneBackup is a local operator entry point. It verifies the
// existing application schema without running migrations, resolves the exact
// artifact through the database inventory, and restores only to tmpfs.
func RestoreNocturneBackup(ctx context.Context, cfg config.Config, artifactPath, output string) error {
	if !cfg.Nocturne.Enabled || len(cfg.Nocturne.MasterWrappingKey) != 32 {
		return errors.New("Nocturne managed backup restore is not configured")
	}
	if err := requireTmpfsRestoreDestination(output); err != nil {
		return err
	}
	pool, err := platformpostgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := migrations.Check(ctx, pool); err != nil {
		return fmt.Errorf("check database migrations before managed backup restore: %w", err)
	}
	repository, err := privacypostgres.NewManagedBackupRepository(pool, cfg.Nocturne.MasterWrappingKey)
	if err != nil {
		return fmt.Errorf("initialize managed backup repository: %w", err)
	}
	controller, err := nocturne.NewBackupRestoreController(cfg.Nocturne.BackupRoot, repository, repository)
	if err != nil {
		return fmt.Errorf("initialize managed backup restore controller: %w", err)
	}
	return restoreNocturneBackupArtifact(ctx, artifactPath, output, repository, controller)
}

func restoreNocturneBackupArtifact(
	ctx context.Context,
	artifactPath string,
	output string,
	inventory privacy.ManagedBackupInventoryRepository,
	restorer managedBackupRestorer,
) error {
	if !validRequestedBackupArtifactPath(artifactPath) || inventory == nil || restorer == nil {
		return privacy.ErrManagedBackupInvalid
	}
	artifacts, err := inventory.ManagedBackupInventory(ctx)
	if err != nil {
		return fmt.Errorf("read managed backup inventory: %w", err)
	}
	var selected *privacy.ManagedBackupArtifact
	for index := range artifacts {
		if artifacts[index].Validate() != nil {
			return privacy.ErrManagedBackupIntegrity
		}
		if artifacts[index].Path == artifactPath {
			if selected != nil {
				return privacy.ErrManagedBackupIntegrity
			}
			selected = &artifacts[index]
		}
	}
	if selected == nil {
		return privacy.ErrManagedBackupIntegrity
	}
	if err := restorer.RestoreToFile(ctx, *selected, output); err != nil {
		return fmt.Errorf("restore managed backup: %w", err)
	}
	return nil
}

func validRequestedBackupArtifactPath(value string) bool {
	return value != "" && value != "." && value != ".." && value == filepath.Base(value) &&
		!filepath.IsAbs(value) && filepath.Clean(value) == value &&
		value != "managed-inventory.json" && value != ".edu-agent-backup.lock" &&
		!strings.ContainsAny(value, "/\\\x00")
}

func requireTmpfsRestoreDestination(output string) error {
	if output == "" || !filepath.IsAbs(output) || filepath.Clean(output) != output || filepath.Base(output) == "." {
		return errors.New("managed backup restore output must be a canonical absolute tmpfs path")
	}
	if _, err := os.Lstat(output); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("managed backup restore output must not already exist")
	}
	parent := filepath.Dir(output)
	fd, err := syscall.Open(parent, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("managed backup restore output parent is unavailable")
	}
	defer syscall.Close(fd)
	var filesystem syscall.Statfs_t
	if err := syscall.Fstatfs(fd, &filesystem); err != nil || uint64(filesystem.Type) != linuxTmpfsMagic {
		return errors.New("managed backup restore output must be on tmpfs")
	}
	return nil
}
