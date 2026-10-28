// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"time"
)

// The constants below are the kernel's (linux/netlink.h, rtnetlink.h,
// if_link.h, if_addr.h, genetlink.h, wireguard.h), defined here rather
// than taken from syscall: darwin's syscall has none of them, Linux's
// lacks some, and the layout in crates/feos-utils/src/wireguard.rs uses
// the same names.

// Netlink.
const (
	afNetlink      = 16
	netlinkRoute   = 0
	netlinkGeneric = 16
	solNetlink     = 270
	netlinkCapAck  = 10
	netlinkExtAck  = 11

	nlmsgHdrLen = 16
	nlaHdrLen   = 4

	nlmsgNoop  = 0x1
	nlmsgError = 0x2
	nlmsgDone  = 0x3

	nlmFRequest  = 0x1
	nlmFMulti    = 0x2
	nlmFAck      = 0x4
	nlmFDumpIntr = 0x10
	nlmFDump     = 0x300 // NLM_F_ROOT | NLM_F_MATCH
	nlmFReplace  = 0x100
	nlmFExcl     = 0x200
	nlmFCreate   = 0x400
	// In an NLMSG_ERROR: the request is not echoed, and attributes follow.
	nlmFCapped  = 0x100
	nlmFAckTLVs = 0x200

	nlaFNested  = 0x8000
	nlaTypeMask = 0x3fff // without NLA_F_NESTED and NLA_F_NET_BYTEORDER

	nlmsgerrAttrMsg = 1
)

// Address families, with Linux's numbers: darwin's AF_INET6 is another.
const (
	afUnspec = 0
	afInet   = 2
	afInet6  = 10
)

// rtnetlink.
const (
	rtmNewLink  = 16
	rtmDelLink  = 17
	rtmGetLink  = 18
	rtmNewAddr  = 20
	rtmDelAddr  = 21
	rtmGetAddr  = 22
	rtmNewRoute = 24

	ifinfomsgLen = 16
	ifaddrmsgLen = 8
	rtmsgLen     = 12

	iffUp = 0x1

	iflaIfname   = 3
	iflaMTU      = 4
	iflaLinkinfo = 18
	iflaNetNsFD  = 28
	iflaInfoKind = 1

	ifaAddress = 1
	ifaLocal   = 2
	ifaFlags   = 8
	ifaFNodad  = 0x02

	rtaDst     = 1
	rtaOif     = 4
	rtaPrefsrc = 7
	rtaMetrics = 8
	rtaxMTU    = 2

	rtTableMain     = 254
	rtprotStatic    = 4
	rtScopeUniverse = 0
	rtScopeLink     = 253
	rtScopeHost     = 254
	rtnUnicast      = 1
)

// Generic netlink's controller.
const (
	genlHdrLen         = 4
	genlIDCtrl         = 0x10
	genlCtrlVersion    = 2
	ctrlCmdGetFamily   = 3
	ctrlAttrFamilyID   = 1
	ctrlAttrFamilyName = 2
)

// WireGuard's generic netlink family and its link kind.
const (
	wgGenlName     = "wireguard"
	wgGenlVersion  = 1
	wgCmdGetDevice = 0
	wgCmdSetDevice = 1
	wgLinkKind     = "wireguard"

	wgdeviceAIfindex    = 1
	wgdeviceAIfname     = 2
	wgdeviceAPrivateKey = 3
	wgdeviceAPublicKey  = 4
	wgdeviceAListenPort = 6
	wgdeviceAPeers      = 8

	wgpeerAPublicKey                   = 1
	wgpeerAFlags                       = 3
	wgpeerAEndpoint                    = 4
	wgpeerAPersistentKeepaliveInterval = 5
	wgpeerALastHandshakeTime           = 6
	wgpeerARxBytes                     = 7
	wgpeerATxBytes                     = 8
	wgpeerAAllowedips                  = 9

	wgpeerFRemoveMe          = 1
	wgpeerFReplaceAllowedips = 2

	wgallowedipAFamily   = 1
	wgallowedipAIpaddr   = 2
	wgallowedipACidrMask = 3
)

// maxMessage is the size up to which a message of peers grows before the
// rest go in the next one, as wg does.
const maxMessage = 32 * 1024

// ne is the byte order of netlink: the host's.
var ne = binary.NativeEndian

// align rounds n up to netlink's alignment of 4 bytes.
func align(n int) int { return (n + 3) &^ 3 }

// writer appends netlink attributes to b. Made by newMessage, b starts
// with a message's header.
type writer struct {
	b []byte
}

// newMessage starts a request of type typ with flags and the fixed header
// of its family (ifinfomsg, ifaddrmsg, rtmsg or genlmsghdr). Its sequence
// number stays 0 until the connection that sends it sets it.
func newMessage(typ, flags uint16, fixed []byte) *writer {
	w := &writer{b: make([]byte, nlmsgHdrLen, 256)}
	ne.PutUint16(w.b[4:], typ)
	ne.PutUint16(w.b[6:], flags)
	w.b = append(w.b, fixed...)
	w.pad()
	return w
}

// message returns the message with its length set.
func (w *writer) message() []byte {
	ne.PutUint32(w.b, uint32(len(w.b)))
	return w.b
}

func (w *writer) pad() {
	for len(w.b)%4 != 0 {
		w.b = append(w.b, 0)
	}
}

func (w *writer) attr(typ uint16, v []byte) {
	w.b = ne.AppendUint16(w.b, uint16(nlaHdrLen+len(v)))
	w.b = ne.AppendUint16(w.b, typ)
	w.b = append(w.b, v...)
	w.pad()
}

// str writes s with the terminating NUL the kernel wants.
func (w *writer) str(typ uint16, s string) { w.attr(typ, append([]byte(s), 0)) }

func (w *writer) u8(typ uint16, v uint8) { w.attr(typ, []byte{v}) }

func (w *writer) u16(typ uint16, v uint16) { w.attr(typ, ne.AppendUint16(nil, v)) }

func (w *writer) u32(typ uint16, v uint32) { w.attr(typ, ne.AppendUint32(nil, v)) }

// ip writes a's 4 or 16 bytes.
func (w *writer) ip(typ uint16, a netip.Addr) { w.attr(typ, a.AsSlice()) }

// raw appends attributes encoded already.
func (w *writer) raw(b []byte) { w.b = append(w.b, b...) }

// nest writes an attribute of type typ, flagged NLA_F_NESTED, that holds
// what fill writes.
func (w *writer) nest(typ uint16, fill func(*writer)) {
	start := len(w.b)
	w.b = append(w.b, 0, 0, 0, 0)
	fill(w)
	ne.PutUint16(w.b[start:], uint16(len(w.b)-start))
	ne.PutUint16(w.b[start+2:], typ|nlaFNested)
}

// attr is a parsed attribute: its type without the flags, and its value.
type attr struct {
	typ  uint16
	data []byte
}

// parseAttrs splits b into its attributes.
func parseAttrs(b []byte) ([]attr, error) {
	var out []attr
	for len(b) > 0 {
		if len(b) < nlaHdrLen {
			return nil, fmt.Errorf("netlink attribute truncated: %d bytes left", len(b))
		}
		l := int(ne.Uint16(b))
		if l < nlaHdrLen || l > len(b) {
			return nil, fmt.Errorf("netlink attribute of length %d in %d bytes", l, len(b))
		}
		out = append(out, attr{typ: ne.Uint16(b[2:]) & nlaTypeMask, data: b[nlaHdrLen:l]})
		b = b[min(align(l), len(b)):]
	}
	return out, nil
}

func (a attr) u8() (uint8, error) {
	if len(a.data) != 1 {
		return 0, a.sizeError(1)
	}
	return a.data[0], nil
}

func (a attr) u16() (uint16, error) {
	if len(a.data) != 2 {
		return 0, a.sizeError(2)
	}
	return ne.Uint16(a.data), nil
}

func (a attr) u32() (uint32, error) {
	if len(a.data) != 4 {
		return 0, a.sizeError(4)
	}
	return ne.Uint32(a.data), nil
}

func (a attr) u64() (uint64, error) {
	if len(a.data) != 8 {
		return 0, a.sizeError(8)
	}
	return ne.Uint64(a.data), nil
}

// str is a string attribute without its terminating NUL.
func (a attr) str() string {
	s := a.data
	for len(s) > 0 && s[len(s)-1] == 0 {
		s = s[:len(s)-1]
	}
	return string(s)
}

// ip is an attribute of 4 or 16 bytes as an address.
func (a attr) ip() (netip.Addr, error) {
	addr, ok := netip.AddrFromSlice(a.data)
	if !ok {
		return netip.Addr{}, fmt.Errorf("netlink attribute %d: an address of %d bytes", a.typ, len(a.data))
	}
	return addr, nil
}

func (a attr) key() (Key, error) {
	var k Key
	if len(a.data) != KeyLen {
		return k, a.sizeError(KeyLen)
	}
	copy(k[:], a.data)
	return k, nil
}

func (a attr) sizeError(want int) error {
	return fmt.Errorf("netlink attribute %d has %d bytes, want %d", a.typ, len(a.data), want)
}

// message is a parsed netlink message: its header and what follows it.
type message struct {
	typ   uint16
	flags uint16
	seq   uint32
	pid   uint32
	data  []byte
}

// parseMessages splits b, as one read from a netlink socket returns it,
// into its messages.
func parseMessages(b []byte) ([]message, error) {
	var out []message
	for len(b) > 0 {
		if len(b) < nlmsgHdrLen {
			return nil, fmt.Errorf("netlink message truncated: %d bytes left", len(b))
		}
		l := int(ne.Uint32(b))
		if l < nlmsgHdrLen || l > len(b) {
			return nil, fmt.Errorf("netlink message of length %d in %d bytes", l, len(b))
		}
		out = append(out, message{
			typ:   ne.Uint16(b[4:]),
			flags: ne.Uint16(b[6:]),
			seq:   ne.Uint32(b[8:]),
			pid:   ne.Uint32(b[12:]),
			data:  b[nlmsgHdrLen:l],
		})
		b = b[min(align(l), len(b)):]
	}
	return out, nil
}

// parseError reads an NLMSG_ERROR: the error code, 0 for an
// acknowledgement and a negative errno for a refusal, and the message of
// the extended acknowledgement if the kernel gave one.
func parseError(m message) (int32, string, error) {
	if len(m.data) < 4+nlmsgHdrLen {
		return 0, "", fmt.Errorf("netlink error message of %d bytes", len(m.data))
	}
	code := int32(ne.Uint32(m.data))
	if m.flags&nlmFAckTLVs == 0 {
		return code, "", nil
	}
	// The error code, the request's header, the rest of the request unless
	// it was capped, then the attributes.
	start := 4 + nlmsgHdrLen
	if m.flags&nlmFCapped == 0 {
		start = align(4 + int(ne.Uint32(m.data[4:])))
	}
	if start > len(m.data) {
		return code, "", nil
	}
	attrs, err := parseAttrs(m.data[start:])
	if err != nil {
		return code, "", nil
	}
	for _, a := range attrs {
		if a.typ == nlmsgerrAttrMsg {
			return code, a.str(), nil
		}
	}
	return code, "", nil
}

// sockaddr is ap as a sockaddr_in or sockaddr_in6, laid out as Linux lays
// them out. An IPv4-mapped address goes as IPv4, as the kernel would
// report it after the peer roamed.
func sockaddr(ap netip.AddrPort) []byte {
	a := ap.Addr().Unmap()
	if a.Is4() {
		b := make([]byte, 16)
		ne.PutUint16(b, afInet)
		binary.BigEndian.PutUint16(b[2:], ap.Port())
		v4 := a.As4()
		copy(b[4:8], v4[:])
		return b
	}
	b := make([]byte, 28)
	ne.PutUint16(b, afInet6)
	binary.BigEndian.PutUint16(b[2:], ap.Port())
	v6 := a.As16()
	copy(b[8:24], v6[:])
	// A numeric zone is the scope ID; a named one cannot be resolved here.
	if id, err := strconv.ParseUint(a.Zone(), 10, 32); err == nil {
		ne.PutUint32(b[24:], uint32(id))
	}
	return b
}

// parseSockaddr reads a sockaddr_in or sockaddr_in6.
func parseSockaddr(b []byte) (netip.AddrPort, error) {
	if len(b) < 4 {
		return netip.AddrPort{}, fmt.Errorf("socket address of %d bytes", len(b))
	}
	port := binary.BigEndian.Uint16(b[2:])
	switch family := ne.Uint16(b); family {
	case afInet:
		if len(b) < 8 {
			return netip.AddrPort{}, fmt.Errorf("IPv4 socket address of %d bytes", len(b))
		}
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), port), nil
	case afInet6:
		if len(b) < 28 {
			return netip.AddrPort{}, fmt.Errorf("IPv6 socket address of %d bytes", len(b))
		}
		a := netip.AddrFrom16([16]byte(b[8:24]))
		if id := ne.Uint32(b[24:]); id != 0 {
			a = a.WithZone(strconv.FormatUint(uint64(id), 10))
		}
		return netip.AddrPortFrom(a, port), nil
	default:
		return netip.AddrPort{}, fmt.Errorf("socket address of family %d", family)
	}
}

// keepaliveSeconds is d in whole seconds as WireGuard takes it, from 0 to
// 65535.
func keepaliveSeconds(d time.Duration) uint16 {
	return uint16(min(max(d/time.Second, 0), 65535))
}

// diffPeers compares the peers a device has with those it should have. It
// returns the keys of the peers to remove, in the order of have, and the
// peers to set, in the order of want: those the device lacks and those
// whose endpoint, allowed IPs or keepalive differ. A peer that is the same
// is in neither, so its session is not touched. A wanted peer without an
// endpoint keeps the one the device has learnt.
func diffPeers(have []PeerState, want []Peer) (remove []Key, set []Peer) {
	wanted := make(map[Key]bool, len(want))
	for _, p := range want {
		wanted[p.PublicKey] = true
	}
	current := make(map[Key]Peer, len(have))
	for _, p := range have {
		current[p.PublicKey] = p.Peer
		if !wanted[p.PublicKey] {
			remove = append(remove, p.PublicKey)
		}
	}
	for _, p := range want {
		if h, ok := current[p.PublicKey]; !ok || !samePeer(h, p) {
			set = append(set, p)
		}
	}
	return remove, set
}

func samePeer(have, want Peer) bool {
	if want.Endpoint.IsValid() && unmap(have.Endpoint) != unmap(want.Endpoint) {
		return false
	}
	if keepaliveSeconds(have.Keepalive) != keepaliveSeconds(want.Keepalive) {
		return false
	}
	return samePrefixes(have.AllowedIPs, want.AllowedIPs)
}

func unmap(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// samePrefixes reports whether a and b hold the same prefixes, masked, in
// any order: the kernel masks them and keeps each once.
func samePrefixes(a, b []netip.Prefix) bool {
	sa, sb := prefixSet(a), prefixSet(b)
	if len(sa) != len(sb) {
		return false
	}
	for p := range sa {
		if !sb[p] {
			return false
		}
	}
	return true
}

func prefixSet(ps []netip.Prefix) map[netip.Prefix]bool {
	s := make(map[netip.Prefix]bool, len(ps))
	for _, p := range ps {
		s[p.Masked()] = true
	}
	return s
}

// errPeerTooLarge is returned for a peer whose entry alone exceeds a
// message.
var errPeerTooLarge = errors.New("WireGuard peer has more allowed IPs than one message holds")

// splitPeerMessages encodes the removal of the peers remove and the
// setting of set as WG_CMD_SET_DEVICE messages to the device name of the
// generic netlink family, removals first, each message at most 32 KiB.
func splitPeerMessages(family uint16, name string, remove []Key, set []Peer) ([][]byte, error) {
	entries := make([][]byte, 0, len(remove)+len(set))
	for _, k := range remove {
		entries = append(entries, removeEntry(k))
	}
	for _, p := range set {
		e, err := peerEntry(p)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	empty := len(peersMessage(family, name, nil))
	var msgs [][]byte
	var batch []byte
	for _, e := range entries {
		if empty+len(e) > maxMessage {
			return nil, errPeerTooLarge
		}
		if len(batch) > 0 && empty+len(batch)+len(e) > maxMessage {
			msgs = append(msgs, peersMessage(family, name, batch))
			batch = nil
		}
		batch = append(batch, e...)
	}
	if len(batch) > 0 {
		msgs = append(msgs, peersMessage(family, name, batch))
	}
	return msgs, nil
}

// peersMessage is a WG_CMD_SET_DEVICE to the device name with the peer
// entries.
func peersMessage(family uint16, name string, entries []byte) []byte {
	w := newMessage(family, nlmFRequest|nlmFAck, genlHeader(wgCmdSetDevice, wgGenlVersion))
	w.str(wgdeviceAIfname, name)
	w.nest(wgdeviceAPeers, func(w *writer) { w.raw(entries) })
	return w.message()
}

// removeEntry is the entry of WGDEVICE_A_PEERS that removes the peer k.
func removeEntry(k Key) []byte {
	w := &writer{}
	w.nest(0, func(w *writer) {
		w.attr(wgpeerAPublicKey, k[:])
		w.u32(wgpeerAFlags, wgpeerFRemoveMe)
	})
	return w.b
}

// peerEntry is the entry of WGDEVICE_A_PEERS that sets p: its endpoint if
// it has one, its keepalive, and its allowed IPs, which replace those it
// had.
func peerEntry(p Peer) ([]byte, error) {
	for _, pr := range p.AllowedIPs {
		if !pr.IsValid() {
			return nil, fmt.Errorf("WireGuard peer %s: allowed IP %v is not a prefix", p.PublicKey, pr)
		}
	}
	w := &writer{}
	w.nest(0, func(w *writer) {
		w.attr(wgpeerAPublicKey, p.PublicKey[:])
		w.u32(wgpeerAFlags, wgpeerFReplaceAllowedips)
		if p.Endpoint.IsValid() {
			w.attr(wgpeerAEndpoint, sockaddr(p.Endpoint))
		}
		w.u16(wgpeerAPersistentKeepaliveInterval, keepaliveSeconds(p.Keepalive))
		w.nest(wgpeerAAllowedips, func(w *writer) {
			for _, pr := range p.AllowedIPs {
				pr = pr.Masked()
				w.nest(0, func(w *writer) {
					w.u16(wgallowedipAFamily, family(pr.Addr()))
					w.ip(wgallowedipAIpaddr, pr.Addr())
					w.u8(wgallowedipACidrMask, uint8(pr.Bits()))
				})
			}
		})
	})
	return w.b, nil
}

// family is a's address family.
func family(a netip.Addr) uint16 {
	if a.Is4() {
		return afInet
	}
	return afInet6
}
