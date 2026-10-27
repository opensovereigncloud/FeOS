FeOS and FeRun on Gardener
==========================

An example of the whole platform on a Gardener shoot: FeOS on the
workers of a pool, in a pod per worker (ADR 0020), FeRun's control plane
`ferund` beside it, FeChat, and an image registry. It was built on
IronCore, whose workers are VMs with nested virtualization (`/dev/kvm`,
`kvm_intel nested=Y`) running Garden Linux. Garden Linux's kernel has
what FeOS needs (ublk, tun, vhost-vsock, WireGuard, nftables).

| Part | Where | What |
|---|---|---|
| [`feos/`](feos/) | namespace `feos`, the DaemonSet `feos-node` on every worker of the pool `WORKER_POOL`, and `feos-k8s` | diskd and feosd in a pod per worker, whose microVMs live in it (ADR 0020); feos-k8s joins new workers and drains cordoned ones in FeRun |
| [`ferun/`](ferun/) | namespace `ferun` | ferund and its PostgreSQL: the k3s manifests ([`../k3s/ferun`](../k3s/ferun)) with the nodes' port at a shoot address; `https://run.<DOMAIN>` |
| [`../k3s/fechat`](../k3s/fechat) | namespace `fechat` | FeChat, unchanged; `https://chat.<DOMAIN>` |
| [`registry/`](registry/) | namespace `registry` | an image registry, `https://oci.<DOMAIN>`: distribution with its blobs in an object store |
| [`cluster.yaml`](cluster.yaml) | | the issuer `letsencrypt-prod` the manifests name, answering DNS-01 |

Every name under `DOMAIN` points at the shoot's public address, the
LoadBalancer of the ingress controller Traefik.

```
 *.<DOMAIN> ── LoadBalancer ── traefik ── ferund :8100 ──── postgres
                                             │ :7443 (mTLS, through kube-proxy)
   workers: feos-node pod ───────────────────┘   dials FERUN_NODES_IP:7443,
       │ feos-mesh, UDP 51820 between workers    then the instances' pod addresses
```

## What it needs

- A shoot with a worker pool for FeOS whose machines have `/dev/kvm` and
  the memory and disk of [the sizing](#differences-to-k3s): workers of
  64 GiB and a 50 GiB root as the manifests stand. The network carries
  UDP between the workers (port 51820, the pods' mesh).
- Traefik as the ingress controller (class `traefik`), behind a
  LoadBalancer, and cert-manager.
- The DNS zone `DOMAIN` at Cloudflare, for the issuer's DNS-01
  ([`cluster.yaml`](cluster.yaml) says where another provider goes).
- An S3-compatible object store: a bucket for diskd, which the pods
  hibernate into, and one for the registry.
- An OpenID Connect provider, at which people sign in to ferund and
  FeChat, and an OpenAI-compatible LLM gateway for FeChat.
- On the machine that deploys: docker, kubectl, `envsubst` (gettext), and
  what `make packages` needs ([`docs/development.md`](../../docs/development.md)).

## Parameters

What differs from one shoot to another is a parameter, set in
`config.env` in this directory: a copy of
[`config.env.example`](config.env.example), which git ignores. A variable
on make's command line wins over the file.

| Parameter | What |
|---|---|
| `DOMAIN` | the DNS zone: ferund at `run.<DOMAIN>`, FeChat at `chat.<DOMAIN>`, the registry at `oci.<DOMAIN>`; `run.<DOMAIN>` is FeRun's trust domain too |
| `REGISTRY` | where the images are pushed and pulled; default `oci.<DOMAIN>` |
| `OIDC_ISSUER` | the OpenID Connect provider, exactly as its discovery document states it |
| `WORKER_POOL` | the worker pool that runs FeOS, its label `worker.gardener.cloud/pool` |
| `NODE_NAME_PREFIX` | the beginning of the pool's node names, on IronCore `shoot--<project>--<shoot>-`; feos-k8s removes from FeRun only nodes with it |
| `FERUN_NODES_IP` | the ClusterIP of the service `ferund-nodes`, which feosd dials: a free address of the shoot's service network (`spec.networking.services`) |
| `FECHAT_MODEL` | FeChat's model at the LLM gateway |

The manifests say `${NAME}` where a parameter goes. The Makefile's
`apply-*` targets fill them in with `envsubst`, write the result to
`target/gardener/manifests/` and apply it; `make -C deploy/gardener
manifests` only writes them.

## Nodes: a pod per worker

FeOS on the shoot is Kubernetes workload (ADR 0020), not host services:
the DaemonSet `feos-node` runs a pod on every worker of the pool, with
two containers of the image `feos-node` (the packages' binaries,
Firecracker and the guest, unpacked): `diskd` and `feosd`. The pod is
privileged and on the host's network, with the host's `/dev` and
`/var/lib/feos`. Nothing is installed on the worker.

- **The microVMs live in the feosd container**, in cgroups below its own,
  and end with it. Its memory request (40 GiB) is what the worker gives
  them, which the scheduler keeps free of other pods; it has no limits.
- **A rollout suspends and resumes them**: on `SIGTERM`, feosd announces
  its restart to FeRun, suspends every running pod into its directory and
  exits; the next feosd resumes them. A crash ends them; FeRun brings their
  sandboxes back from their last hibernation.
- **Versions side by side**: the init container adds the image's
  Firecracker and guest versions to `/var/lib/feos/dist`, which the feosd
  container sees as `/usr/lib/feos`, so that a pod resumes with the
  Firecracker it was suspended with.
- **Workers come and go**: Gardener replaces them on updates and in the
  shoot's maintenance window by cordoning, draining and deleting the
  machine. `feos-k8s` makes a join token for every new worker and keeps a
  guard pod on every worker, under a PodDisruptionBudget that allows no
  eviction; when a worker is cordoned, it drains the node in FeRun, whose
  sandboxes hibernate into the object store and are restored on the
  others, and only then lets the guard go.

The image comes from the packages:

    make packages FEOS_VERSION=0.6.0~dev.$(date -u +%Y%m%d%H%M).$(git rev-parse --short=7 HEAD)
    make -C deploy/gardener deploy-feos      # build, push and roll feos-node

## First setup

From the repository's root, with `KUBECONFIG` at the shoot and
`config.env` written. Keep the secrets' files outside the repository.

The issuer, and the registry, whose secrets
[`registry/registry.yaml`](registry/registry.yaml) describes:

    kubectl -n cert-manager create secret generic cloudflare-api-token --from-literal=api-token=<token>
    kubectl apply -f deploy/gardener/cluster.yaml
    make -C deploy/gardener apply-registry
    kubectl -n registry create secret generic registry-config --from-file=config.yml=<config.yml>
    kubectl -n registry create secret generic registry-auth --from-file=htpasswd=<htpasswd>
    docker login <REGISTRY>

Every namespace that pulls from it, `feos`, `ferun` and `fechat`, needs
the secret `oci-pull` with the registry's credentials, as
[`../k3s/README.md`](../k3s/README.md) makes it.

ferund: the CA and the secrets of [`../k3s/README.md`](../k3s/README.md)
(`ferun-ca`, `oci-pull`, `ferund-postgres`, `ferund`, `ferund-secret-key`,
`oidc-client-ferun`), then:

    make -C deploy/gardener push-ferund apply-ferun

The nodes, and feos-k8s with an admin user of ferund's own
([`feos/feos-k8s.yaml`](feos/feos-k8s.yaml) says how). `<s3.env>` holds
the object store's variables of
[`hack/packages/diskd/default`](../../hack/packages/diskd/default); a key
prefix of the cluster's own lets clusters share a bucket:

    make packages FEOS_VERSION=...
    make -C deploy/gardener push-feos-node apply-feos
    kubectl -n feos create secret docker-registry oci-pull ...
    kubectl -n feos create secret generic diskd-s3 --from-env-file=<s3.env> \
      --from-literal=DISKD_S3_PREFIX=<prefix>/
    kubectl -n feos create secret generic feos-k8s-ferun --from-literal=credentials="feos-k8s:$pw"
    make -C deploy/gardener deploy-feos-k8s

feos-k8s puts a join token for every worker into `feosd-join`; feosd
waits for it.

FeChat: the secrets of [`../../fechat/README.md`](../../fechat/README.md),
with `FERUN_TOKEN` the bootstrap token, then the manifests and the
images:

    make -C deploy/gardener apply-fechat
    REGISTRY=<REGISTRY> fechat/deploy.sh <tag>

Every other client of FeRun gets a tenant of its own: `POST
/api/v1/admin/tenants` with `{"name":"<tenant>"}`, which answers with a
first token, and more with `POST /api/v1/admin/tenants/<tenant>/tokens`,
with an admin user's Basic credentials and `X-Requested-With: ferun`.

## Differences to k3s

- The nodes' port is `FERUN_NODES_IP`, in the shoot's service network,
  and ferund waits up to 8 minutes for a node that restarts
  (`FERUN_ROLLOUT_GRACE`); everything else of ferund is the k3s manifest.
- Pods are denied RFC 1918 and CGNAT (`DENY_PREFIXES`), which hold the
  shoot's networks, its nodes', pods' and services'
  (`spec.networking`); a shoot with a network elsewhere adds it there.
- IronCore routes no IPv6 to the workers: pods are on the mesh's ULA and
  leave over IPv4, through the shoot's NAT gateway.
- The microVMs' memory is the feosd container's request: 40 GiB, for
  workers of 64 GiB; the rest is the shoot's own workloads'. The chunk
  cache is 12 GiB (`DISKD_ARGS`), for a 50 GiB root, which holds the
  pods' disks too. Other machines change both in
  [`feos/feos.yaml`](feos/feos.yaml), and `terminationGracePeriodSeconds`
  with the memory: a stop suspends all of it.
