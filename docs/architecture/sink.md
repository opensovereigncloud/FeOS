# The sink

`ferun-sink` keeps the platform's logs (ADR 0026): every container's
output of the pods FeRun makes, shipped by the nodes, and the requests
`ferun-ingress` answered, written by its instances, kept in its
PostgreSQL first and in Quickwit's indexes after (ADR 0026 §10), and
read by tenants over HTTP. This page is the contract between its parts;
the ADR says why.

```
guest ──CRI lines──▶ logs/container.log
                         │ StreamContainerLogs(follow, from_offset)
feosd (node) ──OTLP LogsService/Export, SinkService (mTLS, node cert)──▶ ferun-sink ──▶ PostgreSQL `sink`
     ▲ TelemetryResponse.sink_endpoint                                       │   └──▶ Quickwit ferun-logs-v2-<days>d (splits in the object store)
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
A restore brings them again (`RestorePodRequest.log_labels`,
`log_redact`): a node that takes the pod from its checkpoint records
them, one that has the pod replaces its own when the labels are not
empty. The guest's log and its ID travel on the pod's disk, so the
restored pod's streams are the same ones, and the node goes on from
where the sink has them (`Positions`), wherever they were shipped from.
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
batch, counted as the limit's drops are. Before a running pod is
stopped or deleted, `feosd` lets its containers end: SIGTERM to each,
and up to 10 s for their processes to exit (the guest writes what they
wrote before it reports an exit), while the pod still runs and its
streams follow. Then, as before a pod is suspended or hibernates, it
sends what its containers wrote: each stream that follows its log reads
until the log was quiet for 400 ms and the sink took what it read, for
at most 5 s in all; a stream that does not follow now (no sink, a
backoff) is not waited for. Only then does the microVM end. A stream
whose pod no longer runs is let go of, never cut off: it sends what it
read and what the guest still sends it, for at most 10 s, and is ended
at the sink (`EndStreams`) once its pod is deleted, also after a stop. A
pod restarted after a crash goes on from the sink's positions: its log
is on its disk. A stop of `feosd`
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

## The ingress's access log

`ferun-ingress` writes every request it answered for an environment
into the sink itself (ADR 0026 §8), when `--sink-endpoint` names the
ingest (its manifest: `ferun-sink-ingest.ferun.svc.cluster.local:4317`;
it runs in the cluster's network and resolves the name itself). The
sink is its only destination: without `--sink-endpoint` the access log
is not kept, and the agent counts its records as dropped.

- **Stream**: one per (ingress instance, start of its agent,
  environment, service). Key: `ferun.pod` = `ingress/<instance name>`,
  `ferun.container` = `<environment id>/<service>`, `ferun.log_id` = the
  agent's start (a UUID). Labels: `ferun.tenant` (the environment's
  owner, as the proxy's routing table has it), `ferun.environment`,
  `ferun.kind` = `service`, `ferun.object` = the service, `ferun.source`
  = `ingress`. A record's `log.offset` is its number in the stream from
  0, `log.next_offset` the number after; `log.iostream` none. The
  stream's `node` is empty.
- **Line**: the record as JSON, the fields and names of `ferund`'s
  `AccessLogRecord` (`tsNs` as a string, `time`, `durationNs`,
  `ingress`, `service`, `instance`, `host`, `method`, `path`, `proto`,
  `status`, `outcome`, `attempts`, `heldNs`, `bytesIn`, `bytesOut`,
  `clientIp`, `userAgent`, `traceId`, `spanId`, `parentSpanId`), and
  `level`: `error` for a 5xx or `aborted`, `warn` for a 4xx, else
  `info`. The LogRecord's time is the request's start.
- **TLS**: the ingress presents its certificate
  (`spiffe://<td>/ingress/<name>`) and checks the sink's as a node does.
  The sink takes streams with `ferun.source` = `ingress` from an
  ingress's certificate only, whose name is the key's (`ferun.pod` =
  `ingress/<name>`), and every other stream from a node's certificate
  only, never one whose `ferun.pod` begins with `ingress/`; a request
  with a stream of another is refused whole with `PERMISSION_DENIED`.
  `SinkService` is the nodes' alone: an ingress needs no positions (each
  start is a new stream) and ends no stream.
- **Batches**: the agent's buffer (10,000 records) as now, a batch at
  most 1,000 records or 1 MiB of lines, every second or once 1,000 wait;
  a failed batch is kept, with its offsets, and sent again with backoff
  (1 to 30 s) while new records wait in the buffer, which drops and
  counts what overflows it. A batch refused with `INVALID_ARGUMENT` or
  `RESOURCE_EXHAUSTED` is dropped and counted, as is a record without a
  tenant.
- **Reading**: the selector `source` (`ingress` or `container`; none:
  both) picks them; a stream's JSON has `source` (`log_streams.source`,
  migration `0003`, `container` for every stream before it). `ferund`'s
  `GET /api/v1/environments/{env}/access-logs` reads them from the sink
  (`environment`, `source=ingress`, `trace`, backward from `before`,
  `since=0`) and answers its `AccessLogList`; the sink is the only
  place it reads ([`ferund`](#ferund)). `ferund`'s own table of before,
  `access_logs`, was dropped on 2026-10-31 (migration `0020`) without
  moving its records.

## The sink

A program of `go/` (`go/cmd/ferun-sink`, `go/internal/sink`), its own
database (`sink`, migrations of its own), two ports:

- **ingest** (gRPC over TLS 1.3, `--ingest-listen`, default `:4317`):
  `LogsService/Export` and `SinkService`, nodes only, and
  `LogsService/Export` for the ingresses' access logs ([The ingress's
  access log](#the-ingresss-access-log)), from the first certificate
  on. A batch is stored in one transaction and then
  answered; a line is stored once per (stream, offset), and one at or
  below the last offset stored counts as stored: a node sends a stream's
  lines in the order of their offsets. A tenant over
  its quota (`--log-quota`, bytes of text a day, UTC, 0: none; reached by
  what is stored, so the batch that crosses it is stored) is refused with
  `RESOURCE_EXHAUSTED`, which names it; the other tenants' streams of the
  same batch are stored. A database failure is `UNAVAILABLE`; a request
  over 8 MiB, of more than 10,000 records, or with a resource without
  `ferun.pod`, `ferun.container`, `ferun.log_id` and `ferun.tenant` or a
  record without `log.offset` is refused. A stream's `node` is the name
  in the node's certificate, not `ferun.node`; the sink does not check
  that the node runs the pod. A stream's `source` is `ingress` for
  `ferun.source` `ingress`, else `container`.
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
nodes' certificates (ClientAuth, one URI `spiffe://<td>/node/<name>`)
and the ingresses' (`spiffe://<td>/ingress/<name>`).

**Tenants.** A read brings the tenant's FeRun token (`Authorization:
Bearer …`); the sink asks `ferund` `GET /api/v1/tenant` with it (`200
{"tenant": "<name>"}`, `401` passed on) and keeps the answer a minute,
by the token's SHA-256.

**Stages.** `log_streams` (one row per stream key: labels, first and
last time, next offset, ended, `chunked_last`, the mark of what the
index holds; its ID `s_` and 16 random hex digits) and `log_batches`
(hot: a row per stream and ingested batch, migration `0006`) in the
database; the cold stage is Quickwit (§10). A batch row has the stream,
the first and last offset, the lines' earliest and latest time, the
number of lines, their bytes of text, the bits of their levels, and
`data`: the lines in offset order, each its offset, time and level
(varints), a flags byte (partial, and `cont`: the line goes on with the
last of its source, which was partial), and its source, text and trace
ID (each a length and its bytes), which PostgreSQL compresses with lz4;
partitioned by the day it arrived. A stream's part of an Export is one
row, several when its text is over 2 MiB. The reads select the batches
whose times and levels may hold lines they want and filter the lines
themselves. Every second, four loops of each instance claim streams with
hot lines (at most 32 a round, `FOR UPDATE SKIP LOCKED`, a lease of
three minutes of the database's clock), send the lines of their batches,
oldest first, whole, at most 6 MiB of text a round (at least one batch),
to the stream's index with `commit=wait_for`, and once Quickwit answers,
the split holding them published, delete those batches and move the mark
to the last one's last offset, in one transaction a stream. A failure of
the index holds the round's claims for 30 s. Until 2026-10-31 the hot
stage was `log_lines`, a row a line; while instances of before still
write it (a rolling update), the reads take its lines as hot lines too
and the indexer drains them with the batches, in the order of their
offsets. That goes, with `log_lines`, in a later release. A
stream's index is `<--index-prefix>-v<version>-<days>d`
(`ferun-logs-v2-30d`): the days of `--log-retention`, or of
`--log-retention-max` for a stream whose `ferun.retention_days` is
longer than the default; the sink makes an index it does not find
(Quickwit's index API), with its retention, `--index-commit-timeout`
(10 s) as its commit interval and `--index-partitions` (16, at most 200)
partitions. A document is a line with its stream's labels: `ts` (ns, the
time field), `tenant`, `stream` (the ID), `environment`, `kind`,
`object`, `replica`, `release`, `instance`, `container`, `job`,
`attempt`, `source`, `iostream`, `level` (the bit), `trace_id`, `off`,
`partial`, `cont`, and `text` (Quickwit's `default` tokenizer, with
positions). Quickwit has no key of its own: a batch sent again is
indexed again, and the reads take one (stream, offset) once.
`--index-ingest-url` names Quickwit's indexers, `--index-search-url` its
searchers (every other call).

**Partitions.** One tenant may hold many projects' environments (FeCode's
does), so an index's lines are hashed into partitions by their tenant,
environment and job (`partition_key: hash_mod((tenant,environment,job),
<--index-partitions>)`), and a split holds one partition's lines: a
commit writes a split per partition that got lines, and merges keep
them apart. `tenant`, `environment` and `job` are tags (`tag_fields`):
Quickwit records a split's values of each while they are fewer than
1,000, and a search for an environment or a job reads only the splits
that have it. More partitions mean fewer environments a split and more,
smaller splits; Quickwit's own bound is 200 (`max_num_partitions`).

**Versions.** Quickwit never changes an index's partition key or time
field, and a changed mapping only applies to new splits, so the index
configuration has a version in the index's name
(`quickwit.MappingVersion`): version 1, `<prefix>-<days>d`, had
`tenant` as its only tag and no partitions; version 2 is the above. A
new version's indexes take every new line; a read searches
`<prefix>-*`, the old and the new together (the fields it reads are the
same in both, `ts` a datetime in each, so they sort as one), and a
delete leaves its task in every index of the prefix. The old indexes
are emptied by their own retention and can then be deleted by hand.
`--index-partitions` (and `--index-commit-timeout`) apply only to
indexes made after a change: an index keeps what it was made with, so a
change takes a new version or a new retention.

**Chunks of before.** Until 2026-10-31 the cold stage wrote gzip NDJSON
chunks to the object store (`log_chunks`); they all moved into the index
that day, and the table, the code that moved them and the sink's bucket
settings went (migration `0007`).

**Level** (§6): an OTLP record's severity number when the text gives
none (1 to 8 debug, 9 to 12 info, 13 to 16 warn, 17 to 24 error); a JSON line's `level`, `lvl` or `severity` (a word, or a
number on pino's scale: 50 and above error, 40 warn, 30 info, 10 and 20
debug); else a leading `ERROR`, `WARN`, `WARNING`, `INFO`, `DEBUG`,
`TRACE`, `FATAL`, `PANIC` (bare, `[…]`, or `level=…`, also after a
leading `time=` or `ts=` field, as Go's slog writes it), case
insensitive; mapped to `error` (also fatal, panic, critical), `warn`,
`info`, `debug` (also trace), or `` (unknown). **Trace ID**: an OTLP
record's `trace_id`, else a JSON line's `trace_id`,
`traceId` or `traceparent` (its second field): 32 lowercase hex, not all
zero.

**Retention**: `--log-retention` (720 h) or a stream's
`ferun.retention_days`, at most `--log-retention-max` (9600 h); a sweep
every 10 min deletes the hot batches whose latest line is past
retention, the streams whose last line is past their retention and that
have nothing left in the database, and drops empty day partitions of
`log_batches` (and `log_lines`). Quickwit drops a
split of an index once its newest line is past the index's retention,
which is at least the stream's; a read never answers a line of a stream
that is gone.

## Reading

All routes need a tenant's token and answer only its streams.

### `GET /api/v1/logs`

| parameter | |
|---|---|
| `environment`, `kind`, `object`, `replica`, `instance`, `container`, `job`, `attempt`, `stream`, `source` | the selector; one of `environment`, `job`, `instance`, `stream` is required; `source` is `ingress` or `container` (none: both) |
| `since`, `until` | RFC 3339 or Unix nanoseconds, both included; `until` defaults to now, `since` to `until` − 1 h (backward) or to the stream's start (forward), and, with `trace`, to the start of what is kept |
| `q` | words that all appear, case insensitive; `-word` must not. The index finds whole words (`erro` finds no `error`; a word with punctuation, `user_id=42`, as a phrase); a line it found must then hold each word as a substring |
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
     "job": "", "attempt": 0, "node": "…", "source": "container", "firstTime": "…", "lastTime": "…", "endedAt": null,
     "lines": 1234, "bytes": 56789}},
  "cursor": "…",
  "partial": false
}
```

A line the node sent in parts (`log.partial`: a 64 KiB part of a longer
line, or one left unfinished for a second) is answered once, joined with
the following lines of its stream and source up to and including the
first that is not partial, with the first part's `ts`, `offset` and
`level`; `partial` is `true` when its rest is not there yet or when it
was cut at 1 MiB. The filters see the joined text. A page holds whole
lines only and its cursor goes on after the last line it considered, so
a page may hold fewer lines than `limit` and still have a cursor. A
follow holds a partial line for its rest for at most 5 s, then gives it
`partial`; a part whose line the follow's first page gave is given on its
own.

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

Deletes the streams and their hot lines, and leaves a delete task of the environment's or job's lines in every log index (Quickwit removes them when its janitor rewrites their splits; no read finds them meanwhile). 204.

## `ferund`

- **Labels**: an environment's instance and job run, and a tenant's job,
  get `log_labels` on their pod's create (the table above), kept on their
  sandbox row (`sandboxes.log_labels`, migration `0019`). `ferun.release`
  is the release the instance was made for. A sandbox a tenant made
  itself (an attached service's workspace too) has none: its logs are
  not shipped. `POST /api/v1/jobs` takes `logRetentionDays` (1 to 400),
  which becomes `ferun.retention_days`.
- `--sink-endpoint` (host:port of the ingest) to the nodes, a name
  resolved every 10 s into one ready instance's address per node;
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
- `GET /api/v1/environments/{env}/access-logs` reads the ingress's
  lines from the sink alone, with the caller's token
  (`environment=<ID>&source=ingress&direction=backward&since=0&until=<beforeTsNs − 1>&trace=<traceId>&limit=<the rest>`,
  paged up to 8 times), each line's JSON an `AccessLogRecord`. Without
  `--sink-url` it answers 503 `no_log_store`, and 503
  `log_store_unavailable` (`Retry-After: 5`) when the sink fails.
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
  pod ships unredacted lines. Every restore sends the row's labels and
  the redactions, asked anew, as well (`RestorePodRequest`), with the
  same failure, so that a hibernated pod restored on another node goes on
  shipping.
- **Parts**: where `ferund` answers lines of a tail, from the node or the
  sink, a partial entry is joined with the next entries of its source up
  to one that is not partial, the line cut to its budget (8 KiB in an
  answer); a line still open at the end goes in as far as it came. From
  the sink it pages back until `lines` + 1 lines have ended. `ferund` redacts a running tenant job's
  sink lines again; an ended job's (its secrets are gone) are the node's
  redaction alone.
