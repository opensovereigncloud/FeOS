// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"io"
	"os"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	watchv1 "github.com/ironcore-dev/feos/go/gen/feos/watch/v1"
	"github.com/ironcore-dev/feos/go/internal/podid"
)

// watchLimit is the guest's cap on inotify watches, which acks report.
const watchLimit = 2048

// watchService is the guest's WatchService with scripted events: a stream
// opens on a container, every subscribe is answered with an ack that
// watches the paths that exist in the pod's directory, and the events are
// what the script sends (Script.WatchEvents, Script.WatchStale), numbered
// per stream from 1.
type watchService struct {
	watchv1.UnimplementedWatchServiceServer
	n *Node

	mu      sync.Mutex
	streams map[*watchStream]struct{}
}

// watchStream is one open Watch stream.
type watchStream struct {
	pod      podid.ID
	batches  chan []*watchv1.Event
	overflow bool // a batch did not fit: the next one is a stale event
}

func newWatchService(n *Node) *watchService {
	return &watchService{n: n, streams: map[*watchStream]struct{}{}}
}

// deliver queues events for every stream of the pod.
func (w *watchService) deliver(pod podid.ID, events []*watchv1.Event) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for s := range w.streams {
		if s.pod != pod {
			continue
		}
		n++
		batch := make([]*watchv1.Event, len(events))
		for i, ev := range events {
			batch[i] = proto.Clone(ev).(*watchv1.Event)
		}
		select {
		case s.batches <- batch:
		default:
			s.overflow = true
		}
	}
	return n
}

func (w *watchService) count(pod podid.ID) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for s := range w.streams {
		if s.pod == pod {
			n++
		}
	}
	return n
}

func (w *watchService) Watch(stream grpc.BidiStreamingServer[watchv1.WatchRequest, watchv1.WatchResponse]) error {
	ctx := stream.Context()
	g, err := guestFrom(ctx)
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err == io.EOF {
		return status.Error(codes.InvalidArgument, "stream ended before `open`")
	}
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "first frame must be `open`")
	}
	root, err := openRoot(g, open.Container)
	if err != nil {
		return status.Error(codes.NotFound, err.Error())
	}
	root.Close()

	s := &watchStream{pod: g.uid, batches: make(chan []*watchv1.Event, 64)}
	w.mu.Lock()
	w.streams[s] = struct{}{}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.streams, s)
		w.mu.Unlock()
	}()

	subs := make(chan []string)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}
			if sub := req.GetSubscribe(); sub != nil {
				select {
				case subs <- sub.Paths:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	var cursor uint64
	var paths []string
	send := func(events []*watchv1.Event) error {
		for _, ev := range events {
			cursor++
			ev.Id = cursor
		}
		return stream.Send(&watchv1.WatchResponse{Frame: &watchv1.WatchResponse_Events{Events: &watchv1.EventBatch{Events: events}}})
	}
	for {
		w.mu.Lock()
		overflow := s.overflow
		s.overflow = false
		w.mu.Unlock()
		if overflow {
			if err := send([]*watchv1.Event{{Kind: watchv1.EventKind_EVENT_KIND_STALE, StalePaths: paths}}); err != nil {
				return err
			}
		}
		select {
		case p := <-subs:
			paths = p
			if err := stream.Send(&watchv1.WatchResponse{Frame: &watchv1.WatchResponse_Ack{Ack: w.ack(g, open.Container, cursor, p)}}); err != nil {
				return err
			}
		case batch := <-s.batches:
			if err := send(batch); err != nil {
				return err
			}
		case <-ended:
			return nil
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}

// ack answers a subscribe: the paths that exist in the pod's directory
// are watched, the others fail.
func (w *watchService) ack(g *guest, container string, cursor uint64, paths []string) *watchv1.Ack {
	ack := &watchv1.Ack{Cursor: cursor, WatchLimit: watchLimit}
	root, err := openRoot(g, container)
	if err != nil {
		for _, p := range paths {
			ack.Failed = append(ack.Failed, &watchv1.PathError{Path: p, Error: err.Error()})
		}
		return ack
	}
	defer root.Close()
	for _, p := range paths {
		rel, err := toRel(p)
		if err == nil {
			_, err = root.Stat(osRel(rel))
		}
		if err != nil {
			if os.IsNotExist(err) {
				err = os.ErrNotExist
			}
			ack.Failed = append(ack.Failed, &watchv1.PathError{Path: p, Error: err.Error()})
			continue
		}
		ack.Watching = append(ack.Watching, p)
	}
	ack.Watches = uint32(len(ack.Watching))
	return ack
}
