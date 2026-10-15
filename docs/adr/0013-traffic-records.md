# ADR 0013: feosd records the pods' traffic

**Status:** Proposed · **Date:** 2026-10-15

## Context

Pods run untrusted code (ADR 0001) at addresses the internet can reach
(ADR 0010). The operator answers for what they send: an abuse report
names an address and a time, an incident asks what a pod talked to. The
users of an agent's work want to see what its code contacted. The
sandbox system observed DNS, TLS and HTTP with an eBPF program on every
TAP.

From the host, a pod is a TAP: the host sees its packets, not its
containers or processes.

## Decision

### What is recorded

- **Flows**: every connection of a pod, in either direction: the pod,
  the protocol, addresses and ports (an IPv4 destination as IPv4), when
  it began and ended, and bytes and packets each way.
- **Drops**: packets that a security group or a denied prefix dropped
  (ADR 0012), limited in rate per pod: the pod, the protocol, addresses
  and ports, and why.
- **DNS**: every query of a pod: the name, the type, the answer's
  addresses and its code.
- **Server names**: for a TLS connection that a pod opens, the name in
  its ClientHello; for plain HTTP, the request's host. Both are joined to
  the connection's flow.
- Nothing else of a payload is kept.

### Where it comes from

- Flows come from connection tracking: `feosd` reads its events, new and
  destroyed, over netlink, with accounting on, and finds the pod by its
  address.
- Drops come from the drop rules, which log through nflog with a limit
  per pod.
- DNS comes from `feosd`'s resolver (ADR 0010), which every query passes.
- Server names come from copies: nftables copies the first packets of
  every TCP connection that a pod opens to `feosd` through nflog.
  `feosd` reassembles the ClientHello, which often spans two segments now
  that key shares are post-quantum and browsers shuffle the order of
  extensions, and reads the name.
- There is no eBPF, and nothing in the data path waits for `feosd`. A
  `feosd` that restarts or falls behind misses records; traffic does not
  stall. `feosd` counts what it missed.

### Server names stay readable

`feosd`'s resolver removes the `ech` parameter from HTTPS records, so
that clients send the server name in the clear. A client that brings an
ECH configuration of its own is not seen.

### Where records go

- `feosd` publishes records in batches on a stream of its node in Valkey
  (ADR 0006). While it cannot reach Valkey, it buffers them, bounded;
  what overflows is dropped and counted.
- Records name the pod. The control plane joins them to tenants, stores
  them, and decides how long they are kept and who sees them.

## Alternatives considered

- **eBPF on every TAP**, as the sandbox system has it: counting in the
  kernel pays off once one event per flow is too many. But connection
  tracking sees every flow already, a ClientHello over two segments wants
  reassembling in user space anyway, and the host sees no processes
  either way.
- **Queueing packets to `feosd`** (NFQUEUE): it could block by server
  name before a connection goes on, but traffic would wait for `feosd`,
  and stall while it restarts.
- **Recording in `feos-guest`**: the guest knows containers and
  processes, but it runs inside the tenant's VM, where the workload can
  forge or suppress its records.
- **Intercepting TLS** with a CA of ours in every container: full URLs,
  but it breaks pinning, needs the CA in every image, and reads tenants'
  content.
- **Capturing whole packets**: the volume, and tenants' content.

## Consequences

- Connection tracking keeps accounting (`nf_conntrack_acct`); `feosd`
  turns it on.
- `feosd`'s work grows with the pods' connections. Under load the kernel
  drops events that `feosd` does not read in time, and the counts say
  so.
- Tenants' DNS names and server names are recorded, and kept on the node
  only until they are published; the control plane's retention covers
  them.

## Open questions

- **QUIC**: its Initial packets carry the server name, encrypted with
  keys derived from the connection ID (RFC 9001), which `feosd` can
  undo; or UDP port 443 stays closed, and clients fall back to TCP.
- **Containers and processes**: `feos-guest` could say which container
  and process opened a connection, as a hint beside the host's record.
- **Retention and access**: how long records are kept, and what tenants
  see of their own.
- **Blocking by name** before a connection goes on, should rules by name
  from DNS (ADR 0012) not be enough.
