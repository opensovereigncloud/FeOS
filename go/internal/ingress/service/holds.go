// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/gardenlinux/feos/go/internal/ingress/wire"
	"github.com/gardenlinux/feos/go/internal/podid"
	"github.com/gardenlinux/feos/go/internal/store"
)

// The holds' timings.
const (
	// holdLease is how long an ingress's hold lasts unless it holds the
	// instance again; the ingress renews its holds every 10 s.
	holdLease = 30 * time.Second
	// defaultHoldTimeout bounds each statement of a Hold: one admission,
	// one release, the leave.
	defaultHoldTimeout = 5 * time.Second
)

// holdInvalid is the refusal of an ID that is not a sandbox's.
const holdInvalid = "invalid"

// Hold holds and releases the instances the ingress sends requests to
// (§6.7 and D11 of the ingress's design), for the ingress of the
// connection's certificate: it records the ingress as seen (best
// effort); it admits each ID of Hold, once, as the ingress's hold of kind
// forward for holdLease, renewed by this instance
// (store.AdmitIngressHold, a transaction each), into Held or into Refused
// with the store's reason ("gone", "not public", "not running", "busy");
// it releases each ID of Release (store.ReleaseIngressHold); and, with
// Leaving, it releases every hold of the ingress and records that it left
// (store.LeaveIngress), after the releases. An ID that is not a
// sandbox's, in either list, is refused with the reason "invalid". A
// list of more than wire.MaxHoldIDs is INVALID_ARGUMENT; a database
// error, a statement's timeout included, is UNAVAILABLE, and the ingress
// sends the request again (each step can be repeated). While the
// instance stops it answers UNAVAILABLE.
func (s *Service) Hold(ctx context.Context, req *wire.HoldRequest) (*wire.HoldResponse, error) {
	p, err := ingressPeer(ctx)
	if err != nil {
		return nil, err
	}
	if s.stopping.Load() {
		return nil, errStopping()
	}
	if n := len(req.Hold); n > wire.MaxHoldIDs {
		return nil, status.Errorf(codes.InvalidArgument, "hold lists %d instances, more than %d", n, wire.MaxHoldIDs)
	}
	if n := len(req.Release); n > wire.MaxHoldIDs {
		return nil, status.Errorf(codes.InvalidArgument, "release lists %d instances, more than %d", n, wire.MaxHoldIDs)
	}
	s.seen(ctx, p.Name)
	holder := store.IngressHolder(p.Name)
	resp := &wire.HoldResponse{LeaseSeconds: int(holdLease / time.Second)}
	admitted := make(map[podid.ID]bool, len(req.Hold))
	for _, raw := range req.Hold {
		id, err := podid.Parse(raw)
		if err != nil {
			resp.Refused = append(resp.Refused, wire.HoldRefusal{Instance: raw, Reason: holdInvalid})
			continue
		}
		if admitted[id] {
			continue
		}
		admitted[id] = true
		refusal, err := s.admitHold(ctx, id, holder)
		if err != nil {
			return nil, dbError(ctx, err, "hold")
		}
		if refusal != "" {
			resp.Refused = append(resp.Refused, wire.HoldRefusal{Instance: raw, Reason: refusal})
			continue
		}
		resp.Held = append(resp.Held, raw)
	}
	for _, raw := range req.Release {
		id, err := podid.Parse(raw)
		if err != nil {
			resp.Refused = append(resp.Refused, wire.HoldRefusal{Instance: raw, Reason: holdInvalid})
			continue
		}
		if err := s.releaseHold(ctx, id, holder); err != nil {
			return nil, dbError(ctx, err, "release")
		}
	}
	if req.Leaving {
		lctx, cancel := context.WithTimeout(ctx, s.holdTimeout)
		err := s.st.LeaveIngress(lctx, p.Name)
		cancel()
		if err != nil {
			return nil, dbError(ctx, err, "leave")
		}
		s.log.Info("ingress left: its holds are released", "ingress", p.Name, "peer", p.Addr)
	}
	return resp, nil
}

// admitHold admits holder's hold on id, within s.holdTimeout.
func (s *Service) admitHold(ctx context.Context, id podid.ID, holder string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.holdTimeout)
	defer cancel()
	return s.st.AdmitIngressHold(ctx, id, holder, s.cfg.InstanceID, holdLease)
}

// releaseHold releases holder's hold on id, within s.holdTimeout.
func (s *Service) releaseHold(ctx context.Context, id podid.ID, holder string) error {
	ctx, cancel := context.WithTimeout(ctx, s.holdTimeout)
	defer cancel()
	_, err := s.st.ReleaseIngressHold(ctx, id, holder)
	return err
}
