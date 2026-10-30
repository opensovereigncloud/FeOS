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
line, `P` a part of a line over 64 KiB that the next entry goes on with.
A log file gets an ID when the guest makes it (`log_id`), so that offsets
of a log made anew start again under a new ID. `StreamContainerLogs`
answers the text without the prefix, with `offset`, `next_offset`,
`time_unix_nano`, `source`, `partial` and `log_id`; with `follow` and
`from_offset` it starts there. Lines of a log written before (no prefix)
are answered as they are, without a time.

## The node

`ferund` sends a pod's labels with its create
(`CreatePodRequest.log_labels`); `feosd` keeps them in the pod's record.
`ferund` names the sink's ingest in every `TelemetryResponse`
(`sink_endpoint`, `--sink-endpoint`); empty: nothing is shipped.

For every running pod with labels and every container of it, `feosd`
asks the sink where the stream is (`SinkService/Positions`), follows the
container's log from there and exports batches over OTLP
(`LogsService/Export`): at most 1 MiB or 1,000 lines a batch, sent once
full or after 500 ms. A batch is kept until the sink answers; on an
error it is sent again after a backoff (1 s to 30 s), to whichever
address the endpoint resolves to. Before a pod pauses, hibernates or is
deleted, `feosd` sends what its containers wrote, for at most 5 s; after
a delete it calls `SinkService/EndStreams` for its streams.

**Limit**: 1 MiB/s per stream with a burst of 8 MiB (a token bucket on
bytes). Lines beyond it are dropped; once the bucket allows, the node
sends a record of its own, `[ferun] dropped N lines (B bytes)`, source
`stderr`, at the offset of the first line it dropped.

**TLS**: the node presents its node certificate
(`spiffe://<td>/node/<name>`) and accepts a server certificate of the
platform's CA whose one URI is `spiffe://<td>/sink/<name>`.

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
`log.offset` (int), `log.iostream` (`stdout`, `stderr`, or none), and
`log.partial` (bool, only when true).

## The sink

A program of `go/` (`go/cmd/ferun-sink`, `go/internal/sink`), its own
database (`sink`, migrations of its own), two ports:

- **ingest** (gRPC over TLS, `--ingest-listen`, default `:4317`):
  `LogsService/Export` and `SinkService`, nodes only. A batch is stored in
  one transaction and then answered; a line is stored once per (stream,
  offset). A tenant over its quota (`--log-quota`, bytes a day, 0: none)
  is refused with `RESOURCE_EXHAUSTED`; a database failure with
  `UNAVAILABLE`.
- **API** (HTTP, `--listen`, default `:8080`): the routes below, behind
  the cluster's Gateway at `run.<domain>/api/v1/logs` and
  `/api/v1/log-streams`; `/healthz`.

**Identity.** The sink holds a service token of kind `sink` (`ferund
service-token --kind sink --name <name>`, kept in a Secret). With it, it
asks `ferund` for a certificate (`POST /api/v1/service-certificates`,
Bearer the token, `{"csr": "<PEM>"}` → `{"certificate": "<PEM chain>",
"bundle": "<PEM roots>", "notAfter": "<RFC 3339>"}`), valid 24 h, the URI
`spiffe://<td>/sink/<name>`, and asks again at half its life. The bundle
verifies the nodes' certificates.

**Tenants.** A read brings the tenant's FeRun token (`Authorization:
Bearer …`); the sink asks `ferund` `GET /api/v1/tenant` with it (`200
{"tenant": "<name>"}`, `401` passed on) and keeps the answer a minute,
by the token's SHA-256.

**Stages.** `log_streams` (one row per stream key: labels, first and last
time, next offset, ended), `log_lines` (hot: stream, offset, time,
source, level, partial, text; partitioned by day), `log_chunks` (stream,
first and last offset and time, lines, bytes, levels as a bit set, the
object's key), `log_traces` (trace ID, stream, time). A stream's lines
become a chunk at 1 MiB, when the oldest is 10 min old, or when the
stream ends: gzip NDJSON (`{"o":offset,"t":ns,"s":"stdout","l":"error","p":true,"x":"text"}`
a line) at `logs/<tenant>/<stream id>/<first offset>.ndjson.gz`, claimed
with `FOR UPDATE SKIP LOCKED`, the row written and the lines deleted in
one transaction after the upload. The object store is S3 with SigV4
(`--bucket-endpoint`, `--bucket`, `--bucket-region`, keys from
`SINK_BUCKET_ACCESS_KEY`, `SINK_BUCKET_SECRET_KEY`), signed with the
standard library.

**Level** (§6): a JSON line's `level`, `lvl` or `severity`; else a
leading `ERROR`, `WARN`, `WARNING`, `INFO`, `DEBUG`, `TRACE`, `FATAL`,
`PANIC` (bare, `[…]`, or `level=…`), case insensitive; mapped to
`error` (also fatal, panic, critical), `warn`, `info`, `debug` (also
trace), or `` (unknown). **Trace ID**: a JSON line's `trace_id`,
`traceId` or `traceparent` (its second field): 32 lowercase hex, not all
zero.

**Retention**: `--log-retention` (720 h) or a stream's
`ferun.retention_days`, at most `--log-retention-max` (9600 h); a sweep
every 10 min deletes expired chunks (object, then row) and drops empty
day partitions of `log_lines`.

## Reading

All routes need a tenant's token and answer only its streams.

### `GET /api/v1/logs`

| parameter | |
|---|---|
| `environment`, `kind`, `object`, `replica`, `instance`, `container`, `job`, `attempt`, `stream` | the selector; one of `environment`, `job`, `instance`, `stream` is required |
| `since`, `until` | RFC 3339 or Unix nanoseconds; `until` defaults to now, `since` to `until` − 1 h (backward) or to the stream's start (forward) |
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
    {"ts": "1761825600123456789", "time": "2026-10-30T12:00:00.123456789Z", "stream": "s_…",
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
`{}` every 15 s.

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
  tenant's job's log reads the sink while the job runs, redacted of its
  secrets; once it has ended, its kept tail, as its secrets are gone then
  (its full log is the sink's own `GET /api/v1/logs?job=<id>`).
