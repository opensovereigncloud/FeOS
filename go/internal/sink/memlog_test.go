// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sink

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gardenlinux/feos/go/internal/sink/kafka"
	"github.com/gardenlinux/feos/go/internal/sink/quickwit"
)

// memLog is the log of record in memory: topics of partitions of records,
// as Kafka keeps them.
type memLog struct {
	mu          sync.Mutex
	topics      map[string][][]kafka.Record
	failProduce error
	produces    int
	// changed is closed and made anew on each produce, for followers.
	changed chan struct{}
}

func newMemLog() *memLog {
	return &memLog{topics: map[string][][]kafka.Record{}, changed: make(chan struct{})}
}

func (m *memLog) EnsureTopic(_ context.Context, topic string, tc kafka.TopicConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.topics[topic]; !ok {
		m.topics[topic] = make([][]kafka.Record, tc.Partitions)
	}
	return nil
}

func (m *memLog) Partitions(_ context.Context, topic string) (int32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.topics[topic]
	if !ok {
		return 0, fmt.Errorf("no topic %s", topic)
	}
	return int32(len(t)), nil
}

func (m *memLog) Produce(_ context.Context, topic string, recs []kafka.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failProduce != nil {
		return m.failProduce
	}
	t, ok := m.topics[topic]
	if !ok {
		return fmt.Errorf("no topic %s", topic)
	}
	for _, r := range recs {
		if r.Partition < 0 || int(r.Partition) >= len(t) {
			return errors.New("no such partition")
		}
		r.Offset = int64(len(t[r.Partition]))
		t[r.Partition] = append(t[r.Partition], r)
	}
	m.produces++
	close(m.changed)
	m.changed = make(chan struct{})
	return nil
}

func (m *memLog) Ends(_ context.Context, topic string) (map[int32]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.topics[topic]
	if !ok {
		return nil, fmt.Errorf("no topic %s", topic)
	}
	out := map[int32]int64{}
	for p, rs := range t {
		out[int32(p)] = int64(len(rs))
	}
	return out, nil
}

func (m *memLog) Read(_ context.Context, topic string, from, to map[int32]int64, max int) ([]kafka.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.topics[topic]
	var out []kafka.Record
	for p, f := range from {
		for o := f; o < to[p] && o < int64(len(t[p])) && len(out) < max; o++ {
			out = append(out, t[p][o])
		}
	}
	return out, nil
}

func (m *memLog) Follow(topic string, from map[int32]int64) (Follower, error) {
	at := map[int32]int64{}
	for p, o := range from {
		at[p] = o
	}
	return &memFollower{log: m, topic: topic, at: at}, nil
}

// memFollower follows partitions of a memLog.
type memFollower struct {
	log   *memLog
	topic string
	at    map[int32]int64
}

func (f *memFollower) Next(ctx context.Context) ([]kafka.Record, error) {
	for {
		f.log.mu.Lock()
		t, changed := f.log.topics[f.topic], f.log.changed
		var out []kafka.Record
		for p, o := range f.at {
			for ; o < int64(len(t[p])); o++ {
				out = append(out, t[p][o])
			}
			f.at[p] = o
		}
		f.log.mu.Unlock()
		if len(out) > 0 {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-changed:
		case <-time.After(100 * time.Millisecond):
			return nil, nil
		}
	}
}

// Add reads the partitions of from too.
func (f *memFollower) Add(from map[int32]int64) {
	f.log.mu.Lock()
	defer f.log.mu.Unlock()
	for p, o := range from {
		f.at[p] = o
	}
}

func (f *memFollower) Close() {}

// count is how many records topic holds.
func (m *memLog) count(topic string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, rs := range m.topics[topic] {
		n += len(rs)
	}
	return n
}

// records are the records of topic, partition by partition.
func (m *memLog) records(topic string) []kafka.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []kafka.Record
	for _, rs := range m.topics[topic] {
		out = append(out, rs...)
	}
	return out
}

// memSources are the Kafka sources of indexes in memory: each index's
// topic and checkpoint (the next offset of each partition), moved on by
// pull, as Quickwit's source does at a commit.
type memSources struct {
	mu     sync.Mutex
	topics map[string]string
	cps    map[string]map[int32]int64
}

func (s *memSources) ensure(id string, src quickwit.Source) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.topics == nil {
		s.topics, s.cps = map[string]string{}, map[string]map[int32]int64{}
	}
	if _, ok := s.topics[id]; !ok {
		s.topics[id], s.cps[id] = src.Topic, map[int32]int64{}
	}
}

func (s *memSources) Checkpoint(_ context.Context, id string) (map[int32]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[int32]int64{}
	for p, o := range s.cps[id] {
		out[p] = o
	}
	return out, nil
}

// pull takes the records each index's source has not taken from lg and
// moves its checkpoint past them, calling add for each.
func (s *memSources) pull(lg *memLog, add func(id string, r kafka.Record)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, topic := range s.topics {
		ends, err := lg.Ends(context.Background(), topic)
		if err != nil {
			continue
		}
		recs, _ := lg.Read(context.Background(), topic, s.cps[id], ends, 1<<30)
		for p := range ends {
			if _, ok := s.cps[id][p]; !ok {
				recs2, _ := lg.Read(context.Background(), topic, map[int32]int64{p: 0}, ends, 1<<30)
				recs = append(recs, recs2...)
			}
			s.cps[id][p] = ends[p]
		}
		for _, r := range recs {
			add(id, r)
		}
	}
}
