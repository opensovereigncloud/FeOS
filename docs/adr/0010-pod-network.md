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
private networks. `feosd` builds that today. Nobody can reach a pod behind
NAT, and every node hands out the same addresses.

Routing pods' addresses between nodes takes addresses that are unique
across nodes, and a prefix per node that the network routes to it. IPv4
makes both scarce. A prefix per node bounds how many pods a node holds,
suspended ones included. An address that is handed out again inherits
what other pods allowed its old owner, until every node has heard of the
change (ADR 0012). IPv6 has room never to hand out an address twice.

Pods resume on other nodes (ADR 0001). An address that followed a pod
from node to node would need a route per pod, changed with every move.

## Decision

### Addresses

- Pods are IPv6 only. Their microVMs and containers have no IPv4
  address.
- Every node has a prefix that its network routes to it, by a static
  route or BGP, and that `feosd` is given (`--pod-prefix`). A /64 is
  best; a smaller one, such as the /80 that AWS delegates to a network
  interface, is enough. A prefix that is only on-link, for which the node
  would answer neighbor discovery on behalf of every pod, is not
  supported.
- `feosd` gives a pod an address of the node's prefix when the pod comes
  to the node, keeps it in the pod's record while the pod is there, and
  never hands an address out twice.
- A pod that resumes on another node gets an address there. Nothing
  stores a pod's address: the control plane's registry, the security
  groups (ADR 0012) and the load balancers follow the pod's record.

### Each pod's TAP is a link of its own

- The pod's TAP device (`pod-<8 hex of its ID>`) is a point-to-point link
  between the node and the pod; there is no bridge. The node is
  `fe80::1` on every TAP, and the pod's address has a /128 route on its
  TAP.
- The node holds the rest of its prefix as unreachable, so that a packet
  for a pod that is not running does not loop back into the network.
- A packet from a TAP whose source address is not routed back out that
  TAP is dropped: one reverse path rule (`fib`) pins every pod to its
  address.
- From a pod, the node takes neighbor discovery on the TAP and DNS
  (below), nothing else. Router advertisements and redirects from a pod
  are dropped.

### IPv4 through NAT64

- Pods reach IPv4 destinations through NAT64, under one /96 prefix that
  DNS64 and NAT64 share (`--nat64-prefix`, `64:ff9b::/96` by default).
- `--nat64=node`: the node routes the prefix into a device of its own.
  An eBPF program there translates statelessly (SIIT, RFC 7915, with an
  explicit mapping per pod, RFC 7757) between the pod's address and an
  IPv4 address of a range of the node's own (`10.200.0.0/16` by
  default), which nftables masquerades to the node's address. These IPv4
  addresses never leave the node. eBPF is here only because netfilter has
  no NAT64.
- `--nat64=network`: the node routes the prefix like any other, and a
  NAT64 in the network translates: a gateway, or a cloud's NAT gateway.
- Either way, the node filters a pod's packets as IPv6, before any
  translation (ADR 0012).

### DNS in feosd

- `feosd` serves DNS to its pods at `fdfd:07b5:61d9::53`, the same
  address on every node, so that a pod's resolver configuration holds
  after a resume elsewhere. `fdfd:07b5:61d9::/48` is FeOS's prefix for
  services on the node (RFC 4193); `--dns-address` changes the address.
- The guest is pushed this resolver and no other, and so are its
  containers.
- `feosd` asks the upstream resolvers that the pod's spec names, or the
  node's (`--dns-upstream`, Google's and Cloudflare's by default). An
  upstream in a denied prefix (below) is refused.
- It is DNS64 (RFC 6147): a name that has only A records gets AAAA
  records under the NAT64 prefix. Under the well-known prefix it
  synthesizes none for non-global IPv4 addresses (RFC 6052). It answers
  `ipv4only.arpa` (RFC 7050).
- Where pods cannot reach the IPv6 internet, because the node's prefix is
  a ULA (`fc00::/7`) as in labs and tests, AAAA records count as absent
  (RFC 6147's exclusion), and every name is reached through NAT64.
- It tells pods apart by the source address of a query, which the TAP
  pins.
- Its sockets outlive restarts of `feosd`: a query waits in them, it does
  not fail.

### What a pod may reach

- A pod's security groups decide what it sends and what reaches it
  (ADR 0012). There is no fixed list of private destinations: a group
  that allows the internet says which addresses that is.
- `--deny-prefix` lists prefixes that no pod reaches, whatever a group
  says: a cloud's metadata service, the control plane, Valkey, management
  networks. An IPv4 prefix in it means its addresses under the NAT64
  prefix. The node's own addresses and link-local, loopback and multicast
  addresses of both families are denied always.
- `feosd` writes its nftables tables over netlink itself; it runs no
  `nft` or `iptables`.

## Alternatives considered

- **IPv4 behind NAT on a bridge of isolated ports**, as the first version
  of this ADR decided and `feosd` builds: no pod can be reached from
  outside or from another pod.
- **Routed IPv4, a prefix per node.** No NAT64, no DNS64, and every
  program works as it is. But a /24 holds 253 pods, suspended ones
  included; addresses come round again, so a change of a group's members
  would have to reach every node before its address is handed out again;
  and private IPv4 ranges collide with tenants' own networks once those
  are joined.
- **Dual stack, with a private IPv4 address behind NAT for leaving
  only**, as Fly.io's machines have: IPv4 literals and programs that only
  speak IPv4 work, without NAT64 or DNS64. Every guest has two families;
  a CLAT in the guest (464XLAT) gives the same where it is needed.
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
- **Jool for NAT64**: mature, but a kernel module out of tree on every
  node, built with DKMS on Debian and by ourselves for Garden Linux and
  `feos-host-init`.
- **Public DNS64 resolvers** (Google's, Cloudflare's): no code, but no
  upstream per pod and no record of queries (ADR 0013).
- **A resolver next to `feosd`**, such as Unbound: one more program to
  package, and the upstreams per pod to configure in it.
- **`fd00::1` for the resolver**: shorter, but `fd00::/64` is a common
  default of routers, and the node would shadow it.

## Consequences

- Pods reach each other, and are reached from outside, where a security
  group allows it (ADR 0001, isolation).
- A program that listens only on `0.0.0.0` cannot be reached; listeners
  bind to `[::]`. A program that connects to an IPv4 literal, or speaks
  only IPv4, reaches no IPv4 destination.
- A pod's connections do not survive a resume on another node.
- Every node needs a prefix routed to it.
- `PodInfo` carries the pod's IPv6 address, and with `--nat64=node` the
  IPv4 address that stands for the pod on the node; the network in
  `PodSpec` names upstream resolvers and security groups. The guest's
  network configuration gains IPv6, by addition (ADR 0007).
- The control plane's front door reaches pods' ports over IPv6, without a
  port forward over vsock (ADR 0004).
- `--nat64=node` brings an eBPF program to build, in a crate of its own on
  nightly (ADR 0002), and to load. It stays attached while `feosd`
  restarts. Its object is committed, built reproducibly with
  `make nat64-bpf` on a dated nightly and a pinned bpf-linker, and
  embedded in `feosd`: no other build needs nightly, and a change of the
  program is a change of the object in the same commit.
- Other tables still apply: where Docker or a firewall drops forwarded
  packets by default, pods reach nothing until it lets them through.

## Open questions

- **Load balancers**: HTTPS with ACME, L4, IPv4 clients, and holding
  requests while a pod resumes. They are the control plane's front doors
  (ADR 0001) and get an ADR of their own.
- **A CLAT in `feos-guest`** (464XLAT), should IPv4 literals turn out to
  matter.
- **How the DNS sockets outlive `feosd`**: socket activation under
  systemd, and its like under `feos-host-init`.
- **Telling an IPv4-only listener apart**, so that a pod's status says
  why its port cannot be reached.
- **Waking a suspended pod** on a packet to its address.
- **Fixed IPv4 addresses for leaving**, per tenant, through a NAT64 in the
  network.
