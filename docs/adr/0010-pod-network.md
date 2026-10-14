# ADR 0010: feosd gives every pod its network

**Status:** Proposed · **Date:** 2026-10-14

## Context

A pod's network was its caller's business: a TAP device the caller made,
and an address it chose. Pods are untrusted workloads (ADR 0001): they
must not reach each other, the node, or the networks behind the node, and
must not pretend to be someone else. The sandbox system solved this on
its nodes with a bridge of isolated ports, NAT, a rule that keeps the
bridge away from the node, and an eBPF program per TAP that pinned each
sandbox's addresses and dropped private destinations.

## Decision

### Addressing

- The pods share one bridge, `feos-pods`, with the node's end of their
  network, `10.200.0.1/16`, the pods' gateway.
- `feosd` gives each pod an address of `10.200.0.0/16`, keeps it in the
  pod's record for its life, and derives the MAC address from it
  (`06:00:ac:` and the address's last three bytes). It hands out an
  address freed by a deleted pod only after all others: a reused address
  meets the old pod's connection tracking state later.
- The guest gets address, gateway and resolvers pushed over vsock, as
  before (ADR 0007). The resolvers default to `8.8.8.8` and `1.1.1.1`.
- Pods are IPv4 only.

### Isolation

- Each pod's TAP device (`pod-<8 hex of its ID>`) is an isolated bridge
  port, set before the device goes up: the bridge forwards nothing from
  one pod to another, ARP and broadcast included.
- `feosd` owns two nftables tables, `inet feos` and `bridge feos`, and
  replaces them whole and atomically whenever a pod comes or goes:
  - pods' traffic to the node is dropped (`input`);
  - pods' traffic to private, link-local, loopback, CGNAT, multicast and
    reserved addresses is dropped (`forward`);
  - what leaves for anywhere else is masqueraded (`postrouting`);
  - each pod's TAP passes only frames with the pod's MAC and IPv4 source
    address, and ARP with the pod's own addresses (`bridge feos`).
- `feosd` writes these rules over netlink itself; it runs no `nft` or
  `iptables`.

## Alternatives considered

- **The caller's TAP and address**, as before: every caller would rebuild
  the same isolation, or not.
- **eBPF on every TAP**, as the sandbox system did: it also observes DNS,
  TLS and HTTP, which is its own decision (observability). For pinning
  and denying, nftables does the same with no program to build, load and
  keep in step with the kernel.
- **A bridge or subnet per pod**: port isolation gives the same
  separation with one bridge.
- **iptables**: the node may have either; nftables tables of our own sit
  beside other tables without touching them.

## Consequences

- `PodSpec.network` no longer names a TAP or an address; a pod has a
  network unless its spec says it has none.
- Another table can still drop what ours accepts: on a node where Docker
  or a firewall sets the `FORWARD` policy to drop, pods reach nothing
  until that policy lets the bridge out.
- Connection tracking state outlives a deleted pod until it times out;
  addresses are therefore reused last.

## Open questions

- Exceptions to the deny list (the sandbox system's `--allow-egress`), and
  the bridge's range and the resolvers as `feosd` options.
- Ingress: ports of a pod reachable from outside (ADR 0001, CaaS).
- Observability of the pods' traffic.
