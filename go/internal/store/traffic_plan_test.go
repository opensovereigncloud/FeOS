// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// planNode is a node of EXPLAIN (ANALYZE, FORMAT JSON)'s plan.
type planNode struct {
	NodeType       string     `json:"Node Type"`
	IndexName      string     `json:"Index Name"`
	ActualRows     float64    `json:"Actual Rows"`
	ActualLoops    float64    `json:"Actual Loops"`
	RemovedFilter  float64    `json:"Rows Removed by Filter"`
	RemovedRecheck float64    `json:"Rows Removed by Index Recheck"`
	Plans          []planNode `json:"Plans"`
}

// walk calls f for n and every node beneath it.
func (n planNode) walk(f func(planNode)) {
	f(n)
	for _, c := range n.Plans {
		c.walk(f)
	}
}

// A page of a node's foreign traffic records reads only foreign records,
// from the partial index traffic_node_foreign. Without it, a node that
// sent many records attributed to sandboxes and a few foreign ones had all
// of them read, filtered and sorted for every page of its activity.
func TestNodeTrafficPageUsesItsIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.New(t)
	s := New(d)
	start := uuid.New()
	const records, foreignEvery = 20000, 100
	var rows []TrafficRow
	for seq := int64(1); seq <= records; seq++ {
		r := TrafficRow{TsNs: seq, Node: "n1", StartID: start, Seq: seq, PodID: podid.Mint(), Kind: "flow"}
		if seq%foreignEvery == 0 {
			r.ForeignPod = true // attributed to no sandbox
		} else {
			r.SandboxID = r.PodID
		}
		rows = append(rows, r)
	}
	if err := s.InsertTraffic(ctx, rows...); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, "ANALYZE traffic_records"); err != nil {
		t.Fatal(err)
	}
	children := map[string]bool{}
	crows, err := d.Pool.Query(ctx, `SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'traffic_node_foreign'::regclass`)
	if err != nil {
		t.Fatalf("the index traffic_node_foreign: %v", err)
	}
	for crows.Next() {
		var name string
		if err := crows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		children[name] = true
	}
	if err := crows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, q := range []ActivityQuery{{Limit: 50}, {Limit: 50, BeforeTsNs: records / 2}} {
		sql, args := trafficPage(nodeTrafficWhere, "n1", q)
		var out []byte
		if err := d.Pool.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql, args...).Scan(&out); err != nil {
			t.Fatal(err)
		}
		var plans []struct {
			Plan planNode `json:"Plan"`
		}
		if err := json.Unmarshal(out, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("plan %s: %v", out, err)
		}
		var read float64
		var fromIndex bool
		plans[0].Plan.walk(func(n planNode) {
			if strings.HasSuffix(n.NodeType, "Scan") {
				read += (n.ActualRows + n.RemovedFilter + n.RemovedRecheck) * max(n.ActualLoops, 1)
			}
			fromIndex = fromIndex || children[n.IndexName]
		})
		if !fromIndex || read > records/foreignEvery {
			t.Errorf("%+v: %v records read (%d are foreign), from traffic_node_foreign: %v; the plan: %s",
				q, read, records/foreignEvery, fromIndex, out)
		}
	}
}
