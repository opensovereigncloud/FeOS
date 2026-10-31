# ADR 0026: The sink — a program of its own that keeps what the platform and its pods say: logs first, traces and audit records after, in the database first and in the object store after

**Status:** Proposed · **Date:** 2026-10-30

## Context

A container's output lives in one file in its guest,
`logs/container.log`, stdout and stderr together and without a time per
line. `ferund` reads a tail of it through the node, at most 10,000 lines
or 4 MiB (`…/objects/{kind}/{name}/logs`), and a job's run keeps its last
1,000 lines on its row. The file goes with its pod: the log of an
instance a release replaced, of one that crashed and was recreated, of a
paused pod (`ferund` never wakes one to read it) or of a job's run beyond
its kept end cannot be read. Nothing searches a service's replicas
together, or a week back.

Two tenants' workarounds show the gap. FeCode's builds are tenant jobs
(ADR 0018, ADR 0022), and a long build's log lost its start, so the
builder now keeps its own output and uploads it to FeCode's bucket as it
exits (FeCode ADR 0009 §11). The ingress's access log went into `ferund`'s
database (`access_logs`, ADR 0021) because there was no other place; its
records are the requests' server spans, and there is nowhere to put the
rest of a trace a pod makes.

The standard log stores split the same way. Elasticsearch and OpenSearch
index every word; ingest and storage are expensive, and nothing here
needs a full-text index. Loki indexes a stream's labels and time only,
keeps lines in compressed chunks in an object store, and searches by
reading the chunks a query's labels and time select. Recent lines sit in
its ingesters' memory and write-ahead log until a chunk is full. That
shape fits this platform: the labels are few and FeRun knows them, the
object store exists, and recent lines want a durable shared place, which
PostgreSQL is here.

## Decision

### 1. A program of its own: `ferun-sink`

`ferun-sink` is a program of this repository (ADR 0003), beside `ferund`
and `ferun-ingress`, with its own database and its own prefix in the
object store. `ferund` neither carries it nor waits for it: an
environment runs, releases and reads its status while the sink is down,
and a node keeps what it could not send (§3). It is where the platform
puts what is said rather than decided: **logs** first (this ADR's
steps), then **traces** (§8) and **audit records** (§9), each a kind of
record with its own tables and index, sharing ingest, the two stages and
retention. The reference deployment runs two or more instances over a
replicated PostgreSQL. They share nothing but the database and the
bucket, and a chunk's flush is claimed with `FOR UPDATE SKIP LOCKED`.

### 2. A stream is one container's output, labelled by FeRun

A **stream** is the output of one container of one pod: an image
service's or an addon's instance, a job's run, a tenant's job, an
attached service's process. Its labels are fixed and set by `ferund`,
never by the pod:

| label | for |
|---|---|
| `tenant` | always |
| `environment`, `kind`, `object`, `replica`, `release` | an environment's pods |
| `job`, `attempt` | a tenant's job, or a job's run |
| `instance` | the pod's sandbox ID |
| `source` | `stdout` or `stderr`, per line |

`ferund` sends them with a pod's create (`CreatePodRequest.labels`), and
the node stamps the stream with them. The labels are bounded by the pods
FeRun makes, so streams stay few: one per instance and run, not per
request or user. A pod adds no labels of its own. What it wants to find
is in its lines (§6). A stream has an end once its pod is gone, and its
labels and time span stay: the record of an instance that is no longer
there.

### 3. The node ships, the guest keeps

The guest writes each line with its time and stream, in the CRI log
format Kubernetes uses (`<RFC 3339 nano> <stdout|stderr> <F|P> <text>`;
`P` for a part of a line over 64 KiB, as `MAX_LINE` cuts today). Tails,
`feosctl` and the present logs call show the text as before.
`StreamContainerLogs` takes `from_offset`, and each entry carries its
offset, time and stream.

`feosd` follows the log of every running container on its node from the
offset the sink last stored for the stream, and exports batches to the
sink over OTLP/gRPC (`LogsService/Export`: a resource per stream with
its labels, a log record per line with `log.offset`). This is the
standard protocol, so a collector or a pod can speak it too (§8). Its
`.proto` files come into `proto/` and are generated with the protobuf
and gRPC this repository has, in Go and in Rust: no OpenTelemetry SDK,
no new module. Chunks are compressed with the standard library's gzip.
The sink stores a batch and then answers. A batch it has seen is stored once,
keyed by (stream, offset). The node keeps what is not acknowledged and
sends it again, to any instance, after a failure: at least once on the
wire, once in the store. The node connects with its node certificate
(ADR 0014), which lets it write the streams of the pods it runs and no
others.

Before a pod pauses, hibernates or is deleted, the node sends what its
containers wrote, for at most five seconds. A pod that dies with its
node loses what the node had not sent, a second's lines as a rule.

**Limits on the node.** A stream may send 1 MiB/s with a burst of 8
MiB. Beyond that the node drops lines and, once the rate allows, sends
one of its own: `[ferun] dropped N lines (B bytes)`. A tenant's lines are
counted against its quota in the sink (§7).

### 4. Two stages: the database first, the object store after

**Hot.** A batch lands in `log_lines` in the sink's PostgreSQL, one row
per line (stream, offset, time, source, level, text), partitioned by day
like `access_logs`. Recent reads and following a log come from here.

**Cold.** A stream's lines become a **chunk** once they reach 1 MiB, once
the oldest is ten minutes old, or once the stream ends. An instance
claims the stream, writes its lines as gzip-compressed NDJSON to
`logs/<tenant>/<stream>/<first offset>.ndjson.gz` in the object store,
records a `log_chunks` row (stream, first and last offset, first and last
time, lines, bytes, levels, key) and deletes the lines in one
transaction. A day's partition is dropped once it is empty. The hot stage
holds minutes of lines, not days.

The database stays the place where everything is known: a chunk exists
when its row does. An object left behind by a flush that failed after its
upload is overwritten by the next try (the key is the first offset) or
swept.

### 5. A few chosen indexes, and the rest scanned

Like Loki, the sink keeps no full-text index:

- **streams by label**: tenant, environment, object, replica, job;
- **chunks by stream and time**: a query reads only the chunks its time
  range overlaps;
- **levels per chunk**: a bit for each level its lines have (`error`,
  `warn`, `info`, `debug`, unknown), so "errors only" skips chunks
  without one;
- **trace IDs**: a line naming a W3C trace ID (§6) records (trace ID,
  stream, time) in `log_traces`. An access-log record leads to the lines
  its request made, and a trace (§8) to its logs.

Anything else is a scan of the chunks the labels, time and levels leave,
newest first. One request reads at most 256 MiB of lines. It answers what
it found with a cursor to continue from, and says so.

### 6. What the sink reads from a line

A line that is a JSON object gives its `level` (also `lvl`, `severity`)
and its trace ID (`trace_id`, `traceId`, `traceparent`, as OpenTelemetry's
log bridges write them). The ingress sends every request a
`traceparent` (ADR 0021), so an application that logs it is found by
it. A line of text gives a level when it starts with one (`ERROR`,
`level=warn`, `[INFO]`). Nothing else is parsed. The text is kept as it
came, cut as §3 says.

### 7. Reading, retention, quota

`GET /api/v1/logs` answers lines, oldest first in each page, with their
labels, source, level and time. The parameters are a stream selector
(`environment`, `object`, `replica`, `instance`, `job`), a time range
(`since`, `until`), filters (`q`: words that must all appear, case
insensitive, `-word` to exclude; `re`: an RE2 expression; `level`;
`trace`), `direction` (backward from `until` by default) and `limit`
(default 500, at most 5,000). The answer carries a cursor to the next
page. `follow=1` streams new lines from the hot stage as they come. This
is LogQL's shape, a selector and line filters, without its syntax.

A tenant reads its own streams with its FeRun token. The sink asks
`ferund` whose token it is (`GET /api/v1/tenant`, new) and keeps the
answer for a minute. `ferund`'s own logs calls answer from the sink when
it has the stream, so a paused pod's and a replaced instance's logs are
read without a node.

**Retention.** Lines are kept for `--log-retention` (30 days). A tenant's
job may ask for longer at its create (`logRetentionDays`, at most
`--log-retention-max`, 400). FeCode asks for that for builds, since a
version's build log matters while the version does. A sweep deletes
expired chunks, the object first and its row after. A tenant may delete
an environment's or a job's streams. **Quota**: bytes per tenant per day
(`--log-quota`), counted at ingest. Beyond it the sink refuses that
tenant's batches with `RESOURCE_EXHAUSTED`, and the node drops and counts
them as §3 says.

### 8. The ingress's access log is a stream of the application's (proposed 2026-10-31)

Asked for by Malte on 2026-10-31: the ingress writes its access log into
the sink itself, tied to the application it served. Until now each
record went to `ferund` (`PublishAccessLogs`, the table `access_logs`,
ADR 0021), beside the sink and out of reach of a search of the
application's logs.

- **A request is a line of the service's.** `ferun-ingress` keeps a
  stream per (instance of the ingress, its start, environment, service)
  and writes each request as one JSON line: time, method, host, path
  (without its query, as now), status, outcome, duration, time held,
  attempts, bytes in and out, client address, user agent, the instance
  that answered, and `trace_id`, `span_id`, `parent_span_id`, with
  `level` from the outcome (5xx and aborted `error`, 4xx `warn`, else
  `info`). The stream's labels are the service's: `ferun.tenant`,
  `ferun.environment`, `ferun.kind` `service`, `ferun.object`, and
  `ferun.source` `ingress`, which a container's stream has not. A search
  of a service finds its requests beside its containers' lines, "errors
  only" finds its 5xx too, and the trace index (§5) ties a request to
  the lines its handling wrote: an application that logs the
  `traceparent` the ingress sent is found by it.
- **The ingress ships it, not `ferund`.** The agent exports its batches
  over OTLP as the nodes do (§3), from its buffer (10,000 records, as
  now), with its ingress certificate (`spiffe://<td>/ingress/<name>`).
  The stream's key is the instance's name, the environment and service,
  and the start, and a record's offset its number in that stream, so a
  batch sent again is stored once. The sink takes streams with
  `ferun.source` `ingress` from an ingress's certificate only, and a
  node's streams from a node's only. The ingress is given the sink's
  address in its own `--sink-endpoint`; it runs in the cluster's network
  and resolves the headless Service's name itself.
- **`ferund` keeps the API, not the table.** `…/access-logs` reads the
  service's ingress lines from the sink and answers them as it does now,
  and the trace filter becomes the sink's `trace`. Without a sink, the
  ingress sends to `ferund` as before. `access_logs` is read for its
  retention after the switch, then dropped (N−1).
- **FeCode** shows a service's requests in its Logs tab as a source of
  their own beside its replicas, a request's trace ID as a link to the
  lines of its trace (FeCode ADR 0009 §12).

Traces, next: the sink takes OTLP traces (`TraceService/Export`) from
pods, each with its certificate (`spiffe://<td>/tenant/<t>/sandbox/<id>`,
ADR 0014), labelled from the stream of that sandbox, so a pod cannot
write as another; hot in `spans`, cold in chunks, indexed by trace ID.
The ingress's request lines then become its server spans as well. That
is a step of its own, with its own decision on sampling.

### 9. Audit records, next: the same store, never dropped

An audit record is what a principal did: FeRun's own (a tenant created
an environment, a release went out), and a tenant's (FeCode's admin
actions) sent as OTLP log records with `event.name` and the actor. They
are a kind of their own: no rate limit drops them (a refused batch is
kept and sent again), they are kept for `--audit-retention` (one year),
and a tenant cannot delete them. Which of FeRun's calls write one is
decided with that step.

### Steps

1. **The guest's format.** CRI lines with time and stream,
   `StreamContainerLogs` with `from_offset`, and entries with offset,
   time and stream; the tails read the new format and the old one.
2. **The sink.** The program, its database (`log_streams`, `log_lines`,
   `log_chunks`), OTLP log ingest with node certificates, `GET
   /api/v1/logs` from the hot stage, `follow`; `GET /api/v1/tenant` in
   `ferund`. Deployed beside `ferund` with two instances.
3. **The node.** `feosd` follows and exports from the stored offset,
   drains before pause and delete, applies the rate limit;
   `CreatePodRequest.labels`.
4. **The cold stage.** Chunks, the object store, queries across both
   stages, retention, quota, `logRetentionDays`.
5. **Levels and trace IDs** (§5, §6).
6. **`ferund` reads from the sink**: the logs call, a job's run's log; a
   run's `log_tail` stops being written.
7. **The ingress's access log** (§8): streams per service with
   `ferun.source` `ingress`, exported by `ferun-ingress` with its
   certificate, `…/access-logs` read from the sink, FeCode's Logs tab
   showing requests and their traces.
8. Later and decided apart: traces (§8), audit records (§9).

## Alternatives considered

- **Into `ferund`.** It has the database, the tenants and the nodes'
  connections, and `access_logs` is there already. Not chosen, on Malte's
  word: logs are a write path of their own with their own load, and the
  control plane should not carry it, nor fail with it. Traces and audit
  records belong in the same place, and that place is not the control
  plane.
- **Grafana Loki.** It does all of §4 and §5: ingesters with a
  write-ahead log, chunks in S3, a TSDB label index, LogQL. Not chosen:
  it is a stateful cluster of its own (a ring, ingesters, a compactor)
  beside the PostgreSQL that is already run replicated, its tenancy
  would be mapped from FeRun's anyway, and it is AGPL-3.0. The labels
  here are fixed and few, so its generality buys little. If the volume
  outgrows a PostgreSQL hot stage, the sink's OTLP ingest lets an
  exporter point at Loki instead.
- **VictoriaLogs** (Apache-2.0): its own disks, no object store.
  **Elasticsearch, OpenSearch**: a full-text index nobody needs. **An
  OpenTelemetry Collector** in front: it ships and transforms but stores
  nothing. It may stand between nodes and the sink later, since both ends
  speak OTLP.
- **FeCode collects** by following each container through `ferund`: a
  tenant's logs would depend on its client being up, over long streams
  per container through the control plane, and only FeCode would have
  them.
- **The guest ships** directly: a guest has no credentials for the
  platform's services, and the node already reads its log.
- **Labels from the pod** (Loki's free labels): a pod could make streams
  without bound and claim another object's name. Its own fields stay in
  its lines (§6).
- **The hot stage in memory with a write-ahead log** per sink instance:
  the instances would have to share it, which the database does.

## Consequences

- A log outlives its pod: a crashed or replaced instance's last lines, a
  paused service's, a build's whole log, for the retention.
- A line costs a row in PostgreSQL for minutes, then about a tenth of
  its size in the object store. Batch inserts (`COPY`) keep the write
  path cheap. The hot stage's size is the lines of ten minutes.
- `ferund` changes little: labels on a pod's create, `GET
  /api/v1/tenant`, and later its logs calls read from the sink. Its own
  log tails stay as the fallback while the sink is down.
- FeCode drops its own build-log upload (FeCode ADR 0009 §12) once step
  4 is live.
- Searching is a scan. A query over a month of a busy service reads a
  lot. Its limit (§5) makes it answer in pieces, not time out.
- The guest's log format changes. A node and guest of before keep the
  old format, and their pods' logs reach the sink from step 3 on only.

## Open questions

1. Whether attached services' process logs (a workspace's sandbox) are
   streams too, or stay with the workspace.
2. Sampling of traces, and whether the ingress's spans of all requests
   are kept or a sample.
3. Which of `ferund`'s calls write audit records, and whether they are
   signed or chained.
