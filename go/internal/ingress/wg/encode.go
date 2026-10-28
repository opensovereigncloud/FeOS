// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wg

import (
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// The requests and the parsing of their answers. They build everywhere, so
// their tests run everywhere; the Linux files only send and receive them.

// ifinfomsg is struct ifinfomsg: family, padding, type, index, flags and
// the mask of the flags that change.
func ifinfomsg(index int, flags, change uint32) []byte {
	b := make([]byte, ifinfomsgLen)
	b[0] = afUnspec
	ne.PutUint32(b[4:], uint32(int32(index)))
	ne.PutUint32(b[8:], flags)
	ne.PutUint32(b[12:], change)
	return b
}

// ifaddrmsg is struct ifaddrmsg: family, prefix length, flags, scope and
// index.
func ifaddrmsg(fam, prefixLen, flags, scope uint8, index int) []byte {
	b := []byte{fam, prefixLen, flags, scope, 0, 0, 0, 0}
	ne.PutUint32(b[4:], uint32(index))
	return b
}

// genlHeader is struct genlmsghdr: command, version, reserved.
func genlHeader(cmd, version uint8) []byte { return []byte{cmd, version, 0, 0} }

// checkName checks a link's name as the kernel would: 1 to 15 bytes
// (IFNAMSIZ less the NUL), without a slash, a colon or white space.
func checkName(name string) error {
	if name == "" || len(name) > 15 || name == "." || name == ".." {
		return fmt.Errorf("link name %q: 1 to 15 bytes, not . or ..", name)
	}
	for _, c := range []byte(name) {
		if c == '/' || c == ':' || c <= ' ' || c == 0x7f {
			return fmt.Errorf("link name %q has %q", name, c)
		}
	}
	return nil
}

// newLinkMessage creates the link name of kind wireguard, and fails if a
// link of that name exists.
func newLinkMessage(name string) []byte {
	w := newMessage(rtmNewLink, nlmFRequest|nlmFAck|nlmFCreate|nlmFExcl, ifinfomsg(0, 0, 0))
	w.str(iflaIfname, name)
	w.nest(iflaLinkinfo, func(w *writer) { w.str(iflaInfoKind, wgLinkKind) })
	return w.message()
}

// getLinkMessage asks for the link name.
func getLinkMessage(name string) []byte {
	w := newMessage(rtmGetLink, nlmFRequest|nlmFAck, ifinfomsg(0, 0, 0))
	w.str(iflaIfname, name)
	return w.message()
}

// setMTUMessage sets the MTU of the link index.
func setMTUMessage(index, mtu int) []byte {
	w := newMessage(rtmNewLink, nlmFRequest|nlmFAck, ifinfomsg(index, 0, 0))
	w.u32(iflaMTU, uint32(mtu))
	return w.message()
}

// setUpMessage brings the link index up.
func setUpMessage(index int) []byte {
	return newMessage(rtmNewLink, nlmFRequest|nlmFAck, ifinfomsg(index, iffUp, iffUp)).message()
}

// delLinkMessage deletes the link index.
func delLinkMessage(index int) []byte {
	return newMessage(rtmDelLink, nlmFRequest|nlmFAck, ifinfomsg(index, 0, 0)).message()
}

// moveLinkMessage moves the link index to the network namespace that the
// file descriptor fd refers to.
func moveLinkMessage(index, fd int) []byte {
	w := newMessage(rtmNewLink, nlmFRequest|nlmFAck, ifinfomsg(index, 0, 0))
	w.u32(iflaNetNsFD, uint32(fd))
	return w.message()
}

// addrScope is the scope ip gives an address.
func addrScope(a netip.Addr) uint8 {
	switch {
	case a.IsLoopback():
		return rtScopeHost
	case a.IsLinkLocalUnicast():
		return rtScopeLink
	default:
		return rtScopeUniverse
	}
}

// replaceAddrMessage puts the address p on the link index, without
// duplicate address detection, or updates it if it is there.
func replaceAddrMessage(index int, p netip.Prefix) []byte {
	a := p.Addr()
	w := newMessage(rtmNewAddr, nlmFRequest|nlmFAck|nlmFCreate|nlmFReplace,
		ifaddrmsg(uint8(family(a)), uint8(p.Bits()), ifaFNodad, addrScope(a), index))
	w.ip(ifaLocal, a)
	w.ip(ifaAddress, a)
	w.u32(ifaFlags, ifaFNodad)
	return w.message()
}

// delAddrMessage deletes the address p from the link index.
func delAddrMessage(index int, p netip.Prefix) []byte {
	a := p.Addr()
	w := newMessage(rtmDelAddr, nlmFRequest|nlmFAck,
		ifaddrmsg(uint8(family(a)), uint8(p.Bits()), 0, 0, index))
	w.ip(ifaLocal, a)
	w.ip(ifaAddress, a)
	return w.message()
}

// getAddrMessage dumps the addresses of every family. Without strict
// checking the kernel dumps every link's, so parseAddrs filters by index.
func getAddrMessage(index int) []byte {
	return newMessage(rtmGetAddr, nlmFRequest|nlmFDump, ifaddrmsg(afUnspec, 0, 0, 0, index)).message()
}

// replaceRouteMessage puts r in the main table, or replaces the route
// there with the same destination and metric.
func replaceRouteMessage(r Route) []byte {
	dst := r.Dst.Masked()
	scope := uint8(rtScopeUniverse)
	if dst.Addr().Is4() {
		// ip's choice for a route without a gateway; IPv6 has no scopes.
		scope = rtScopeLink
	}
	hdr := []byte{
		uint8(family(dst.Addr())), uint8(dst.Bits()), 0, 0, // family, dst_len, src_len, tos
		rtTableMain, rtprotStatic, scope, rtnUnicast, // table, protocol, scope, type
		0, 0, 0, 0, // flags
	}
	w := newMessage(rtmNewRoute, nlmFRequest|nlmFAck|nlmFCreate|nlmFReplace, hdr)
	w.ip(rtaDst, dst.Addr())
	w.u32(rtaOif, uint32(r.Index))
	if r.Src.IsValid() {
		w.ip(rtaPrefsrc, r.Src)
	}
	if r.MTU > 0 {
		w.nest(rtaMetrics, func(w *writer) { w.u32(rtaxMTU, uint32(r.MTU)) })
	}
	return w.message()
}

// getFamilyMessage asks the controller for the generic netlink family
// name. Asking for wireguard loads its module.
func getFamilyMessage(name string) []byte {
	w := newMessage(genlIDCtrl, nlmFRequest|nlmFAck, genlHeader(ctrlCmdGetFamily, genlCtrlVersion))
	w.str(ctrlAttrFamilyName, name)
	return w.message()
}

// setDeviceMessage gives the device name its private key and port.
func setDeviceMessage(family uint16, name string, privateKey Key, listenPort int) []byte {
	w := newMessage(family, nlmFRequest|nlmFAck, genlHeader(wgCmdSetDevice, wgGenlVersion))
	w.str(wgdeviceAIfname, name)
	w.attr(wgdeviceAPrivateKey, privateKey[:])
	w.u16(wgdeviceAListenPort, uint16(listenPort))
	return w.message()
}

// getDeviceMessage dumps the device name.
func getDeviceMessage(family uint16, name string) []byte {
	w := newMessage(family, nlmFRequest|nlmFDump, genlHeader(wgCmdGetDevice, wgGenlVersion))
	w.str(wgdeviceAIfname, name)
	return w.message()
}

// parseLink reads an RTM_NEWLINK.
func parseLink(m message) (Link, error) {
	if m.typ != rtmNewLink || len(m.data) < ifinfomsgLen {
		return Link{}, fmt.Errorf("not a link: message type %d of %d bytes", m.typ, len(m.data))
	}
	l := Link{
		Index: int(int32(ne.Uint32(m.data[4:]))),
		Up:    ne.Uint32(m.data[8:])&iffUp != 0,
	}
	attrs, err := parseAttrs(m.data[ifinfomsgLen:])
	if err != nil {
		return Link{}, err
	}
	for _, a := range attrs {
		switch a.typ {
		case iflaIfname:
			l.Name = a.str()
		case iflaMTU:
			mtu, err := a.u32()
			if err != nil {
				return Link{}, err
			}
			l.MTU = int(mtu)
		case iflaLinkinfo:
			info, err := parseAttrs(a.data)
			if err != nil {
				return Link{}, err
			}
			for _, i := range info {
				if i.typ == iflaInfoKind {
					l.Kind = i.str()
				}
			}
		}
	}
	return l, nil
}

// parseAddrs reads the RTM_NEWADDR messages of a dump and returns the
// addresses of the link index, with their prefix lengths.
func parseAddrs(msgs []message, index int) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, m := range msgs {
		if m.typ != rtmNewAddr {
			continue
		}
		if len(m.data) < ifaddrmsgLen {
			return nil, fmt.Errorf("address message of %d bytes", len(m.data))
		}
		if int(ne.Uint32(m.data[4:])) != index {
			continue
		}
		attrs, err := parseAttrs(m.data[ifaddrmsgLen:])
		if err != nil {
			return nil, err
		}
		// IFA_LOCAL is the address; IFA_ADDRESS is the peer's when the
		// address has one, and the address otherwise (IPv6 sends only it).
		var local, address netip.Addr
		for _, a := range attrs {
			switch a.typ {
			case ifaLocal:
				if local, err = a.ip(); err != nil {
					return nil, err
				}
			case ifaAddress:
				if address, err = a.ip(); err != nil {
					return nil, err
				}
			}
		}
		if !local.IsValid() {
			local = address
		}
		if !local.IsValid() {
			continue
		}
		p := netip.PrefixFrom(local, int(m.data[1]))
		if !p.IsValid() {
			return nil, fmt.Errorf("address %s with prefix length %d", local, m.data[1])
		}
		out = append(out, p)
	}
	return out, nil
}

// parseFamily reads the controller's answer to getFamilyMessage: the
// family's ID.
func parseFamily(msgs []message) (uint16, error) {
	for _, m := range msgs {
		if m.typ != genlIDCtrl || len(m.data) < genlHdrLen {
			continue
		}
		attrs, err := parseAttrs(m.data[genlHdrLen:])
		if err != nil {
			return 0, err
		}
		for _, a := range attrs {
			if a.typ == ctrlAttrFamilyID {
				return a.u16()
			}
		}
	}
	return 0, errors.New("the kernel did not name the generic netlink family's ID")
}

// parseDevice reads the messages of a WG_CMD_GET_DEVICE dump. The device's
// own attributes come in the first; its peers may take several, and a
// peer's allowed IPs may go on in the next message, under the peer's key
// alone.
func parseDevice(msgs []message) (Device, error) {
	var d Device
	at := map[Key]int{}
	for _, m := range msgs {
		if len(m.data) < genlHdrLen {
			return Device{}, fmt.Errorf("WireGuard message of %d bytes", len(m.data))
		}
		attrs, err := parseAttrs(m.data[genlHdrLen:])
		if err != nil {
			return Device{}, err
		}
		for _, a := range attrs {
			switch a.typ {
			case wgdeviceAIfindex:
				index, err := a.u32()
				if err != nil {
					return Device{}, err
				}
				d.Index = int(index)
			case wgdeviceAIfname:
				d.Name = a.str()
			case wgdeviceAPublicKey:
				if d.PublicKey, err = a.key(); err != nil {
					return Device{}, err
				}
			case wgdeviceAListenPort:
				port, err := a.u16()
				if err != nil {
					return Device{}, err
				}
				d.ListenPort = int(port)
			case wgdeviceAPeers:
				peers, err := parseAttrs(a.data)
				if err != nil {
					return Device{}, err
				}
				for _, pa := range peers {
					p, err := parsePeer(pa.data)
					if err != nil {
						return Device{}, err
					}
					if i, ok := at[p.PublicKey]; ok {
						d.Peers[i].AllowedIPs = append(d.Peers[i].AllowedIPs, p.AllowedIPs...)
						continue
					}
					at[p.PublicKey] = len(d.Peers)
					d.Peers = append(d.Peers, p)
				}
			}
		}
	}
	return d, nil
}

// parsePeer reads a peer of WGDEVICE_A_PEERS.
func parsePeer(b []byte) (PeerState, error) {
	var p PeerState
	attrs, err := parseAttrs(b)
	if err != nil {
		return p, err
	}
	for _, a := range attrs {
		switch a.typ {
		case wgpeerAPublicKey:
			if p.PublicKey, err = a.key(); err != nil {
				return p, err
			}
		case wgpeerAEndpoint:
			if p.Endpoint, err = parseSockaddr(a.data); err != nil {
				return p, err
			}
		case wgpeerAPersistentKeepaliveInterval:
			s, err := a.u16()
			if err != nil {
				return p, err
			}
			p.Keepalive = time.Duration(s) * time.Second
		case wgpeerALastHandshakeTime:
			// struct __kernel_timespec: two signed 64-bit integers.
			if len(a.data) != 16 {
				return p, a.sizeError(16)
			}
			sec, nsec := int64(ne.Uint64(a.data)), int64(ne.Uint64(a.data[8:]))
			if sec != 0 || nsec != 0 {
				p.LastHandshake = time.Unix(sec, nsec)
			}
		case wgpeerARxBytes:
			if p.RxBytes, err = a.u64(); err != nil {
				return p, err
			}
		case wgpeerATxBytes:
			if p.TxBytes, err = a.u64(); err != nil {
				return p, err
			}
		case wgpeerAAllowedips:
			ips, err := parseAttrs(a.data)
			if err != nil {
				return p, err
			}
			for _, ia := range ips {
				pr, err := parseAllowedIP(ia.data)
				if err != nil {
					return p, err
				}
				p.AllowedIPs = append(p.AllowedIPs, pr)
			}
		}
	}
	return p, nil
}

func parseAllowedIP(b []byte) (netip.Prefix, error) {
	attrs, err := parseAttrs(b)
	if err != nil {
		return netip.Prefix{}, err
	}
	var addr netip.Addr
	bits := -1
	for _, a := range attrs {
		switch a.typ {
		case wgallowedipAIpaddr:
			if addr, err = a.ip(); err != nil {
				return netip.Prefix{}, err
			}
		case wgallowedipACidrMask:
			n, err := a.u8()
			if err != nil {
				return netip.Prefix{}, err
			}
			bits = int(n)
		}
	}
	p := netip.PrefixFrom(addr, bits)
	if !p.IsValid() {
		return netip.Prefix{}, fmt.Errorf("allowed IP %s/%d is not a prefix", addr, bits)
	}
	return p, nil
}
