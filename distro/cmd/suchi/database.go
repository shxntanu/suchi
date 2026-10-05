// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"

	"github.com/johnnybravo-xyz/suchi/core/config"
	"github.com/johnnybravo-xyz/suchi/core/db"
)

func openConfiguredDB(ctx context.Context, cfg *config.Config, path string) (*db.DB, error) {
	return db.OpenWithOptions(ctx, path, db.OpenOptions{
		TursoURL:   cfg.TursoDatabaseURL,
		TursoToken: cfg.TursoAuthToken,
	})
}
