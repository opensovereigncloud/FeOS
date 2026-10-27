FeOS and FeRun on Gardener (IronCore)
=====================================

The platform runs on the Gardener shoot `fecode-dev` (project `feos`,
IronCore region wdf4), beside FeCode (`~/fecode/deploy/fecode`; its git
gateway is https://git.fecode.dev). Every name of the zone fecode.dev points
at the shoot's one public address, the LoadBalancer
of Traefik (`traefik/traefik`), 194.11.242.33. The k3s lab
([`../k3s`](../k3s)) is off.

| Part | Where | What |
|---|---|---|
| [`feos/`](feos/) | namespace `feos`, the DaemonSet `feos-node` on every worker of the pool `worker-w1pmo`, and `feos-k8s` | diskd and feosd in a pod per worker, whose microVMs live in it (ADR 0020); feos-k8s joins new workers and drains cordoned ones in FeRun |
| [`ferun/`](ferun/) | namespace `ferun` | ferund and its PostgreSQL: the k3s lab's manifests ([`../k3s/ferun`](../k3s/ferun)) with the nodes' port at a shoot address; https://run.fecode.dev |
| [`../k3s/fechat`](../k3s/fechat) | namespace `fechat` | FeChat, unchanged; https://chat.fecode.dev |
| `~/idp/k8s/idp.yaml` | namespace `idp` | FeCode ID, unchanged; https://id.fecode.dev |
| [`registry/`](registry/) | namespace `registry` | the image registry, https://oci.fecode.dev: distribution with its blobs in the bucket `fecore-oci`; every image here comes from it |
| [`cluster.yaml`](cluster.yaml) | | the issuer `letsencrypt-prod` the manifests name, answering DNS-01 |

```
 *.fecode.dev ── IronCore LB ── traefik ── ferund :8100 ──── postgres
                                              │ :7443 (mTLS, through kube-proxy)
   workers: feos-node pod ────────────────────┘   dials 100.72.70.70:7443,
       │ feos-mesh, UDP 51820 between workers      then the instances' pod addresses
```

The workers are IronCore VMs with nested virtualization (`/dev/kvm`,
`kvm_intel nested=Y`) running Garden Linux. Its kernel has what FeOS needs
(ublk, tun, vhost-vsock, WireGuard, nftables), and IronCore's network
carries the mesh's UDP between workers.

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
- **Workers come and go**: Gardener replaces them on updates and in
  maintenance (22:00–23:00 CEST) by cordoning, draining and deleting the
  machine. `feos-k8s` makes a join token for every new worker and keeps a
  guard pod on every worker, under a PodDisruptionBudget that allows no
  eviction; when a worker is cordoned, it drains the node in FeRun, whose
  sandboxes hibernate into the object store and are restored on the
  others, and only then lets the guard go.

The image comes from the packages:

    make packages FEOS_VERSION=0.6.0~dev.$(date -u +%Y%m%d%H%M).$(git rev-parse --short=7 HEAD)
    make -C deploy/gardener deploy-feos      # build, push and roll feos-node

## First setup

From the repository's root, with `KUBECONFIG` at the shoot. The secrets'
values are in `~/.config/feos/gardener/` on the operator's machine.

    kubectl apply -f deploy/gardener/cluster.yaml
    # the registry's secrets registry-config and registry-auth, then
    kubectl apply -f deploy/gardener/registry/registry.yaml

FeCode ID (`~/idp`): `bootstrap.sh` (it copies `oci-pull` from namespace
`fecode`), the secret `idp-github` copied from before, `kubectl apply -f
k8s/idp.yaml`, then the clients: `./create-client.sh ferun ferun`,
`./create-client.sh fechat fechat`, `./create-client.sh fecode` (FeCode's
`oidc.json` in its secret `fecode-backend`).

ferund: the secrets of [`../k3s/README.md`](../k3s/README.md) (`oci-pull`,
`ferund-postgres`, `ferund`, `ferund-secret-key`, `ferun-ca` from the
platform CA's issuer), then:

    make -C deploy/gardener apply-ferun

The nodes, and feos-k8s with an admin user of ferund's own
([`feos/feos-k8s.yaml`](feos/feos-k8s.yaml) says how):

    kubectl -n feos create secret generic diskd-s3 --from-env-file=<s3.env with DISKD_S3_PREFIX=gardener/>
    kubectl -n feos create secret generic feos-k8s-ferun --from-literal=credentials="feos-k8s:$pw"
    make -C deploy/gardener deploy-feos-k8s
    kubectl apply -f deploy/gardener/feos/feos.yaml

feos-k8s puts a join token for every worker into `feosd-join`; feosd
waits for it.

FeChat: [`../../fechat/README.md`](../../fechat/README.md), with
`FERUN_TOKEN` the bootstrap token, then `kubectl apply -f deploy/k3s/fechat/`.

FeCode's backend uses a token of the tenant `fecode` (its secret's
`ferun.token`): `POST /api/v1/admin/tenants` with `{"name":"fecode"}`, then
`POST /api/v1/admin/tenants/fecode/tokens`, with the admin users' Basic
credentials and `X-Requested-With: ferun`. The tenant's creation answers
with a first token too.

## Differences to the k3s lab

- The nodes' port is `100.72.70.70` (the shoot's services are
  100.72.0.0/13); everything else of ferund is the lab's manifest.
- Pods are denied the workers (10.250/16) and the shoot's pods and
  services (100.64/13, 100.72/13), all within `DENY_PREFIXES`.
- IronCore routes no IPv6 to the workers: pods are on the mesh's ULA and
  leave over IPv4, through the shoot's NAT gateway.
- The microVMs' memory is the feosd container's request: 40 GiB of the
  workers' 64 GiB; the rest is the shoot's own workloads'.
- The chunk cache is 12 GiB of the workers' 50 GiB root, which holds the
  pods' disks too.
