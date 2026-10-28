# ADR 0022: Images come through the node: pulled with the tenant's credential, baked once into a shared layer, built on the platform, kept in the platform's registry, and a service's pod supervised by its guest

**Status:** Proposed · **Date:** 2026-10-27

## Context

A service's pod is not yet what ADR 0004 decided it is, and its image
does not come the way ADR 0001 and ADR 0011 want it to.

**Images.** The guest pulls its own image: `feos-guest`'s image service
fetches the manifest and the layers over the pod's own network,
anonymously, gzip layers only, and unpacks them into the pod's disk.
Every new pod pulls from scratch, "once per pod and node even when the
same image ran next door a minute ago" (ADR 0011), into a disk of 8 GiB
that also holds its writable layer and its logs. Every replica of a
rollout pulls again. `ferund` resolves a tag to a digest before it
places a pod, anonymously too: "private registries are not supported",
a 401 looks like a 404, and a reference that carries a digest already
skips the registry and fails in the guest instead. The addons' images
come from Docker Hub, anonymously, under its rate limits. `diskd` has
`Bake` and `ImportBase` and `feosd` never calls them: "no baked images".
ADR 0001 lists private images as the first thing CaaS needs; ADR 0004
leaves open how: "images from private registries pulled through the
host, which adds the registry's credential"; the principle stands that
no credential enters a pod. The platform runs a registry already,
`oci.<domain>`, distribution with its blobs in the object store, whose
users are an operator's htpasswd.

**Builds.** Nothing on the platform builds an image. FeCode's stage
answers "builds are not available yet" for a service without an image,
and until something builds one, a project's stage and production run
images someone pushed by hand, from a sandbox that has no Docker
(FeCode's ADR 0008, FeOS ADR 0018).

**The pod.** ADR 0004 decided that the guest supervises its containers:
roles, a restart policy with backoff, startup, readiness and liveness
probes, `SIGTERM` and a grace period at stop, the image's user. None of
it is built. A container that exits stays exited; `ferund` notices by
asking `GetPod` every second for every running instance, over the node
connection and vsock, runs readiness checks of its own through the port
door, and restarts an exited container by rebooting its pod. Stopping a
pod is a reboot: no signal reaches a process, "a stop is a SIGKILL".
Every container runs as uid 0, whatever its image says. Its log is one
file, appended to without bound, without timestamps or the stream it
came from, gone with the pod. ADR 0018 called this the interim, and so
does `docs/architecture/control-plane.md` (follow-up 11).

A production application needs the opposite of each: a private image
pulled once, a rollout that starts a replica in the time a disk layer
takes to attach, a stop that lets a request finish, a crash that is
restarted inside the pod while the control plane is away, and a log
that is there when the pod is not.

## Decision

### Images are the node's artifacts, baked once and shared

- The node, not the guest, fetches a container image for a pod of a
  service, a job or an addon, and turns it into a **baked layer**: one
  sealed, content-addressed disk layer of `diskd`'s (ADR 0011), keyed by
  the image's manifest digest, holding the image's root filesystem
  unpacked. The pod's container mounts that layer read-only and puts
  its writable layer on the pod's own disk, as it does with the
  unpacked image today; its process, exits and restarts do not change.
- A baked layer is made once per cluster and cached per node: `diskd`
  tiers it to the object store as it tiers hibernated disks, and a node
  that needs an image another node baked fetches the sealed layer,
  lazily by chunk, instead of baking again. A second replica, a replica
  on another node, a restart after a crash and a revert to last week's
  release all attach a layer that is there.
- Baking is a pod's work, since the node shells out to nothing
  (ADR 0001) and writes no filesystem of its own: `feosd` pulls the
  layers over HTTPS with the image service's client on the host, keeps
  the blobs by digest in a cache of the node's, and runs a **bake pod**,
  a microVM of the node's own with `feos-guest` and no container, which
  takes the layers over vsock, unpacks them in order into a fresh volume
  (formatted as ADR 0016 formats one) and stops; `feosd` seals the
  volume as the image's layer. No tenant's code runs in a bake pod, and
  no credential enters it either.
- The guest's own image service stays for what a sandbox's agent pulls
  itself, anonymously, as today. A sandbox's default image is baked
  like any other, so a sandbox starts from a layer too.

### Private registries: the tenant's credential, used by ferund and the node, never by a pod

- A **registry credential** is an object of the tenant: a registry host,
  a username and a secret, sealed in the database under the key that
  seals secrets (ADR 0018), set and rotated through the API, never
  returned. `ferund` resolves a tag to a digest with it, and the plan's
  finding for an image that does not resolve says whether the registry
  refused or does not know it.
- The credential travels to the node with the bake, over the node
  connection, as a pod's sealed bindings travel (ADR 0014, ADR 0015):
  `feosd` holds it in memory for the pull and writes it nowhere. The
  pod gets a layer; the guest never sees the registry.
- A reference with a digest is resolved like a tag: the registry is
  asked for the manifest, with the credential, so that a private image
  named by digest fails at plan, not in a pod. Layers may be gzip, zstd
  or uncompressed.

### The platform's registry: ferund is its authority

- The platform's registry, distribution at `oci.<domain>` with its blobs
  in the object store, becomes a part of FeRun: its authentication is
  token authentication whose issuer is `ferund`
  (`/api/v1/registry/token`), and it has no users of its own. A
  repository's namespace is a tenant's, `oci.<domain>/<tenant>/<name>`;
  a token of the tenant's grants pull and push in its namespace, a
  person signed in to `ferund` as an admin grants everything, and a
  node pulls a baked image's layers with a token `ferund` mints for
  that one bake, scoped to that repository, expiring in minutes. A
  `docker login` with a tenant's token is a `docker push` into the
  tenant's namespace.
- The platform's own images, `ferund`, `feos-node`, the builder below,
  and the addons' images live under `platform/`, readable by every
  node; the addons' blueprints name them there, so that Docker Hub's
  limits never touch an addon's rollout. An operator mirrors them in
  with a make target; a mirror `ferund` keeps itself comes later.

### Builds run on the platform

- `ferund` builds images: `POST /api/v1/builds` with a source, a build
  context and a Dockerfile within it, build arguments, and the image to
  push, `<tenant>/<name>:<tag>` in the platform's registry. A build is
  a run of the platform's **builder** image, BuildKit, in a pod of the
  tenant's like a job's run (ADR 0018): placed, logged, bounded in time,
  retried never. Its result is a digest, kept with the build's record;
  its failure a record with the log's tail.
- The source is a snapshot `ferund` fetches for the build and puts into
  the pod as files: a git repository at a commit, fetched over HTTPS
  with the credential the caller passes for that one fetch (FeCode's
  git gateway with a token of a landing's; any forge with a token), or
  an archive the caller uploads. The credential is used by `ferund` and
  discarded; the pod gets the tree and the commit's ID as a build
  argument. No credential enters the build pod, and the builder pushes
  with a token minted for the build alone.
- The microVM is the boundary between tenants (ADR 0001), so the
  builder's container may hold what a build needs, mount and user
  namespaces among them, which no container of a service holds. A
  build's cache is BuildKit's registry cache in the image's own
  repository, so the second build of a project is the first's work
  reused.

### A service's pod is supervised by its guest

ADR 0004 is built, in this order, and `ferund` drops its interim as
each part lands; the three points ADR 0004 left open are decided here:

1. **Stop.** `StopPod` sends the main containers `SIGTERM`, waits the
   pod's grace period, 30 s when unset, sends `SIGKILL`, then the
   sidecars the same way, then shuts the microVM down. A stop of the
   guest's container defaults to `SIGTERM` as its API says.
2. **Restarts in the guest** with Kubernetes's backoff, the restart
   count and last exit in the container's status, the writable layer
   kept. `ferund` stops rebooting pods for an exited container; its
   crash loop reads the guest's count.
3. **Exits and readiness as events**, not polls: the guest reports a
   container's start, exit and change of readiness on its uplink, and
   `feosd` forwards them to the control plane as pod events (ADR 0015).
   `ferund`'s one-second `GetPod` poll goes.
4. **Probes in the guest**: startup, readiness and liveness, exec, HTTP
   and TCP, run inside the pod's network; a failed liveness probe ends
   the process; readiness goes into the status and the events, and from
   there into the routes (ADR 0018, ADR 0021). `ferund`'s readiness
   checks through the port door go.
5. **Roles and the reaper**: init, sidecar and main containers in their
   order, an idle container's PID 1 the reaper, `reaper` on request.
6. **The image's user** runs the process, as Docker and Kubernetes do;
   `uid 0` only when the image says so or the spec asks for it. The
   image's `StopSignal` is honoured.
7. **Logs** are bounded: a container's log is a ring of a fixed size on
   the pod's disk, rotated, each line stamped with its time and stream;
   `StreamContainerLogs` serves it as before. When a pod ends, the last
   64 KiB of each container's log go into the pod's record and stay
   until the pod is deleted: a job's output is there when the job is
   not (ADR 0004's open question).

Per-container resources, ephemeral volumes for sidecars and volumes for
services (ADR 0016) are not part of this ADR.

## Alternatives considered

- **The credential in the guest**, pushed into the pod for its pull: a
  credential in a pod, against ADR 0001, and still a pull per pod.
- **A pull-through proxy on the host over vsock**, as ADR 0014 proxies
  the Kubernetes credential, with a blob cache on the node: the
  credential stays on the host and blobs are fetched once per node, but
  every pod still unpacks its image into its disk, and every replica's
  start is an unpack. Baking makes the proxy unnecessary; the host's
  client of the registry is what the bake pulls with.
- **A layer per image layer**, mounted as overlay lowers in the guest,
  as Kata and firecracker-containerd do: exact sharing of a base layer
  between images, but a drive per layer against the pod's drive limit
  and an overlay of many lowers in the guest. One layer per image, with
  `diskd`'s chunk deduplication across layers, gives most of the
  sharing for none of that.
- **Unpacking on the host** into a filesystem image: the node runs no
  `mkfs` (ADR 0001, ADR 0016's one exception is the guest's), and
  writing ext4 or erofs from Rust is a project of its own; a pod that
  boots in under a second is the node's own tool.
- **Lazy image formats** (eStargz, Nydus) and a converter beside the
  registry: a second image format to keep; a baked layer in the object
  store is fetched lazily by chunk already (ADR 0011).
- **Builds in the agent's sandbox**: the sandbox has no privileges and
  the build would depend on a workspace being there; a build at landing
  needs no sandbox and gives stage the image of main itself.
- **Builds by a CI outside** (GitHub Actions pushing to the registry):
  the GitOps path a caller may take (ADR 0018); FeCode's projects have
  no CI, FeCode is their workbench, and "stage follows main" is true
  only when the platform builds what main is.
- **kaniko or buildah** as the builder: BuildKit is what `docker build`
  is, with the cache and the frontends people's Dockerfiles expect, and
  in a microVM it may run with the privileges it wants.
- **A registry per tenant**: distribution is built for namespaces and a
  token issuer; one registry, one issuer.
- **htpasswd kept**: users by hand, no tenant namespaces, and a node
  with a long-lived credential for every pull.
- **Restarts and probes kept in `ferund`**: ADR 0004 rejected it, and
  ADR 0018 called it the interim: every exit and probe crosses vsock and
  the node connection, nothing restarts while `feosd` restarts, and a
  pod's stop cannot be graceful from outside.

## Consequences

- `feosd` gains the host's registry client with credentials, a blob
  cache by digest, the bake pod and the sealing of its volume as a
  layer; `diskd` serves baked layers and tiers them; the pod spec names
  an image's layer instead of, or beside, an image reference the guest
  pulls. A pod of a service starts without a pull, in the time its
  layer takes to attach.
- `ferund` gains registry credentials of tenants, resolution with them,
  the registry's token endpoint and the tenants' namespaces, builds
  with the builder image and their records, pull tokens per bake, and
  loses its probes, its `GetPod` poll and its restarts by reboot as the
  guest takes them over. Releases pin digests as before.
- `feos-guest` gains what ADR 0004 lists and the ring log; its
  `ContainerSpec` and status grow additively (ADR 0007). Every image
  rolls once when its pods move from an unpacked image to a baked
  layer.
- The registry's configuration changes to token authentication against
  `ferund`; the operator's htpasswd goes once the platform's images are
  pushed under `platform/` with an admin's token. Addon blueprints name
  `platform/` images.
- A pod's disk no longer holds its image: the 8 GiB are the writable
  layer's and the log's; a size per pod stays ADR 0011's question.
- FeCode's stage and production get images the platform built from
  main, promotion carries their digests, and `values.images` stays for
  an image a project names (FeCode's ADR 0009).
- Built in this order: stop, restarts, exits and readiness as events,
  probes, so that `ferund` sheds its interim first; registry
  credentials and the token endpoint, which need nothing of the node;
  builds, which need a registry with namespaces; baked layers last,
  since a pod that pulls its private image through the host's resolve
  and a short-lived pull token already runs — until layers are baked,
  the pull token is what the guest gets, scoped to one repository and
  expiring in minutes, the one credential that enters a pod while the
  bake is built, and the one thing this order costs.

## Open questions

- **Garbage collection** of baked layers and blobs on nodes and in the
  object store: which release's images a cluster keeps, and for how
  long.
- **Multi-platform images** and a second architecture of nodes.
- **Per-container resources** and limits in the guest, and the memory a
  pod's sidecars may take.
- **Build limits**: how many builds a tenant runs at once, how long one
  may run, and the builder pod's size.
- **A mirror `ferund` keeps** of upstream images, so that an addon's
  minor upgrade needs no operator.
- **Scanning** images for known vulnerabilities before a release runs
  them.
- **Signing**: whether a build's result is signed and a release checks
  the signature.
