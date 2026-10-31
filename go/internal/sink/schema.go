// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gardenlinux/feos/go/internal/db"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// The hot stage's tables, partitioned by the day a row arrived: one
// partition a day, <table>_YYYYMMDD. log_batches holds it; log_lines did
// before (a row a line) and goes once the instances of before are gone.
const (
	tableBatches = "log_batches"
	tableLines   = "log_lines"
)

// hotTables are the tables whose partitions are made and dropped.
var hotTables = []string{tableBatches, tableLines}

// partitionLockKey serialises the creation and dropping of the hot stage's
// partitions across instances ("sink" and a 2).
const partitionLockKey int64 = 0x73696e6b_0002

// daysAhead is how many days after today have partitions.
const daysAhead = 2

// Migrate applies the sink's migrations (its own schema_migrations in its
// own database) and creates the hot stage's partitions of today and the
// next two days (DB clock, UTC), of each of hotTables.
func Migrate(ctx context.Context, d *db.DB) error {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return err
	}
	return d.MigrateFS(ctx, sub, func(ctx context.Context, tx pgx.Tx) error {
		return ensurePartitions(ctx, tx)
	})
}

func day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func partitionName(table string, t time.Time) string {
	return table + "_" + day(t).Format("20060102")
}

// ensurePartitions creates the partitions of today and the next daysAhead
// days that do not exist yet, under the partition lock.
func ensurePartitions(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", partitionLockKey); err != nil {
		return fmt.Errorf("partition lock: %w", err)
	}
	var today time.Time
	if err := tx.QueryRow(ctx, "SELECT (now() AT TIME ZONE 'utc')::date").Scan(&today); err != nil {
		return err
	}
	for _, table := range hotTables {
		for i := 0; i <= daysAhead; i++ {
			dd := today.AddDate(0, 0, i)
			// The names are made here from a constant and a date, never
			// from input.
			stmt := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
				pgx.Identifier{partitionName(table, dd)}.Sanitize(), table,
				dd.Format(time.DateOnly), dd.AddDate(0, 0, 1).Format(time.DateOnly))
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("create partition %s: %w", partitionName(table, dd), err)
			}
		}
	}
	return nil
}

// maintainPartitions creates the partitions ahead and drops those of the
// days before yesterday that are empty: a row arrives into today's, so a
// past day's only empties. It returns the names dropped.
func maintainPartitions(ctx context.Context, d *db.DB) ([]string, error) {
	if err := d.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '1s'"); err != nil {
			return err
		}
		return ensurePartitions(ctx, tx)
	}); err != nil {
		return nil, err
	}
	var today time.Time
	if err := d.Pool.QueryRow(ctx, "SELECT (now() AT TIME ZONE 'utc')::date").Scan(&today); err != nil {
		return nil, err
	}
	type partition struct{ table, name string }
	var parts []partition
	for _, table := range hotTables {
		rows, err := d.Pool.Query(ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
			WHERE i.inhparent = $1::regclass ORDER BY c.relname`, table)
		if err != nil {
			return nil, err
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			parts = append(parts, partition{table, n})
		}
	}
	var dropped []string
	for _, p := range parts {
		n := p.name
		suffix, ok := strings.CutPrefix(n, p.table+"_")
		if !ok {
			continue
		}
		t, err := time.Parse("20060102", suffix)
		if err != nil || !t.Before(today.AddDate(0, 0, -1)) {
			continue
		}
		ident := pgx.Identifier{n}.Sanitize()
		gone := false
		err = d.Tx(ctx, func(tx pgx.Tx) error {
			gone = false
			if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '1s'"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", partitionLockKey); err != nil {
				return err
			}
			var exists bool
			if err := tx.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", n).Scan(&exists); err != nil || !exists {
				return err
			}
			var full bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+ident+")").Scan(&full); err != nil || full {
				return err
			}
			if _, err := tx.Exec(ctx, "DROP TABLE "+ident); err != nil {
				return err
			}
			gone = true
			return nil
		})
		if err != nil {
			return dropped, fmt.Errorf("drop partition %s: %w", n, err)
		}
		if gone {
			dropped = append(dropped, n)
		}
	}
	return dropped, nil
}
