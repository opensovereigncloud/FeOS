# ADR 0003: The node's programs and where they run

**Status:** Accepted · **Date:** 2026-10-14

## Context

FeOS was one binary, `feos`, in two profiles chosen by cargo features: the
host profile was PID 1 of a bare-metal hypervisor, the guest profile PID 1
in a microVM. The host profile also ran as an ordinary process for
development, told apart by `getpid() == 1` throughout its startup.

The sandbox system adds a manager on every node: about 20,000 lines of Go
that run the microVMs, keep their records, take commands from the control
plane, isolate the network and run per-sandbox proxies. Its VM half has
moved into FeOS already, as the microVM layer under the pods. An earlier
assessment of the merge rejected moving the rest into FeOS, because it
would have had to live inside PID 1.

Nodes are to run FeOS as their operating system or a general-purpose
distribution under systemd (ADR 0009).

## Decision

### A binary of its own where the code differs

Each binary is named after where it runs. Where only the role differs,
the binary chooses at runtime.

| Binary | Built from | Runs |
|---|---|---|
| `feos-host-init` | `crates/feos-host-init` | as PID 1 when FeOS is the node's operating system |
| `feosd` | `crates/feos`, feature `host` | as the node agent, under `feos-host-init` or systemd, never as PID 1 |
| `feos-guest` | `crates/feos`, feature `guest` | as PID 1 in a pod's microVM (ADR 0007) |
| `feosctl` | `crates/feosctl` | as the client, anywhere |

- `feos-host-init` sets the machine up: mounts, the network over DHCPv6
  and router advertisements, NTP, SR-IOV VFs. It takes
  Ctrl-Alt-Del over, reaps orphans, starts `feosd` and starts it again
  when it ends. It serves no API and logs to `/dev/kmsg`. It reboots and
  powers off on systemd's signals (`SIGRTMIN+5`, `SIGRTMIN+4`), so `feosd`
  asks for either the same way under both.
- `feosd` and `feos-guest` are two `[[bin]]` targets of `crates/feos` with
  `required-features`. The features select the services and stay additive;
  the startup both share lives in the crate's library.
- Both start their task service by running themselves again
  (`feosd --task-service`, `feos-guest --task-service`), so the two
  processes are always of one version.
- No binary is called `feos`. FeOS is the project's name; it stays in the
  paths (`/run/feos`, `/var/lib/feos`) and in the proto packages.

### The manager's node half becomes feosd

There is no manager next to `feosd`. The sandbox system's manager is split
along the line of ADR 0001: what concerns pods is ported into `feosd`'s
crates, in Rust, and what concerns sandboxes goes to the control plane
(ADR 0004).

- `feosd` runs pods (ADR 0004), takes the control plane's calls
  (ADR 0006), isolates the pods' network, runs their proxies and port
  forwards, and suspends and restores them (ADR 0008).
- The control plane gets the sandbox as an object, the agent's MCP tools
  mapped onto a pod's tool and exec calls, and the policies that pause and
  hibernate. `feosd` reports what they need and decides none of it.

Since `feosd` is not PID 1, a fault in the ported code restarts `feosd`,
and the pods keep running.

## Alternatives considered

- **Two profiles of one binary named `feos`**, as FeOS had. Two files of
  one name did different things and needed a profile of their own
  (`release-guest`) not to overwrite each other. On a distribution the
  host profile carried the bare-metal setup (DHCPv6, raw sockets, NTP); on
  bare metal PID 1 carried the whole node agent.
- **One binary for host and guest that selects its role at runtime.**
  Every pod would carry the node agent's code and dependencies (the VM
  service, SQLite): more to attack in every pod.
- **The task service as a binary of its own.** It would keep libcontainer
  out of the main process, but two files would have to be upgraded
  together and kept at one version; running itself again gives that for
  free.
- **A manager next to `feosd`** that speaks `feos.vm.v1` to `feosd` and the
  guest's API through `vsock_path`. Two programs on every node to version
  against each other, and the guest's API a public one.
- **The whole manager in `feosd`**, sandboxes included. `feosd` would know
  one of the two products, and CaaS would need its own semantics next to
  them in the node agent.

## Consequences

- The code that sets the machine up leaves `feosd` for `feos-host-init`,
  and so do pnet, dhcproto and sntpc. The `release-guest` profile goes.
- `feosd` becomes FeOS's largest program.

## Open questions

- **The per-pod proxies**: binaries of their own, or `feosd` running
  itself again, as for the task service.
- **The order of the port.** Suggested, each step usable on its own, the
  Go manager doing what has not moved yet:
  1. pods on top of the microVM layer (ADR 0004), done;
  2. the Valkey transport, events and telemetry (ADR 0006);
  3. network isolation and the guest's DNS (ADR 0010);
  4. the Kubernetes and SSH proxies and port forwarding;
  5. hibernation, with the disk daemon (ADR 0008).
