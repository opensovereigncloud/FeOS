// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	networkv1 "github.com/ironcore-dev/feos/go/gen/feos/network/v1"
)

// groupService is feosd's SecurityGroupService over groups in memory,
// with feosd's rules (network-service store.rs, group.rs): a put replaces
// the group whatever its version, with its members as a sorted set of
// IPv6 addresses in canonical form; an update applies a delta from the
// version the node has; a group a pod on the node names is not deleted.
// With Config.CheckSecurityGroups a pod that names a group the node does
// not have fails at its bring-up, as on feosd.
type groupService struct {
	networkv1.UnimplementedSecurityGroupServiceServer
	n *Node

	mu     sync.Mutex
	groups map[string]*networkv1.SecurityGroup
}

func newGroupService(n *Node) *groupService {
	return &groupService{n: n, groups: map[string]*networkv1.SecurityGroup{}}
}

// validGroupID: 1 to 64 letters, digits, "-", "_" and ".".
func validGroupID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func checkGroupID(id string) error {
	if !validGroupID(id) {
		return status.Errorf(codes.InvalidArgument, "invalid security group ID %q: 1 to 64 letters, digits, \"-\", \"_\" and \".\"", id)
	}
	return nil
}

// groupMembers parses members, IPv6 addresses, into a sorted set in
// canonical form.
func groupMembers(members []string) ([]string, error) {
	set := map[string]bool{}
	for _, m := range members {
		a, err := netip.ParseAddr(m)
		if err != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" {
			return nil, status.Errorf(codes.InvalidArgument, "member %q is not an IPv6 address", m)
		}
		set[a.String()] = true
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b string) int {
		return netip.MustParseAddr(a).Compare(netip.MustParseAddr(b))
	})
	return out, nil
}

// checkRule is parse_rule: a direction, a peer, ports only for TCP and
// UDP, and a block whose exceptions lie inside it, of its family.
func checkRule(r *networkv1.Rule) error {
	switch r.GetDirection() {
	case networkv1.Direction_DIRECTION_INGRESS, networkv1.Direction_DIRECTION_EGRESS:
	default:
		return fmt.Errorf("no direction")
	}
	if len(r.GetPorts()) > 0 && r.GetProtocol() != networkv1.Protocol_PROTOCOL_TCP && r.GetProtocol() != networkv1.Protocol_PROTOCOL_UDP {
		return fmt.Errorf("ports are for TCP and UDP only")
	}
	for _, p := range r.GetPorts() {
		if p.GetFirst() == 0 || p.GetFirst() > 65535 || p.GetLast() > 65535 || (p.GetLast() != 0 && p.GetLast() < p.GetFirst()) {
			return fmt.Errorf("port range %d-%d", p.GetFirst(), p.GetLast())
		}
	}
	switch peer := r.GetPeer().(type) {
	case *networkv1.Rule_Group:
		if !validGroupID(peer.Group) {
			return fmt.Errorf("invalid peer group %q", peer.Group)
		}
	case *networkv1.Rule_Block:
		cidr, err := netip.ParsePrefix(peer.Block.GetCidr())
		if err != nil || cidr.Masked() != cidr {
			return fmt.Errorf("invalid block %q", peer.Block.GetCidr())
		}
		for _, e := range peer.Block.GetExcept() {
			ex, err := netip.ParsePrefix(e)
			if err != nil || ex.Masked() != ex || ex.Addr().Is4() != cidr.Addr().Is4() || ex.Bits() < cidr.Bits() ||
				!cidr.Contains(ex.Addr()) {
				return fmt.Errorf("exception %q is not a prefix inside %s", e, cidr)
			}
		}
	default:
		return fmt.Errorf("no peer")
	}
	return nil
}

// normalizeGroup checks a group and writes its members as a sorted set,
// so that two groups compare as they apply.
func normalizeGroup(in *networkv1.SecurityGroup) (*networkv1.SecurityGroup, error) {
	g := proto.Clone(in).(*networkv1.SecurityGroup)
	if err := checkGroupID(g.GetId()); err != nil {
		return nil, err
	}
	for i, r := range g.GetRules() {
		if err := checkRule(r); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "rule %d of security group %s: %v", i, g.GetId(), err)
		}
	}
	members, err := groupMembers(g.GetMembers())
	if err != nil {
		return nil, err
	}
	g.Members = members
	return g, nil
}

func (s *groupService) PutSecurityGroup(_ context.Context, req *networkv1.PutSecurityGroupRequest) (*networkv1.PutSecurityGroupResponse, error) {
	if req.GetGroup() == nil {
		return nil, status.Error(codes.InvalidArgument, "no group")
	}
	g, err := normalizeGroup(req.GetGroup())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups[g.Id] = g
	return &networkv1.PutSecurityGroupResponse{}, nil
}

func (s *groupService) UpdateSecurityGroup(_ context.Context, req *networkv1.UpdateSecurityGroupRequest) (*networkv1.UpdateSecurityGroupResponse, error) {
	id := req.GetId()
	if err := checkGroupID(id); err != nil {
		return nil, err
	}
	if req.GetVersion() <= req.GetFromVersion() {
		return nil, status.Errorf(codes.InvalidArgument, "version %d does not come after %d", req.GetVersion(), req.GetFromVersion())
	}
	add, err := groupMembers(req.GetAddMembers())
	if err != nil {
		return nil, err
	}
	remove, err := groupMembers(req.GetRemoveMembers())
	if err != nil {
		return nil, err
	}
	for _, a := range add {
		if slices.Contains(remove, a) {
			return nil, status.Errorf(codes.InvalidArgument, "member %s is both added and removed", a)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groups[id]
	if g == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "security group %s is not on the node", id)
	}
	if g.Version != req.GetFromVersion() {
		return nil, status.Errorf(codes.FailedPrecondition, "security group %s is at version %d, not %d", id, g.Version, req.GetFromVersion())
	}
	var members []string
	for _, m := range g.Members {
		if !slices.Contains(remove, m) {
			members = append(members, m)
		}
	}
	members, _ = groupMembers(append(members, add...))
	u := proto.Clone(g).(*networkv1.SecurityGroup)
	u.Version, u.Members = req.GetVersion(), members
	s.groups[id] = u
	return &networkv1.UpdateSecurityGroupResponse{}, nil
}

func (s *groupService) DeleteSecurityGroup(_ context.Context, req *networkv1.DeleteSecurityGroupRequest) (*networkv1.DeleteSecurityGroupResponse, error) {
	id := req.GetId()
	inUse := s.n.pods.namesGroup(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.groups[id] == nil {
		return nil, status.Errorf(codes.NotFound, "security group %s is not on the node", id)
	}
	if inUse {
		return nil, status.Errorf(codes.FailedPrecondition, "a pod on the node names security group %s", id)
	}
	delete(s.groups, id)
	return &networkv1.DeleteSecurityGroupResponse{}, nil
}

func (s *groupService) GetSecurityGroup(_ context.Context, req *networkv1.GetSecurityGroupRequest) (*networkv1.SecurityGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.groups[req.GetId()]
	if g == nil {
		return nil, status.Errorf(codes.NotFound, "security group %s is not on the node", req.GetId())
	}
	return proto.Clone(g).(*networkv1.SecurityGroup), nil
}

func (s *groupService) ListSecurityGroups(context.Context, *networkv1.ListSecurityGroupsRequest) (*networkv1.ListSecurityGroupsResponse, error) {
	return &networkv1.ListSecurityGroupsResponse{Groups: s.list()}, nil
}

// list returns the groups, sorted by ID.
func (s *groupService) list() []*networkv1.SecurityGroup {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*networkv1.SecurityGroup, 0, len(s.groups))
	for _, g := range s.groups {
		out = append(out, proto.Clone(g).(*networkv1.SecurityGroup))
	}
	slices.SortFunc(out, func(a, b *networkv1.SecurityGroup) int { return strings.Compare(a.Id, b.Id) })
	return out
}

// missing returns the groups of ids the node does not have, in order.
func (s *groupService) missing(ids []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, id := range ids {
		if s.groups[id] == nil {
			out = append(out, id)
		}
	}
	return out
}

// attachError is a pod's bring-up's failure when its security groups are
// not all on the node (Config.CheckSecurityGroups), as feosd's attach of
// its network fails.
func (s *groupService) attachError(ids []string) error {
	if !s.n.cfg.CheckSecurityGroups {
		return nil
	}
	if missing := s.missing(ids); len(missing) > 0 {
		return fmt.Errorf("the node does not have the security groups %s", strings.Join(missing, ", "))
	}
	return nil
}

// SecurityGroups returns the node's security groups, sorted by ID.
func (n *Node) SecurityGroups() []*networkv1.SecurityGroup { return n.groups.list() }
