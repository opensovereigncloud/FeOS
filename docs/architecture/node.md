The Node
========

What runs on a node and how. The decisions behind it:
[ADR 0003](../adr/0003-programs.md) (programs),
[ADR 0005](../adr/0005-firecracker.md) (Firecracker),
[ADR 0008](../adr/0008-suspend-and-restore.md) (suspend and restore),
[ADR 0009](../adr/0009-node-os-and-packages.md) (operating system and
packages), [ADR 0010](../adr/0010-pod-network.md) (the pods' network),
[ADR 0012](../adr/0012-security-groups.md) (security groups),
[ADR 0013](../adr/0013-traffic-records.md) (traffic records).

Parts that are decided but not built are marked as such.

## Processes

- **`feos-host-init`**: PID 1 when FeOS is the node's operating system. It
  sets the machine up, starts `feosd` and starts it again when it ends.
- **`feosd`**: the node agent: the API, the pod and microVM services, the container and
  image services. It runs under systemd or `feos-host-init`, never as
  PID 1.
- **`feosd --task-service`**: the container runtime (libcontainer) in a
  process of its own, started and restarted by `feosd`. Its pid is in
  `/run/feos/task-service.pid`.
- **The jailer and Firecracker**, one process per running pod.

## Machine setup: feos-host-init

`feos-host-init` moves its root off the initramfs, mounts `/proc`, `/sys`,
`/dev`, `/dev/pts`, `/run`, a tmpfs at `/var/lib/feos` and the cgroup
filesystem, configures SR-IOV VFs, configures the network
from router advertisements and DHCPv6, and keeps the clock with NTP from
the servers DHCPv6 names. It takes Ctrl-Alt-Del over (a SIGINT makes it
sync and reboot), reboots on `SIGRTMIN+5` and powers off on `SIGRTMIN+4`,
reaps orphans, and runs `/usr/sbin/feosd --self-upgrade --tcp [::]:1337`,
with `--pod-prefix` and the prefix DHCPv6 delegated to the node if it got
one, which it also routes as unreachable; it starts `feosd` again a second
after it ends. It serves no API and logs to `/dev/kmsg`.

On a distribution all of this is the operating system's
([ADR 0009](../adr/0009-node-os-and-packages.md)).

## feosd

`feosd [--tcp ADDRESS] [--self-upgrade] [--pod-prefix PREFIX] ...` sets
nothing of the machine up but the pods' network ([below](#the-pods-network)).

- It serves the API on `/run/feos/api.sock` and, with `--tcp`, on TCP,
  without authentication ([api.md](api.md)). If a group `feos` exists, it
  owns the socket, mode 0660: its members can do on the node what root
  can.
- Once its listeners are bound, it sends `READY=1` to `$NOTIFY_SOCKET` if
  systemd set one.
- `HostService.Reboot` and `Shutdown` send PID 1 `SIGRTMIN+5` and
  `SIGRTMIN+4`. `UpgradeFeosBinary` replaces the binary and runs it with
  the same pid only with `--self-upgrade`; without, the call fails with
  `FAILED_PRECONDITION`.
- It is the subreaper of its children. Pods and the task service outlive
  it: a `feosd` that starts again takes the running pods over and keeps the
  running task service.

## Directories

| Path | Holds |
|---|---|
| `/var/lib/feos/pods/<id>.pb` | a pod: its spec, its state and why, its addresses (a `feos.pod.v1.PodInfo`) |
| `/var/lib/feos/pods/next-ipv6-address` | the counter of the pods' IPv6 addresses: the next one handed out |
| `/var/lib/feos/pods/next-address` | where the next pod's NAT64 address is looked for in the pool |
| `/var/lib/feos/security-groups/<id>.pb` | a security group (a `feos.network.v1.SecurityGroup`) |
| `/var/lib/feos/vms.db` | the microVM records (SQLite) |
| `/var/lib/feos/vms/firecracker/<id>/` | a microVM's jail: `jail.json` and the chroot `root/` |
| `/var/lib/feos/images/` | pulled images, a `metadata.json` each |
| `/var/lib/feos/containers/` | the host's containers, a `container.json` each |
| `/var/lib/feos/upgrade/` | a downloaded binary before an upgrade |
| `/run/feos/` | the API sockets, the task service's pid, the runtime state of containers |
| `/sys/fs/bpf/feos/nat64/` | NAT64's pinned maps: the pods' IPv4 addresses and the counters |
| `/usr/lib/feos/firecracker/<version>/` | Firecracker and its jailer, a version each, side by side |
| `/usr/lib/feos/guest/<version>/` | the guest's `vmlinuz` and `initramfs`, a version each, side by side |

`/var/lib/feos` is a tmpfs when FeOS is the node's operating system and a
directory on disk on a distribution.

## A pod

A pod ([ADR 0004](../adr/0004-pods.md)) is a microVM
with the pod's ID, booted from the newest installed guest, and the
containers its spec names, which the guest runs with the names as their
IDs. Its spec gives vCPUs and memory, the containers (image, command,
environment) and whether it has a network. The microVM has a balloon with
free page reporting.

- **Create** records the pod as `PENDING` and brings it up in the
  background: create and start the microVM, wait for the guest to attach
  (60 s at most), then create (which pulls) and start the containers, in
  order. The pod is then `RUNNING`, or `FAILED` with the reason. Each step
  looks at what is there first, so a bring-up that a restart of `feosd`
  cut off is taken up again where it ended. A create with the ID of a pod
  that exists returns that pod if the spec is the same, and fails with
  `ALREADY_EXISTS` if not.
- **Stop** ends a bring-up, shuts the microVM down, or removes it if it
  does not go or is not running; **Start** brings a stopped or failed pod
  up anew. A pod has no disk yet: what its containers wrote is gone.
- **Suspend** and **Resume** suspend and resume the microVM, with
  `purge_guest`; the containers come back as they were.
- **Delete** ends a bring-up and removes the microVM and the record.
- A running pod whose microVM crashes is `FAILED`; one whose microVM stops
  is `STOPPED`.

What changes a pod runs under a lock of the pod's. `GetPod` asks the guest
for the containers' states while the pod runs.

## The pods' network

Every pod has a network unless its spec says `none`. Pods are IPv6 only
([ADR 0010](../adr/0010-pod-network.md)), their security groups decide
what they reach ([ADR 0012](../adr/0012-security-groups.md)), and their
traffic is recorded ([ADR 0013](../adr/0013-traffic-records.md)). The code
is `crates/services/pod-service/src/network*`, on top of
`network-service` (the groups and their compiler), `dns-service` (the
resolver), `nat64` and `traffic-service` (the records).

`feosd` takes these options:

| Option | Default | What it is |
|---|---|---|
| `--pod-prefix PREFIX` | none | the IPv6 prefix the network routes to the node, /1 to /120; a /64 is best |
| `--nat64 node\|network` | `node` | translate pods' IPv4 traffic on the node, or leave it to a NAT64 in the network |
| `--nat64-prefix PREFIX` | `64:ff9b::/96` | the NAT64 prefix, a /96, which DNS64 synthesizes under |
| `--nat64-pool PREFIX` | `10.200.0.0/16` | with `--nat64=node`, the IPv4 addresses that stand for pods; `.1` stands for the node |
| `--dns-address ADDRESS` | `fdfd:7b5:61d9::53` | the pods' resolver, the same on every node |
| `--dns-upstream ADDRESS` | Google's and Cloudflare's, IPv6 first | the upstreams of pods that name none (repeatable) |
| `--deny-prefix PREFIX` | none | what no pod reaches, IPv4 prefixes under the NAT64 prefix (repeatable) |

Without `--pod-prefix`, or when the network cannot be set up, `feosd`
logs the error and runs on: creating a pod with a network fails with
`FAILED_PRECONDITION` and the reason, and a kept pod with a network fails
when it boots or resumes. Pods without a network and the rest of the API
work as before.

### Devices and routes

At its start `feosd`:

1. removes the first version's network: the bridge `feos-pods` and the
   table `bridge feos` (a kernel without the bridge family, as
   `feos-host-init`'s, has none to remove);
2. turns IPv4 and IPv6 forwarding on; interfaces that take router
   advertisements (`accept_ra` 1) go on taking them (2);
3. routes the pod prefix as `unreachable`: a packet for a pod that is not
   here goes back as unreachable rather than out again;
4. gives the dummy device `feos-dns` the resolver's address, a /128
   without duplicate address detection, and binds the resolver's UDP and
   TCP sockets to it, port 53;
5. with `--nat64=node`, attaches NAT64 (below); with `--nat64=network`,
   removes it.

Before every boot and resume of a pod, `feosd` makes its TAP device,
`pod-<8 hex digits of the ID>`, a link of its own between the node and
the pod: no router advertisements, no address autoconfiguration and no
link-local address of the kernel's (set before it goes up), no answers to
ARP, up, `fe80::1/64` without duplicate address detection, and a route
for the pod's address, a /128, to it. The guest is pushed its address as
a /128, `fe80::1` as its gateway, the resolver and no IPv4
([guest.md](guest.md)); its MAC address is `06:00` and the 8 hex digits
of its ID. Deleting the pod deletes the TAP device, which takes the route
with it.

### Addresses

A pod gets an IPv6 address of the prefix when it is created, and keeps it
in its record (`PodInfo.ipv6_address`) while it is on the node. The next
address comes from a counter in `/var/lib/feos/pods/next-ipv6-address`,
from `::2` on, written before an address is handed out: no address is
handed out twice, not after a restart either. The counter goes past any
address a kept pod holds. With `--nat64=node` a pod also gets an IPv4
address of the pool (`PodInfo.ipv4_address`), from a cursor in
`/var/lib/feos/pods/next-address`, from `.2` on; a freed one comes round
last.

A pod kept from the first, IPv4 network gets an IPv6 address at the start
of `feosd`, and keeps its IPv4 address as its NAT64 address if it is in
the pool. Its microVM was made for that network: one that is stopped is
made anew at the next boot; one that runs or is suspended keeps the old
network until the pod is stopped and started.

### DNS

`feosd`'s resolver (`dns-service`) answers the pods it knows, by the
source address of a query, which the TAP pins, and refuses everyone
else. It asks the upstreams of the pod's spec (`PodNetwork.dns_upstreams`),
or `--dns-upstream`, in order. A create refuses an upstream that is not an
IP address, is the resolver itself, a loopback, link-local or multicast
address, in a denied prefix, the pod prefix, the NAT64 pool or one of the
node's addresses; one under the NAT64 prefix is asked as the IPv4 address
it stands for.

It is DNS64: an AAAA query for a name without AAAA records gets the
name's A records under the NAT64 prefix. When the pod prefix is a ULA
(`fc00::/7`), as in labs, every AAAA record counts as absent and every
name is reached through NAT64. It answers `ipv4only.arpa`, takes `ech`
and `ipv4hint` out of HTTPS records, and records every query it answers.

### NAT64

With `--nat64=node`, `feosd` attaches the translator of the crate `nat64`:
the dummy device `feos-nat64`, routes for the NAT64 prefix and the pool
into it, and an eBPF program on its egress that translates between a
pod's IPv6 address and its IPv4 address of the pool (SIIT, RFC 7915,
with an explicit mapping per pod) and hands the packet back to the node.
The program's maps are pinned in `/sys/fs/bpf/feos/nat64/` (`feosd`
mounts the bpf filesystem if it is not), so translation goes on while
`feosd` restarts; at its start `feosd` brings the mappings in line with
the pods' records. A pod is mapped before every boot and resume and
unmapped when it is deleted. No interface carries a pool address; the
table below masquerades the pool to the node's address.

The object `crates/nat64/bpf/nat64.bpf.o` is committed; `make nat64-bpf`
builds it from `crates/nat64-ebpf` ([development.md](../development.md)).

### The table

`feosd` writes one nftables table over netlink, `inet feos`. Its base
chains accept what they do not decide on, so that the node's other
traffic is not theirs:

| Chain | What it does |
|---|---|
| `prerouting` (filter, -150) | from a pod's TAP: drops packets whose source is not routed back out the TAP (`fib saddr . iif oif missing`), of either family, logged as `spoofed`, and router advertisements and redirects of both families |
| `input` | from pods takes neighbor solicitations and advertisements and DNS to the resolver's address over UDP and TCP, and drops the rest |
| `forward` | copies the first 4 packets of the TCP connections pods open to nflog group 3 (`ct direction original`, `ct original packets < 5`), of both families, before anything else; accepts what belongs to a connection; drops invalid packets from and to pods; then jumps to `from-pods` what comes from a TAP and to `to-pods` what goes to one |
| `from-pods`, `to-pods` | go through the verdict maps `pod-out` (by input interface) and `pod-in` (by output interface) to the pod's chains, and drop and log (`no-rule`) what a TAP the maps do not have sends or receives, such as one whose rules failed to be written |
| `out-<TAP>`, `in-<TAP>` | a pod's chains: drop and log (`deny`) what goes to or comes from a denied address, `return` what a rule of the pod's groups allows, drop and log (`no-rule`) the rest; `out-` of both families, `in-` of IPv6 alone, since nothing new comes in over IPv4 |
| `postrouting` (nat, 100) | masquerades the pods' IPv4 range, except into a pod's TAP |

A pod's chains end allowed traffic with `return`, not `accept`, which
would end the base chain: a packet between two pods passes the sender's
`out-` chain and the receiver's `in-` chain. The maps go to them
(`goto`), so their `return` leads back to `forward`. The sets are what
`network-service` compiles, each of one family: `deny4` and `deny6` (the
denied prefixes, the node's own addresses, link-local, loopback,
multicast and unspecified addresses), `internet4` and `internet6` (the
default group's), `g-<group>` (a group's members, of IPv6) and
`b-<hash>` (an address block without its exceptions, of its family). An
IPv4 block counts in rules out; rules in ignore it. ICMP rules are of
IPv4, ICMPv6 rules of IPv6. Drops are logged to nflog group 2, at most
10 a second per pod and reason.

`feosd` replaces the table at its start and whenever pods come or go or
rules change; when only members of groups or the node's addresses change,
it adds and removes set elements in one batch. It writes the rules again
when a group changes and every minute, which picks up new addresses of
the node.

Other tables still apply: where Docker or a firewall sets the `FORWARD`
policy to drop, pods reach nothing until it lets them through.

### Security groups

`feosd` keeps the groups it is sent (`feos.network.v1.SecurityGroupService`)
in `/var/lib/feos/security-groups/<id>.pb`. A pod's spec names its groups
(`PodNetwork.security_groups`); one that names none has the default
group: out to the internet, nothing in. A pod whose groups the node does
not have does not boot or resume: its bring-up fails, and the pod is
`FAILED` with a reason that names them; `StartPod` brings it up once they
are there, and `ResumePod` fails with `FAILED_PRECONDITION` until then. A
group that a pod names cannot be deleted.

### Traffic records

`feosd` records what its pods do on the network
(`feos.network.v1.TrafficService`, `feosctl traffic watch`):

- **flows** from connection tracking's events over ctnetlink, with
  `nf_conntrack_acct` and `nf_conntrack_timestamp` on, which `feosd` turns
  on once the table has loaded connection tracking;
- **drops** from nflog group 2, the reason from the rule's prefix;
- **server names** from the copies on nflog group 3: a TLS ClientHello's
  server name, or an HTTP request's Host;
- **DNS** queries from the resolver.

The collector's threads and the resolver feed one channel, from which
every call streams; a pod's queries to the resolver are left out of the
flows, which the DNS records cover. What is lost on the way is counted in
the next record's `missed`.

## A pod's microVM

The microVM layer under the pods (`crates/services/microvm-service`,
internal `feos.microvm.v1`) runs the microVMs; FeOS runs no VMs of their
own. A microVM's directory, `/var/lib/feos/vms/firecracker/<id>/root/`, is
the chroot its Firecracker runs in:

| File | What it is |
|---|---|
| `config.json` | the Firecracker configuration, written at creation |
| `fc.sock` | Firecracker's API socket |
| `v.sock` | the host end of the guest's vsock device; `v.sock_10001` is where the guest's uplink arrives |
| `console.in` | a FIFO, the serial console's input (Firecracker's stdin) |
| `serial.log` | the serial console's output since the last boot (Firecracker's stdout) |
| `vmm.log` | Firecracker's own log |
| `firecracker.pid`, `firecracker`, `dev/` | the jailer's |
| `vmlinuz`, `initramfs` | the installed guest's files, linked in |
| `snapshot/` | the snapshot of a suspended microVM: `vmstate` and `memory` |

`jail.json`, beside the chroot, records what a start links in and what a
deletion undoes.

### Lifecycle

- **Create** takes the newest installed guest and keeps that version: its
  jail links that version's files. It writes `config.json`; no process
  starts.
- **Start** runs the jailer, which starts Firecracker, which boots the
  microVM at once. The process ends when the guest reboots or shuts down:
  exit code 0 makes the microVM stopped, anything else crashed.
- **Shutdown** sends Ctrl-Alt-Del and waits for the process to end.
- **Suspend** writes a snapshot, ends the process and punches holes over
  the pages of zeroes. **Resume** loads the snapshot with the vCPUs
  stopped, then resumes them and removes the snapshot.
- **Delete** stops the microVM, removes what `jail.json` records, its TAP
  devices and its directory.

The layer records a change of state after the call that made it returns;
the pod service waits for it. A microVM has no data disks (the disk daemon
brings volumes) and no huge pages, which Firecracker backs neither an
initramfs nor a balloon with.

### Jail, user and cgroup

Firecracker runs in the jailer: chrooted into the microVM's directory, in
a mount namespace of its own, as a user of the microVM's own, which owns
its TAP devices. The layer creates TAP devices that do not exist and
deletes them with the microVM.

Every microVM has a cgroup of its own, `vms/<id>` below the cgroup `feosd` is
given, with `cpu.weight` 100 per vCPU and no caps: `/sys/fs/cgroup/vms/<id>`
under `feos-host-init`, `/sys/fs/cgroup/system.slice/feosd.service/vms/<id>`
under systemd. A cgroup whose children use controllers may hold no
processes, so `feosd` moves into the leaf `feosd` of its cgroup, where
systemd starts it already with `DelegateSubgroup=feosd`. `feosd` creates
the microVM's cgroup and enables `cpu` and `memory` in its own cgroup and
in `vms/`, never above; the jailer only moves Firecracker into it.

### The guest

The layer listens on `v.sock_10001` from the microVM's creation to its
deletion and again after a restart of `feosd`. When the guest attaches
(after a boot, or when its call was lost) and after every resume, it
pushes the wall time and, if the pod has an address, its network
configuration through the guest's API. After a resume it pings the guest
once instead of waiting for the uplink.

### Balloon

A pod's microVM has a virtio balloon with free page reporting: the guest
hands the memory it frees back. A suspend first asks the guest to drop
its caches and compact its memory, and waits until the VMM stops
shrinking.

## Firecracker

Firecracker versions are installed side by side in
`/usr/lib/feos/firecracker/<version>/`, as the packages lay them out
([ADR 0009](../adr/0009-node-os-and-packages.md)). A boot runs the newest
version that has `firecracker` and `jailer`, or, where none is installed,
the ones in `PATH`; `jail.json` records which. A restore runs what the
snapshot's boot ran, and fails with `FAILED_PRECONDITION` if that is not
installed any more. The tests download Firecracker v1.17.0, pinned by
sha256.

## Packages

On Debian 13 and Garden Linux, `feosd` and what it runs come as `.deb`
packages from https://apt.fecode.dev
([ADR 0009](../adr/0009-node-os-and-packages.md)):

| Package | Contents |
|---|---|
| `feosd` | `/usr/sbin/feosd`, `feosd.service`, the group `feos` (sysusers), `/etc/default/feosd`, `/etc/apt/apt.conf.d/50feosd` |
| `feosctl` | `/usr/bin/feosctl` |
| `feos-firecracker-<version>` | `/usr/lib/feos/firecracker/<version>/{firecracker,jailer}` |
| `feos-guest-<version>` | `/usr/lib/feos/guest/<version>/{vmlinuz,initramfs}` |
| `feos-firecracker`, `feos-guest` | nothing; they depend on the newest versioned package |

`feosd` depends on `feos-firecracker` and `feos-guest`, which depend on the
newest versioned packages, as `linux-image-amd64` does on the newest
kernel: an upgrade installs a new Firecracker or guest beside the old
ones, which the pods booted with them keep using. `50feosd` keeps every
version from `apt autoremove`; a version nothing uses is removed with
`apt remove`.

`feosd.service` is `Type=notify`, with `Delegate=yes`,
`DelegateSubgroup=feosd` and `KillMode=process`: a restart or an upgrade
stops `feosd` alone, and the next one takes the pods over.
`StateDirectory=feos` makes `/var/lib/feos` a directory on disk.
`FEOSD_ARGS` in `/etc/default/feosd` passes options, such as `--tcp`
and the pods' network's, of which the file has a description; it gives
no `--pod-prefix`, so pods with a network fail until one is set.
Installing the package enables the unit the first time and starts it, or
restarts it after an upgrade, whenever it is enabled; removing the package
stops the unit; purging it disables the unit. `/var/lib/feos` stays.

The repository has one suite, `stable`, with the component `main`, and is
signed by its own key:

    curl -fsSL https://apt.fecode.dev/feos.gpg -o /usr/share/keyrings/feos.gpg
    echo 'deb [signed-by=/usr/share/keyrings/feos.gpg] https://apt.fecode.dev stable main' \
      > /etc/apt/sources.list.d/feos.list
    apt-get update && apt-get install feosd feosctl

`make publish-apt` builds the packages of `HEAD` as
`<version>~dev.<UTC time>.<commit>`, below the release they lead to, and
`hack/packages/publish-apt` uploads them to the bucket behind the
repository, keeping what was published before.
