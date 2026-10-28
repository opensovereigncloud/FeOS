// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wg

import (
	"bytes"
	"encoding/hex"
	"net/netip"
	"strings"
	"testing"
)

// unhex decodes hex written in groups, with spaces between them.
func unhex(t *testing.T, parts ...string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(strings.Join(parts, ""), " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// littleEndian skips a test of golden bytes on a big-endian host: netlink
// uses the host's byte order.
func littleEndian(t *testing.T) {
	t.Helper()
	if ne.Uint16([]byte{1, 0}) != 1 {
		t.Skip("the golden bytes are little-endian")
	}
}

func checkBytes(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Errorf("%s:\n got %s\nwant %s", name, hex.EncodeToString(got), hex.EncodeToString(want))
	}
}

func TestWriter(t *testing.T) {
	t.Parallel()
	littleEndian(t)
	w := newMessage(0x1234, nlmFRequest|nlmFAck, []byte{9, 9, 9})
	w.u8(1, 0xab)
	w.u16(2, 0x0102)
	w.u32(3, 0x01020304)
	w.str(4, "abcd")
	w.nest(5, func(w *writer) {
		w.ip(6, netip.MustParseAddr("10.0.0.1"))
		w.nest(0, func(w *writer) { w.attr(7, nil) })
	})
	checkBytes(t, "message", w.message(), unhex(t,
		"4c000000 3412 0500 00000000 00000000", // header: length 76
		"090909 00",                            // fixed header, padded
		"0500 0100 ab 000000",                  // u8
		"0600 0200 0201 0000",                  // u16
		"0800 0300 04030201",                   // u32
		"0900 0400 6162636400 000000",          // string with its NUL
		"1400 0580",                            // nested, 20 bytes
		"0800 0600 0a000001",                   // address
		"0800 0080 0400 0700",                  // nested in nested, an empty attribute
	))
}

func TestParseAttrs(t *testing.T) {
	t.Parallel()
	littleEndian(t)
	b := unhex(t, "0500 0100 ab 000000", "0800 0280 0400 0300", "0600 0400 0201")
	attrs, err := parseAttrs(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(attrs) != 3 || attrs[0].typ != 1 || attrs[1].typ != 2 || attrs[2].typ != 4 {
		t.Fatalf("attributes %+v", attrs)
	}
	if v, err := attrs[0].u8(); err != nil || v != 0xab {
		t.Fatalf("u8 %x, %v", v, err)
	}
	if v, err := attrs[2].u16(); err != nil || v != 0x0102 {
		t.Fatalf("u16 %x, %v (the last attribute needs no padding)", v, err)
	}
	if inner, err := parseAttrs(attrs[1].data); err != nil || len(inner) != 1 || inner[0].typ != 3 {
		t.Fatalf("nested %+v, %v", inner, err)
	}
	if _, err := attrs[0].u32(); err == nil {
		t.Fatal("u32 of one byte")
	}
	for _, bad := range []string{"0500", "0300 0100", "0900 0100 0000 0000", "0800 0100 00000000 00"} {
		if _, err := parseAttrs(unhex(t, bad)); err == nil {
			t.Errorf("parsed %s", bad)
		}
	}
}

func TestParseMessages(t *testing.T) {
	t.Parallel()
	littleEndian(t)
	b := unhex(t,
		"14000000 1000 0200 07000000 2a000000 01020304",
		"10000000 0300 0200 07000000 2a000000",
	)
	msgs, err := parseMessages(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].typ != rtmNewLink || msgs[0].flags != nlmFMulti ||
		msgs[0].seq != 7 || msgs[0].pid != 42 || !bytes.Equal(msgs[0].data, []byte{1, 2, 3, 4}) ||
		msgs[1].typ != nlmsgDone || len(msgs[1].data) != 0 {
		t.Fatalf("messages %+v", msgs)
	}
	for _, bad := range []string{"0f000000 0000 0000 00000000 000000", "20000000 1000 0000 00000000 00000000",
		"0c000000 1000 0000 00000000 00000000"} {
		if _, err := parseMessages(unhex(t, bad)); err == nil {
			t.Errorf("parsed %s", bad)
		}
	}
}

func TestParseError(t *testing.T) {
	t.Parallel()
	littleEndian(t)
	// An acknowledgement.
	code, text, err := parseError(message{typ: nlmsgError, data: unhex(t,
		"00000000", "24000000 1000 0500 07000000 00000000")})
	if err != nil || code != 0 || text != "" {
		t.Fatalf("ack: %d %q %v", code, text, err)
	}
	// A refusal, the request capped, with the kernel's message.
	code, text, err = parseError(message{typ: nlmsgError, flags: nlmFCapped | nlmFAckTLVs, data: unhex(t,
		"efffffff", "24000000 1000 0500 07000000 00000000",
		"1000 0100 46696c6520657869737473 00")})
	if err != nil || code != -17 || text != "File exists" {
		t.Fatalf("capped: %d %q %v", code, text, err)
	}
	// The same with the request's 6 bytes of payload echoed.
	code, text, err = parseError(message{typ: nlmsgError, flags: nlmFAckTLVs, data: unhex(t,
		"edffffff", "16000000 1000 0500 07000000 00000000", "010203040506 0000",
		"0700 0100 6e6f00 00")})
	if err != nil || code != -19 || text != "no" {
		t.Fatalf("uncapped: %d %q %v", code, text, err)
	}
	if _, _, err := parseError(message{typ: nlmsgError, data: unhex(t, "efffffff")}); err == nil {
		t.Fatal("parsed a short error")
	}
}

func TestSockaddr(t *testing.T) {
	t.Parallel()
	littleEndian(t)
	for _, c := range []struct {
		in, out string
		bytes   string
	}{
		{"10.0.0.11:51820", "10.0.0.11:51820", "0200 ca6c 0a00000b 0000000000000000"},
		{"[2001:db8::11]:51820", "[2001:db8::11]:51820",
			"0a00 ca6c 00000000 20010db8000000000000000000000011 00000000"},
		{"[fe80::1%3]:1", "[fe80::1%3]:1", "0a00 0001 00000000 fe800000000000000000000000000001 03000000"},
		{"[::ffff:10.0.0.11]:51820", "10.0.0.11:51820", "0200 ca6c 0a00000b 0000000000000000"},
	} {
		b := sockaddr(netip.MustParseAddrPort(c.in))
		checkBytes(t, c.in, b, unhex(t, c.bytes))
		got, err := parseSockaddr(b)
		if err != nil || got.String() != c.out {
			t.Errorf("%s: parsed %s, %v", c.in, got, err)
		}
	}
	for _, bad := range []string{"0200", "0200 ca6c 0a00", "0a00 ca6c 00000000 2001", "6300 ca6c 0a00000b"} {
		if _, err := parseSockaddr(unhex(t, bad)); err == nil {
			t.Errorf("parsed %s", bad)
		}
	}
}
