// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The tables partitioned by the day (UTC, DB clock) a row was recorded.
// Each has one partition per day, named <table>_YYYYMMDD.
const (
	TableActivity   = "activity"
	TableTraffic    = "traffic_records"
	TableAccessLogs = "access_logs"
)

// PartitionedTables lists the tables with daily partitions.
var PartitionedTables = []string{TableActivity, TableTraffic, TableAccessLogs}

// partitionLockKey serialises the creation and dropping of partitions
// across instances ("ferun" and a 2).
const partitionLockKey int64 = 0x666572756e_0002

// maxPartitionDays bounds one EnsurePartitions call.
const maxPartitionDays = 400

func day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// PartitionName is the name of table's partition for the UTC day of t.
func PartitionName(table string, t time.Time) string {
	return table + "_" + day(t).Format("20060102")
}

func isPartitioned(table string) bool {
	for _, t := range PartitionedTables {
		if t == table {
			return true
		}
	}
	return false
}

// EnsurePartitions creates the partitions of every partitioned table for
// every UTC day from the day of from to the day of to, both included, that
// do not exist yet. It waits at most a second for the tables' locks; the
// transaction is retried on a lock timeout.
func (d *DB) EnsurePartitions(ctx context.Context, from, to time.Time) error {
	return d.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '1s'"); err != nil {
			return err
		}
		return ensurePartitions(ctx, tx, from, to)
	})
}

func ensurePartitions(ctx context.Context, tx pgx.Tx, from, to time.Time) error {
	from, to = day(from), day(to)
	if to.Before(from) {
		return nil
	}
	if to.Sub(from) > maxPartitionDays*24*time.Hour {
		return fmt.Errorf("ensure partitions: %s to %s spans more than %d days", from.Format(time.DateOnly), to.Format(time.DateOnly), maxPartitionDays)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", partitionLockKey); err != nil {
		return fmt.Errorf("partition lock: %w", err)
	}
	for _, table := range PartitionedTables {
		for dd := from; !dd.After(to); dd = dd.AddDate(0, 0, 1) {
			name := PartitionName(table, dd)
			var exists bool
			if err := tx.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil {
				return err
			}
			if exists {
				continue
			}
			// The names are made here from a constant and a date, never
			// from input.
			stmt := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
				pgx.Identifier{name}.Sanitize(), pgx.Identifier{table}.Sanitize(),
				dd.Format(time.DateOnly), dd.AddDate(0, 0, 1).Format(time.DateOnly))
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("create partition %s: %w", name, err)
			}
		}
	}
	return nil
}

// PartitionDays returns the days (UTC midnight) table has partitions for,
// in ascending order.
func (d *DB) PartitionDays(ctx context.Context, table string) ([]time.Time, error) {
	return partitionDays(ctx, d.Pool, table)
}

func partitionDays(ctx context.Context, q Querier, table string) ([]time.Time, error) {
	if !isPartitioned(table) {
		return nil, fmt.Errorf("%s is not a partitioned table", table)
	}
	rows, err := q.Query(ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = $1::regclass ORDER BY c.relname`, table)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var days []time.Time
	for _, n := range names {
		suffix, ok := strings.CutPrefix(n, table+"_")
		if !ok {
			continue
		}
		t, err := time.Parse("20060102", suffix)
		if err != nil {
			continue
		}
		days = append(days, t)
	}
	return days, nil
}

// DropPartitions drops the partitions of table whose whole day lies before
// before: the partition of day D goes when D + 1 day <= before. It returns
// the names dropped. Each partition is dropped in a transaction of its own
// that waits at most a second for the table's lock.
func (d *DB) DropPartitions(ctx context.Context, table string, before time.Time) ([]string, error) {
	days, err := d.PartitionDays(ctx, table)
	if err != nil {
		return nil, err
	}
	var dropped []string
	for _, dd := range days {
		if dd.AddDate(0, 0, 1).After(before) {
			continue
		}
		name := PartitionName(table, dd)
		err := d.Tx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '1s'"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", partitionLockKey); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "DROP TABLE IF EXISTS "+pgx.Identifier{name}.Sanitize())
			return err
		})
		if err != nil {
			return dropped, fmt.Errorf("drop partition %s: %w", name, err)
		}
		dropped = append(dropped, name)
	}
	return dropped, nil
}
