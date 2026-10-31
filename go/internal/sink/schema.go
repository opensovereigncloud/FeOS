// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"embed"
	"io/fs"

	"github.com/gardenlinux/feos/go/internal/db"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate applies the sink's migrations (its own schema_migrations in its
// own database).
func Migrate(ctx context.Context, d *db.DB) error {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return err
	}
	return d.MigrateFS(ctx, sub, nil)
}
