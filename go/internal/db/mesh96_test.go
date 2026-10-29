// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package db_test

import (
	"context"
	"net/netip"
	"testing"

	"github.com/gardenlinux/feos/go/internal/db/dbtest"
	"github.com/gardenlinux/feos/go/internal/mesh"
)

// 0011 moves a cluster meshed on a /48 to its /64 of subnet 0 (ADR 0017):
// every node and ingress keeps its number, its prefix becomes that
// number's /96, as mesh.NodePrefix writes it, an ingress's address its
// <prefix>::1, and the peers' and ferun-ingress's versions move on.
func TestMesh96Migration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 11)

	for _, q := range []string{
		"INSERT INTO pod_network (ula, peers_version, ingress_group_version) VALUES ('fdf0:5f76:17b6::/48', 7, 4)",
		"SELECT setval('mesh_subnets', 7)",
		`INSERT INTO nodes (name, mesh_subnet, mesh_prefix, mesh_endpoint) VALUES
			('n3', 3, 'fdf0:5f76:17b6:3::/64', '10.250.0.2:51820'),
			('n4', 4, 'fdf0:5f76:17b6:4::/64', '10.250.0.1:51820'),
			('n0', NULL, '', '')`,
		`INSERT INTO ingresses (name, mesh_subnet, mesh_prefix, mesh_address, mesh_endpoint, group_version) VALUES
			('g6', 6, 'fdf0:5f76:17b6:6::/64', 'fdf0:5f76:17b6:6::1', '100.144.1.45:51820', 3),
			('g7', 7, 'fdf0:5f76:17b6:7::/64', 'fdf0:5f76:17b6:7::1', '100.144.2.47:51820', 4),
			('g0', NULL, '', '', '', 0)`,
	} {
		if _, err := d.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := d.Migrate(ctx, nil); err != nil {
		t.Fatal(err)
	}

	var ula netip.Prefix
	var peers, group int64
	if err := d.Pool.QueryRow(ctx, "SELECT ula, peers_version, ingress_group_version FROM pod_network").
		Scan(&ula, &peers, &group); err != nil {
		t.Fatal(err)
	}
	if ula != netip.MustParsePrefix("fdf0:5f76:17b6::/64") || peers != 8 || group != 5 {
		t.Fatalf("pod network %v, peers %d, group %d", ula, peers, group)
	}
	for name, id := range map[string]int{"n3": 3, "n4": 4} {
		var prefix string
		if err := d.Pool.QueryRow(ctx, "SELECT mesh_prefix FROM nodes WHERE name = $1", name).Scan(&prefix); err != nil {
			t.Fatal(err)
		}
		if want := mesh.NodePrefix(ula, id).String(); prefix != want {
			t.Errorf("node %s: %q, want %q", name, prefix, want)
		}
	}
	for name, id := range map[string]int{"g6": 6, "g7": 7} {
		var prefix, address string
		var version int64
		if err := d.Pool.QueryRow(ctx, "SELECT mesh_prefix, mesh_address, group_version FROM ingresses WHERE name = $1",
			name).Scan(&prefix, &address, &version); err != nil {
			t.Fatal(err)
		}
		p := mesh.NodePrefix(ula, id)
		if prefix != p.String() || address != mesh.HostAddress(p).String() || version != group {
			t.Errorf("ingress %s: %q %q %d, want %q %q %d", name, prefix, address, version, p, mesh.HostAddress(p), group)
		}
	}
	var unassigned int
	if err := d.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM nodes WHERE name = 'n0' AND mesh_prefix = '')
		+ (SELECT count(*) FROM ingresses WHERE name = 'g0' AND mesh_prefix = '' AND group_version = 0)`).
		Scan(&unassigned); err != nil || unassigned != 2 {
		t.Fatalf("rows without a number changed: %d %v", unassigned, err)
	}
	// The sequence goes on from where it was, up to 2^32 - 1.
	var next, last int64
	if err := d.Pool.QueryRow(ctx, "SELECT nextval('mesh_subnets'), (SELECT max_value FROM pg_sequences WHERE sequencename = 'mesh_subnets')").
		Scan(&next, &last); err != nil || next != 8 || last != mesh.LastSubnet {
		t.Fatalf("sequence %d, max %d, %v", next, last, err)
	}
}

// 0011 on a routed cluster changes nothing but the types.
func TestMesh96MigrationRouted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := dbtest.Open(t)
	migrateBefore(t, d, 11)
	if _, err := d.Pool.Exec(ctx, "INSERT INTO nodes (name) VALUES ('n1')"); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var meshed bool
	if err := d.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pod_network)").Scan(&meshed); err != nil || meshed {
		t.Fatalf("meshed %v %v", meshed, err)
	}
}
