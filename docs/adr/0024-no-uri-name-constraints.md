# ADR 0024: The platform CA carries no URI name constraint, and nodes trust a root for its key

**Status:** Accepted · **Date:** 2026-10-29

## Context

`ferund init-ca` gave the root and the issuer the same critical name
constraints: DNS names under the platform's domains, no IP address, and
URIs only in the trust domain. The control plane's architecture said why
the URI constraint was harmless: Go's verifier enforces it, and
rustls-webpki ignored it.

rustls-webpki 0.103.16, taken in for its security advisories, no longer
ignores it: it does not check URI name constraints, and since a fix of
that release it fails closed, refusing every URI name beneath a URI
constraint. The control plane's certificate on the nodes' port carries
the URI `spiffe://<trust domain>/ferun`, which is how a node tells it from
a pod's certificate of the same issuer. A `feosd` built with that
release refuses every chain from a CA of `init-ca` with
`NameConstraintViolation`: on the join, on every node connection and on
the uplink. On shoot `fecode` the nodes went down within a minute of the
rollout, and the rollout was taken back.

The URI constraint bought little. Every verifier of this platform reads
an identity for the exact trust domain: ferund reads nodes and ingress
instances with `ParseNodeID` and `ParseIngressID`, a node holds the
control plane to its one URI, and issuance never writes a URI outside
the trust domain. The constraint only kept a compromised issuer from
naming another trust domain, which no verifier would accept. The trust
domain's binding to the CA, which `LoadIssuer` checked through the
constraint, is a configuration check that does not need the
certificate.

## Decision

- **No URI constraint.** `ferund init-ca` makes the root and the issuer
  with DNS and IP constraints only. `issuer.json` records the trust
  domain beside the control-plane domain, and `serve` refuses a
  `--trust-domain` other than the one recorded there. An `issuer.json`
  of before has none, and its issuer's URI constraint binds as before.
- **A node trusts a root for its key.** `feosd` makes its trust anchors
  from the trust bundle, and on a join from the pinned root, without the
  root's name constraints. The constraints of the certificates between
  the root and the leaf, the issuer's, still hold, and the control
  plane's certificate is held to the configured DNS name, the one URI and
  no IP address, which is more than the root's constraints asked.
- **A new issuer under the same root.** `ferund new-issuer --root-dir R
  --dir D` reads the root's certificate and key and makes a new issuer
  into a new CA directory, without a URI constraint, and loads it before
  it returns. The root, the bundle and the join tokens' pin stay, so
  nodes keep their identities and need no new join. It replaces an issuer
  of an old CA, and an issuer near its end.
- **A warning, not a refusal.** `serve` warns when its issuer constrains
  URIs and names `ferund new-issuer`. It does not refuse to start: an
  instance of this version with an old issuer still serves nodes of
  before.

## Consequences

- **Order on an existing platform.** Make the new issuer (the root key's
  ceremony), put it into every instance's `--ca-dir` and restart them,
  then roll the nodes. A node of before accepts both issuers, since its
  rustls-webpki ignores URI constraints; a node of this version accepts
  only the new one. Node certificates of the old issuer stay valid until
  they are renewed, because ferund checks them against the bundle.
- **The root's constraints are a ceremony's promise, no longer a
  node's check.** A node no longer enforces the root's DNS and IP
  constraints itself. It relies on the issuer's, which `new-issuer` and
  `init-ca` make the same, and on its own checks of the leaf.
- **Go's chain verification admits a foreign trust domain's URI.**
  Nothing reads one: every identity is parsed for the exact trust
  domain, and a test says so.

## Considered

- **feosd checks the chain itself**: the issuer against the root by
  hand, the leaf with rustls-webpki against the issuer, and the URI
  constraint in its own code. No ceremony, but path validation of our
  own on the node's most sensitive connection, kept only until
  rustls-webpki checks URI constraints.
- **A new root without the constraint**: the cleanest result, but every
  node and ingress instance must trust the new root before the instances
  serve under it, which takes a renewal of every client or a restart and a
  join again, and every pod and ingress certificate is issued again.
  Where the old root's key is lost, it is the only way, as on shoot
  `fecode` ([Replacing the root](../architecture/control-plane.md#the-ca-ceremony)).
- **Pinning rustls-webpki below 0.103.16**: it would take back a
  security update.
