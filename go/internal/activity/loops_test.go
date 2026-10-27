// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package activity_test

import (
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/activity"
	"github.com/gardenlinux/feos/go/internal/db"
	"github.com/gardenlinux/feos/go/internal/db/dbtest"
)

func days(t *testing.T, d *db.DB, table string) []time.Time {
	t.Helper()
	ds, err := d.PartitionDays(bg, table)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

// The partition loop keeps today and the next two days of both tables and
// drops each table's partitions past its own retention.
func TestPartitionLoop(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	var now time.Time
	if err := d.Pool.QueryRow(bg, "SELECT now()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	// Old partitions, 40 to 3 days back, with rows.
	if err := d.EnsurePartitions(bg, today.AddDate(0, 0, -40), today.AddDate(0, 0, -3)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(bg, "INSERT INTO activity (day, ts_ns, source, kind) VALUES ($1, 1, 'ferund', 'old')", today.AddDate(0, 0, -40)); err != nil {
		t.Fatal(err)
	}

	loops := activity.Loops(d, activity.RetentionConfig{Activity: 30 * 24 * time.Hour, Traffic: 7 * 24 * time.Hour}, dbtest.Logger(t))
	if len(loops) != 1 || loops[0].Name != activity.LoopPartitions || loops[0].Interval != time.Hour {
		t.Fatalf("loops %+v", loops)
	}
	if err := loops[0].Run(bg); err != nil {
		t.Fatal(err)
	}
	// Activity: the partition of day D goes once D + 1 day lies past the
	// retention; the rest stays, up to two days ahead.
	act := days(t, d, db.TableActivity)
	if !act[len(act)-1].Equal(today.AddDate(0, 0, 2)) {
		t.Fatalf("activity ends %s", act[len(act)-1])
	}
	cutoff := now.Add(-30 * 24 * time.Hour)
	for _, day := range act {
		if !day.AddDate(0, 0, 1).After(cutoff) {
			t.Fatalf("activity partition %s is past the retention", day)
		}
	}
	if !act[0].Before(today.AddDate(0, 0, -29)) {
		t.Fatalf("activity starts %s: dropped within the retention", act[0])
	}
	traf := days(t, d, db.TableTraffic)
	if !traf[len(traf)-1].Equal(today.AddDate(0, 0, 2)) || traf[0].Before(today.AddDate(0, 0, -8)) || !traf[0].Before(today.AddDate(0, 0, -6)) {
		t.Fatalf("traffic partitions %s to %s", traf[0], traf[len(traf)-1])
	}
	var old int
	if err := d.Pool.QueryRow(bg, "SELECT count(*) FROM activity WHERE kind = 'old'").Scan(&old); err != nil {
		t.Fatal(err)
	}
	if old != 0 {
		t.Fatal("a row past the retention survived")
	}

	// Idempotent; a retention of 0 keeps everything.
	if err := loops[0].Run(bg); err != nil {
		t.Fatal(err)
	}
	keep := activity.Loops(d, activity.RetentionConfig{}, nil)
	before := len(days(t, d, db.TableTraffic))
	if err := keep[0].Run(bg); err != nil {
		t.Fatal(err)
	}
	if len(days(t, d, db.TableTraffic)) != before {
		t.Fatal("retention 0 dropped partitions")
	}
}
