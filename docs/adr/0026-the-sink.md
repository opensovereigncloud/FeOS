# ADR 0026: The sink — a program of its own that keeps what the platform and its pods say: logs first, traces and audit records after, in the database first and in the object store after

**Status:** Accepted (Malte, 2026-10-30), steps 1 to 7 built and live on shoot `fecode` since 2026-10-31; §10, Quickwit as the index and the cold store, accepted 2026-10-31 (Malte); §11, Kafka as the log of record in place of the database's hot stage, accepted 2026-10-31 (Malte) · **Date:** 2026-10-30

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

*Changed on 2026-10-31 (§10): the cold stage is Quickwit's. The hot
stage stays as below, but holds a line only until Quickwit has published
it, seconds rather than minutes; there are no chunks of the sink's own.*

*Changed on 2026-10-31: the hot stage is a row per batch, not per line
(`log_batches`). A stream's part of an ingested batch is one row: its
lines encoded in one column, which PostgreSQL compresses, beside their
first and last offset, first and last time, count, bytes and level bits,
which the reads and the indexer select by. A row a line cost an insert,
two index entries, their WAL and its replication, then a delete, a dead
tuple and vacuum, for every line; at a company's volume that made the
database the bottleneck. The indexer takes whole batches and deletes
them; a read filters a batch's lines itself. `log_lines` stays until the
instances that wrote it are gone; the indexer drains what they left.*

**Hot.** A batch lands in `log_lines` in the sink's PostgreSQL, one row
per line (stream, offset, time, source, level, text), partitioned by day
like `ferund`'s `traffic_records`. Recent reads and following a log come from here.

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

*Changed on 2026-10-31 (§10): Quickwit indexes every word of a line and
every label, level and trace ID; the sink keeps only the streams'
table. `log_traces` and the level bits per chunk are gone.*

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
  and the trace filter becomes the sink's `trace`. *Changed on
  2026-10-31, by Malte's decision that no history is needed:* the sink
  is the only place. `access_logs` was dropped the same day with its
  partitions (migration `0020`), its records not moved and not read for
  their retention; `PublishAccessLogs` and `--access-log-retention` are
  gone. Without `--sink-url`, or with the sink down, `…/access-logs`
  answers 503 (`no_log_store`, `log_store_unavailable`); an ingress
  without `--sink-endpoint` keeps no access log and counts its records
  as dropped.
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

### 10. Quickwit is the index and the cold store (2026-10-31)

Decided by Malte on 2026-10-31, when two things changed: traces are
wanted for certain, not later and maybe, and the platform is to be
rolled out to a large organisation's teams, so the volume is a
company's, not a lab's. The
sink's own chunks answer a selector and a time well and a word poorly:
a search over weeks is a scan (§5). A trace store of its own would be a
second program of that kind, with columns, filters per block and
merges, which is what Grafana Tempo is. [Quickwit](https://quickwit.io)
(0.9, Apache-2.0) is both, for logs and traces: an index of every word
in immutable splits in the object store, its metadata in PostgreSQL,
merges and retention of its own, OTLP for logs and traces, and a
Jaeger-compatible API for traces.

- **The sink stays the gate.** Nothing else reaches Quickwit: it has no
  authentication of its own, so a NetworkPolicy lets only `ferun-sink`
  in. The sink keeps what is FeRun's: the nodes' and ingresses'
  certificates (§3, §8), the labels `ferund` sets (§2), the streams with
  their positions and the rule that a line is stored once per (stream,
  offset), the hot stage and `follow`, the quota, the tenants' reads.
  `GET /api/v1/logs` keeps its parameters and its answer.
- **A line goes to Quickwit once it is stored.** The hot stage is the
  source of truth until Quickwit has the line. The cold stage of §4
  becomes the indexer: it claims streams with hot lines (`FOR UPDATE
  SKIP LOCKED`, as the chunks were claimed), sends their lines to
  Quickwit's ingest with `commit=wait_for`, which answers once the split
  that holds them is published in the bucket and the metastore, and only
  then moves the stream's mark (`chunked_last`) past them and deletes them
  from `log_lines`, in one transaction. Quickwit's write-ahead log is not
  synced to disk and does not survive a lost volume; waiting for the
  publish makes that not matter. A line is searchable at most a commit's
  interval (`commit_timeout_secs`, 10 s) after it is stored, and readable
  from the hot stage at once.
- **Once in the store, as far as reads tell.** Quickwit keeps no key of
  its own: a batch sent again after an answer that did not arrive is
  indexed twice. Each document carries its stream and offset, and a read
  drops a line it has already seen, and one above its stream's mark (that
  one is still in the hot stage of its snapshot). A reader sees each line
  once; a count over Quickwit's documents may not.
- **A document is a line with its stream's labels.** Fields: `ts` (ns,
  the time field), `tenant` (a tag, so a tenant's search prunes splits),
  `stream`, `environment`, `kind`, `object`, `replica`, `release`,
  `instance`, `container`, `job`, `attempt`, `source`, `iostream`,
  `level`, `trace_id`, `off`, `partial`, `cont`, and `text` with every
  word and its position, so a phrase is found as such.
- **An index per retention.** Quickwit drops whole splits by age, per
  index, so a stream's retention picks its index: `ferun-logs-30d` for
  `--log-retention`, `ferun-logs-400d` for a stream that asks for more,
  up to `--log-retention-max`. A stream that asks for 90 days is kept for
  400; never shorter than asked. A read searches `ferun-logs-*`.
- **Partitions, and a version in the name.** *Changed on 2026-10-31:*
  one tenant holds many projects' environments (FeCode's), so splits
  that mix them all let a search for one environment prune none. An
  index hashes its lines into partitions by tenant, environment and job
  (`hash_mod((tenant,environment,job), 16)`, `--index-partitions`), a
  split holds one partition's, and the three are tags, so such a search
  reads only the splits that have its environment or job. Quickwit
  changes neither an index's partition key nor, for its splits, its
  mapping, so an index's name carries the version of its configuration:
  `ferun-logs-v2-30d` and `ferun-logs-v2-400d` take the new lines, and
  the indexes of before (`ferun-logs-30d`, `ferun-logs-400d`, version 1)
  are still read through `ferun-logs-*` and get every delete task, until
  their own retention has emptied them.
- **What a search is.** A read selects its streams in the database as
  before (their labels, their marks, the hot lines in one snapshot) and
  asks Quickwit for the cold part: the selector's labels, the time range,
  the levels, the trace, and each word of `q` as a phrase in `text`,
  `-word` as one that must not be there, newest or oldest first, in pages.
  The words now match whole words, as in every search engine: `erro`
  finds no `error`. The sink then applies the filters exactly as before
  (substring, `re`), so what Quickwit found is narrowed, never widened. A
  read bounded by `ScanLimit` and a cursor answers in pieces as before.
- **Trace IDs.** The sink takes a record's own `trace_id`, `span_id` and
  severity when the record has them (OTLP's fields), else what §6 reads
  from the line. Quickwit indexes `trace_id`, so `log_traces` goes.
- **Deleting** a tenant's environment or job deletes its streams' rows
  at once, so no read finds their lines any more, and leaves a delete
  task in Quickwit, which removes the documents when it rewrites their
  splits (Quickwit's janitor, within two days).
- **What was in chunks** moves into Quickwit once, chunk by chunk, after
  which the chunk's object and row go. The sink keeps its bucket only
  for that, and drops it with the last chunk (done on 2026-10-31: all
  74 chunks moved, `log_chunks` dropped).
- **Traces**, next and decided with this: pods send OTLP to their node
  (`169.254.169.1:4317` and `:4318`, a node address of its own, held as
  DNS's `169.254.0.53` is; not the pods' gateway `169.254.0.1`, which is
  the node's own gateway on IronCore and which the node never holds);
  the node knows the pod by its source address, stamps its `ferun.*`
  labels as resource attributes and sends on to the sink with its
  certificate, as it does logs (§3). The sink keeps spans in its hot
  stage (`trace_spans`) until its indexer has them in Quickwit's trace
  index (`ferun-traces-v1-30d`, a mapping of the sink's own with the
  labels, partitioned by tenant, environment and job), and answers
  Jaeger's query API for a tenant itself, the tenant's filter forced;
  Quickwit's own OTLP and Jaeger endpoints stay off. The ingress's
  request lines become its server spans there. Every span is kept for
  now; sampling stays open (open question 2). The contract is
  docs/architecture/traces.md. *Decided by Malte on 2026-10-31: the hot
  stage for spans, OTLP over gRPC and HTTP/protobuf (no JSON), no
  sampling, and the address `169.254.169.1`.*
- **Deployment.** Quickwit 0.9.3 in the namespace `ferun`: one metastore,
  one control plane, one janitor, one indexer on a persistent volume, two
  searchers; its metastore a database `quickwit` in `ferun-db`, its
  splits in the sink's bucket under `quickwit/`. Its chart (0.8.17) runs
  the 0.9.3 image; R2 takes it without request checksums.

### 11. Kafka is the log of record; the database keeps no line (2026-10-31)

Decided by Malte on 2026-10-31, after a look at the path as built: a
PostgreSQL in the middle of the ingest is a queue, a lock service and a
live tail at once, it shares its cluster with `ferund`'s (§1 says logs
must not burden the control plane), every line is written twice (its
write-ahead log and the index), and the sink's indexer rebuilds by hand
what Quickwit does for a Kafka topic by itself: it keeps a topic's
offsets in its metastore in the same transaction that publishes a split,
so each record is indexed exactly once. Traces had the same shape again
(`trace_spans` and an indexer of their own).

*Built first on Redpanda, then moved the same day, on Malte's word, to
Apache Kafka: Redpanda's Community Edition is under the Business Source
License, which forbids a service that lets third parties cause the
creation of a topic, directly or indirectly. A log service sold to
customers comes close enough to that for doubt, and the architecture
should not have to bend around a licence. Kafka is Apache-2.0, run by
Strimzi (Apache-2.0); the sink speaks the Kafka protocol either way.*

```
guest container.log ─▶ feosd ──OTLP (node cert)──▶ ferun-sink ──acks=all──▶ Kafka topic ─▶ Quickwit Kafka source ─▶ splits (bucket)
ferun-ingress ───────────────OTLP (ingress cert)──▶    │  gate: peers, labels,                │ ▲ read back: the tail
pods ─OTLP─▶ feosd (169.254.169.1) ───────────────▶    │  quota, positions                    │ │ Quickwit has not
                                                       ▼                                       │ │ committed, and follow
                                              PostgreSQL: streams, positions, quota ◀─────────┘ │
tenants ── GET /api/v1/logs, /api/v1/jaeger ─▶ ferun-sink: Quickwit + the topic's tail ─────────┘
```

- **One log for every signal.** A topic per index: `<prefix>-v<version>-<days>d`, the
  index's own name (`ferun-logs-v5-30d`, `ferun-logs-v5-400d`,
  `ferun-traces-v4-30d`, 64 partitions each), each indexed by one Kafka source of its index.
  The topics are a replay buffer, kept a day (`--kafka-retention`); the
  index is the store. Kafka (three nodes in KRaft mode, replication
  three, `min.insync.replicas` two) answers a produce (`acks=all`) once
  two replicas have it.
- **The topics are the platform's.** The sink makes its topics, indexes
  and sources at its start, a fixed set (two log classes and the
  traces), and nothing a tenant does ever makes one: an ingest or a read
  of a topic that is not there fails. A tenant never reaches Kafka.
- **The sink stays the gate, and keeps no line.** It checks the peers
  (§3, §8), takes the labels, counts the quota, and for a log stream
  keeps its row: labels, first and last time, `next_offset` for the
  node, and the rule that a line at or below the last offset stored is
  stored. It locks the stream, takes the new lines, produces them, and
  answers the node only after the produce and the commit of the stream's
  row. A batch the node sends again stores nothing; one sent again after
  an answer that did not arrive may lie in the topic twice, which the
  reads drop (one (stream, offset), one span ID). `log_batches`,
  `log_lines` and `trace_spans` and both indexers go.
- **The partition is the reader's address.** A log record's key is its
  stream's object: tenant, environment, kind and object (a tenant's job:
  tenant and job), the partition the key's hash, of 64: a stream's lines
  stay in order, an object's replicas and releases share a partition,
  and a follow of a service reads one partition per topic, which a busy
  neighbour of the same environment rarely shares. A read of a whole
  environment reads the partitions of its objects. A span's key is its
  trace ID: a trace is read from one partition. *(First keyed by tenant
  and environment; on Malte's word the same day keyed by the object, as
  a follow is of one service.)*
- **Reading is the index and the tail.** A read asks Quickwit for the
  source's checkpoint first (per partition, what it has published), then
  searches the index as before, then reads the topic's partitions of
  its selector from the checkpoint to their end: the lines Quickwit has
  not committed, seconds of them. Every line is in one of the two, some
  in both; the read takes each (stream, offset) once. The trace by ID
  reads its trace's partition the same way, so a fresh request's trace
  is found at once; the searches of traces read the index alone, so a
  span is found by a search after a commit (5 s).
- **Following is the topic, of one object.** A follow reads its object's
  partition from its end as records come (a fetch waits up to half a
  second), after the backward page it gives first; a line it gave on
  that page is not given again. No database is polled. A follow is of
  one service (or job, or tenant's job, or one of its instances or
  streams); an environment as a whole is searched, not followed. A
  follow gives at most 100 lines a second (a burst of 500), then 50 more
  a second of error and warn lines, and says once a second how many it
  skipped (`{"skipped": n}`), which FeCode shows: a screen can show no
  more, and a noisy service must not starve the reader.
- **On the nodes, nothing changes, on purpose.** The guest's
  `container.log` is the node's buffer and stays its source (§3): feosd
  follows it from the position the sink keeps and sends one batch a
  stream at a time. A node producing to Kafka itself was weighed and
  not chosen: Kafka's ACLs know a client, not a tenant or a stream, so
  neither the rule that a node writes only container streams and an
  ingress only its own, nor a tenant's quota, could be kept there; the
  position a moved or restored pod's stream goes on from must be kept
  beside the node anyway; and feosd would carry a Kafka client of its
  own. The gate stays one program with one certificate check.
- **Deployment.** Kafka 4.3.1 run by Strimzi (its operator in the
  namespace `strimzi`, the cluster `ferun-log` in `ferun`): three nodes,
  each a controller and a broker, one a worker, a volume each, its
  listener reached only by the sink and Quickwit (Strimzi's
  NetworkPolicy, no TLS or SASL inside the cluster).
- **The move.** The new indexes' sources fill them from the topics. What
  lay hot in the database when the new sink started was produced to the
  topics once and deleted, and the tables went (migration `0008`); the
  indexes of before, and Redpanda, were deleted on 2026-10-31, on
  Malte's word, with what they held.

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
9. **Quickwit** (§10): Quickwit deployed beside the sink; the indexer in
   place of the chunks, reads, joins and follows from Quickwit,
   deletion as delete tasks, the indexes per retention, the chunks moved
   once, `log_traces` dropped, OTLP's own trace IDs and severities.
10. **Traces** (§10): the node's OTLP listener for pods, spans through the
    sink into Quickwit, the Jaeger API per tenant, the ingress's spans.
11. **Kafka** (§11): the topics, the produce in place of the hot
    stage, the Kafka sources, reads and follows from the index and the
    tail, the move of the hot rows; then the tables' drop.

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
- **The sink's own chunks for good** (steps 4 and 5, until 2026-10-31):
  fine for a selector and a time, a scan for a word, and no place for
  traces without building a trace store as well. Replaced by §10.
- **The database as the hot stage** (§4, §10, until 2026-10-31): one
  transaction for positions, quota and lines, but a queue in the control
  plane's database cluster, every line written twice, and an indexer of
  our own beside Quickwit's Kafka source. Replaced by §11. **Apache
  Kafka** or **Redpanda**: the same API; Redpanda is one binary without a
  JVM, but under the Business Source License (§11), so Kafka, by
  Strimzi. **feosd producing to the topics
  itself**: see §11, on the nodes.
- **Grafana Tempo** for traces (and Loki for logs): AGPL-3.0, and a
  cluster each with a ring, ingesters and compactors. **ClickHouse**
  (with an OpenTelemetry schema): strong where traces are aggregated
  (latencies, rates), but it keeps its data on disks of its own with
  replicas and Keeper; the object store as its main storage is awkward
  in the open-source build. The choice if analytics over spans comes to
  matter more than search. **Jaeger** stores nothing in an object store
  itself.

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
- Searching was a scan (§5). With Quickwit (§10) a word is looked up in
  an index; only `re` and substrings are still scanned, within what the
  other filters left.
- Quickwit is a program the platform runs and watches: five kinds of
  pods, a database, a volume for its indexer, and a NetworkPolicy that is
  its only lock.
- The guest's log format changes. A node and guest of before keep the
  old format, and their pods' logs reach the sink from step 3 on only.

## Open questions

1. Whether attached services' process logs (a workspace's sandbox) are
   streams too, or stay with the workspace.
2. Sampling of traces, and whether the ingress's spans of all requests
   are kept or a sample.
3. Which of `ferund`'s calls write audit records, and whether they are
   signed or chained.
