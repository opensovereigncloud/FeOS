Architecture Decision Records
=============================

An ADR records one decision: its context, what was decided, the
alternatives, and what follows. How the system works is described in
[docs/overview.md](../overview.md) and [docs/architecture/](../architecture/);
the ADRs say why it works that way.

## Index

| # | Area | Decision | Status | Built |
|---|---|---|---|---|
| [0001](0001-what-we-build.md) | Foundation | Agent sandboxes and CaaS on isolated pods; FeOS is the node layer, the control plane one or two central services; CaaS has its own API, no Kubernetes | Accepted | – |
| [0002](0002-one-repository.md) | Foundation | One repository for node, control plane and clients, laid out by language; workspace rules; static musl | Accepted | yes |
| [0003](0003-programs.md) | Node | `feos-host-init`, `feosd`, `feos-guest`, `feosctl`, and `feos-authn` beside each pod; the sandbox manager's node half becomes `feosd`, its sandboxes go to the control plane | Accepted | partly: the binaries; the manager's port has not started |
| [0004](0004-pods.md) | Node | Pods instead of VMs in the API, and no VMs at all; one kind of pod for sandboxes, services and jobs, told apart by what an exit means: idle, init, sidecar and main containers, a restart policy, probes; a pod's services through `feosd`; the guest's API is internal | Accepted | partly: `feos.pod.v1`, the forwarding, no VMs; no credentials or proxies in the spec, the caller makes the pod's TAP; no roles, restarts, probes or reaper |
| [0005](0005-firecracker.md) | Node | Firecracker is the only VMM, every VM in its jailer and a cgroup of its own | Accepted | yes |
| [0006](0006-control-transport.md) | Node ↔ control plane | `feosd` connects out to Valkey, which carries the same gRPC calls; no inbound port | Proposed | no |
| [0007](0007-guest.md) | Guest | `feos-guest`: vsock, configuration pushed by the host, the API mapping, libcontainer in its own process | Accepted | partly: guest RPCs still in `HostService` |
| [0008](0008-suspend-and-restore.md) | Node | Balloon with free page reporting, full snapshots, two-step restore, version pinning | Accepted | yes |
| [0009](0009-node-os-and-packages.md) | Operations | Debian and Garden Linux first, `.deb` packages, Firecracker side by side, `feosd` under systemd | Proposed | partly: the packages, the unit and the apt repository; no Garden Linux feature, no release workflow |
| [0010](0010-pod-network.md) | Node | `feosd` gives every pod its network: an IPv6 address of a prefix routed to the node, and a link-local IPv4 address, a /32, for leaving through NAT or a CLAT on the node; a routed TAP per pod pinned to its addresses, DNS in `feosd`, metadata from Firecracker's MMDS, prefixes no pod reaches | Proposed | partly: the second version, IPv6 only, with NAT64 on the node (eBPF) or in the network and DNS64 in `feosd`; `--pod-prefix`, routed TAPs, `--deny-prefix` |
| [0011](0011-disk-daemon.md) | Node | `diskd`: a pod's disk as a ublk device over a chain of sealed, content-addressed layers and a sparse top, cached locally and tiered to S3 | Proposed | partly: the daemon, its store, `diskctl` and the labs; not wired to pods, not packaged |
| [0012](0012-security-groups.md) | Node | Security groups: stateful rules that only allow, without order, peers as address blocks with exceptions or as other groups; the control plane sends each node its groups whole or as deltas, with a version per group; nftables enforces them | Proposed | partly: groups on disk, `SecurityGroupService`, one table `inet feos` with a chain per pod and direction, set deltas; nothing sends groups over Valkey yet |
| [0013](0013-traffic-records.md) | Node | `feosd` records the pods' traffic: flows from connection tracking, drops, DNS from its resolver, server names from copies of a connection's first packets; no eBPF, nothing in the data path | Proposed | partly: flows, drops, DNS and server names, streamed by `TrafficService`; not published to Valkey |
| [0014](0014-identity-and-credentials.md) | Node, clients | A pod's identity and credentials stay on the host: `feos-authn`, one jailed process per running pod, holds its mTLS identity (a SPIFFE certificate whose key never leaves it, signing only TLS 1.3 CertificateVerify) and its TLS, Kubernetes and SSH bindings, sealed to it; one vsock port with a preamble; audited before it answers. FeID, the libraries with which Go and Rust applications use it without configuration | Proposed | no: `feos-guest` pipes Kubernetes and SSH to ports 10002 and 10003; nothing answers on the host |

## Rules

- **Format:** Status and date, then Context, Decision, Alternatives
  considered, Consequences, and Open questions if there are any. Four-digit
  numbers, in English.
- **Status:** `Proposed` until decided, then `Accepted`. An accepted ADR
  that a later one replaces becomes `Superseded by NNNN`; one that a later
  one changes in part says `Amended by NNNN`.
- **Accepted ADRs are not rewritten** once 0.6.0 is released. A change of
  mind is a new ADR that supersedes or amends the old one. Until then, the
  ADRs are still being shaped and are edited in place.
- **Decisions, not descriptions.** What the system looks like (paths,
  ports, directories, RPC lists) belongs in `docs/architecture/`, which
  changes with the code in the same commit. An ADR names what it decides
  and why.
- **The sandbox system's decisions** come into this directory when their
  part is built here, rewritten in this project's terms: node, pod, guest,
  control plane, sandbox, service.
