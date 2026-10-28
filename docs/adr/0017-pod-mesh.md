# ADR 0017: A WireGuard mesh carries pods' IPv6 between nodes that their network routes no prefix to

**Status:** Proposed · **Date:** 2026-10-17

## Context

ADR 0010 gives every pod an IPv6 address of a prefix that the node's
network routes to it, and leaves ULA addresses with an overlay between
nodes as the way for networks that route none. Many route none: a cloud
that gives a VM one address of IPv6, or no IPv6 at all, and the k3s lab,
whose VMs have IPv4 alone.

Such a node takes a ULA by hand today, as the lab's `fd00:fe05:<last
octet of its address>::/64`. Its pods reach each other on the node and
nothing else over IPv6, not even the pods on the next node, and they still
have a default route over IPv6. A name with addresses of both families is
tried over IPv6 first (ADR 0010), and the node answers with destination
unreachable, which Linux sends at most once a second
(`net.ipv6.icmp.ratelimit`). In the lab, curl and Node.js, which try both
families at once (RFC 8305), lost 200 to 250 ms per connection; Python,
which tries one address after the other, lost up to a second per IPv6
address, and `pip download requests` took 6.8 seconds.

Those prefixes are not random, as RFC 4193 wants a ULA's to be: every lab
that copies the scheme has the same addresses.

Whatever carries pods' packets between nodes carries their source
addresses, and security groups allow by them (ADR 0012): it must let
nothing but a pod's own node put the pod's address on a packet.

## Decision

### A ULA /48 per cluster, a /64 per node

- A cluster's pod network is routed, every node with a prefix that its
  network routes to it (ADR 0010), or a mesh, every node with a /64 of the
  cluster's own ULA. The control plane is set up for one of the two, for
  all its nodes.
- A meshed cluster's ULA is a /48: `fd` and a Global ID of 40 random bits
  (RFC 4193, section 3.2). The control plane draws it when its database
  is created and keeps it there; an operator may give one instead, such
  as to keep apart two clusters that will be joined.
- The control plane gives every node a /64 of it, the next subnet ID,
  when the node first connects after its join, and never gives a subnet
  ID out twice: a member set that still names a removed node's pods
  (ADR 0012) never lets in the pods of another node. Subnet 0 is not
  given to a node. A cluster has room for 65,535 joins.
- The /64 is the node's pod prefix, as `--pod-prefix` names it in ADR
  0010: `feosd` keeps it in its state directory and hands out its
  addresses as before, across restarts and renewals of its certificate,
  for as long as the node is in the fleet. A node that joins anew gets a
  new one. A node of a meshed cluster refuses `--pod-prefix`.

### A WireGuard device on every node

- Every node of a meshed cluster has one WireGuard device, `feos-mesh`,
  with a key pair of its own, made when the node first comes onto the
  mesh. The private key stays in the node's state directory, readable by
  root alone, beside its certificate's (ADR 0015).
- The device listens on one UDP port, WireGuard's 51820 unless
  configured, at the node's endpoint: the address from which the node
  reaches the control plane, or the one it is configured with where the
  nodes reach each other at another.
- Every other node of the cluster is a peer: its public key, its endpoint
  and its /64 as the one prefix allowed for it. WireGuard routes by these
  prefixes: a packet leaves for the peer whose /64 holds its destination,
  and a packet that comes in counts only when that peer's key
  authenticated it and its source lies in that peer's /64. A node speaks
  for its own pods alone, and nothing without a node's key speaks at all,
  a pod that sends to another node's port through its own node's
  masquerade included.
- The node routes the cluster's /48 to the device. Its own /64 stays as
  ADR 0010 has it: unreachable, but for the pods' /128s on their TAPs.
- The reverse path rule of ADR 0010 holds for the mesh: a packet with a
  source in the cluster's /48 comes in through the mesh or through its
  pod's TAP, or it is dropped.
- The device's MTU is 1420, WireGuard's, which leaves room for its
  headers over either family on a network of 1500.
- Pods are denied the peers' endpoints, as they are denied the node's own
  addresses (ADR 0010): what a pod sends to another node goes through the
  mesh, to that node's pods.
- Security groups do not change: the node of the pod that a packet is for
  filters it in `forward`, by the source address that the mesh vouches
  for (ADR 0012).
- The port is no port for control (ADR 0001, requirement 7): it carries
  pods' packets, and WireGuard answers nothing that no peer's key
  authenticated.
- `feosd` sets the device up over netlink, in WireGuard's generic netlink
  family; it runs no `wg`.

### The control plane sends every node its peers

- A node reports its public key and its endpoint on its uplink, and the
  control plane keeps them in the node's row. Which node a key is from,
  it knows from the connection's certificate (ADR 0015).
- It sends every node the others as its peers, through
  `feos.network.v1` on the node connection: whole with a version, or as
  a delta from one version to the next, as ADR 0012 sends groups.
  `feosd` refuses a delta that does not start at the version it has, and
  keeps its peers on disk, so that a restart loses none.
- A node that is down stays a peer: it may come back with its pods. A
  node removed from the fleet stops being one.
- The mesh does not need the control plane. When no instance is
  reachable, the nodes keep their peers, and their pods go on reaching
  each other. A new node, or a new endpoint, reaches the others after a
  delay; until then their pods do not reach its pods.

### A pod with a ULA address has no default route over IPv6

- The guest's network configuration gets routes, each a prefix, a
  gateway and an MTU; the guest's API only grows (ADR 0007).
- A pod whose IPv6 address is global gets a default route over `fe80::1`,
  as it has. A pod whose address is a ULA, on a mesh or not, gets a route
  for `fc00::/7` over `fe80::1` instead: no network routes a ULA to the
  internet (RFC 4193), so the pod has no IPv6 there to try. On a mesh,
  the route carries the mesh's MTU, so that TCP agrees on segments that
  fit from its first packet; the link and IPv4 keep theirs.
- With no route to a global IPv6 address, the resolver's sort (RFC 6724,
  rule 1) puts a name's IPv6 addresses after its IPv4 ones, and programs
  connect over IPv4 at once. IPv4 stays for leaving only (ADR 0010).
- A guest that does not know routes keeps its default route until its
  pod boots anew.

## Alternatives considered

- **VXLAN**, as flannel carries pods between nodes, with offloads in many
  network cards. It authenticates nothing: a packet to a node's port 4789
  delivers an inner packet of any source, a pod's own too, sent through
  its node's masquerade with the node's address as its source. The mesh
  would need rules of its own: VXLAN from peers alone, pods kept off the
  peers' addresses, and each peer's inner sources checked against its /64,
  with nftables' matching of inner headers (Linux 6.2). Its packets are
  not encrypted, and an Ethernet overlay needs a forwarding entry, a
  neighbor entry and a route per peer, where WireGuard needs a prefix.
- **IPv6 in IPv4 (SIT, protocol 41), GRE, Geneve or FOU**: they
  authenticate nothing either. Protocol 41 has no ports for a network to
  spread flows by, and many clouds' networks do not pass it.
- **IPsec in the kernel**: authenticated and encrypted, but keys for
  every pair of nodes, from an IKE daemon or by hand, and policies per
  pair.
- **A mesh in userspace** (Tailscale, Nebula, ZeroTier): a program to
  package, its control plane beside ours, and every packet through
  userspace.
- **BGP with EVPN**, FRR on every node: a routing daemon per node and
  route reflectors, where the control plane knows every node and its
  prefix already. A network that takes routes over BGP routes a prefix to
  the node, and ADR 0010 applies.
- **Routes in Kubernetes's node objects**, as flannel keeps them: FeOS
  runs without Kubernetes (ADR 0001); the k3s lab is one deployment.
- **Gossip between nodes** (memberlist, as in Serf): membership without
  the control plane, but the control plane is the fleet's truth (ADR
  0015), and a node's key needs something other than the node to vouch
  for it.
- **Prefixes from the node's address**, as the lab's `fd00:fe05:<last
  octet>::/64`: not random, so every cluster that copies the scheme has
  the same addresses, and an address the network gives a node again
  brings back the prefix of the node that had it.
- **A /80 per node**, as ADR 0010 allows: room for 2^32 joins, but a /64
  is what ADR 0010 calls best, and 65,535 joins are enough.
- **No IPv6 in pods**: security groups and pods reaching each other would
  lose the address they rest on (ADR 0010, ADR 0012).
- **IPv4 first in `/etc/gai.conf`**: glibc alone reads it, not musl or
  Go, and every container has an `/etc` of its own.
- **ICMPv6 errors unthrottled on the node** (`net.ipv6.icmp.ratemask`
  without destination unreachable): every connection still tries IPv6
  first and loses a round trip to the node. A stopgap until the route is
  gone.
- **NAT66 to the internet**, from the ULA to an address of the node's:
  the nodes this ADR is for have no IPv6 to translate to.

## Consequences

- Pods of a meshed cluster reach each other across nodes over IPv6,
  where their security groups allow, at addresses unique in the cluster.
  They reach the internet over IPv4 alone, at once.
- ADR 0010's every node needing a prefix routed to it becomes a prefix
  routed to it, or a /64 of its cluster's mesh.
- `feosd` gains the mesh's device and key, the peers on disk with their
  versions, the peers' endpoints among the denied addresses, and the
  guest's routes. `feos.fleet.v1` carries a node's key and endpoint and
  hands it its /64; `feos.network.v1` gets the peers.
- The control plane keeps the cluster's /48, the subnet IDs it gave out,
  and the nodes' keys and endpoints in their rows, and sends the peers
  over the node connections.
- The kernel needs WireGuard: Debian's has it as a module; the kernel of
  `feos-host-init` needs `CONFIG_WIREGUARD`.
- The nodes' network has to pass UDP on the mesh's port between them, and
  every packet between nodes costs encryption.
- Nodes that took a ULA by hand, as the lab's, move to a /64 of their
  cluster's; their kept pods get addresses of it as ADR 0010's kept pods
  of an earlier network do.
- A node without a control plane, as a lab with `feosctl` alone, has no
  mesh. Its pods get the route for `fc00::/7` too.

## Open questions

- **Rotating the mesh's keys**: with the node's certificate, or on their
  own.
- **Clusters with both kinds of node**: a routed node on the mesh too,
  its prefix among the ones allowed for it, and a route for it in the
  meshed nodes' pods.
- **Peers that are not nodes**: the control plane's front doors and load
  balancers reaching pods' ports over IPv6 (ADR 0010), at addresses of
  subnet 0. Answered by ADR 0021: the ingress's instances are the
  first peers that are not nodes. They take /64s of the same counter, so
  nodes need no other kind of peer. Subnet 0 stays reserved.
- **Nodes that cannot reach each other's endpoints**, both behind NAT: a
  relay.
- **NAT66** for meshed nodes that have IPv6 of their own.
