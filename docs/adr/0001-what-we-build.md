# ADR 0001: What we build

**Status:** Accepted · **Date:** 2026-10-14

## Context

Two lines of work meet in this repository. FeOS was a Rust PID 1 for
bare-metal hypervisors that ran VMs and containers for an orchestrator. An
agent sandbox system ran Firecracker microVMs with a Rust PID 1 that ran an
agent's containers, a manager on every node, and a control plane with a
REST API and MCP. The decisions so far record how the two merge; none says
what the result is for.

We want to offer two things:

- **Agent sandboxes.** An LLM agent runs commands and tools in a workspace
  for a user, often for days. The work is bursty: seconds of commands,
  tens of seconds of thinking, then minutes to hours of waiting for the
  user. The dominant resource of an idle sandbox is memory. Nodes get
  drained, so a sandbox has to resume elsewhere.
- **A container as a service (CaaS).** Customers run their own container
  images as services without operating a cluster. Many services are idle
  most of the time, too.

Both run untrusted code of many tenants in containers, and both need a
stronger boundary between tenants than a shared kernel gives.

## Decision

### Two products on one platform

Agent sandboxes and CaaS are two products on one platform. What the
products are, sandboxes with tools and tokens, services with replicas and
routes, lives in the control plane. The nodes do the same for both.

### The pod is the unit

A node runs **pods**: a Firecracker microVM that runs `feos-guest` and one
or more containers. The VM is the boundary between tenants, the container
the boundary around the workload. An isolated container is a pod with one
container. A pod can be suspended to a snapshot and resumed.

### FeOS is the node layer

FeOS is everything that runs on a node: `feosd`, `feos-guest`,
`feos-host-init` where FeOS is the node's operating system, `feosctl`, and
what joins them on the node (a disk daemon, per-pod proxies). FeOS's APIs
speak of pods, containers and images, not of sandboxes or services.

### The control plane is one or two central services

Tenants, tokens, the registry of sandboxes and services, placement, the
policies that pause, hibernate and scale to zero, and the front doors (a
REST API, MCP, HTTP to the ports of pods) run centrally, in one or two
services. They reach nodes only through Valkey.

### One repository

Node and control plane are coupled through the `feos.*` APIs: a feature
usually changes both sides at once. They live in one repository (ADR 0002).

### Requirements

1. **Isolation.** The VM is the boundary between tenants. No control
   surface is reachable without authentication; pods do not see each
   other; no credential enters a pod.
2. **Cheap suspend.** Suspending is frequent: it writes what changed, not
   the whole memory.
3. **Fast resume.** Under a second to the first instruction. Memory and
   disk may follow lazily behind the running guest.
4. **Memory back on suspend.** A suspended pod holds no guest memory.
5. **Resume on another node**, through object storage, without fetching
   the whole memory or disk first.
6. **Pods outlive their node agent.** Restarting or upgrading `feosd` does
   not stop pods.
7. **Nodes open no port for control.** They connect out.

### Non-goals

- Arbitrary VMs: no other guest operating systems, no firmware boot.
- PCI passthrough: no GPUs or NVMe in pods, for now.
- Confidential computing (TDX, SEV-SNP).
- Point-in-time recovery: a snapshot is the latest state, not a history.

## Open questions

CaaS needs what the sandboxes ruled out for themselves:

- **Inbound traffic.** Sandboxes take none, except through the control
  plane's front door to their ports. Services must be reachable: is HTTP
  through a central front door enough, or do services need L4 and
  addresses of their own?
- **What a service's pod needs**: restart policies, health checks,
  persistent volumes, images from private registries (the sandboxes pull
  public images only), several containers in one pod, jobs as well as
  services.
- **Kubernetes.** A CaaS API of our own, or one that Kubernetes tooling
  understands, through a CRI adapter or a virtual kubelet?

For both products:

- **Suspend tiers.** When a suspended pod stays on its node and when it
  goes to object storage: this shapes the cost as much as the formats do.
- **The split of the control plane** into one or two services: whether the
  front doors run apart from the registry and placement.

## Alternatives considered

- **FeOS as an operating system only, the platform in a repository of its
  own** that takes released FeOS artifacts. Every feature that crosses the
  API would become a change, a release and a version bump on two sides.
- **A runtime per product**: microVMs for sandboxes, containers on a shared
  kernel for CaaS. Two runtimes to build, and CaaS tenants are no more
  trusted than agents.
- **Kubernetes with a VM runtime** as the platform, a pod per sandbox. The
  sandbox system measured a constant 1 to 1.5 s added to every resume, and
  suspend and restore are not part of Kubernetes's model.

## Consequences

- Decisions are written in these terms: node, pod, guest, control plane,
  sandbox and service.
- FeOS's APIs stay product-neutral. A capability one product needs, such
  as the agent's tools, is a capability of pods that the other product may
  leave unused.
- The sandbox system's decisions come into this repository when their part
  is built here, rewritten in these terms.
- CaaS adds requirements that the sandbox system excluded: inbound traffic
  and private images first.
