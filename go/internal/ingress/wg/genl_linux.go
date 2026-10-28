// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package wg

import (
	"errors"
	"fmt"
	"syscall"
)

// resolveFamily asks the controller for the ID of the generic netlink
// family name, which the kernel gives a family when its module loads.
func resolveFamily(c *conn, name string) (uint16, error) {
	msgs, err := c.execute(getFamilyMessage(name))
	if errors.Is(err, syscall.ENOENT) {
		return 0, fmt.Errorf("the kernel has no generic netlink family %s (CONFIG_WIREGUARD): %w", name, err)
	}
	if err != nil {
		return 0, fmt.Errorf("resolve generic netlink family %s: %w", name, err)
	}
	return parseFamily(msgs)
}

// dialWireGuard opens a generic netlink socket and resolves WireGuard's
// family on it.
func dialWireGuard() (*conn, uint16, error) {
	c, err := dial(netlinkGeneric)
	if err != nil {
		return nil, 0, err
	}
	family, err := resolveFamily(c, wgGenlName)
	if err != nil {
		_ = c.close()
		return nil, 0, err
	}
	return c, family, nil
}
