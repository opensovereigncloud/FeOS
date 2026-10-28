// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gardenlinux/feos/go/internal/ingress/wg"
)

// ipv6Conf is where the kernel keeps the IPv6 settings per interface.
const ipv6Conf = "/proc/sys/net/ipv6/conf"

// KernelMesh is the mesh device in the kernel, set up with package wg in
// the network namespace of the calling thread. It needs CAP_NET_ADMIN.
type KernelMesh struct {
	log *slog.Logger

	mu   sync.Mutex
	name string // the device, once configured
	mtu  int
}

var _ MeshDevice = (*KernelMesh)(nil)

// NewKernelMesh returns the kernel's mesh device.
func NewKernelMesh(log *slog.Logger) *KernelMesh {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &KernelMesh{log: log}
}

// Configure sets the device up in wg's order: IPv6 checked, the link
// ensured, its MTU, up, the address (other addresses deleted, such as an
// old prefix after a re-join), the route of the pod network, then key and
// port.
func (m *KernelMesh) Configure(c MeshConfig) error {
	if c.Device == "" {
		return errors.New("no mesh device name")
	}
	if !c.Address.IsValid() || !c.PodNetwork.IsValid() {
		return errors.New("no mesh address or pod network")
	}
	for _, conf := range []string{"all", "default"} {
		if err := enableIPv6(conf, true); err != nil {
			return err
		}
	}
	index, err := wg.EnsureLink(c.Device)
	if err != nil {
		return err
	}
	// A device made while IPv6 was off for new interfaces keeps it off.
	if err := enableIPv6(c.Device, false); err != nil {
		return err
	}
	mtu := c.MTU
	if mtu == 0 {
		mtu = m.autoMTU(c.Endpoint.Addr())
	}
	if err := wg.SetMTU(index, mtu); err != nil {
		return err
	}
	if err := wg.SetUp(index); err != nil {
		return err
	}
	if err := wg.ReplaceAddress(index, c.Address); err != nil {
		return err
	}
	have, err := wg.Addresses(index)
	if err != nil {
		return err
	}
	for _, p := range have {
		if p == c.Address {
			continue
		}
		if err := wg.DeleteAddress(index, p); err != nil {
			return err
		}
		m.log.Info("mesh device: deleted an address it no longer has", "device", c.Device, "address", p)
	}
	if err := wg.ReplaceRoute(wg.Route{Dst: c.PodNetwork, Index: index, Src: c.Address.Addr()}); err != nil {
		return err
	}
	if err := wg.Configure(c.Device, c.PrivateKey, c.ListenPort); err != nil {
		return err
	}
	m.mu.Lock()
	m.name, m.mtu = c.Device, mtu
	m.mu.Unlock()
	return nil
}

// SetPeers sets the configured device's peers.
func (m *KernelMesh) SetPeers(peers []wg.Peer) error {
	name, err := m.device()
	if err != nil {
		return err
	}
	return wg.SetPeers(name, peers)
}

// Status reads the configured device.
func (m *KernelMesh) Status() (wg.Device, error) {
	name, err := m.device()
	if err != nil {
		return wg.Device{}, err
	}
	return wg.Get(name)
}

// MTU is the configured device's MTU; 0 before Configure.
func (m *KernelMesh) MTU() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mtu
}

func (m *KernelMesh) device() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.name == "" {
		return "", errors.New("the mesh device is not configured")
	}
	return m.name, nil
}

// autoMTU is ComputeMTU of the interface that holds addr; 1420 when no
// interface holds it.
func (m *KernelMesh) autoMTU(addr netip.Addr) int {
	ifs, err := net.Interfaces()
	if err == nil {
		for _, ifc := range ifs {
			addrs, err := ifc.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				ipn, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				if ip, ok := netip.AddrFromSlice(ipn.IP); ok && ip.Unmap() == addr.Unmap() {
					return ComputeMTU(ifc.MTU)
				}
			}
		}
	}
	m.log.Warn("mesh device: no interface holds the mesh endpoint's address; using MTU 1420", "address", addr, "err", err)
	return maxMTU
}

// enableIPv6 makes sure IPv6 is on for conf ("all", "default" or a
// device): it writes 0 to disable_ipv6 when it holds 1. Without the right
// to, it fails with the hint to run `ferun-ingress prepare` first. A
// missing setting is an error only when required.
func enableIPv6(conf string, required bool) error {
	path := filepath.Join(ipv6Conf, conf, "disable_ipv6")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if !required {
			return nil
		}
		return fmt.Errorf("IPv6 is not available in this network namespace (no %s); the mesh needs it", path)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if strings.TrimSpace(string(data)) == "0" {
		return nil
	}
	if err := os.WriteFile(path, []byte("0\n"), 0o644); err != nil {
		return fmt.Errorf("IPv6 is disabled (%s is %s) and cannot be enabled: %w; run `ferun-ingress prepare` as root before (the init container does)",
			path, strings.TrimSpace(string(data)), err)
	}
	return nil
}
