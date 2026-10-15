# ADR 0010: feosd gives every pod its network

**Status:** Proposed · **Date:** 2026-10-15

## Context

Pods are untrusted workloads (ADR 0001). Agent sandboxes reach the
internet, and their users reach the ports of their sandboxes. CaaS needs
more: services that take traffic from outside, and services that talk to
each other, a service to its database, across nodes.

The first version of this ADR gave pods what the sandbox system had: an
IPv4 address of `10.200.0.0/16` on a bridge of isolated ports, NAT to the
outside, and nftables rules that kept pods from each other, the node and
private networks. Nobody can reach a pod behind NAT, and every node hands
out the same addresses.

Routing pods' addresses between nodes takes addresses that are unique
across nodes, and a prefix per node that the network routes to it. IPv4
makes both scarce. A prefix per node bounds how many pods a node holds,
suspended ones included. An address that is handed out again inherits
what other pods allowed its old owner, until every node has heard of the
change (ADR 0012). IPv6 has room never to hand out an address twice.

The second version made pods IPv6 only, with IPv4 destinations behind
NAT64 and DNS64. In an agent's workspace runs whatever tool the agent
picks, and much of it breaks there: IPv4 literals, programs that speak
only IPv4, resolvers of their own or over HTTPS, and DNSSEC validation,
which synthesized records fail.

Pods resume on other nodes (ADR 0001). An address that followed a pod
from node to node would need a route per pod, changed with every move.

## Decision

### Two families, two roles

- A pod has an IPv6 address and an IPv4 address. IPv6 is its address on
  the network: unique across nodes, routed, and reachable where a
  security group allows it (ADR 0012). IPv4 is for leaving only.
- Where a name has addresses of both families, programs take IPv6, since
  the pod's IPv4 address is link-local (RFC 6724); IPv4 carries what has
  no IPv6.
- A pod that resumes on another node gets both addresses there. Nothing
  stores a pod's addresses: the control plane's registry, the security
  groups and the load balancers follow the pod's record.

### IPv6: a prefix routed to the node

- Every node has a prefix that its network routes to it, by a static
  route or BGP, and that `feosd` is given (`--pod-prefix`). A /64 is
  best; a smaller one, such as the /80 that AWS delegates to a network
  interface, is enough. A prefix that is only on-link, for which the node
  would answer neighbor discovery on behalf of every pod, is not
  supported.
- `feosd` gives a pod an address of the node's prefix when the pod comes
  to the node, keeps it in the pod's record while the pod is there, and
  never hands an address out twice.
- The node holds the rest of its prefix as unreachable, so that a packet
  for a pod that is not running does not loop back into the network.

### IPv4: a link-local address per pod

- `feosd` gives every pod an address of `169.254.16.0/20`
  (`--pod-ipv4-range`), as a /32, unique on the node and not across
  nodes. No network routes link-local addresses (RFC 3927), and no
  tenant's network collides with them.
- No rule allows by a pod's IPv4 address: security groups name pods by
  their IPv6 addresses (ADR 0012), and link-local destinations are denied
  (below). So an IPv4 address may come round again: `feosd` hands out the
  one that has been free longest, and drops the connection tracking
  entries of a deleted pod's address.
- The /20 bounds a node to 4094 pods, suspended ones included.

### Each pod's TAP is a link of its own

- The pod's TAP device (`pod-<8 hex of its ID>`) is a point-to-point link
  between the node and the pod; there is no bridge. The pod's addresses
  have a /128 and a /32 route on its TAP.
- The pod's gateways are `fe80::1` and `169.254.0.1`. The node is
  `fe80::1` on every TAP, and answers ARP for `169.254.0.1` on it by
  proxy, without holding the address: some clouds' gateways are
  `169.254.0.1`, and a node there would lose its own.
- A packet from a TAP whose source address is not routed back out that
  TAP is dropped: one reverse path rule (`fib`) pins every pod to its
  addresses, in both families.
- From a pod, the node takes neighbor discovery and ARP on the TAP and
  DNS (below), nothing else. Router advertisements and redirects from a
  pod are dropped.

### How IPv4 leaves the node

- nftables masquerades the pods' IPv4 to the node's address, with
  Linux's connection tracking.
- The node filters a pod's packets before NAT, in the family the pod sent
  them in (ADR 0012).

### DNS in feosd

- `feosd` serves DNS to its pods at `169.254.0.53`, the same address on
  every node, so that a pod's resolver configuration holds after a resume
  elsewhere. It is the one link-local address the node holds;
  `--dns-address` and `--pod-ipv4-range` move the resolver and the pods'
  addresses where a node's own network uses theirs.
- The guest is pushed this resolver and no other, and so are its
  containers.
- `feosd` asks the upstream resolvers that the pod's spec names, or the
  node's (`--dns-upstream`, Google's and Cloudflare's by default). An
  upstream in a denied prefix (below) is refused.
- It passes answers on as the upstream gave them. Pods have IPv4, so
  there is no DNS64.
- It tells pods apart by the source address of a query, which the TAP
  pins.
- Its sockets outlive restarts of `feosd`: a query waits in them, it does
  not fail.

### Metadata

- `169.254.169.254` is the pod's metadata service. Firecracker answers it
  inside the microVM's network device (MMDS), in the version that wants a
  session token first, as AWS's IMDSv2 does, so that a request that a
  pod's code is tricked into sending gets nothing.
- Its packets never leave the VMM. Link-local destinations are denied on
  the node, so no pod reaches the node's own cloud metadata service.
- What it holds is decided apart; a pod's credentials are not in it
  (ADR 0014).

### What a pod may reach

- A pod's security groups decide what it sends and what reaches it
  (ADR 0012). There is no fixed list of private destinations: a group
  that allows the internet says which addresses that is.
- `--deny-prefix` lists prefixes of either family that no pod reaches,
  whatever a group says: a cloud's metadata service, the control plane,
  Valkey, management networks. The node's own addresses and link-local,
  loopback and multicast addresses of both families are denied always.
  Other pods' IPv4 addresses are among them: pods reach each other over
  IPv6 alone.
- `feosd` writes its nftables tables over netlink itself; it runs no
  `nft` or `iptables`.

## Alternatives considered

- **IPv4 behind NAT on a bridge of isolated ports**, as the first version
  of this ADR decided: no pod can be reached from outside or from another
  pod.
- **IPv6 only, IPv4 destinations through NAT64 and DNS64**, as the second
  version decided: one family in the guest, but IPv4 literals, programs
  that speak only IPv4, resolvers of their own and DNSSEC validation
  break.
- **Routed IPv4, a prefix per node.** Every program works as it is, and
  IPv4 clients reach pods. But a /24 holds 253 pods, suspended ones
  included; addresses come round again, so a change of a group's members
  would have to reach every node before its address is handed out again;
  and private IPv4 ranges collide with tenants' own networks once those
  are joined.
- **The same IPv4 address in every pod**, such as `169.254.0.2`: the
  guest's IPv4 would be the same on every node. But connection tracking
  and routing cannot tell such pods apart; the node would have to rewrite
  every pod's address to one of its own at the TAP, in eBPF, the inner
  headers of ICMP errors included. And a resume elsewhere changes the
  pod's IPv6 address anyway.
- **A private range for the pods' IPv4**, such as `10.200.0.0/16`: it
  collides with the node's and tenants' networks; link-local addresses do
  not.
- **A CLAT (464XLAT, RFC 6877)**, in the guest or on the node, so that
  pods have IPv4 on nodes without IPv4 of their own, through a NAT64 in
  the network: a translator in eBPF to build and keep, for networks we do
  not run in.
- **ULA addresses, an overlay between nodes and NAT66 to the internet**,
  as Fly.io does since 2026: no prefix routed to a node, but an overlay
  mesh between all nodes. It stays the way for networks that cannot route
  a prefix to a node.
- **Addresses that move with a pod**, a route per pod over BGP: stable
  addresses, and a routing table that changes with every resume
  elsewhere.
- **An on-link prefix, with the node as neighbor discovery proxy**:
  providers that resolve every address by neighbor discovery made IPv6
  unreliable for Fly.io, which dropped public addresses per machine for
  it.
- **A bridge with isolated ports**, as before: rules for MAC addresses,
  ARP and neighbor discovery that a route per TAP does not need.
- **The node holding `169.254.0.1` on every TAP**: no proxy ARP, but a
  node whose network's gateway is `169.254.0.1` would deliver its own
  traffic for the gateway to itself.
- **Public resolvers for pods** (Google's, Cloudflare's): no code, but no
  upstream per pod and no record of queries (ADR 0013).
- **A resolver next to `feosd`**, such as Unbound: one more program to
  package, and the upstreams per pod to configure in it.
- **A ULA address for the resolver**, `fdfd:07b5:61d9::53` as the second
  version had: pods have IPv4, and a link-local address next to the
  gateway needs no prefix of its own.
- **A metadata service on the node**: it could answer from `feosd`'s
  state, but it would be one more listener that every pod reaches, where
  MMDS is in the VMM already.

## Consequences

- Pods reach each other, and are reached from outside, over IPv6 where a
  security group allows it (ADR 0001, isolation).
- Pods reach IPv4 destinations as they are: IPv4 literals and programs
  that speak only IPv4 work. A program that listens only on `0.0.0.0`
  cannot be reached from outside; listeners bind to `[::]`.
- A pod's connections do not survive a resume on another node.
- Every node needs a prefix routed to it, and IPv4 of its own, through
  which its pods leave.
- `PodInfo` carries the pod's IPv6 and IPv4 addresses; the network in
  `PodSpec` names upstream resolvers and security groups. The guest's
  network configuration has both families (ADR 0007).
- The control plane's front door reaches pods' ports over IPv6, without a
  port forward over vsock (ADR 0004).
- The pods' network is routing and netfilter: no eBPF, no translator,
  nothing to build on nightly.
- Other tables still apply: where Docker or a firewall drops forwarded
  packets by default, pods reach nothing until it lets them through.

## Open questions

- **Load balancers**: HTTPS with ACME, L4, IPv4 clients, and holding
  requests while a pod resumes. They are the control plane's front doors
  (ADR 0001) and get an ADR of their own.
- **How the DNS sockets outlive `feosd`**: socket activation under
  systemd, and its like under `feos-host-init`.
- **What the metadata service holds**, and how it is filled again after a
  pod is restored from a snapshot.
- **Telling an IPv4-only listener apart**, so that a pod's status says
  why its port cannot be reached.
- **Waking a suspended pod** on a packet to its address.
- **Fixed IPv4 addresses for leaving**, per tenant: an address of the
  node's per tenant to masquerade to.
