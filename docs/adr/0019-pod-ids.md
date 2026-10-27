# ADR 0019: Pods have short IDs that the control plane mints, and their TAP devices are named by them

**Status:** Accepted · **Date:** 2026-10-26

## Context

A pod's ID names its microVM, its paths on the node, and its TAP device
(ADR 0010). Pod IDs have been UUIDs, chosen by whoever creates the pod: a
client of ferund for a sandbox (FeCode its worktree's ID, FeChat its
conversation's), ferund for an environment's replica, feosctl by hand.

A TAP device's name has 15 characters at most (`IFNAMSIZ`), so feosd
names it `pod-` and the first 8 hex digits of the ID, and ferund keeps two
pods whose IDs start alike off the same node (a placement gate). The first
8 hex digits of a UUIDv7 are the high bits of its time stamp: every UUIDv7
made within the same 65 seconds starts alike. FeCode makes its worktrees'
IDs that way, and on the Gardener shoot, with two nodes, its third sandbox
within a minute found no node at all ("TAP prefix taken").

Any rule that lets a client choose a pod's ID and squeezes it into 15
characters can collide. The ID itself has to fit.

## Decision

### The format

A pod ID is 11 characters of lowercase Crockford base32
(`0123456789abcdefghjkmnpqrstvwxyz`), the first of them a letter:

    ^[a-hjkmnp-tv-z][0-9a-hjkmnp-tv-z]{10}$

That is about 2^54 IDs. `pod-` and the ID are 15 characters, a TAP
device's name. The ID is also a DNS label, a file name, a cgroup's name,
and a jailer's `--id` as it is; no character needs escaping anywhere. The
leading letter keeps an ID from being read as a number, and the alphabet
leaves out `i`, `l`, `o` and `u`, which are misread for digits.

### Who mints them

- **ferund mints every pod's ID:** a sandbox's and an environment's
  replica's, from a cryptographic random source. Its sandboxes' IDs are
  unique over all tenants (the table's primary key); a create whose new ID
  is taken draws another.
- **A client does not choose one.** `POST /api/v1/sandboxes` takes no
  `sandboxId` and refuses a request that names one (400); the answer
  carries the new sandbox's ID, which the client keeps. A create is not
  idempotent any more: the client keeps the ID before it relies on the
  sandbox.
- **feosctl** mints one the same way for a pod it creates on a node
  without a control plane.

### What the node makes of them

- The TAP device is `pod-<ID>`.
- The guest's MAC address is `06:00` and the first four bytes of the
  SHA-256 of the ID: locally administered, stable for the pod, and not
  the ID's characters, which are not hex.
- feosd checks a pod ID on every call that takes one, and refuses
  anything else.

### Pods with UUIDs stay

A pod made before this keeps its UUID until it is deleted: its microVM's
snapshot names its TAP device, and a hibernated pod comes back on any node
under the same name. feosd accepts a UUID as a pod ID and names its TAP
`pod-` and the first 8 hex digits, and its MAC from the same digits, as
before; ferund stores UUIDs and short IDs side by side and keeps its
placement gate for TAP names, which only UUIDs can share. Neither makes
new pods with UUIDs. The UUID path goes once no pod with one is left.

### Rollout

Each step works with the one before it:

1. **Clients** stop sending `sandboxId` and keep the ID the create
   returns. ferund already mints one when a create names none.
2. **feosd** accepts short IDs beside UUIDs.
3. **ferund** mints short IDs, refuses `sandboxId`, and stores IDs as text.

## Consequences

- A pod's ID is its TAP device's name: no two pods on a node can collide,
  and the placement gate only matters for pods with UUIDs.
- Clients keep a mapping from their objects to sandbox IDs: FeCode in its
  database, FeChat, which keeps no database, by handing the browser the
  sandbox ID bound to the user.
- ferund's sandbox ID columns change type, from `uuid` to `text`, with a
  check of the two formats; other IDs (tokens, environments, volumes,
  incarnations) stay UUIDs.
- 2^54 IDs are not secrets and not meant to be: a sandbox is reached with
  its token, never by knowing its ID.
