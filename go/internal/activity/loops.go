// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package activity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ironcore-dev/feos/go/internal/db"
	"github.com/ironcore-dev/feos/go/internal/model"
)

// RetentionConfig is how long the partitioned logs are kept
// (--activity-retention, --traffic-retention); 0 keeps them forever.
type RetentionConfig struct {
	Activity, Traffic time.Duration
}

// LoopPartitions names the partition and retention loop.
const LoopPartitions = "activity-partitions"

// daysAhead is how many days after today have partitions.
const daysAhead = 2

// Loops returns the leader's hourly loop that keeps the partitions of
// activity and traffic_records for today and the next two days (DB clock,
// UTC) and drops the partitions past their retention. It returns
// model.Loop, which leader.Loop aliases.
func Loops(d *db.DB, rc RetentionConfig, log *slog.Logger) []model.Loop {
	if log == nil {
		log = slog.Default()
	}
	return []model.Loop{{
		Name:     LoopPartitions,
		Interval: time.Hour,
		Run: func(ctx context.Context) error {
			return maintainPartitions(ctx, d, rc, log)
		},
	}}
}

// maintainPartitions is one run of the loop. It is idempotent and unfenced:
// two leaders running it at once create and drop the same partitions.
func maintainPartitions(ctx context.Context, d *db.DB, rc RetentionConfig, log *slog.Logger) error {
	var now time.Time
	if err := d.Pool.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		return fmt.Errorf("partitions: read the clock: %w", err)
	}
	var errs []error
	if err := d.EnsurePartitions(ctx, now, now.AddDate(0, 0, daysAhead)); err != nil {
		errs = append(errs, fmt.Errorf("partitions: %w", err))
	}
	for _, t := range []struct {
		table     string
		retention time.Duration
	}{{db.TableActivity, rc.Activity}, {db.TableTraffic, rc.Traffic}} {
		if t.retention <= 0 {
			continue
		}
		dropped, err := d.DropPartitions(ctx, t.table, now.Add(-t.retention))
		if len(dropped) > 0 {
			log.Info("dropped partitions past retention", "table", t.table, "retention", t.retention, "partitions", dropped)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("retention of %s: %w", t.table, err))
		}
	}
	return errors.Join(errs...)
}
