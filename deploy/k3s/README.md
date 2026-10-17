FeOS and FeRun on the lab's k3s
===============================

The lab cluster (three Debian 13 VMs with nested virtualization, k3s,
traefik, cert-manager, the registry oci.fecore.dev) runs the whole platform:

| Part | Where | What |
|---|---|---|
| [`feos/`](feos/) | namespace `feos`, a DaemonSet on every node labelled `feos.fecode.dev/node=true` | installs FeOS's packages (feosd, diskd, feosctl, Firecracker, the guest) on the node and has systemd run them |
| [`ferun/`](ferun/) | namespace `ferun` | ferund, FeRun's control plane (ADR 0015), and its PostgreSQL; https://run.fecode.dev, sign-in with a FeCode ID |
| [`fechat/`](fechat/) | namespace `fechat` | FeChat, the chat client whose tools run in FeRun's sandboxes; https://chat.fecode.dev, sign-in with a FeCode ID |

FeOS runs on the nodes as it would on any Debian machine: the DaemonSet is
only how the packages get there. feosd and diskd are the host's services, so
their pods and disks outlive the DaemonSet's pods, a rollout, and the
DaemonSet itself. feosd connects out to ferund; no node opens a port.

```
 run.fecode.dev ── traefik ── ferund :8100 ──── postgres
                                  │ :7443 (mTLS, TCP through kube-proxy)
       hosts: feosd ──────────────┘   dials 10.43.70.70:7443 to join,
                                      then every instance's pod address
```

## Nodes

Built from `make packages` (FeOS's .deb packages, ADR 0009), with a version
per build so that apt upgrades the nodes:

    make packages FEOS_VERSION=0.6.0~dev.$(date -u +%Y%m%d%H%M).$(git rev-parse --short=7 HEAD)
    make -C deploy/k3s deploy-feos           # build, push and roll the image

The first time:

    kubectl apply -f deploy/k3s/feos/feos.yaml
    # diskd's object store, which pods hibernate into: the variables of
    # hack/packages/diskd/default, with a prefix of the bucket's own
    kubectl -n feos create secret generic diskd-s3 \
      --from-env-file=$HOME/.config/feos/diskd/s3.env --from-literal=DISKD_S3_PREFIX=k3s/
    kubectl label node <node> feos.fecode.dev/node=true

The lab's network routes no IPv6 to the nodes. With the control plane,
ferund runs with `FERUN_POD_NETWORK=mesh`: each node gets a /64 of the
cluster's ULA from it and reaches the other nodes' pods through the
WireGuard device `feos-mesh`, UDP port 51820 between the nodes (ADR 0017).
Without, each node takes `fd00:fe05:<last octet of its address, hex>::/64`,
which no other node reaches. Either way pods leave over IPv4 alone. Pods
are denied the nodes' and the cluster's networks (configmap `feos-node`).

On a node, `feosctl` talks to feosd as usual, and the services' logs are
in its journal (`journalctl -u feosd -u diskd`), and in `kubectl -n feos
logs <pod> -c journal`.

FeOS runs beside the old sandbox system's manager and disk daemon on the
same nodes: its own state (/var/lib/feos), devices (`pod-…` TAPs), nftables
table (`feos`) and addresses (169.254.0.0/16 on the TAPs).

## The control plane

### Once: the CA

The platform CA's root stays offline, in `~/.config/feos/ferun/root/` on the
operator's machine; ferund holds only the issuer:

    ferund init-ca --root-dir ~/.config/feos/ferun/root --dir ~/.config/feos/ferun/ca \
      --trust-domain run.fecode.dev --dns-domain run.fecode.dev
    kubectl -n ferun create secret generic ferun-ca --from-file=$HOME/.config/feos/ferun/ca

That makes the control-plane domain `ferun.run.fecode.dev`, the name the
instances' certificates on the nodes' port carry and the nodes check
(`CONTROL_PLANE_NAME` in configmap `feos-node`).

### Once: the secrets

    kubectl -n ferun create secret docker-registry oci-pull --docker-server=oci.fecore.dev \
      --docker-username='<registry user>' --docker-password='<registry password>'
    kubectl -n ferun create secret generic ferund-postgres --from-literal=password=$(openssl rand -hex 24)
    kubectl -n ferun create secret generic ferund \
      --from-literal=FERUN_DATABASE_URL="postgres://ferun:<password>@postgres.ferun.svc.cluster.local:5432/ferun?sslmode=disable" \
      --from-literal=FERUN_ADMIN_USERS="<user>:<password>" \
      --from-literal=FERUN_BOOTSTRAP_TOKEN=$(openssl rand -hex 32)

The bootstrap token becomes a token of the tenant `operator` while there is
no tenant: FeChat's `FERUN_TOKEN`. The admin users are a fallback: people
sign in to ferund's UI and admin API with their FeCode ID
(https://id.fecode.dev, the users FeCode, FeRun and FeChat share), and the
group `admins` there gets the admin surface. ferund's client at the IdP:

    ~/idp/create-client.sh ferun ferun      # secret oidc-client-ferun

### Deploy

    make -C deploy/k3s push-ferund           # build and push the image
    kubectl apply -f deploy/k3s/ferun/

Afterwards, a new image: `make -C deploy/k3s deploy-ferund`.

### Join the nodes

Every node joins once, with a token of its own, under its hostname;
afterwards its key and certificate are in `/var/lib/feos/node` on the node:

    args=""
    for n in debian13 debian13-vm-2 debian13-vm-3; do
      args="$args --from-literal=$n=$(kubectl -n ferun exec deploy/ferund -- /ferund join-token --ttl 2h)"
    done
    kubectl -n feos create secret generic feosd-join $args
    kubectl apply -f deploy/k3s/feos/feos.yaml     # CONTROL_PLANE: 10.43.70.70:7443
    kubectl -n feos rollout restart daemonset/feos-node

A node that lost its key joins again with a token pinned to its name
(`join-token --node <name>`).

## FeChat

See [`fechat/README.md`](../../fechat/README.md).

## DNS

run.fecode.dev and chat.fecode.dev point at the cluster's public address,
194.11.242.75, like the other names of the zone; cert-manager gets their
certificates over HTTP-01.
