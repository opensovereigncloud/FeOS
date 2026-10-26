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
| [`feos/`](feos/) | namespace `feos`, a DaemonSet on every worker labelled `feos.fecode.dev/node=true` | lays FeOS's packages over the worker's `/usr` as a systemd system extension and has systemd run feosd and diskd |
| [`ferun/`](ferun/) | namespace `ferun` | ferund and its PostgreSQL: the k3s lab's manifests ([`../k3s/ferun`](../k3s/ferun)) with the nodes' port at a shoot address; https://run.fecode.dev |
| [`../k3s/fechat`](../k3s/fechat) | namespace `fechat` | FeChat, unchanged; https://chat.fecode.dev |
| `~/idp/k8s/idp.yaml` | namespace `idp` | FeCode ID, unchanged; https://id.fecode.dev |
| [`registry/`](registry/) | namespace `registry` | the image registry, https://oci.fecode.dev: distribution with its blobs in the bucket `fecore-oci`; every image here comes from it |
| [`cluster.yaml`](cluster.yaml) | | the issuer `letsencrypt-prod` the manifests name, answering DNS-01 |

```
 *.fecode.dev ── IronCore LB ── traefik ── ferund :8100 ──── postgres
                                              │ :7443 (mTLS, through kube-proxy)
   workers: feosd (systemd) ──────────────────┘   dials 100.72.70.70:7443,
       │ feos-mesh, UDP 51820 between workers      then the instances' pod addresses
```

The workers are IronCore VMs with nested virtualization (`/dev/kvm`,
`kvm_intel nested=Y`) running Garden Linux. Its kernel has what FeOS needs
(ublk, tun, vhost-vsock, WireGuard, nftables), and IronCore's network
carries the mesh's UDP between workers.

## Nodes: a system extension instead of apt

Garden Linux mounts `/usr` read-only and has no apt to install packages
with, but feosd boots Firecracker and the guest from `/usr/lib/feos` and
systemd runs it from `/usr/lib/systemd/system`. So
[`feos/install`](feos/install), the DaemonSet's init container, unpacks the
`/usr` of the image's packages into `/var/lib/extensions/feos-<version>`
and `systemd-sysext refresh` lays it over `/usr`: the files land where the
packages put them on Debian (ADR 0009), and the units are the packages'.
`systemd-sysext.service` merges it again at boot. The configuration goes
to `/etc/default/{feosd,diskd}` as on k3s, and the group `feos` comes from
the packages' sysusers file.

A new version is a new extension that takes over the Firecracker and guest
versions of the one before (hard links), so that pods keep resuming and
restoring with what they started with, as packages installed side by side
do. Then diskd and feosd restart; neither stops a pod (KillMode=process).

The image is the k3s one's Dockerfile with this install script, from the
same packages:

    make packages FEOS_VERSION=0.6.0~dev.$(date -u +%Y%m%d%H%M).$(git rev-parse --short=7 HEAD)
    make -C deploy/gardener deploy-feos      # build, push and roll feos-node-gardenlinux

Gardener replaces workers on updates and in maintenance (22:00–23:00
CEST): a new worker gets FeOS from the DaemonSet, but it needs the label
and a join token for its name (below), and pods on the old worker live on
only as far as they hibernated into the object store.

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

The nodes:

    kubectl -n feos create secret generic diskd-s3 --from-env-file=<s3.env with DISKD_S3_PREFIX=gardener/>
    args=""
    for n in $(kubectl get nodes -o name | sed 's|node/||'); do
      args="$args --from-literal=$n=$(kubectl -n ferun exec deploy/ferund -- /ferund join-token --ttl 2h)"
    done
    kubectl -n feos create secret generic feosd-join $args
    kubectl label node --all feos.fecode.dev/node=true
    kubectl apply -f deploy/gardener/feos/feos.yaml

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
- The kubelet does not know the microVMs' memory. The workers have 64 GiB;
  keep the shoot's own workloads small or reserve memory for FeOS in the
  shoot's kubelet configuration (`systemReserved`).
- The chunk cache is 12 GiB of the workers' 50 GiB root, which holds the
  pods' disks too.
