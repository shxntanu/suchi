// SPDX-License-Identifier: AGPL-3.0-or-later

// Package storage assembles the configured immutable blob provider.
package storage

import (
	"context"
	"fmt"

	"github.com/johnnybravo-xyz/suchi/core/blob"
	"github.com/johnnybravo-xyz/suchi/core/blob/gdrive"
	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
	"github.com/johnnybravo-xyz/suchi/core/gc"
)

// DriveConfig maps explicit Suchi settings to the owner-authorized provider.
// It never reads another application's environment files.
func DriveConfig(cfg *config.Config, create bool) gdrive.Config {
	return gdrive.Config{
		ClientID: cfg.GDriveClientID, ClientSecret: cfg.GDriveClientSecret,
		RefreshToken: cfg.GDriveRefreshToken, FolderID: cfg.GDriveFolderID,
		CreateNamespace: create,
	}
}

// CleanupWorkingFiles removes abandoned owned working files during server startup.
// All readers and writers must be stopped; CLI constructors never run this sweep.
func CleanupWorkingFiles(cfg *config.Config) error {
	if cfg.StorageProvider != "gdrive" {
		return nil
	}
	return blob.CleanupRemoteTempFiles(cfg.DataDir)
}

// New opens the configured CAS. Read-only commands must pass create=false.
func New(ctx context.Context, cfg *config.Config, create bool) (*blob.CAS, error) {
	switch cfg.StorageProvider {
	case "", "local":
		return blob.New(cfg.DataDir)
	case "gdrive":
		backend, err := gdrive.New(ctx, DriveConfig(cfg, create))
		if err != nil {
			return nil, err
		}
		return blob.NewRemote(cfg.DataDir, backend)
	default:
		return nil, fmt.Errorf("unknown storage provider")
	}
}

// ValidateReferences refuses remote startup with missing catalog bytes, including
// an accidental provider switch before the local-to-Drive transfer is complete.
func ValidateReferences(ctx context.Context, d *db.DB, cas *blob.CAS) error {
	if !cas.IsRemote() {
		return nil
	}
	refs, err := gc.CollectReferences(ctx, d)
	if err != nil {
		return err
	}
	for sum := range refs {
		if _, err := cas.StatContext(ctx, sum); err != nil {
			return fmt.Errorf("Drive archive is incomplete at blob %s; run offline storage-transfer: %w", sum, err)
		}
	}
	return nil
}
