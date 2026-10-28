// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wg

import (
	"fmt"
)

// Configure gives the WireGuard device name its private key and its UDP
// port, 1 to 65535. Its peers stay as they are.
func Configure(name string, privateKey Key, listenPort int) error {
	if err := checkName(name); err != nil {
		return err
	}
	if listenPort <= 0 || listenPort > 65535 {
		return fmt.Errorf("WireGuard port %d out of range", listenPort)
	}
	c, family, err := dialWireGuard()
	if err != nil {
		return err
	}
	defer func() { _ = c.close() }()
	if _, err := c.execute(setDeviceMessage(family, name, privateKey, listenPort)); err != nil {
		return fmt.Errorf("configure WireGuard device %s: %w", name, err)
	}
	return nil
}

// Get returns what the WireGuard device name has: its public key, its
// port and its peers with their handshakes and traffic. A missing device
// is an error that matches syscall.ENODEV.
func Get(name string) (Device, error) {
	if err := checkName(name); err != nil {
		return Device{}, err
	}
	c, family, err := dialWireGuard()
	if err != nil {
		return Device{}, err
	}
	defer func() { _ = c.close() }()
	return get(c, family, name)
}

func get(c *conn, family uint16, name string) (Device, error) {
	msgs, err := c.execute(getDeviceMessage(family, name))
	if err != nil {
		return Device{}, fmt.Errorf("get WireGuard device %s: %w", name, err)
	}
	d, err := parseDevice(msgs)
	if err != nil {
		return Device{}, fmt.Errorf("get WireGuard device %s: %w", name, err)
	}
	return d, nil
}

// SetPeers makes peers the peers of the WireGuard device name: those it
// has that are not among them are removed, and those it lacks or that
// differ are set, their allowed IPs replaced. A peer that is the same is
// not touched and keeps its session.
func SetPeers(name string, peers []Peer) error {
	if err := checkName(name); err != nil {
		return err
	}
	seen := make(map[Key]bool, len(peers))
	for _, p := range peers {
		if seen[p.PublicKey] {
			return fmt.Errorf("WireGuard peer %s is listed twice", p.PublicKey)
		}
		seen[p.PublicKey] = true
	}
	c, family, err := dialWireGuard()
	if err != nil {
		return err
	}
	defer func() { _ = c.close() }()
	d, err := get(c, family, name)
	if err != nil {
		return err
	}
	remove, set := diffPeers(d.Peers, peers)
	if len(remove) == 0 && len(set) == 0 {
		return nil
	}
	msgs, err := splitPeerMessages(family, name, remove, set)
	if err != nil {
		return fmt.Errorf("set the peers of WireGuard device %s: %w", name, err)
	}
	for _, m := range msgs {
		if _, err := c.execute(m); err != nil {
			return fmt.Errorf("set the peers of WireGuard device %s: %w", name, err)
		}
	}
	return nil
}
