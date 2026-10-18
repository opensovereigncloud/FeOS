// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/tools"
)

// The hosts blocks (§4.6): in /etc/hosts of an environment's image
// instances and of the sandboxes its attached services run in, the part
// between "# ferun environment <name> begin" and "… end" names the
// environment's addons and image services by their addresses (one ready
// replica of a service, any replica when none is ready; never a retiring
// one) and, in a sandbox, its own attached services by 127.0.0.1 and ::1.
// Everything else of the file stays.

// hostsPath is the file the blocks are written into.
const hostsPath = "/etc/hosts"

// hostsBase is the file's content when the image has none.
const hostsBase = "127.0.0.1 localhost\n::1 localhost\n"

func blockBegin(env string) string { return "# ferun environment " + env + " begin" }
func blockEnd(env string) string   { return "# ferun environment " + env + " end" }

// hostsEntries are the names of the environment's addons and image
// services with their addresses, by name.
func hostsEntries(v *view) map[string]string {
	out := map[string]string{}
	best := map[string]int{} // 2: a ready replica's address, 1: another's
	for _, r := range v.rows {
		inst := r.Instance
		if inst.Phase == model.PhaseRetiring || r.Sandbox.Address == "" {
			continue
		}
		switch inst.Kind {
		case spec.KindAddon:
			if _, ok := v.target.Addons[inst.Object]; !ok {
				continue
			}
		case spec.KindService:
			if svc, ok := v.target.Services[inst.Object]; !ok || runtimeOf(svc) != spec.RuntimeImage {
				continue
			}
		default:
			continue
		}
		rank := 1
		if inst.Phase == model.PhaseReady {
			rank = 2
		}
		if rank > best[inst.Object] {
			best[inst.Object], out[inst.Object] = rank, r.Sandbox.Address
		}
	}
	return out
}

// hostsBlock is the environment's block; own are the attached services
// of the sandbox it is written into (none in an image instance).
func hostsBlock(v *view, own []string) string {
	entries := hostsEntries(v)
	var b strings.Builder
	b.WriteString(blockBegin(v.env.Name) + "\n")
	for _, name := range slices.Sorted(maps.Keys(entries)) {
		fmt.Fprintf(&b, "%s %s\n", entries[name], name)
	}
	for _, name := range slices.Sorted(slices.Values(own)) {
		fmt.Fprintf(&b, "127.0.0.1 %s\n::1 %s\n", name, name)
	}
	b.WriteString(blockEnd(v.env.Name) + "\n")
	return b.String()
}

// blockHash is what hosts_written keeps of a block.
func blockHash(block string) string {
	sum := sha256.Sum256([]byte(block))
	return hex.EncodeToString(sum[:])
}

// spliceHosts replaces the environment's block in a hosts file (appends
// it when the file has none; removes it for block ""): the other lines
// stay as they are.
func spliceHosts(file, env, block string) string {
	if file == "" {
		file = hostsBase
	}
	begin, end := blockBegin(env), blockEnd(env)
	var out []string
	inBlock := false
	for _, line := range strings.SplitAfter(file, "\n") {
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
		case trimmed == begin:
			inBlock = true
		case trimmed == end && inBlock:
			inBlock = false
		case !inBlock:
			if !strings.HasSuffix(line, "\n") {
				line += "\n"
			}
			out = append(out, line)
		}
	}
	return strings.Join(out, "") + block
}

// writeHosts writes the environment's block into the hosts file of the
// lease's main container, hold-free.
func (c *Controller) writeHosts(ctx context.Context, l model.Lease, env, block string) error {
	if c.d.Runner == nil {
		return errors.New("no tools' runner on this instance")
	}
	file, err := c.d.Runner.ReadFileDetached(ctx, l, hostsPath)
	if err != nil && !errors.Is(err, tools.ErrNoFile) {
		return err
	}
	next := spliceHosts(file, env, block)
	if next == file {
		return nil
	}
	return c.d.Runner.WriteFileDetached(ctx, l, hostsPath, []byte(next))
}
