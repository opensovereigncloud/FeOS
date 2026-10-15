# ADR 0014: feos-authn holds a pod's identity and credentials

**Status:** Proposed · **Date:** 2026-10-15

## Context

Code in a pod needs access: an agent deploys to a Kubernetes cluster or
pushes over SSH, a service talks to its database or to another service.
Every credential inside a pod counts as compromised. A pod runs untrusted
code (ADR 0001), and a hijacked agent sends out whatever it finds. A
credential it finds stays valid from anywhere, for as long as it lives.

The sandbox system kept two kinds of credentials out of its sandboxes,
each with a jailed process of its own per sandbox on the host:

- a **Kubernetes proxy**: the sandbox sees a plaintext Kubernetes API on
  a loopback port, and the proxy adds the token or client certificate on
  the way out;
- an **SSH agent**: the sandbox sees an agent socket, and the agent signs
  only logins to servers that prove they hold a bound host key, as the
  bound user, with the bound key.

`feos-guest` already pipes both to the host, over vsock ports 10002 and
10003 (ADR 0007). Nothing on the host answers yet.

A third kind comes from FeID, formerly FeTLS (ADR 0015): **managed mTLS
identities**. Every workload gets an X.509 certificate with a SPIFFE ID,
and the private key stays on the host. The workload runs an ordinary
TLS 1.3 handshake, and its TLS stack delegates the one private key
operation, the CertificateVerify signature, to the host over the
protocol feid/1. FeID's libraries for Go, Rust, Python and TypeScript do
that. A database that authenticates by certificate (PostgreSQL, MariaDB,
Valkey, Kafka, NATS) then sees the workload itself, with no password
anywhere and no proxy in the data path.

All three follow one pattern: the material stays on the host, the pod
gets the use of it, never the bytes. In all three, the socket a
connection arrives on tells which pod is asking, never what the pod
sends. And all three need the same things: bindings per pod, a jail,
audit records and a way for secrets to reach the node.

## Decision

### One program, one process per running pod

- `feos-authn` is a fifth program (ADR 0003): a small static binary of
  its own, shipped in the `feosd` package. It serves all three kinds:
  Kubernetes, SSH and mTLS, the last also through TLS bindings for
  applications without FeID.
- `feosd` starts one `feos-authn` per pod that runs with an identity or
  credentials. It starts it with the microVM on create, resume and
  restore, and stops it when the run ends. A suspended pod has no
  process, and no key or secret of it is on any node.
- The process holds one pod's material and nothing else. There is no
  lookup from a connection to a tenant, and a bug in a parser costs one
  pod's credentials, not the node's.
- It survives a restart of `feosd` with the microVM. `feosd` adopts it
  again by its pid and start time, as it adopts Firecracker. The protocol
  on its control socket is versioned, so that an upgraded `feosd` adopts
  the processes that the version before it started.

### One vsock port, the socket names the pod

- The guest pipes every connection to vsock port 10002. A one-line
  preamble selects what the connection is for: `kube <port>`,
  `tls <port>`, `ssh` or `feid`. Port 10003 goes.
- The preamble carries no authority. Identity comes from the host-side
  socket the stream arrives on, which belongs to the pod's microVM. A
  process in the pod that dials the vsock port itself gains nothing that
  the guest's sockets do not already give it.
- In the guest, Kubernetes and TLS bindings are loopback ports, as
  Kubernetes bindings were. SSH is `/run/feos/ssh-agent.sock` with
  `SSH_AUTH_SOCK`. The identity is `/run/feos/feid.sock` with
  `FEID_SOCK`, where FeID's libraries look without being told
  (ADR 0015), and the trust bundle is `/run/feos/ca.pem`. Each exists only
  while the pod has what it serves.
- A binding's port is not forwardable. That rule stays, and covers TLS
  bindings too.

### What it answers

- **Kubernetes:** HTTP/1.1, one upstream connection per guest connection.
  After an upgrade, both sides are a pipe, so `exec`, `attach`,
  `port-forward` and watches pass through. The proxy resolves the
  binding's server, checks every address it would dial, and dials the
  address it checked. It drops `Authorization`, `Impersonate-*` and
  `Proxy-*` from the request and adds the credential. A denied
  destination answers 403, an unreachable one 502. A request never
  leaves without the credential. What the credential may do is RBAC in
  the target cluster.
- **SSH:** the agent answers four requests and refuses everything else:
  - list the identities;
  - `session-bind@openssh.com`, without forwarding;
  - sign, but only a `publickey` user authentication for a bound session
    whose host key, user and key match one binding;
  - the `query` extension.

  A client without `session-bind` gets nothing. RSA signs with SHA-2 only.
- **mTLS (feid/1):** the guest's TLS stack fetches the pod's
  certificate chain and the trust bundle, and asks for a signature. The
  signer signs only input of the exact shape of a TLS 1.3 CertificateVerify
  for client or server. Keys are Ed25519, so the input is unhashed and
  can be checked. It is not a signing oracle. `feos-authn` speaks feid/1
  through FeID's protocol crate, so that the check exists once.
- **TLS bindings**, for applications without FeID: a binding names an
  upstream, a host and a port, and its CA, or the WebPKI when it has none.
  The application connects in plaintext to the binding's loopback port;
  `feos-authn` checks and dials the upstream as for Kubernetes, runs
  TLS 1.3 with the pod's certificate as its client certificate, and pipes
  both directions. `psql` or an application that cannot be changed gets
  the pod's identity this way. The plaintext passes through the host
  process, which FeID's libraries avoid.
- Every signature and every request is audited **before** it is answered.
  A signature whose record cannot be written is not given.

### The pod's identity

- A pod's spec names its identity: a SPIFFE ID that the control plane,
  the registration authority, chose. `feosd` does not choose identities.
  The subject CN follows the ID by FeID's mapping: the trust domain
  and path, cut at 55 characters plus a hash of the whole ID, so that
  databases that map on the CN see it. The names under which the pod's
  service is reachable are DNS names in the certificate (ADR 0015).
- `feos-authn` makes the pod's key pair in its own memory, at every
  start. Its certificate signing request goes to the control plane,
  which issues a short-lived certificate. The private key never exists
  outside the process that signs with it: not on a disk, not in
  `feosd`, not in Valkey and not in the control plane.
- It renews the certificate at three quarters of its lifetime. An issuer
  that cannot be reached is retried; until the certificate expires, the
  old one serves. After that, the signer refuses, loudly.
- Until a run's first certificate arrives, a request to sign waits a few
  seconds, then gets a refusal that FeID's libraries retry.
- The trust bundle is public and comes to `feosd` with the pod.

### Secrets reach the process sealed to it

- Kubernetes, TLS and SSH bindings are sets: the spec names all of a pod's
  bindings, and a change states the whole set again. What it does not
  name is gone.
- At start, `feos-authn` makes a second key pair, for HPKE (RFC 9180),
  and its public half goes out with the certificate signing request. The
  control plane seals the bindings to it. `feosd` and Valkey relay
  ciphertext. Only the pod's own process can open them, and it keeps
  them in memory only.
- `feosctl` does the same for a node without a control plane: it asks
  `feosd` for the process's public key and seals the bindings itself.
- A new run has a new key, so the control plane seals again after every
  start, resume and restore. A run never starts with bindings it cannot
  open: a pod whose bindings do not arrive does not start.

### The jail

- Each process runs as a user ID of its own, one per running pod, so
  that two pods' processes cannot signal or trace each other.
- It runs in a mount namespace with an empty root, without capabilities,
  with `no_new_privs`, a seccomp allowlist derived from its tests, and
  rlimits. Its only descriptors are its two inherited sockets, the vsock
  listener and the control socket to `feosd`, and stderr.
- It runs in a network namespace that reaches only the internet: a veth
  to the node, and blackhole routes for every prefix of `--deny-prefix`
  (ADR 0010) and for the node's own networks. A proxy that a parser bug
  has taken over has no way into the node network or the platform's own
  cluster.

### Audit

- `feos-authn` reports every Kubernetes request (binding, host, method,
  path, status, bytes), every TLS binding's connection (binding, host,
  bytes), every SSH signature (binding, host key, user), every TLS
  signature (identity, context) and the handshake reports of FeID's
  libraries to `feosd` over its control socket. Refusals are reported
  with their reason.
- `feosd` publishes them as the pod's events (ADR 0006). The control
  plane can join TLS signatures to the flows and server names of
  ADR 0013, as FeTLS's signer joined signatures, library reports and
  wire observations.

## Alternatives considered

- **One process per kind and pod**, as the sandbox system ran them. The
  SSH agent there sat in an empty network namespace, so that a bug in it
  cost one kind of credential, not two. It halves the isolation for
  little: a process that has been taken over can always write back to the
  guest over its vsock stream, so the empty namespace keeps little in.
  The tenant boundary, one pod per process, stays.
- **One daemon per node**, as FeTLS's signerd and its rule "no
  sidecars" have it. Fewer processes, but every tenant's keys in one
  address space and a lookup on every connection. A per-pod process on
  the host is no sidecar: nothing runs in the pod.
- **`feosd` running itself again**, as for the task service. It needs no
  new program, but the process would carry all of `feosd`'s code, and
  its seccomp allowlist would be larger.
- **Inside `feosd`.** It would parse hostile HTTP and SSH in the
  privileged process that owns every microVM on the node.
- **The control plane makes the keys and sends them with the pod**,
  as FeTLS's issuer did. It needs one message fewer at start, but the
  key would exist outside the process that uses it.
- **Bindings over Valkey in plaintext, or pushed directly from the
  control plane to the node**, as the sandbox system did. The first
  shows them to everything that reads the bus. The second needs a
  listener on the node (ADR 0006 has none), and the plaintext would
  still pass through `feosd`.
- **A TLS-intercepting proxy with a CA per pod**, for any destination
  the pod chooses. Applications would need neither a library nor a
  binding, but the platform would sit in the plaintext of every
  connection, and the CA would have to be baked into the pod. A TLS
  binding sits in the plaintext only of the upstreams it names.

## Consequences

- The pod's spec gains an identity and its sealed bindings, and the pod
  API gains a way to set the bindings of a running pod. Preflights,
  which check that a binding works before any pod has it, are the
  control plane's.
- Starting or resuming a pod with an identity or credentials costs a
  round trip to the control plane: the request and its certificate, the
  public key and its sealed bindings. A pod without them starts as
  before.
- The control plane becomes an issuer. Behind its interface may sit an
  issuer of its own or SPIRE; the node does not care.
- The kernel's network on the node gains one namespace for the
  processes. The guest replaces port 10003 with a preamble on 10002 and
  gains the FeID socket, the trust bundle and TLS bindings.
- Offloading protects the bytes, not the authority. A Kubernetes
  credential that may read secrets gives the agent a real token in one
  command. A pod that has been taken over can have signatures made for
  new connections while it runs. Narrow roles, short lifetimes, rate
  limits and the audit are the boundary.
- An application uses its identity through FeID's libraries, end to
  end, or through a TLS binding, without code. Kubernetes and SSH need
  nothing in the pod.
- Credentials supported first: bearer tokens and client certificates for
  Kubernetes, Ed25519, ECDSA and RSA keys of 2048 bits or more for SSH.
  Not supported: exec plugins, cloud auth providers, OIDC with refresh.

## Open questions

- The node's own identity and its attestation: a join token first, the
  node's TPM later. With it, `feosd` could authenticate to Valkey and the
  control plane by mTLS, too.
- The trust domain and the layout of the IDs: tenant and workload, and
  what a sandbox is called.
- Whether the Kubernetes proxy dials from the node's address, or from
  the pod's, so that a cluster's firewall could tell pods apart.
- How many signatures a pod may ask for per second.
- Whether TLS bindings get an inbound side, which terminates mTLS for a
  server in the pod that has no FeID.
