// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	networkv1 "github.com/ironcore-dev/feos/go/gen/feos/network/v1"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// The security groups of environments (§4.5): env-<env id> for the pods
// of an environment (its instances and the sandboxes its attached
// services run in), sbx-<sandbox id> for an attachable sandbox. Both
// allow the internet (the egress table); the env group lets its members
// reach each other; the sbx group lets its sandbox reach the env groups
// it is attached to (no ingress: image pods never reach a sandbox).

// internet4Except are the IPv4 prefixes the egress table leaves out of
// 0.0.0.0/0: what is not the internet (feosd's internet4 set).
var internet4Except = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24",
	"192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"224.0.0.0/4", "240.0.0.0/4",
}

// internet6Except are the IPv6 prefixes the egress table leaves out of
// 2000::/3 (feosd's internet6 set). The contract's list has 5f00::/16
// too (SRv6 SIDs), which lies outside 2000::/3: feosd refuses an
// exception outside its block, and the block leaves it out anyway.
var internet6Except = []string{"2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"}

// egressTable are the rules that let a pod reach the internet, of either
// family: never ::/0 or a bare 0.0.0.0/0, which would open the pods'
// network and the private ones wherever --deny-prefix is unset. DNS and
// the metadata service need no rule.
func egressTable() []*networkv1.Rule {
	return []*networkv1.Rule{
		{Direction: networkv1.Direction_DIRECTION_EGRESS, Protocol: networkv1.Protocol_PROTOCOL_ANY,
			Peer: &networkv1.Rule_Block{Block: &networkv1.AddressBlock{Cidr: "0.0.0.0/0", Except: slices.Clone(internet4Except)}}},
		{Direction: networkv1.Direction_DIRECTION_EGRESS, Protocol: networkv1.Protocol_PROTOCOL_ANY,
			Peer: &networkv1.Rule_Block{Block: &networkv1.AddressBlock{Cidr: "2000::/3", Except: slices.Clone(internet6Except)}}},
	}
}

// groupRule is a rule of any protocol to or from a group's members.
func groupRule(dir networkv1.Direction, group string) *networkv1.Rule {
	return &networkv1.Rule{Direction: dir, Protocol: networkv1.Protocol_PROTOCOL_ANY, Peer: &networkv1.Rule_Group{Group: group}}
}

// envRules are an env group's rules: the egress table, and any traffic
// between its members.
func envRules(id string) []*networkv1.Rule {
	return append(egressTable(),
		groupRule(networkv1.Direction_DIRECTION_INGRESS, id),
		groupRule(networkv1.Direction_DIRECTION_EGRESS, id))
}

// sbxRules are an sbx group's rules: the egress table, and egress to each
// env group the sandbox is attached to.
func sbxRules(envGroups []string) []*networkv1.Rule {
	rules := egressTable()
	for _, g := range envGroups {
		rules = append(rules, groupRule(networkv1.Direction_DIRECTION_EGRESS, g))
	}
	return rules
}

// members are the IPv6 ones of addresses, as a sorted set in canonical
// form (as feosd keeps them).
func members(addresses []string) []string {
	set := map[netip.Addr]bool{}
	for _, a := range addresses {
		if ip, err := netip.ParseAddr(a); err == nil && ip.Is6() && !ip.Is4In6() && ip.Zone() == "" {
			set[ip] = true
		}
	}
	ips := make([]netip.Addr, 0, len(set))
	for ip := range set {
		ips = append(ips, ip)
	}
	slices.SortFunc(ips, func(a, b netip.Addr) int { return a.Compare(b) })
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out
}

// sameContent reports whether two groups have the same rules and members.
func sameContent(a, b *networkv1.SecurityGroup) bool {
	return slices.EqualFunc(a.GetRules(), b.GetRules(), func(x, y *networkv1.Rule) bool { return proto.Equal(x, y) }) &&
		slices.Equal(members(a.GetMembers()), members(b.GetMembers()))
}

// hashVersion is an sbx group's version: from its content, so that equal
// content has an equal version on every node.
func hashVersion(g *networkv1.SecurityGroup) uint64 {
	c := proto.Clone(g).(*networkv1.SecurityGroup)
	c.Version = 0
	c.Members = members(c.Members)
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	if err != nil {
		b = []byte(g.GetId())
	}
	sum := sha256.Sum256(b)
	return binary.BigEndian.Uint64(sum[:8]) >> 1
}

// parseGroup reads a group ID of the controller's: env (true) or sbx, and
// the UUID; ok false for any other group.
func parseGroup(id string) (env bool, u uuid.UUID, ok bool) {
	kind, rest, found := strings.Cut(id, "-")
	if !found || (kind != "env" && kind != "sbx") {
		return false, uuid.Nil, false
	}
	u, err := uuid.Parse(rest)
	if err != nil || u.String() != rest {
		return false, uuid.Nil, false
	}
	return kind == "env", u, true
}

// errNoGroup: the group's environment or sandbox does not exist.
var errNoGroup = errors.New("the group's environment or sandbox does not exist")

// buildGroup is a group as the database says it should be, with its
// version (an env group's gen, an sbx group's hash), and the groups its
// rules name.
func (c *Controller) buildGroup(ctx context.Context, id string) (*networkv1.SecurityGroup, []string, error) {
	isEnv, u, ok := parseGroup(id)
	if !ok {
		return nil, nil, fmt.Errorf("%s is not a group of the environments", id)
	}
	if isEnv {
		env, err := c.st.GetEnvironment(ctx, u)
		if errors.Is(err, store.ErrNoRow) {
			return nil, nil, errNoGroup
		}
		if err != nil {
			return nil, nil, err
		}
		addrs, err := c.st.EnvCtlMembers(ctx, u)
		if err != nil {
			return nil, nil, err
		}
		return &networkv1.SecurityGroup{Id: id, Version: uint64(max(env.Gen, 0)), Rules: envRules(id), Members: members(addrs)},
			[]string{id}, nil
	}
	sb, err := c.st.GetSandbox(ctx, u)
	if errors.Is(err, model.ErrNotFound) {
		return nil, nil, errNoGroup
	}
	if err != nil {
		return nil, nil, err
	}
	envs, err := c.st.EnvCtlWithAttached(ctx, u)
	if err != nil {
		return nil, nil, err
	}
	var refs []string
	for _, e := range envs {
		refs = append(refs, model.EnvGroupID(e))
	}
	g := &networkv1.SecurityGroup{Id: id, Rules: sbxRules(refs), Members: members([]string{sb.Address})}
	g.Version = hashVersion(g)
	return g, refs, nil
}

// builtGroup is a group as buildGroup made it, with the groups its rules
// name.
type builtGroup struct {
	g    *networkv1.SecurityGroup
	refs []string
}

// groupsOf are the groups a row's pod names and the groups their rules
// name, each once, in order; cache keeps the groups built in a pass.
func (c *Controller) groupsOf(ctx context.Context, sb store.Sandbox, cache map[string]*builtGroup) (map[string]*networkv1.SecurityGroup, []string, error) {
	built := map[string]*networkv1.SecurityGroup{}
	var order []string
	var visit func(id string) error
	visit = func(id string) error {
		if _, ok := built[id]; ok {
			return nil
		}
		b, ok := cache[id]
		if !ok {
			g, refs, err := c.buildGroup(ctx, id)
			if err != nil {
				return fmt.Errorf("security group %s: %w", id, err)
			}
			b = &builtGroup{g: g, refs: refs}
			cache[id] = b
		}
		built[id] = b.g
		order = append(order, id)
		for _, r := range b.refs {
			if err := visit(r); err != nil {
				return err
			}
		}
		return nil
	}
	if sb.PodTemplate != nil {
		for _, id := range sb.PodTemplate.SecurityGroups {
			if err := visit(id); err != nil {
				return nil, nil, err
			}
		}
	}
	return built, order, nil
}

// syncGroup puts want on node unless the node has it (have, nil when
// absent) with the same content and version, and returns the group as put.
// An env group whose content changed under the same version gets a new
// version first (BumpEnvironmentGen): Put ignores versions, so they are
// compared, never assumed to grow.
func (c *Controller) syncGroup(ctx context.Context, node string, want, have *networkv1.SecurityGroup) (*networkv1.SecurityGroup, error) {
	if have != nil && have.GetVersion() == want.GetVersion() && sameContent(want, have) {
		return want, nil
	}
	if have != nil && have.GetVersion() == want.GetVersion() {
		if isEnv, u, _ := parseGroup(want.Id); isEnv {
			gen, err := c.st.BumpEnvironmentGen(ctx, u)
			if err != nil {
				return nil, err
			}
			want = proto.Clone(want).(*networkv1.SecurityGroup)
			want.Version = uint64(max(gen, 0))
		}
	}
	ctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	cc, err := c.conn(ctx, node)
	if err != nil {
		return nil, err
	}
	if _, err := networkv1.NewSecurityGroupServiceClient(cc).PutSecurityGroup(ctx,
		&networkv1.PutSecurityGroupRequest{Group: want}); err != nil {
		return nil, err
	}
	return want, nil
}

// nodeGroup reads a group from a node; nil when the node does not have it.
func (c *Controller) nodeGroup(ctx context.Context, node, id string) (*networkv1.SecurityGroup, error) {
	ctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	cc, err := c.conn(ctx, node)
	if err != nil {
		return nil, err
	}
	g, err := networkv1.NewSecurityGroupServiceClient(cc).GetSecurityGroup(ctx, &networkv1.GetSecurityGroupRequest{Id: id})
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	return g, err
}

// putGroups puts a row's groups, and the groups their rules name, on node
// where it lacks them or has them otherwise: the prepare hook's first
// part.
func (c *Controller) putGroups(ctx context.Context, node string, sb store.Sandbox) error {
	built, order, err := c.groupsOf(ctx, sb, map[string]*builtGroup{})
	if err != nil {
		return err
	}
	for _, id := range order {
		have, err := c.nodeGroup(ctx, node, id)
		if err != nil {
			return fmt.Errorf("read security group %s: %w", id, err)
		}
		if _, err := c.syncGroup(ctx, node, built[id], have); err != nil {
			return fmt.Errorf("put security group %s: %w", id, err)
		}
	}
	return nil
}

// groupsLoop is env-groups: per connected node, the groups that the
// templates of its rows name (and their rules name) are compared with
// what the node lists and re-put where they differ; the controller's
// groups of environments and sandboxes that no longer exist are deleted
// (a pod that still names one keeps it: FAILED_PRECONDITION).
func (c *Controller) groupsLoop(ctx context.Context) error {
	if c.d.Conns == nil {
		return nil
	}
	var errs []error
	cache := map[string]*builtGroup{}
	for _, node := range c.d.Conns.Nodes() {
		if err := c.syncNodeGroups(ctx, node, cache); err != nil && ctx.Err() == nil {
			errs = append(errs, fmt.Errorf("node %s: %w", node, err))
		}
	}
	return errors.Join(errs...)
}

// syncNodeGroups is env-groups for one node; cache has the groups built
// in the pass (with the versions they were put with).
func (c *Controller) syncNodeGroups(ctx context.Context, node string, cache map[string]*builtGroup) error {
	rows, err := c.st.EnvCtlTemplatedOnNode(ctx, node)
	if err != nil {
		return err
	}
	want := map[string]*networkv1.SecurityGroup{}
	for _, sb := range rows {
		built, _, err := c.groupsOf(ctx, sb, cache)
		if err != nil {
			continue
		}
		for id, g := range built {
			want[id] = g
		}
	}
	lctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	cc, err := c.conn(lctx, node)
	if err != nil {
		cancel()
		return err
	}
	list, err := networkv1.NewSecurityGroupServiceClient(cc).ListSecurityGroups(lctx, &networkv1.ListSecurityGroupsRequest{})
	cancel()
	if err != nil {
		return err
	}
	have := map[string]*networkv1.SecurityGroup{}
	for _, g := range list.GetGroups() {
		have[g.GetId()] = g
	}
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(want)) {
		put, err := c.syncGroup(ctx, node, want[id], have[id])
		if err != nil {
			errs = append(errs, fmt.Errorf("put %s: %w", id, err))
			continue
		}
		// A bumped version is the pass's for the other nodes too.
		cache[id].g = put
	}
	// Groups of environments and sandboxes that are gone.
	var envs, sbxs []uuid.UUID
	for id := range have {
		if _, wanted := want[id]; wanted {
			continue
		}
		if isEnv, u, ok := parseGroup(id); ok {
			if isEnv {
				envs = append(envs, u)
			} else {
				sbxs = append(sbxs, u)
			}
		}
	}
	envOK, sbxOK, err := c.st.EnvCtlExisting(ctx, envs, sbxs)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, u := range envs {
		if !envOK[u] {
			errs = append(errs, c.deleteGroup(ctx, node, model.EnvGroupID(u)))
		}
	}
	for _, u := range sbxs {
		if !sbxOK[u] {
			errs = append(errs, c.deleteGroup(ctx, node, model.SandboxGroupID(u)))
		}
	}
	return errors.Join(errs...)
}

// deleteGroup deletes a group from a node: NOT_FOUND is done, and so is
// FAILED_PRECONDITION for now (a pod still names it; the next pass tries
// again).
func (c *Controller) deleteGroup(ctx context.Context, node, id string) error {
	ctx, cancel := context.WithTimeout(ctx, c.t.nodeCall)
	defer cancel()
	cc, err := c.conn(ctx, node)
	if err != nil {
		return err
	}
	_, err = networkv1.NewSecurityGroupServiceClient(cc).DeleteSecurityGroup(ctx, &networkv1.DeleteSecurityGroupRequest{Id: id})
	switch status.Code(err) {
	case codes.OK, codes.NotFound, codes.FailedPrecondition:
		return nil
	}
	return err
}
