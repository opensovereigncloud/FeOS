// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestDraw(t *testing.T) {
	t.Parallel()
	seen := map[netip.Prefix]bool{}
	for range 64 {
		p, err := Draw()
		if err != nil {
			t.Fatal(err)
		}
		a := p.Addr().As16()
		if p.Bits() != 48 || a[0] != 0xfd || p.Masked() != p {
			t.Fatalf("drew %v", p)
		}
		seen[p] = true
	}
	if len(seen) < 60 {
		t.Fatalf("only %d of 64 draws differ", len(seen))
	}
}

func TestNodePrefix(t *testing.T) {
	t.Parallel()
	ula := netip.MustParsePrefix("fd5e:1b2c:93a0::/48")
	for subnet, want := range map[int]string{
		1: "fd5e:1b2c:93a0:1::/64", 7: "fd5e:1b2c:93a0:7::/64", 0x1234: "fd5e:1b2c:93a0:1234::/64",
		65535: "fd5e:1b2c:93a0:ffff::/64",
	} {
		if got := NodePrefix(ula, subnet).String(); got != want {
			t.Errorf("subnet %d: %s, want %s", subnet, got, want)
		}
	}
}

func TestCheckEndpoint(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"10.0.0.11:51820": "10.0.0.11:51820", "[2001:db8::11]:51820": "[2001:db8::11]:51820",
		"[2001:0db8:0::11]:1": "[2001:db8::11]:1", "127.0.0.1:65535": "127.0.0.1:65535",
	} {
		if got, err := CheckEndpoint(in); err != nil || got != want {
			t.Errorf("%q: %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{
		"", "10.0.0.11", "10.0.0.11:0", "10.0.0.11:65536", "node1:51820", "2001:db8::11:51820",
		"[fe80::1%eth0]:51820", "[::ffff:10.0.0.1]:51820", "0.0.0.0:51820", "[::]:51820", "224.0.0.1:51820",
	} {
		if _, err := CheckEndpoint(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}

func TestDelta(t *testing.T) {
	t.Parallel()
	peer := func(name, endpoint string) Peer {
		return Peer{Node: name, PublicKey: []byte(name + "-key"), Endpoint: endpoint, PodPrefix: "fd00:0:0:1::/64"}
	}
	from := Peers{Version: 3, Nodes: map[string]Peer{
		"a": peer("a", "10.0.0.1:51820"), "b": peer("b", "10.0.0.2:51820"), "c": peer("c", "10.0.0.3:51820"),
	}}
	to := Peers{Version: 5, Nodes: map[string]Peer{
		"a": peer("a", "10.0.0.1:51820"), "b": peer("b", "10.0.0.9:51820"), "d": peer("d", "10.0.0.4:51820"),
	}}
	put, remove := Delta(from, to, "a")
	if !reflect.DeepEqual(put, []Peer{to.Nodes["b"], to.Nodes["d"]}) || !reflect.DeepEqual(remove, []string{"c"}) {
		t.Fatalf("a: put %v remove %v", put, remove)
	}
	// A node's own changes and its own removal are not its peers'.
	put, remove = Delta(from, to, "b")
	if !reflect.DeepEqual(put, []Peer{to.Nodes["d"]}) || !reflect.DeepEqual(remove, []string{"c"}) {
		t.Fatalf("b: put %v remove %v", put, remove)
	}
	put, remove = Delta(from, to, "c")
	if !reflect.DeepEqual(put, []Peer{to.Nodes["b"], to.Nodes["d"]}) || len(remove) != 0 {
		t.Fatalf("c: put %v remove %v", put, remove)
	}
	put, remove = Delta(to, to, "a")
	if len(put) != 0 || len(remove) != 0 {
		t.Fatalf("none: put %v remove %v", put, remove)
	}
	if got := to.For("b"); !reflect.DeepEqual(got, []Peer{to.Nodes["a"], to.Nodes["d"]}) {
		t.Fatalf("for b: %v", got)
	}
}
