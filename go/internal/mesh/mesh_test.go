// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
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
		if p.Bits() != 64 || a[0] != 0xfd || a[6] != 0 || a[7] != 0 || p.Masked() != p {
			t.Fatalf("drew %v", p)
		}
		seen[p] = true
	}
	if len(seen) < 60 {
		t.Fatalf("only %d of 64 draws differ", len(seen))
	}
}

// CheckKey refuses keys of the wrong length and every point of small
// order, in its canonical form and with the top bit set (libsodium's
// list), and takes real keys.
func TestCheckKey(t *testing.T) {
	t.Parallel()
	small := []string{
		"0000000000000000000000000000000000000000000000000000000000000000",
		"0100000000000000000000000000000000000000000000000000000000000000",
		"e0eb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b800",
		"5f9c95bca3508c24b1d0b1559c83ef5b04445cc4581c8e86d8224eddd09f1157",
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
	}
	for _, h := range small {
		k, err := hex.DecodeString(h)
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckKey(k); err == nil {
			t.Errorf("CheckKey(%s) took a point of small order", h)
		}
		k[31] |= 0x80
		if err := CheckKey(k); err == nil {
			t.Errorf("CheckKey(%x) took a point of small order", k)
		}
	}
	for range 16 {
		priv, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckKey(priv.PublicKey().Bytes()); err != nil {
			t.Fatalf("a real key: %v", err)
		}
	}
	for _, b := range []byte{1, 2, 7, 9} {
		if err := CheckKey(bytes.Repeat([]byte{b}, KeyLen)); err != nil {
			t.Errorf("32 bytes of %d: %v", b, err)
		}
	}
	for _, k := range [][]byte{nil, make([]byte, 31), bytes.Repeat([]byte{1}, 33)} {
		if err := CheckKey(k); err == nil {
			t.Errorf("CheckKey took %d bytes", len(k))
		}
	}
}

func TestNodePrefix(t *testing.T) {
	t.Parallel()
	ula := netip.MustParsePrefix("fd5e:1b2c:93a0::/64")
	for subnet, want := range map[int]string{
		1: "fd5e:1b2c:93a0::1:0:0/96", 7: "fd5e:1b2c:93a0::7:0:0/96", 0x1234: "fd5e:1b2c:93a0::1234:0:0/96",
		65535: "fd5e:1b2c:93a0::ffff:0:0/96", 65536: "fd5e:1b2c:93a0:0:1::/96",
		0x12345678: "fd5e:1b2c:93a0:0:1234:5678::/96", LastSubnet: "fd5e:1b2c:93a0:0:ffff:ffff::/96",
	} {
		if got := NodePrefix(ula, subnet).String(); got != want {
			t.Errorf("subnet %d: %s, want %s", subnet, got, want)
		}
	}
}

// An ingress instance's address is <prefix>::1 of its /96, whatever host
// bits the prefix carries.
func TestHostAddress(t *testing.T) {
	t.Parallel()
	ula := netip.MustParsePrefix("fd5e:1b2c:93a0::/64")
	for in, want := range map[netip.Prefix]string{
		NodePrefix(ula, 7):                                  "fd5e:1b2c:93a0::7:0:1",
		NodePrefix(ula, 65535):                              "fd5e:1b2c:93a0::ffff:0:1",
		netip.MustParsePrefix("fd5e:1b2c:93a0:12::/64"):     "fd5e:1b2c:93a0:12::1",
		netip.MustParsePrefix("fd5e:1b2c:93a0:12:ab::9/64"): "fd5e:1b2c:93a0:12::1",
		netip.MustParsePrefix("10.20.30.40/24"):             "10.20.30.1",
	} {
		got := HostAddress(in)
		if got.String() != want || !in.Contains(got) {
			t.Errorf("%v: %v, want %s", in, got, want)
		}
	}
	if got := HostAddress(netip.Prefix{}); got.IsValid() {
		t.Errorf("the zero prefix: %v", got)
	}
}

// A cluster meshed on a /48 keeps its Global ID, on its /64 of subnet 0.
func TestFromLegacy(t *testing.T) {
	t.Parallel()
	got := FromLegacy(netip.MustParsePrefix("fd5e:1b2c:93a0::/48"))
	if got != netip.MustParsePrefix("fd5e:1b2c:93a0::/64") {
		t.Fatalf("%v", got)
	}
	if NodePrefix(got, 3).Overlaps(netip.MustParsePrefix("fd5e:1b2c:93a0:3::/64")) {
		t.Fatal("a new prefix overlaps an old one")
	}
	if got := HostAddress(netip.Prefix{}); got.IsValid() {
		t.Errorf("the zero prefix: %v", got)
	}
}

// An ingress instance's key among the peers never collides with a node's.
func TestIngressPeerName(t *testing.T) {
	t.Parallel()
	if got := IngressPeerName("in-1"); got != "ingress.in-1" || !IsIngressPeer(got) {
		t.Fatalf("%q", got)
	}
	for _, node := range []string{"node1", "ingress", "ingress-1", "ingressa", ""} {
		if IsIngressPeer(node) {
			t.Errorf("node %q is taken for an ingress", node)
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
