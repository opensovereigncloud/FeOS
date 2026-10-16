# ADR 0014: A pod's identity and credentials stay on the host

**Status:** Proposed · **Date:** 2026-10-15

## Context

Code in a pod needs access: an agent deploys to a Kubernetes cluster or
pushes over SSH, a service talks to its database or to another service.
Every credential inside a pod counts as compromised. A pod runs untrusted
code (ADR 0001), and a hijacked agent sends out whatever it finds. A
credential it finds stays valid from anywhere, for as long as it lives.

So the material stays on the host, and the pod gets the use of it, never
the bytes. The host knows which pod is asking from the socket a
connection arrives on, never from what the pod sends. Three forms:

- **An identity.** The pod gets an X.509 certificate with a SPIFFE ID,
  and the private key stays on the host. The application runs an
  ordinary TLS 1.3 handshake from its own socket, and its TLS stack
  delegates the one private-key operation, the CertificateVerify
  signature, to a signer on the host. A database that authenticates by
  certificate (PostgreSQL, MariaDB, Valkey, Kafka, NATS) sees the pod
  itself, with no password anywhere and no proxy in the data path. A
  prototype, FeTLS, validated this: the delegated signer over vsock from
  Firecracker guests, a signer that signs only CertificateVerify inputs,
  libraries for Go and Rust. Its protocol and code are the start.
- **A Kubernetes proxy.** The pod sees a plaintext Kubernetes API on a
  loopback port, and the host adds the token or client certificate on
  the way out. The sandbox system ran one per sandbox, jailed on the
  host.
- **An SSH agent.** The pod sees an agent socket, and the host signs
  only logins to servers that prove they hold a bound host key, as the
  bound user, with the bound key. The sandbox system ran one per
  sandbox, too.

`feos-guest` pipes the last two to the host already (ADR 0007). Nothing
on the host answers yet.

All three need the same things: bindings per pod, a jail, audit records
and a way for secrets to reach the node.

## Decision

### What a pod gets

- **An identity**: a certificate with the pod's SPIFFE ID, whose private
  key stays on the host.
- **FeID**: libraries with which an application uses that identity for
  mTLS end to end, from its own sockets.
- **TLS bindings**, for applications without FeID: a loopback port whose
  connections the host carries to a named upstream over mTLS with the
  pod's identity.
- **Kubernetes bindings**: a loopback port with a plaintext Kubernetes
  API; the host adds the credential.
- **SSH bindings**: an agent socket; the host signs bound logins.

The host side of all of them is `feos-authn`. The application side,
where there is one, is FeID.

### feos-authn: one process per running pod

- `feos-authn` is a program of its own (ADR 0003): a small static
  binary, shipped in the `feosd` package.
- `feosd` starts one `feos-authn` per pod that runs with an identity or
  bindings, with the microVM on create, resume and restore, and stops it
  when the run ends. A suspended pod has no process, and no key or
  secret of it is on any node.
- The process holds one pod's material and nothing else. There is no
  lookup from a connection to a tenant, and a bug in a parser costs one
  pod's credentials, not the node's.
- It survives a restart of `feosd` with the microVM. `feosd` binds a
  control socket in the pod's directory and hands it to the process,
  which listens on it; a `feosd` that starts again dials it and adopts
  the process by its pid and start time, as it adopts Firecracker. The
  protocol on the control socket is versioned, so that an upgraded
  `feosd` adopts the processes that the version before it started.

### One vsock port, the socket names the pod

- The guest pipes every connection to one vsock port (ADR 0007). A
  one-line preamble selects what the connection is for: a Kubernetes or
  TLS binding by its port, SSH, or FeID.
- The preamble carries no authority. Identity comes from the host-side
  socket the stream arrives on, which belongs to the pod's microVM. A
  process in the pod that dials the vsock port itself gains nothing that
  the guest's sockets do not already give it.
- In the guest, Kubernetes and TLS bindings are loopback ports, SSH an
  agent socket, FeID a socket at a fixed path, and the trust bundle a
  file beside it. `feosd` pushes the ports with the pod's configuration
  (ADR 0007). Each exists only while the pod has what it serves.
- A binding's port is not forwardable.

### What feos-authn answers

- **FeID (feid/1):** the guest's TLS stack fetches the pod's certificate
  chain and the trust bundle, and asks for a signature. The signer signs
  only input of the exact shape of a TLS 1.3 CertificateVerify, for
  client or server. Keys are Ed25519, so the input arrives unhashed and
  can be checked. It is not a signing oracle.
- **TLS bindings:** a binding names an upstream, a host and a port, and
  its CA, or the WebPKI when it has none. `feos-authn` checks and dials
  the upstream as for Kubernetes, runs TLS 1.3 with the pod's
  certificate as its client certificate, and pipes both directions.
  `psql`, or an application whose TLS stack cannot delegate a signature,
  gets the pod's identity this way. The plaintext passes through the
  host process, which FeID avoids.
- **Kubernetes:** HTTP/1.1, one upstream connection per guest connection.
  After an upgrade, both sides are a pipe, so `exec`, `attach`,
  `port-forward` and watches pass through. The proxy resolves the
  binding's server, checks every address it would dial, and dials the
  address it checked. It drops `Authorization`, `Impersonate-*` and
  `Proxy-*` from the request and adds the credential. A denied
  destination answers 403, an unreachable one 502. A request never
  leaves without the credential. What the credential may do is RBAC in
  the target cluster.
- **SSH:** the agent lists its identities, binds a session
  (`session-bind@openssh.com`, without forwarding), and signs only a
  `publickey` user authentication for a bound session whose host key,
  user and key match one binding. Everything else is refused. A client
  without session binding gets nothing. RSA signs with SHA-2 only.

### The pod's identity

- A pod's spec names its identity: a SPIFFE ID that the control plane,
  the registration authority, chose. `feosd` does not choose identities,
  and the application never names its own.
- The certificate carries the ID as a URI name and, as DNS names, the
  names under which the pod's service is reachable. Its subject CN is
  derived from the ID, the trust domain and path, truncated with a hash
  of the ID when longer than a CN may be, so that databases that map on
  the CN see it.
- `feos-authn` makes the pod's key pair in its own memory, at every
  start. Its certificate signing request goes to the control plane,
  which issues a short-lived certificate. The private key never exists
  outside the process that signs with it: not on a disk, not in
  `feosd` and not in the control plane.
- It renews the certificate at three quarters of its lifetime. An issuer
  that cannot be reached is retried; until the certificate expires, the
  old one serves. After that, the signer refuses, loudly.
- The trust bundle is public and comes to `feosd` with the pod. The
  platform CA is constrained to the platform's names: it cannot issue
  for a name it does not own, and a leaked CA key reaches no name on the
  internet.

### Secrets reach the process sealed to it

- TLS, Kubernetes and SSH bindings are sets: the spec names all of a
  pod's bindings, and a change states the whole set again. What it does
  not name is gone.
- At start, `feos-authn` makes a second key pair, for HPKE (RFC 9180),
  and its public half goes out in the certificate signing request. The
  control plane seals the bindings to it. `feosd` relays ciphertext.
  Only the pod's own process can open them, and it keeps them in memory
  only.
- `feosctl` does the same for a node without a control plane: it asks
  `feosd` for the process's public key and seals the bindings itself.
- A new run has a new key, so the control plane seals again after every
  start, resume and restore.
- The sealing keeps the bindings off the bus and out of `feosd`'s logs
  and dumps. It does not protect against a `feosd` that has been taken
  over: that owns the microVM's memory anyway.

### What a run waits for

- The certificate and the sealed bindings come in one round trip, and
  the microVM boots meanwhile. On a start, the guest starts the pod's
  containers only when both have arrived and `feosd` has pushed the
  configuration: nothing in the pod runs before the pod has what it was
  promised. A pod whose certificate or bindings do not come does not
  start.
- After a resume or restore, the containers are already running. Until
  the new certificate and bindings arrive, a request waits a few
  seconds, then gets a refusal that FeID's libraries retry.

### Audit

- Every signature and every request is recorded **before** it is
  answered: every Kubernetes request (binding, host, method, path,
  status, bytes), every TLS binding's connection (binding, host, bytes),
  every SSH signature (binding, host key, user), every TLS signature
  (identity, context), and the handshake reports of FeID's libraries.
  Refusals are recorded with their reason.
- The record goes to an append-only file in the pod's directory, which
  `feosd` opens and the process inherits. A signature whose record
  cannot be written is not given. `feosd` reads the file and publishes
  the records as the pod's events (ADR 0006); a `feosd` that restarts
  stalls no handshake.
- The control plane joins TLS signatures, handshake reports and the
  flows and server names of ADR 0013 into one record per connection. A
  signature without a handshake report is an anomaly: a library
  bypassed, or a report lost.

### The jail

- Each process runs as a user ID of its own, one per running pod, so
  that two pods' processes cannot signal or trace each other.
- It runs in a mount namespace with an empty root, without capabilities,
  with `no_new_privs`, a seccomp allowlist and rlimits. Its descriptors
  are its vsock listener, its control socket, its audit file and stderr.
- It runs in a network namespace that reaches only the internet: a veth
  to the node, and blackhole routes for every prefix of `--deny-prefix`
  (ADR 0010) and for the node's own networks. A proxy that a parser bug
  has taken over has no way into the node network or the platform's own
  cluster.
- Its connections leave from an address of the pod's: a second address
  of the node's prefix, given to the pod with its first (ADR 0010). The
  pod's security groups (ADR 0012) and its traffic records (ADR 0013)
  cover what it reaches through its bindings like its own connections.

### FeID, the application side

- FeID is the SDK with which an application on FeOS uses its pod's
  identity.
- **Nothing to configure.** The libraries find the signer at its fixed
  path, or where an environment variable points, for development.
  Creating a client without configuration works in every pod that has
  an identity. A pod without one has no socket, and the library fails at
  start and says why: no identity, not a broken platform.
- **Peers by name.** A client dials a service by its name and checks the
  server by that name, as any TLS client does, against the platform CA
  and the WebPKI together. Pinning a SPIFFE ID stays an option. A server
  reads the client's SPIFFE ID and authorizes on it. A client without
  FeID checks a server with FeID against the platform CA, by name.
- **Development without FeOS.** `feosctl` runs a development signer:
  feid/1 on a Unix socket, with a CA made for the run and an identity
  named on the command line. The same application runs on a laptop and
  on FeOS.
- **The protocol**, feid/1, is versioned NDJSON over the socket: a hello
  negotiates the version, and within a version everything is additive,
  so a library in an image built months before a node's upgrade keeps
  working, and a new library with an old node. TLS 1.3 only, Ed25519
  only. Its server side (framing, negotiation, the CertificateVerify
  check, the error codes and the golden frames) is a Rust crate,
  `feid-proto`. `feos-authn` uses it, and so does the Rust library for
  its client side. The check exists once.
- **Libraries** for Go and Rust, whose TLS stacks let a library sign
  (`crypto.Signer`, rustls's `SigningKey`). They are published from this
  repository (ADR 0002), as a Go module and to crates.io, and versioned
  on their own, not with FeOS.

## Alternatives considered

- **One process per kind and pod**, as the sandbox system ran them. The
  SSH agent there sat in an empty network namespace, so that a bug in it
  cost one kind of credential, not two. It halves the isolation for
  little: a process that has been taken over can always write back to the
  guest over its vsock stream, so the empty namespace keeps little in.
  The tenant boundary, one pod per process, stays.
- **One daemon per node**, as the prototype's signer had it, under the
  rule "no sidecars". Fewer processes, but every tenant's keys in one
  address space and a lookup on every connection. A per-pod process on
  the host is no sidecar: nothing runs in the pod.
- **`feosd` running itself again**, as for the task service. It needs no
  new program, but the process would carry all of `feosd`'s code, and
  its seccomp allowlist would be larger.
- **Inside `feosd`.** It would parse hostile HTTP and SSH in the
  privileged process that owns every microVM on the node.
- **The control plane makes the keys and sends them with the pod.** One
  message fewer at start, but the key would exist outside the process
  that uses it.
- **Bindings over Valkey in plaintext, or pushed directly from the
  control plane to the node**, as the sandbox system did. The first
  shows them to everything that reads the bus. The second needs a
  listener on the node (ADR 0015 has none), and the plaintext would
  still pass through `feosd`.
- **Audit records to `feosd` only, without a file.** No signature is
  given before its record is written, so every restart of `feosd` would
  stall every handshake on the node.
- **Proxies dialing from the node's address.** The pod's security groups
  would not apply to them, and the pod's records would not show what it
  reached through its bindings.
- **A TLS-intercepting proxy with a CA per pod**, for any destination
  the pod chooses. Applications would need neither a library nor a
  binding, but the platform would sit in the plaintext of every
  connection, and the CA would have to be baked into the pod. A TLS
  binding sits in the plaintext only of the upstreams it names.
- **Peers only by SPIFFE ID**, as the prototype had it. Every client
  would have to know the IDs of its peers, clients without FeID could
  not check a server, and the libraries had to switch hostname
  verification off and verify by hand: the one place where a copied
  configuration loses all verification.
- **protobuf or gRPC for feid/1**, like FeOS's APIs. It would put a
  protobuf or HTTP/2 runtime into every application for a handful of
  messages.

## Consequences

- An application in Go or Rust adds a FeID library and creates a client.
  It configures nothing, holds no key, and its database sees its SPIFFE
  ID. Python's and Node's TLS stacks cannot delegate a signature;
  applications in them, and everything built on OpenSSL, get the same
  identity through a TLS binding. Kubernetes and SSH need nothing in
  the pod.
- The pod's spec gains an identity and its sealed bindings, and the pod
  API gains a way to set the bindings of a running pod. Preflights,
  which check that a binding works before any pod has it, are the
  control plane's.
- Starting or resuming a pod with an identity or bindings costs a round
  trip to the control plane; the microVM boots meanwhile. A pod without
  them starts as before.
- The control plane becomes an issuer, and has to know a service's DNS
  names, which is part of the CaaS API. Behind its issuer interface may
  sit an issuer of its own or SPIRE; the node does not care.
- The node gains one network namespace for the processes, and every pod
  with bindings a second address. The guest gains the preamble, the FeID
  socket, the trust bundle and TLS bindings.
- Two libraries are released from this repository, and the CI runs each
  against `feos-authn`.
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
- The protocol and the libraries start from FeTLS's code
  (github.com/MalteJ/FeTLS); the Kubernetes proxy and the SSH agent from
  the sandbox system's.

## Open questions

- Attestation of the node at its join: the node's TPM. ADR 0015 gives
  the node its identity from a join token.
- The trust domain and the layout of the IDs: tenant and workload, and
  what a sandbox is called.
- Python and TypeScript. An OpenSSL provider that delegates to the
  signer would give Python's `ssl`, libpq, curl and perhaps Node the
  identity end to end, without a library and without plaintext on the
  host: one native module in the image instead of a TLS binding.
- Whether TLS bindings get an inbound side, which terminates mTLS for a
  server in the pod that has no FeID.
- How many signatures a pod may ask for per second.
- ECDSA P-256 for peers without Ed25519. Go's TLS hands an ECDSA signer
  a digest, not the message, so the signer could not check its shape: it
  would be an oracle for that key. If P-256 comes, it is a key of its
  own, for TLS only.
- Integrity of the audit: a hash chain per pod, so that the control
  plane sees a gap.
- Tokens: JWTs for HTTP APIs, a pod as an OAuth client. A later ADR.
- Libraries for Java and .NET.
