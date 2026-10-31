// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package kafka is the sink's log of record (ADR 0026 §11): topics on a
// Kafka API (Redpanda in the reference deployment) that the sink appends
// to, Quickwit's Kafka sources index from, and the sink reads the newest
// records of back. The sink picks a record's partition itself (Partition),
// so that a reader knows where a key's records are.
package kafka

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// Record is a record of a topic: its key and value, and where it lies
// (set by a read).
type Record struct {
	Partition int32
	Offset    int64
	Key       []byte
	Value     []byte
}

// TopicConfig is how a topic is made: its partitions, its retention
// (the replay buffer, not the store), its replication.
type TopicConfig struct {
	Partitions  int32
	Retention   time.Duration
	Replication int16
}

// Client is a producer, a reader of partitions and an admin of topics
// of one cluster.
type Client struct {
	seeds []string
	cl    *kgo.Client
	adm   *kadm.Client
}

// New returns a client of the brokers seeds: an idempotent producer that
// waits for every in-sync replica (acks=all), the record's partition the
// caller's.
func New(seeds []string) (*Client, error) {
	if len(seeds) == 0 {
		return nil, errors.New("kafka: no brokers")
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(seeds...),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.RecordPartitioner(kgo.ManualPartitioner()),
		kgo.ProducerBatchCompression(kgo.ZstdCompression()),
		kgo.ProducerLinger(2*time.Millisecond),
		kgo.ProducerBatchMaxBytes(4<<20),
		kgo.RecordDeliveryTimeout(20*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("kafka: %w", err)
	}
	return &Client{seeds: seeds, cl: cl, adm: kadm.NewClient(cl)}, nil
}

// Close closes the client.
func (c *Client) Close() { c.cl.Close() }

// Partition is the partition of key among n: FNV-1a, the same for every
// writer and reader.
func Partition(key string, n int32) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int32(h.Sum32() % uint32(n))
}

// EnsureTopic makes the topic unless it exists; an existing one keeps
// its settings.
func (c *Client) EnsureTopic(ctx context.Context, topic string, tc TopicConfig) error {
	// Retention deletes closed segments only: a segment closes after an
	// hour, so that a record goes about an hour after its retention, not
	// weeks. Records are synced to disk before a produce is answered.
	retention := strconv.FormatInt(tc.Retention.Milliseconds(), 10)
	cleanup, segment, caching := "delete", "3600000", "false"
	resp, err := c.adm.CreateTopic(ctx, tc.Partitions, tc.Replication, map[string]*string{
		"retention.ms": &retention, "cleanup.policy": &cleanup, "segment.ms": &segment, "write.caching": &caching,
	}, topic)
	if err == nil {
		err = resp.Err
	}
	if errors.Is(err, kerr.TopicAlreadyExists) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("kafka: topic %s: %w", topic, err)
	}
	return nil
}

// Partitions is how many partitions topic has.
func (c *Client) Partitions(ctx context.Context, topic string) (int32, error) {
	md, err := c.adm.Metadata(ctx, topic)
	if err != nil {
		return 0, fmt.Errorf("kafka: metadata %s: %w", topic, err)
	}
	t, ok := md.Topics[topic]
	if !ok || t.Err != nil {
		return 0, fmt.Errorf("kafka: topic %s: %v", topic, t.Err)
	}
	return int32(len(t.Partitions)), nil
}

// Produce appends recs (each with its Partition) to topic and returns once
// every in-sync replica has them all; a record not taken fails the call.
func (c *Client) Produce(ctx context.Context, topic string, recs []Record) error {
	rs := make([]*kgo.Record, len(recs))
	for i, r := range recs {
		rs[i] = &kgo.Record{Topic: topic, Partition: r.Partition, Key: r.Key, Value: r.Value}
	}
	if err := c.cl.ProduceSync(ctx, rs...).FirstErr(); err != nil {
		return fmt.Errorf("kafka: produce to %s: %w", topic, err)
	}
	return nil
}

// Ends are the offsets after the last record of each of topic's
// partitions. A partition whose leader moved (stale metadata) is asked
// again after the metadata are refreshed, three times at most.
func (c *Client) Ends(ctx context.Context, topic string) (map[int32]int64, error) {
	var errs []error
	for try := 0; try < 3; try++ {
		if try > 0 {
			c.cl.ForceMetadataRefresh()
			t := time.NewTimer(time.Duration(try) * 200 * time.Millisecond)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-t.C:
			}
		}
		lo, err := c.adm.ListEndOffsets(ctx, topic)
		if err != nil {
			errs = []error{err}
			continue
		}
		out := map[int32]int64{}
		errs = errs[:0]
		lo.Each(func(o kadm.ListedOffset) {
			if o.Err != nil {
				errs = append(errs, o.Err)
				return
			}
			out[o.Partition] = o.Offset
		})
		if len(errs) == 0 {
			return out, nil
		}
		if !retriable(errs) {
			break
		}
	}
	return nil, fmt.Errorf("kafka: end offsets of %s: %w", topic, errors.Join(errs...))
}

// retriable reports whether every error is one the next try may not have.
func retriable(errs []error) bool {
	for _, err := range errs {
		if !kerr.IsRetriable(err) {
			return false
		}
	}
	return true
}

// Read reads topic's records of each partition of from, from its offset
// up to (not including) its offset in to, at most max records; a
// partition whose range is empty is skipped.
func (c *Client) Read(ctx context.Context, topic string, from, to map[int32]int64, max int) ([]Record, error) {
	want := map[int32]kgo.Offset{}
	for p, f := range from {
		if t, ok := to[p]; ok && t > f {
			want[p] = kgo.NewOffset().At(f)
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(c.seeds...),
		kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{topic: want}),
		kgo.FetchMaxWait(200*time.Millisecond))
	if err != nil {
		return nil, fmt.Errorf("kafka: %w", err)
	}
	defer cl.Close()
	// A range ends at its last record, or, should offsets skip (control
	// records), once two polls in a row bring nothing more.
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out []Record
	open := len(want)
	done := map[int32]bool{}
	empty := 0
	for open > 0 && len(out) < max && empty < 2 {
		fs := cl.PollFetches(rctx)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if rctx.Err() != nil {
			return nil, fmt.Errorf("kafka: read %s: %w", topic, rctx.Err())
		}
		if fs.NumRecords() == 0 {
			empty++
		} else {
			empty = 0
		}
		var errs []error
		fs.EachError(func(_ string, _ int32, err error) { errs = append(errs, err) })
		if len(errs) > 0 {
			return nil, fmt.Errorf("kafka: read %s: %w", topic, errors.Join(errs...))
		}
		fs.EachRecord(func(r *kgo.Record) {
			if done[r.Partition] || r.Offset >= to[r.Partition] {
				return
			}
			out = append(out, Record{Partition: r.Partition, Offset: r.Offset, Key: r.Key, Value: r.Value})
			if r.Offset >= to[r.Partition]-1 {
				done[r.Partition] = true
				open--
			}
		})
	}
	return out, nil
}

// Follower reads partitions of a topic from given offsets on, as records
// come.
type Follower struct {
	cl *kgo.Client
}

// Follow starts reading topic's partitions of from at their offsets.
func (c *Client) Follow(topic string, from map[int32]int64) (*Follower, error) {
	want := map[int32]kgo.Offset{}
	for p, f := range from {
		want[p] = kgo.NewOffset().At(f)
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(c.seeds...),
		kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{topic: want}),
		kgo.FetchMaxWait(500*time.Millisecond))
	if err != nil {
		return nil, fmt.Errorf("kafka: %w", err)
	}
	return &Follower{cl: cl}, nil
}

// Next returns the records that came, waiting at most until ctx ends or a
// fetch's wait is over (then none).
func (f *Follower) Next(ctx context.Context) ([]Record, error) {
	fs := f.cl.PollFetches(ctx)
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, nil
	}
	var errs []error
	fs.EachError(func(_ string, _ int32, err error) { errs = append(errs, err) })
	if len(errs) > 0 {
		return nil, fmt.Errorf("kafka: follow: %w", errors.Join(errs...))
	}
	var out []Record
	fs.EachRecord(func(r *kgo.Record) {
		out = append(out, Record{Partition: r.Partition, Offset: r.Offset, Key: r.Key, Value: r.Value})
	})
	return out, nil
}

// Close stops the follower.
func (f *Follower) Close() { f.cl.Close() }
