// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package activity

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ironcore-dev/feos/go/internal/db/dbtest"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// A batch the database refuses for some of its rows loses only those:
// here an unpaired surrogate that bypassed CleanJSON and a source the
// table's check refuses, among rows that are fine.
func TestFlushDropsOnlyRefusedRows(t *testing.T) {
	t.Parallel()
	d := dbtest.New(t)
	r := NewRecorder(store.New(d), "i1", dbtest.Logger(t))
	batch := make([]store.ActivityRow, maxBatch)
	for i := range batch {
		batch[i] = r.Row(Event{Kind: KindSandboxState, Name: "fine"})
	}
	batch[100].Params = json.RawMessage(`{"a":"\ud800"}`)
	batch[200].Source = "bogus"
	if !r.flush(context.Background(), nil, batch) {
		t.Fatal("flush reports a failure other than the refused rows")
	}
	var n int
	if err := d.Pool.QueryRow(context.Background(), "SELECT count(*) FROM activity WHERE name = 'fine'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != maxBatch-2 || r.Dropped() != 2 {
		t.Fatalf("%d rows stored and %d dropped, want %d and 2", n, r.Dropped(), maxBatch-2)
	}
}
