// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wg

import (
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The golden bytes below are written from the kernel's structures, not
// from the encoder: a netlink header (length, type, flags, sequence 0,
// port 0), the family's fixed header, then attributes (length, type,
// value, padded to 4 bytes).

const (
	keyHex01 = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
	ulaFd001 = "fd000001000000000000000000000001" // fd00:1::1
)

func seqKey() Key {
	var k Key
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func fillKey(b byte) Key {
	var k Key
	for i := range k {
		k[i] = b
	}
	return k
}

func TestRtnetlinkMessages(t *testing.T) {
	t.Parallel()
	littleEndian(t)
	ifinfo := func(index string) string { return "00 00 0000 " + index + " 00000000 00000000" }
	for _, c := range []struct {
		name string
		got  []byte
		want []string
	}{
		{"RTM_NEWLINK kind wireguard", newLinkMessage("wg0"), []string{
			"3c000000 1000 0506 00000000 00000000", // REQUEST|ACK|EXCL|CREATE
			ifinfo("00000000"),
			"0800 0300 77673000",                   // IFLA_IFNAME
			"1400 1280",                            // IFLA_LINKINFO, nested
			"0e00 0100 776972656775617264 00 0000", // IFLA_INFO_KIND
		}},
		{"RTM_GETLINK", getLinkMessage("wg0"), []string{
			"28000000 1200 0500 00000000 00000000", ifinfo("00000000"), "0800 0300 77673000",
		}},
		{"RTM_NEWLINK MTU", setMTUMessage(7, 1420), []string{
			"28000000 1000 0500 00000000 00000000", ifinfo("07000000"), "0800 0400 8c050000",
		}},
		{"RTM_NEWLINK up", setUpMessage(7), []string{
			"20000000 1000 0500 00000000 00000000", "00 00 0000 07000000 01000000 01000000",
		}},
		{"RTM_DELLINK", delLinkMessage(7), []string{
			"20000000 1100 0500 00000000 00000000", ifinfo("07000000"),
		}},
		{"RTM_NEWLINK netns", moveLinkMessage(7, 5), []string{
			"28000000 1000 0500 00000000 00000000", ifinfo("07000000"), "0800 1c00 05000000",
		}},
		{"RTM_NEWADDR /128 NODAD", replaceAddrMessage(7, netip.MustParsePrefix("fd00:1::1/128")), []string{
			"48000000 1400 0505 00000000 00000000", // REQUEST|ACK|REPLACE|CREATE
			"0a 80 02 00 07000000",                 // AF_INET6, /128, IFA_F_NODAD, universe, index 7
			"1400 0200 " + ulaFd001,                // IFA_LOCAL
			"1400 0100 " + ulaFd001,                // IFA_ADDRESS
			"0800 0800 02000000",                   // IFA_FLAGS
		}},
		{"RTM_GETADDR dump", getAddrMessage(7), []string{
			"18000000 1600 0103 00000000 00000000", // REQUEST|DUMP
			"00 00 00 00 07000000",
		}},
		{"RTM_DELADDR", delAddrMessage(7, netip.MustParsePrefix("10.1.2.3/24")), []string{
			"28000000 1500 0500 00000000 00000000",
			"02 18 00 00 07000000",
			"0800 0200 0a010203",
			"0800 0100 0a010203",
		}},
		{"RTM_NEWROUTE prefsrc MTU", replaceRouteMessage(Route{
			Dst: netip.MustParsePrefix("fd00::/16"), Index: 7, Src: netip.MustParseAddr("fd00:1::1"), MTU: 1420,
		}), []string{
			"58000000 1800 0505 00000000 00000000",
			"0a 10 00 00 fe 04 00 01 00000000",           // AF_INET6, /16, main, static, universe, unicast
			"1400 0100 fd000000000000000000000000000000", // RTA_DST
			"0800 0400 07000000",                         // RTA_OIF
			"1400 0700 " + ulaFd001,                      // RTA_PREFSRC
			"0c00 0880 0800 0200 8c050000",               // RTA_METRICS { RTAX_MTU 1420 }
		}},
		{"RTM_NEWROUTE IPv4", replaceRouteMessage(Route{Dst: netip.MustParsePrefix("10.1.0.0/16"), Index: 7}), []string{
			"2c000000 1800 0505 00000000 00000000",
			"02 10 00 00 fe 04 fd 01 00000000", // scope link
			"0800 0100 0a010000",
			"0800 0400 07000000",
		}},
	} {
		checkBytes(t, c.name, c.got, unhex(t, c.want...))
	}
}

func TestGenetlinkMessages(t *testing.T) {
	t.Parallel()
	littleEndian(t)
	checkBytes(t, "CTRL_CMD_GETFAMILY", getFamilyMessage("wireguard"), unhex(t,
		"24000000 1000 0500 00000000 00000000", // GENL_ID_CTRL
		"03 02 0000",                           // CTRL_CMD_GETFAMILY, version 2
		"0e00 0200 776972656775617264 00 0000", // CTRL_ATTR_FAMILY_NAME
	))
	checkBytes(t, "WG_CMD_SET_DEVICE", setDeviceMessage(0x15, "wg0", seqKey(), 51820), unhex(t,
		"48000000 1500 0500 00000000 00000000",
		"01 01 0000",          // WG_CMD_SET_DEVICE, version 1
		"0800 0200 77673000",  // WGDEVICE_A_IFNAME
		"2400 0300 "+keyHex01, // WGDEVICE_A_PRIVATE_KEY
		"0600 0600 6cca 0000", // WGDEVICE_A_LISTEN_PORT
	))
	checkBytes(t, "WG_CMD_GET_DEVICE", getDeviceMessage(0x15, "wg0"), unhex(t,
		"1c000000 1500 0103 00000000 00000000", "00 01 0000", "0800 0200 77673000",
	))
	checkBytes(t, "remove entry", removeEntry(fillKey(9)), unhex(t,
		"3000 0080",                           // nested entry
		"2400 0100 "+strings.Repeat("09", 32), // WGPEER_A_PUBLIC_KEY
		"0800 0300 01000000",                  // WGPEER_A_FLAGS REMOVE_ME
	))
	allowed := []string{
		"2c00 0980",           // WGPEER_A_ALLOWEDIPS
		"2800 0080",           // an allowed IP
		"0600 0100 0a00 0000", // WGALLOWEDIP_A_FAMILY AF_INET6
		"1400 0200 fd5e1b2c93a000070000000000000000", // WGALLOWEDIP_A_IPADDR
		"0500 0300 40 000000",                        // WGALLOWEDIP_A_CIDR_MASK 64
	}
	for _, c := range []struct {
		endpoint string
		want     []string
	}{
		{"10.0.0.11:51820", append([]string{
			"7800 0080",
			"2400 0100 " + strings.Repeat("07", 32),
			"0800 0300 02000000", // REPLACE_ALLOWEDIPS
			"1400 0400 0200 ca6c 0a00000b 0000000000000000",
			"0600 0500 1900 0000", // keepalive 25
		}, allowed...)},
		{"[2001:db8::11]:51820", append([]string{
			"8400 0080",
			"2400 0100 " + strings.Repeat("07", 32),
			"0800 0300 02000000",
			"2000 0400 0a00 ca6c 00000000 20010db8000000000000000000000011 00000000",
			"0600 0500 1900 0000",
		}, allowed...)},
	} {
		got, err := peerEntry(Peer{
			PublicKey:  fillKey(7),
			Endpoint:   netip.MustParseAddrPort(c.endpoint),
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("fd5e:1b2c:93a0:7::1/64")}, // masked when sent
			Keepalive:  25 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		checkBytes(t, "set entry "+c.endpoint, got, unhex(t, c.want...))
	}
	if _, err := peerEntry(Peer{AllowedIPs: []netip.Prefix{{}}}); err == nil {
		t.Fatal("encoded an invalid allowed IP")
	}
}

// kernelPeer encodes a peer as the kernel reports it in a dump.
func kernelPeer(w *writer, k Key, endpoint string, handshake time.Time, rx, tx uint64, keepalive uint16, allowed ...string) {
	w.nest(0, func(w *writer) {
		w.attr(wgpeerAPublicKey, k[:])
		if endpoint != "" {
			w.attr(wgpeerAEndpoint, sockaddr(netip.MustParseAddrPort(endpoint)))
		}
		ts := make([]byte, 16)
		if !handshake.IsZero() {
			ne.PutUint64(ts, uint64(handshake.Unix()))
			ne.PutUint64(ts[8:], uint64(handshake.Nanosecond()))
		}
		w.attr(wgpeerALastHandshakeTime, ts)
		w.u16(wgpeerAPersistentKeepaliveInterval, keepalive)
		w.attr(wgpeerARxBytes, ne.AppendUint64(nil, rx))
		w.attr(wgpeerATxBytes, ne.AppendUint64(nil, tx))
		w.u32(10, 1) // WGPEER_A_PROTOCOL_VERSION, ignored
		allowedIPs(w, allowed...)
	})
}

func allowedIPs(w *writer, allowed ...string) {
	w.nest(wgpeerAAllowedips, func(w *writer) {
		for _, s := range allowed {
			p := netip.MustParsePrefix(s)
			w.nest(0, func(w *writer) {
				w.u16(wgallowedipAFamily, family(p.Addr()))
				w.ip(wgallowedipAIpaddr, p.Addr())
				w.u8(wgallowedipACidrMask, uint8(p.Bits()))
			})
		}
	})
}

// reply makes an answer of the kernel's from a message the writer built.
func reply(w *writer, flags uint16) []byte {
	b := w.message()
	ne.PutUint16(b[6:], flags)
	ne.PutUint32(b[8:], 3)
	ne.PutUint32(b[12:], 77)
	return b
}

// TestParseDevice parses a dump of two messages: the device and two peers
// in the first, the second peer's last allowed IP and a third peer in the
// second, as the kernel splits them.
func TestParseDevice(t *testing.T) {
	t.Parallel()
	hs := time.Unix(1_700_000_000, 123_456_789)
	priv := seqKey()
	m1 := newMessage(0x15, 0, genlHeader(wgCmdGetDevice, wgGenlVersion))
	m1.u32(wgdeviceAIfindex, 7)
	m1.str(wgdeviceAIfname, "wg0")
	m1.attr(wgdeviceAPrivateKey, priv[:])
	pub := priv.PublicKey()
	m1.attr(wgdeviceAPublicKey, pub[:])
	m1.u16(wgdeviceAListenPort, 51820)
	m1.u32(7, 0) // WGDEVICE_A_FWMARK, ignored
	m1.nest(wgdeviceAPeers, func(w *writer) {
		kernelPeer(w, fillKey(1), "10.0.0.11:51820", hs, 100, 200, 25, "fd00:1::/64")
		kernelPeer(w, fillKey(2), "[2001:db8::2]:51820", time.Time{}, 0, 0, 0, "fd00:2::/64", "10.2.0.0/16")
	})
	m2 := newMessage(0x15, 0, genlHeader(wgCmdGetDevice, wgGenlVersion))
	m2.nest(wgdeviceAPeers, func(w *writer) {
		w.nest(0, func(w *writer) {
			k := fillKey(2)
			w.attr(wgpeerAPublicKey, k[:])
			allowedIPs(w, "fd00:22::/64")
		})
		kernelPeer(w, fillKey(3), "", time.Time{}, 0, 0, 0)
	})
	done := newMessage(nlmsgDone, 0, []byte{0, 0, 0, 0})
	stream := append(append(reply(m1, nlmFMulti), reply(m2, nlmFMulti)...), reply(done, nlmFMulti)...)
	msgs, err := parseMessages(stream)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[2].typ != nlmsgDone {
		t.Fatalf("messages %+v", msgs)
	}
	d, err := parseDevice(msgs[:2])
	if err != nil {
		t.Fatal(err)
	}
	want := Device{
		Name: "wg0", Index: 7, PublicKey: pub, ListenPort: 51820,
		Peers: []PeerState{
			{
				Peer: Peer{PublicKey: fillKey(1), Endpoint: netip.MustParseAddrPort("10.0.0.11:51820"),
					AllowedIPs: []netip.Prefix{netip.MustParsePrefix("fd00:1::/64")}, Keepalive: 25 * time.Second},
				LastHandshake: hs, RxBytes: 100, TxBytes: 200,
			},
			{Peer: Peer{PublicKey: fillKey(2), Endpoint: netip.MustParseAddrPort("[2001:db8::2]:51820"),
				AllowedIPs: []netip.Prefix{netip.MustParsePrefix("fd00:2::/64"), netip.MustParsePrefix("10.2.0.0/16"),
					netip.MustParsePrefix("fd00:22::/64")}}},
			{Peer: Peer{PublicKey: fillKey(3)}},
		},
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("device\n got %+v\nwant %+v", d, want)
	}
	if !d.Peers[0].LastHandshake.Equal(hs) || !d.Peers[1].LastHandshake.IsZero() {
		t.Fatalf("handshakes %v %v", d.Peers[0].LastHandshake, d.Peers[1].LastHandshake)
	}

	bad := newMessage(0x15, 0, genlHeader(wgCmdGetDevice, wgGenlVersion))
	bad.attr(wgdeviceAPublicKey, []byte{1, 2, 3})
	if _, err := parseDevice([]message{{data: bad.message()[nlmsgHdrLen:]}}); err == nil {
		t.Fatal("parsed a key of 3 bytes")
	}
}

func TestParseAddrs(t *testing.T) {
	t.Parallel()
	addr := func(index int, prefixLen uint8, attrs map[uint16]string) []byte {
		p := netip.MustParseAddr(attrs[ifaLocal] + attrs[ifaAddress])
		w := newMessage(rtmNewAddr, 0, ifaddrmsg(uint8(family(p)), prefixLen, 0, 0, index))
		for _, typ := range []uint16{ifaAddress, ifaLocal} {
			if s, ok := attrs[typ]; ok {
				w.ip(typ, netip.MustParseAddr(s))
			}
		}
		w.u32(ifaFlags, 0x80) // IFA_F_PERMANENT
		w.str(3, "wg0")       // IFA_LABEL
		return reply(w, nlmFMulti)
	}
	var stream []byte
	for _, b := range [][]byte{
		addr(1, 8, map[uint16]string{ifaLocal: "127.0.0.1"}),
		addr(7, 128, map[uint16]string{ifaAddress: "fd00:1::1"}),
		addr(1, 128, map[uint16]string{ifaAddress: "::1"}),
		addr(7, 64, map[uint16]string{ifaAddress: "fe80::1"}),
		addr(7, 32, map[uint16]string{ifaLocal: "10.9.0.1"}),
	} {
		stream = append(stream, b...)
	}
	// A point-to-point address: IFA_ADDRESS is the peer's.
	w := newMessage(rtmNewAddr, 0, ifaddrmsg(afInet, 32, 0, 0, 7))
	w.ip(ifaAddress, netip.MustParseAddr("10.9.0.2"))
	w.ip(ifaLocal, netip.MustParseAddr("10.9.0.3"))
	stream = append(stream, reply(w, nlmFMulti)...)
	msgs, err := parseMessages(stream)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseAddrs(msgs, 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("fd00:1::1/128"), netip.MustParsePrefix("fe80::1/64"),
		netip.MustParsePrefix("10.9.0.1/32"), netip.MustParsePrefix("10.9.0.3/32")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses %v, want %v", got, want)
	}
	if got, err := parseAddrs(msgs, 9); err != nil || len(got) != 0 {
		t.Fatalf("link 9: %v, %v", got, err)
	}
}

func TestParseLinkAndFamily(t *testing.T) {
	t.Parallel()
	w := newMessage(rtmNewLink, 0, ifinfomsg(7, iffUp|0x40, 0))
	w.str(iflaIfname, "wg0")
	w.u32(iflaMTU, 1420)
	w.nest(iflaLinkinfo, func(w *writer) {
		w.str(iflaInfoKind, "wireguard")
		w.nest(2, func(w *writer) {}) // IFLA_INFO_DATA
	})
	msgs, err := parseMessages(reply(w, 0))
	if err != nil {
		t.Fatal(err)
	}
	l, err := parseLink(msgs[0])
	if err != nil || l != (Link{Index: 7, Name: "wg0", Kind: "wireguard", MTU: 1420, Up: true}) {
		t.Fatalf("link %+v, %v", l, err)
	}
	if _, err := parseLink(message{typ: rtmNewAddr, data: make([]byte, 16)}); err == nil {
		t.Fatal("parsed an address as a link")
	}

	f := newMessage(genlIDCtrl, 0, genlHeader(1, 2)) // CTRL_CMD_NEWFAMILY
	f.u16(ctrlAttrFamilyID, 0x1d)
	f.str(ctrlAttrFamilyName, "wireguard")
	msgs, err = parseMessages(reply(f, 0))
	if err != nil {
		t.Fatal(err)
	}
	if id, err := parseFamily(msgs); err != nil || id != 0x1d {
		t.Fatalf("family %x, %v", id, err)
	}
	if _, err := parseFamily(nil); err == nil {
		t.Fatal("a family without an answer")
	}
}

// TestSplitPeerMessages splits the removal of one peer and the setting of
// 600 into messages of at most 32 KiB, which hold every entry in order.
func TestSplitPeerMessages(t *testing.T) {
	t.Parallel()
	var set []Peer
	for i := range 600 {
		var k Key
		ne.PutUint32(k[:], uint32(i+1))
		set = append(set, Peer{
			PublicKey:  k,
			Endpoint:   netip.MustParseAddrPort(fmt.Sprintf("[2001:db8::%x]:51820", i+1)),
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix(fmt.Sprintf("fd00:%x::/64", i+1))},
			Keepalive:  25 * time.Second,
		})
	}
	removed := fillKey(0xee)
	msgs, err := splitPeerMessages(0x15, "ferun-mesh", []Key{removed}, set)
	if err != nil {
		t.Fatal(err)
	}
	// 132 bytes an entry: about 248 a message.
	if len(msgs) != 3 {
		t.Fatalf("%d messages", len(msgs))
	}
	var keys []Key
	var flags []uint32
	for i, b := range msgs {
		if len(b) > maxMessage {
			t.Fatalf("message %d has %d bytes", i, len(b))
		}
		parsed, err := parseMessages(b)
		if err != nil || len(parsed) != 1 || parsed[0].typ != 0x15 || parsed[0].flags != nlmFRequest|nlmFAck {
			t.Fatalf("message %d: %+v, %v", i, parsed, err)
		}
		attrs, err := parseAttrs(parsed[0].data[genlHdrLen:])
		if err != nil || len(attrs) != 2 || attrs[0].typ != wgdeviceAIfname || attrs[0].str() != "ferun-mesh" ||
			attrs[1].typ != wgdeviceAPeers {
			t.Fatalf("message %d: attributes %+v, %v", i, attrs, err)
		}
		entries, err := parseAttrs(attrs[1].data)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			p, err := parsePeer(e.data)
			if err != nil {
				t.Fatal(err)
			}
			keys = append(keys, p.PublicKey)
			fields, _ := parseAttrs(e.data)
			for _, f := range fields {
				if f.typ == wgpeerAFlags {
					v, _ := f.u32()
					flags = append(flags, v)
				}
			}
			if len(keys) > 1 {
				want := set[len(keys)-2]
				if !reflect.DeepEqual(p.Peer, want) {
					t.Fatalf("entry %d: %+v, want %+v", len(keys)-1, p.Peer, want)
				}
			}
		}
	}
	if len(keys) != 601 || keys[0] != removed || flags[0] != wgpeerFRemoveMe || flags[1] != wgpeerFReplaceAllowedips {
		t.Fatalf("%d keys, the first %s, flags %v", len(keys), keys[0], flags[:2])
	}

	var many []netip.Prefix
	for i := range 1000 {
		many = append(many, netip.MustParsePrefix(fmt.Sprintf("fd00:%x::/64", i)))
	}
	if _, err := splitPeerMessages(0x15, "wg0", nil, []Peer{{AllowedIPs: many}}); !errors.Is(err, errPeerTooLarge) {
		t.Fatalf("a peer of 40 KB: %v", err)
	}
	if msgs, err := splitPeerMessages(0x15, "wg0", nil, nil); err != nil || len(msgs) != 0 {
		t.Fatalf("nothing to do: %d messages, %v", len(msgs), err)
	}
}

func TestCheckName(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"wg0", "ferun-mesh", "a", "123456789012345"} {
		if err := checkName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "1234567890123456", "a/b", "a b", "a:b", "a\x00"} {
		if err := checkName(bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}
