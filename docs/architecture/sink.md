# The sink

`ferun-sink` keeps the platform's logs (ADR 0026): every container's
output of the pods FeRun makes, shipped by the nodes, kept in its
PostgreSQL first and in the object store after, and read by tenants over
HTTP. This page is the contract between its parts; the ADR says why.

```
guest ──CRI lines──▶ logs/container.log
                         │ StreamContainerLogs(follow, from_offset)
feosd (node) ──OTLP LogsService/Export, SinkService (mTLS, node cert)──▶ ferun-sink ──▶ PostgreSQL `sink`
     ▲ TelemetryResponse.sink_endpoint                                       │   └──▶ object store logs/…
ferund ──CreatePodRequest.log_labels──▶ feosd                                │
     ◀── GET /api/v1/tenant (a tenant's token) ──────────────────────────────┤
tenants (FeCode) ── GET /api/v1/logs, /api/v1/log-streams (Bearer) ──────────┘
```

## The guest

A container's stdout and stderr go through pipes the guest reads; it
writes each line to `logs/container.log` in the CRI log format:

```
2026-10-30T12:00:00.123456789Z stdout F the line's text
2026-10-30T12:00:00.123500000Z stderr P the first 64 KiB of a longer line…
```

The time is when the guest read the line, UTC, nanoseconds; `F` a full
line, `P` a part that the next entry of its stream goes on with: the
first 64 KiB of a longer line, or what came of a line that stayed
unfinished for a second. A part and the rest of its line are two entries,
the rest possibly empty. A log file gets an ID when the guest makes it
(`log_id`, 32 lowercase hex digits, kept beside it in
`container.log.id`), so that offsets of a log made anew start again under
a new ID; the ID stays with the file across the guest's restarts,
upgrades, reboots (an adopted container keeps its log) and suspends.
`StreamContainerLogs` answers the text without the prefix, with
`offset`, `next_offset`, `time_unix_nano`, `source`, `partial` and
`log_id`; with `follow` and `from_offset` it starts there. Lines of a log
written before (no prefix) are answered as they are, without a time.
docs/architecture/guest.md says how the guest reads the containers'
output.

## The node

`ferund` sends a pod's labels with its create
(`CreatePodRequest.log_labels`); `feosd` keeps them in the pod's record.
`ferund` names the sink's ingest in every `TelemetryResponse`
(`sink_endpoint`, `--sink-endpoint`); empty: nothing is shipped.

For every running pod with labels and every container of it, `feosd`
learns the log's ID from its last entry, asks the sink where the stream
is (`SinkService/Positions`), follows the container's log from there and
exports batches over OTLP (`LogsService/Export`): at most 1 MiB or 1,000
lines a batch, sent once full or after 500 ms. An empty log is followed
from its start, and its first entry names its ID; if the sink has that
stream further on, the node starts again from there. A batch is kept
until the sink answers; on an error it is sent again after a backoff (1 s
to 30 s), to whichever address the endpoint resolves to, and the stream
reads nothing meanwhile: the guest's log is the buffer, and the node
holds a batch and a few entries a stream. `RESOURCE_EXHAUSTED` drops the
batch, counted as the limit's drops are. Before a pod is suspended,
hibernates or is deleted, `feosd` sends what its containers wrote: each
stream that follows its log reads until the log was quiet for 400 ms and
the sink took what it read, for at most 5 s in all; a stream that does
not follow now (no sink, a backoff) is not waited for. A stop of `feosd`
that suspends its pods does not drain them: the next `feosd` goes on
from the sink's positions. After a delete it calls
`SinkService/EndStreams` for the streams whose log it knew, asked again
until the sink answers. A guest of before ADR 0026 (entries without
`log_id`) is not shipped.

**Redaction**: `CreatePodRequest.log_redact` names values (the pod's
secrets) that `feosd` replaces in every line's text with `[redacted]`,
the longest first, before the line counts against the limit and is
sent. They are kept in the pod's record (`PodInfo.log_redact`, empty in
every answer of the API), and never sent or logged. A value cut across
two parts of a line is not found.

**Limit**: 1 MiB/s per stream with a burst of 8 MiB (a token bucket on
bytes). Lines beyond it are dropped; once the bucket allows, the node
sends a record of its own, `[ferun] dropped N lines (B bytes)`, source
`stderr`, at the offset of the first line it dropped.

**TLS**: TLS 1.3, ALPN `h2`. The node presents its node certificate
(`spiffe://<td>/node/<name>`) and accepts a server certificate of the
platform's CA (its trust bundle) for server authentication, without an
IP address, whose one URI is `spiffe://<td>/sink/<name>` (a DNS label);
the name the node dials is not checked, so the endpoint may be an
address.

### The OTLP shape

One `ResourceLogs` per stream. Its resource attributes are the pod's
`log_labels`, as `ferund` set them, and the node's own:

| attribute | value | set by |
|---|---|---|
| `ferun.pod` | the pod's ID on its node | node |
| `ferun.container` | the container's name | node |
| `ferun.log_id` | the guest's ID of the log | node |
| `ferun.node` | the node's name | node |
| `ferun.tenant` | the tenant | `ferund` |
| `ferun.instance` | the sandbox's ID (`InstanceStatus.id`) | `ferund` |
| `ferun.environment` | the environment's ID | `ferund`, an environment's pods |
| `ferun.kind`, `ferun.object` | `service`, `addon` or `job`, and its name | `ferund`, an environment's pods |
| `ferun.replica`, `ferun.release` | numbers, as text | `ferund`, an environment's pods |
| `ferun.job`, `ferun.attempt` | a tenant's job's ID, the attempt | `ferund`, a tenant's job |
| `ferun.retention_days` | days, as text | `ferund`, when a job asked |

One `LogRecord` per entry: `time_unix_nano` the line's time (0 for an
old line: the sink uses `observed_time_unix_nano`, when the node read
it), `body` the text as a string (invalid UTF-8 replaced), attributes
`log.offset` (int), `log.next_offset` (int, the entry's `next_offset`:
the offset just past the line), `log.iostream` (`stdout`, `stderr`, or
none), and `log.partial` (bool, only when true). The node's record of
dropped lines has `log.offset` of the first line it dropped and
`log.next_offset` past the last. `Positions` answers the greatest
`log.next_offset` stored; from a node that sends none, the offset of the
last line stored, which the node then sends again and the sink stores
once.

## The sink

A program of `go/` (`go/cmd/ferun-sink`, `go/internal/sink`), its own
database (`sink`, migrations of its own), two ports:

- **ingest** (gRPC over TLS 1.3, `--ingest-listen`, default `:4317`):
  `LogsService/Export` and `SinkService`, nodes only, from the first
  certificate on. A batch is stored in one transaction and then
  answered; a line is stored once per (stream, offset), and one at or
  below the stream's last chunked offset counts as stored. A tenant over
  its quota (`--log-quota`, bytes of text a day, UTC, 0: none; reached by
  what is stored, so the batch that crosses it is stored) is refused with
  `RESOURCE_EXHAUSTED`, which names it; the other tenants' streams of the
  same batch are stored. A database failure is `UNAVAILABLE`; a request
  over 8 MiB, of more than 10,000 records, or with a resource without
  `ferun.pod`, `ferun.container`, `ferun.log_id` and `ferun.tenant` or a
  record without `log.offset` is refused. A stream's `node` is the name
  in the node's certificate, not `ferun.node`; the sink does not check
  that the node runs the pod.
- **API** (HTTP, `--listen`, default `:8080`): the routes below, behind
  the cluster's Gateway at `run.<domain>/api/v1/logs` and
  `/api/v1/log-streams`; `/healthz`, 200 once the ingest serves and while
  the database answers.

**Identity.** The sink holds a service token of kind `sink` (`ferund
service-token --kind sink --name <name>`, kept in a Secret). With it, it
asks `ferund` for a certificate (`POST /api/v1/service-certificates`,
Bearer the token, `{"csr": "<PEM>"}` → `{"certificate": "<PEM chain>",
"bundle": "<PEM roots>", "notAfter": "<RFC 3339>"}`, the CSR of an
Ed25519 key), valid 24 h, the URI `spiffe://<td>/sink/<name>` and
ServerAuth, and asks again at half its life. `<name>` is the token's,
and the sink's `--name` must be it: the instances of a deployment share
one token and name (`ferun-sink`). The sink checks the certificate's one
URI and key and that it verifies to the bundle. The bundle verifies the
nodes' certificates (ClientAuth, one URI `spiffe://<td>/node/<name>`).

**Tenants.** A read brings the tenant's FeRun token (`Authorization:
Bearer …`); the sink asks `ferund` `GET /api/v1/tenant` with it (`200
{"tenant": "<name>"}`, `401` passed on) and keeps the answer a minute,
by the token's SHA-256.

**Stages.** `log_streams` (one row per stream key: labels, first and last
time, next offset, ended; its ID `s_` and 16 random hex digits),
`log_lines` (hot: stream, offset, time, source, level, partial, text;
partitioned by the day it arrived), `log_chunks` (stream,
first and last offset and time, lines, bytes, levels as a bit set, the
object's key), `log_traces` (trace ID, stream, time). A stream's lines
become a chunk at 1 MiB, when the oldest is 10 min old, or when the
stream ends: gzip NDJSON (`{"o":offset,"t":ns,"s":"stdout","l":"error","p":true,"x":"text"}`
a line; `s`, `l` and `p` left out when empty or false) at
`<prefix>logs/<tenant>/<stream id>/<first offset>.ndjson.gz`, at most 16
MiB of text a chunk, claimed with `FOR UPDATE SKIP LOCKED` and a lease of
two minutes (the database's clock), the row written and the lines
deleted in one transaction after the upload. The object store is S3 with
SigV4 at path-style URLs (`--bucket-endpoint`, `--bucket`,
`--bucket-region`, `auto` for Cloudflare R2, `--bucket-prefix`, keys
from `SINK_BUCKET_ACCESS_KEY`, `SINK_BUCKET_SECRET_KEY`), signed with
the standard library.

**Level** (§6): a JSON line's `level`, `lvl` or `severity` (a word, or a
number on pino's scale: 50 and above error, 40 warn, 30 info, 10 and 20
debug); else a leading `ERROR`, `WARN`, `WARNING`, `INFO`, `DEBUG`,
`TRACE`, `FATAL`, `PANIC` (bare, `[…]`, or `level=…`, also after a
leading `time=` or `ts=` field, as Go's slog writes it), case
insensitive; mapped to `error` (also fatal, panic, critical), `warn`,
`info`, `debug` (also trace), or `` (unknown). **Trace ID**: a JSON line's `trace_id`,
`traceId` or `traceparent` (its second field): 32 lowercase hex, not all
zero.

**Retention**: `--log-retention` (720 h) or a stream's
`ferun.retention_days`, at most `--log-retention-max` (9600 h); a sweep
every 10 min deletes expired chunks (object, then row), hot lines and
trace rows by the lines' times, the streams with nothing left, and drops
empty day partitions of `log_lines`.

## Reading

All routes need a tenant's token and answer only its streams.

### `GET /api/v1/logs`

| parameter | |
|---|---|
| `environment`, `kind`, `object`, `replica`, `instance`, `container`, `job`, `attempt`, `stream` | the selector; one of `environment`, `job`, `instance`, `stream` is required |
| `since`, `until` | RFC 3339 or Unix nanoseconds, both included; `until` defaults to now, `since` to `until` − 1 h (backward) or to the stream's start (forward), and, with `trace`, to the start of what is kept |
| `q` | words that all appear, case insensitive; `-word` must not |
| `re` | an RE2 expression |
| `level` | a comma list of `error`, `warn`, `info`, `debug`, `unknown` |
| `trace` | a trace ID |
| `direction` | `backward` (default: the newest first) or `forward` |
| `limit` | lines, default 500, at most 5,000 |
| `cursor` | from an answer before: the next page in the same direction |
| `follow` | `1`: new lines as they come, as NDJSON |

```json
{
  "lines": [
    {"ts": "1793361600123456789", "time": "2026-10-30T12:00:00.123456789Z", "stream": "s_…",
     "offset": "4096", "source": "stdout", "level": "error", "partial": false, "text": "…"}
  ],
  "streams": {"s_…": {"id": "s_…", "tenant": "…", "instance": "…", "container": "main",
     "environment": "…", "kind": "service", "object": "api", "replica": 0, "release": 12,
     "job": "", "attempt": 0, "node": "…", "firstTime": "…", "lastTime": "…", "endedAt": null,
     "lines": 1234, "bytes": 56789}},
  "cursor": "…",
  "partial": false
}
```

A page's lines are oldest first in either direction; a backward page's
`cursor` leads to older lines, a forward page's to newer. `cursor` is
empty at the end. `partial` says the page stopped at the scan limit (256
MiB of lines read) before `limit`: there may be more behind `cursor`.
With `follow=1` the answer is `application/x-ndjson`, one object a line:
`{"line": {…}, "stream": {…}}` (the stream once, with its first line), and
`{}` after 15 s without a line. It gives the page a backward read gives
(`since`, `until`, `limit` and the filters as there), oldest first, then
every line of the selector's open streams stored after it, by each
stream's offsets, polling every 500 ms; it takes no `cursor`. A
`level` of `unknown` selects the lines whose `level` is `""`.

### `GET /api/v1/log-streams`

The selector of `/logs` and `since` (default 7 days back) →
`{"streams": [ … ]}`, as in `/logs`, the latest `lastTime` first, at
most 500.

### `DELETE /api/v1/log-streams?environment=<id>` or `?job=<id>`

Deletes the streams, their lines and chunks. 204.

## `ferund`

- **Labels**: an environment's instance and job run, and a tenant's job,
  get `log_labels` on their pod's create (the table above), kept on their
  sandbox row (`sandboxes.log_labels`, migration `0019`). `ferun.release`
  is the release the instance was made for. A sandbox a tenant made
  itself (an attached service's workspace too) has none: its logs are
  not shipped. `POST /api/v1/jobs` takes `logRetentionDays` (1 to 400),
  which becomes `ferun.retention_days`.
- `--sink-endpoint` (host:port of the ingest) to the nodes;
  `--sink-url` (the API, inside the cluster) for its own reads.
- `GET /api/v1/tenant`: the token's tenant.
- `ferund service-token --kind sink --name <name>` prints
  `feos-sink-<secret>` once; `GET /api/v1/admin/service-tokens` lists the
  tokens, `DELETE /api/v1/admin/service-tokens/{id}` revokes one (the
  certificates issued under it live on, 24 h at most). `POST
  /api/v1/service-certificates` issues a server certificate
  (ServerAuth, no DNS name) for an Ed25519 or ECDSA P-256 key; at most 10
  requests of a token an hour (429).
- `InstanceStatus.image`: the image the instance runs,
  `registry/repository@sha256:…` as the release resolved it.
- The objects' logs call and a job run's log read from the sink when
  `--sink-url` is set, with the caller's token
  (`instance=<sandbox ID>&container=main&direction=backward&since=<its
  start>`, paged), and from the node as before when the sink has no lines
  or fails; a paused instance's lines are answered from the sink. A
  tenant's job's log reads the sink too, also once the job has ended;
  its kept tail when the sink has no line.
- **Redaction**: a create with labels names `log_redact`, asked anew at
  each create and stored nowhere: the values `ferund` redacts its stored
  tails of, each also in its URL-userinfo escaping, of 4 bytes or more,
  longest first. An environment's pod: its addons' credentials, its
  tenant secrets (latest versions and those its releases pin) and those
  its template names; a tenant's job's: its secrets and image
  credentials. A failure to compute them fails the create, so that no
  pod ships unredacted lines. `ferund` redacts a running tenant job's
  sink lines again; an ended job's (its secrets are gone) are the node's
  redaction alone.
