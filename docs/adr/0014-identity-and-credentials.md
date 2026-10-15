# ADR 0014: A pod's identity and credentials stay on the host

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

A third kind comes from **FeTLS** (github.com/MalteJ/FeTLS): managed
mTLS identities. Every workload gets an X.509 certificate with a SPIFFE
ID, and the private key stays on the host. The workload runs an ordinary
TLS 1.3 handshake, and its TLS stack delegates the one private key
operation, the CertificateVerify signature, to a signer on the host. A
database that authenticates by certificate (PostgreSQL, MariaDB, Valkey,
Kafka, NATS) then sees the workload itself, with no password anywhere
and no proxy in the data path. FeTLS consists of:

- **fetls/1**, the protocol between a workload's TLS stack and the
  signer: versioned NDJSON, negotiated at hello, additive within a
  version. The signer signs only TLS 1.3 CertificateVerify inputs.
- **Libraries** for Go, Rust, Python and TypeScript. They fetch the
  workload's certificate chain and the trust bundle at each handshake,
  delegate the signature, retry, and report each handshake.
- **signerd**, a daemon per node in Go, with an issuer interface, a
  rotation cache and an audit log. It tells workloads apart by peercred
  for containers, by the CID for Kata, by the vsock socket for
  Firecracker.
- A mapping from SPIFFE IDs to subject CNs, which databases map on.
- **fecaas**, a CaaS control plane of CRDs on k3s with Kata, and
  **wired** and **agility**, an eBPF observer of handshakes and an
  application that evaluates the crypto it sees.

FeTLS was built for several runtimes and for a platform that did not
exist yet. An application had to be told where the signer is
(`SECCONT_SOCK`, or `vsock:2:5000` in a microVM), and named its peers by
SPIFFE ID. FeOS has since decided what FeTLS left open: pods are
microVMs (ADR 0004), CaaS runs on them without Kubernetes (ADR 0001),
and traffic is recorded without eBPF (ADR 0013). Applications deployed
on FeOS are to use FeTLS's libraries, and that has to work without
ceremony.

All three kinds follow one pattern: the material stays on the host, the
pod gets the use of it, never the bytes. In all three, the socket a
connection arrives on tells which pod is asking, never what the pod
sends. And all three need the same things: bindings per pod, a jail,
audit records and a way for secrets to reach the node. TLS is not the
last form: applications will want tokens too, JWTs and OAuth and OIDC,
with the secrets kept on the host.

## Decision

### What a pod gets

- **An identity**: a certificate with the pod's SPIFFE ID, whose private
  key stays on the host.
- **FeID**, formerly FeTLS: libraries with which an application uses
  that identity for mTLS end to end, from its own sockets.
- **TLS bindings**, for applications without FeID: a loopback port whose
  connections the host carries to a named upstream over mTLS with the
  pod's identity.
- **Kubernetes bindings**: a loopback port with a plaintext Kubernetes
  API; the host adds the credential.
- **SSH bindings**: an agent socket; the host signs bound logins.

The host side of all of them is `feos-authn`. The application side,
where there is one, is FeID.

### feos-authn: one process per running pod

- `feos-authn` is a fifth program (ADR 0003): a small static binary of
  its own, shipped in the `feosd` package.
- `feosd` starts one `feos-authn` per pod that runs with an identity or
  bindings. It starts it with the microVM on create, resume and restore,
  and stops it when the run ends. A suspended pod has no process, and no
  key or secret of it is on any node.
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
- In the guest, Kubernetes and TLS bindings are loopback ports. SSH is
  `/run/feos/ssh-agent.sock` with `SSH_AUTH_SOCK`. FeID is
  `/run/feos/feid.sock` with `FEID_SOCK`, and the trust bundle is
  `/run/feos/ca.pem`. Each exists only while the pod has what it serves.
- A binding's port is not forwardable.

### What feos-authn answers

- **FeID (feid/1):** the guest's TLS stack fetches the pod's certificate
  chain and the trust bundle, and asks for a signature. The signer signs
  only input of the exact shape of a TLS 1.3 CertificateVerify, for
  client or server. Keys are Ed25519, so the input is unhashed and can
  be checked. It is not a signing oracle.
- **TLS bindings:** a binding names an upstream, a host and a port, and
  its CA, or the WebPKI when it has none. `feos-authn` checks and dials
  the upstream as for Kubernetes, runs TLS 1.3 with the pod's
  certificate as its client certificate, and pipes both directions.
  `psql` or an application that cannot be changed gets the pod's
  identity this way. The plaintext passes through the host process,
  which FeID avoids.
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
- Every signature and every request is audited **before** it is answered.
  A signature whose record cannot be written is not given.

### The pod's identity

- A pod's spec names its identity: a SPIFFE ID that the control plane,
  the registration authority, chose. `feosd` does not choose identities,
  and the application never names its own.
- The certificate carries the ID as a URI name and, as DNS names, the
  names under which the pod's service is reachable. The subject CN
  follows the ID by FeTLS's mapping: the trust domain and path, cut at
  55 characters plus a hash of the whole ID, so that databases that map
  on the CN see it.
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

- TLS, Kubernetes and SSH bindings are sets: the spec names all of a
  pod's bindings, and a change states the whole set again. What it does
  not name is gone.
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

- `feos-authn` reports to `feosd` over its control socket: every
  Kubernetes request (binding, host, method, path, status, bytes), every
  TLS binding's connection (binding, host, bytes), every SSH signature
  (binding, host key, user), every TLS signature (identity, context) and
  the handshake reports of FeID's libraries. Refusals are reported with
  their reason.
- `feosd` publishes them as the pod's events (ADR 0006). The control
  plane can join TLS signatures to the flows and server names of
  ADR 0013, as FeTLS's signer joined signatures, library reports and
  wire observations.

### FeID, the application side

- FeTLS is renamed **FeID**: TLS is one form of the identity it offers,
  and the tokens to come are others. FeID is the SDK with which
  applications on FeOS use their pod's identity, and FeOS is its
  platform: it follows FeOS's decisions.
- **Nothing to configure.** The libraries find the signer at
  `FEID_SOCK`, else at `/run/feos/feid.sock`. Creating a client without
  configuration works in every pod that has an identity. A pod without
  one has no socket, and the library fails at start and says why: no
  identity, not a broken platform.
- **Peers by name.** A client dials a service by its name and checks the
  server by that name, as any TLS client does. Pinning a SPIFFE ID stays
  an option. A server reads the client's SPIFFE ID and authorizes on it.
  A client without FeID checks a server with FeID against the platform
  CA, by name. Towards the internet, the libraries trust the WebPKI and
  the platform CA together.
- **Development without FeOS.** `feosctl` runs a development signer:
  feid/1 on a Unix socket, with a CA made for the run and an identity
  named on the command line. The same application runs on a laptop and
  on FeOS. The libraries keep their test kits.
- **The protocol** is fetls/1 as it is, renamed feid/1. New operations
  are added to it, as its rules allow; libraries in images built months
  before a node's upgrade keep working with it, and a new library with
  an old node. `handshake_report` stays. `wire_event`, which the host's
  observer sent, goes: ADR 0013 has no such observer. TLS 1.3 only,
  Ed25519 only.
- The protocol's server side (framing, negotiation, the CertificateVerify
  check, the error codes and the golden frames) is a Rust crate,
  `feid-proto`. `feos-authn` uses it, and so does the Rust library for
  its client side. The check exists once.

### Where the code comes from

- The Kubernetes proxy and the SSH agent are the sandbox system's, ported
  into `feos-authn`.
- FeTLS moves into this repository (ADR 0002), without its history,
  which has no authors beyond test identities; the commit names its
  source commit. `feid-proto` and the Rust library `feid` go under
  `crates/`, the Go, Python and TypeScript libraries under a top-level
  directory for each language, the protocol into `docs/architecture/`.
- The libraries are called `feid`. They are published from here to
  crates.io, as a Go module, to PyPI and to npm, and versioned on their
  own, not with FeOS.
- signerd does not move: `feos-authn` is the host side, and attribution
  by peercred and by Kata's CID and the vsock addresses in the libraries
  go with it. fecaas does not move: CaaS is FeOS's own (ADR 0001). wired
  does not move (ADR 0013). agility stays out; whether it reads the
  records of ADR 0013 and `feos-authn`'s events is decided later. The
  FeTLS repository is archived once FeID is here.

## Alternatives considered

- **One process per kind and pod**, as the sandbox system ran them. The
  SSH agent there sat in an empty network namespace, so that a bug in it
  cost one kind of credential, not two. It halves the isolation for
  little: a process that has been taken over can always write back to the
  guest over its vsock stream, so the empty namespace keeps little in.
  The tenant boundary, one pod per process, stays.
- **One daemon per node**, as FeTLS's signerd and its rule "no sidecars"
  have it. Fewer processes, but every tenant's keys in one address space
  and a lookup on every connection. A per-pod process on the host is no
  sidecar: nothing runs in the pod. signerd, in Go, could not be linked
  into a Rust program either.
- **`feosd` running itself again**, as for the task service. It needs no
  new program, but the process would carry all of `feosd`'s code, and
  its seccomp allowlist would be larger.
- **Inside `feosd`.** It would parse hostile HTTP and SSH in the
  privileged process that owns every microVM on the node.
- **The control plane makes the keys and sends them with the pod**, as
  FeTLS's issuer did. It needs one message fewer at start, but the key
  would exist outside the process that uses it.
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
- **FeTLS as a repository of its own that FeOS uses.** A change across
  the protocol would be a release on both sides (ADR 0002), and the
  libraries' conformance tests would run against a copy of the signer,
  not against `feos-authn`.
- **FeTLS gains Kubernetes and SSH.** They share only the pattern with
  it. What makes them work is FeOS's: attribution by the pod's socket,
  the pod's lifecycle, the bindings and the jail. FeTLS would need a
  node model of its own, for runtimes that FeOS does not have.
- **Keeping the name FeTLS.** It is known, but would name only the first
  of the forms.
- **Peers only by SPIFFE ID**, as FeTLS had it. Every client would have
  to know the IDs of its peers, and clients without FeID could not check
  a server.
- **protobuf or gRPC for feid/1**, like FeOS's APIs. It would put a
  protobuf or HTTP/2 runtime into every application for a handful of
  messages; FeTLS chose NDJSON for that reason.

## Consequences

- An application on FeOS adds a FeID library and creates a client. It
  configures nothing, holds no key, and its database sees its SPIFFE ID.
  An application without FeID gets the same identity through a TLS
  binding. Kubernetes and SSH need nothing in the pod.
- The pod's spec gains an identity and its sealed bindings, and the pod
  API gains a way to set the bindings of a running pod. Preflights,
  which check that a binding works before any pod has it, are the
  control plane's.
- Starting or resuming a pod with an identity or bindings costs a round
  trip to the control plane: the request and its certificate, the public
  key and its sealed bindings. A pod without them starts as before.
- The control plane becomes an issuer, and has to know a service's DNS
  names, which is part of the CaaS API. Behind its issuer interface may
  sit an issuer of its own or SPIRE; the node does not care.
- The node gains one network namespace for the processes. The guest
  replaces port 10003 with a preamble on 10002, and gains the FeID
  socket, the trust bundle and TLS bindings.
- FeTLS's libraries change their names and API: their defaults, and the
  vsock form goes. FeTLS's demo deployments stop working.
- Four libraries are released from this repository. The CI builds and
  tests Go, Python and TypeScript, and runs each library against
  `feos-authn`.
- Offloading protects the bytes, not the authority. A Kubernetes
  credential that may read secrets gives the agent a real token in one
  command. A pod that has been taken over can have signatures made for
  new connections while it runs. Narrow roles, short lifetimes, rate
  limits and the audit are the boundary.
- A peer that cannot do Ed25519 cannot check a FeID server: old Java,
  browsers. Browsers reach services through an ingress with public
  certificates.
- Credentials supported first: bearer tokens and client certificates for
  Kubernetes, Ed25519, ECDSA and RSA keys of 2048 bits or more for SSH.
  Not supported: exec plugins, cloud auth providers, OIDC with refresh.

## Open questions

- **JWTs.** A `get_jwt(audience)` operation for short-lived JWT-SVIDs,
  with OIDC discovery published by the control plane, would give pods
  HTTP APIs and the clouds' workload identity federation without a cloud
  key in the pod. It puts a bearer token into the pod, if a short-lived
  one bound to its audience.
- **OAuth and OIDC.** A pod as an OAuth client of another service, its
  client secret and refresh token kept by `feos-authn`, which adds the
  access token as a Kubernetes binding adds its credential. A pod's web
  application whose OIDC login the platform runs. Which of these FeID
  offers, and which are bindings that need no library.
- The node's own identity and its attestation: a join token first, the
  node's TPM later. With it, `feosd` could authenticate to Valkey and the
  control plane by mTLS, too.
- The trust domain and the layout of the IDs: tenant and workload, and
  what a sandbox is called.
- Whether the Kubernetes proxy and TLS bindings dial from the node's
  address, or from the pod's, so that an upstream's firewall could tell
  pods apart.
- Whether TLS bindings get an inbound side, which terminates mTLS for a
  server in the pod that has no FeID.
- How many signatures a pod may ask for per second.
- ECDSA P-256 for peers without Ed25519. The protocol has a `hash` field
  reserved for it.
- Libraries for Java and .NET.
- agility.
