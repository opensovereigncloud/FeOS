// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	volumev1 "github.com/gardenlinux/feos/go/gen/feos/volume/v1"
)

// feosd's bounds of a volume's size (volumes.rs).
const (
	minVolumeBytes  = 64 << 20
	maxVolumeBytes  = 16 << 40
	volumeSizeUnit  = 1 << 20
	volumeSizeError = "a volume has from 64 MiB to 16 TiB, in whole MiB"
)

// volumeService is feosd's VolumeService over records in memory, with
// feosd's rules (volumes.rs): a create is idempotent by the ID when the
// size and labels are the same; a pod claims its volumes at its create,
// all or none, each one the node has and no other pod holds; a deleted
// pod lets go of them (an attached one is available again, a checkpointed
// one stays so); a hibernation checkpoints them with the pod, and a
// restore takes them over from the checkpoint (check_restorable); a
// volume a pod has is not deleted. The test node keeps no data of its
// own for a volume: a pod's directory is its disk.
type volumeService struct {
	volumev1.UnimplementedVolumeServiceServer
	n *Node

	mu      sync.Mutex
	volumes map[string]*volumev1.Volume
}

func newVolumeService(n *Node) *volumeService {
	return &volumeService{n: n, volumes: map[string]*volumev1.Volume{}}
}

func checkVolumeID(id string) error {
	if u, err := uuid.Parse(id); err != nil || u.String() != id {
		return status.Errorf(codes.InvalidArgument, "volume ID %q is not a UUID in lowercase, with hyphens", id)
	}
	return nil
}

func cloneVolume(v *volumev1.Volume) *volumev1.Volume { return proto.Clone(v).(*volumev1.Volume) }

func sameLabels(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func (s *volumeService) CreateVolume(_ context.Context, req *volumev1.CreateVolumeRequest) (*volumev1.Volume, error) {
	id := req.GetVolumeId()
	if err := checkVolumeID(id); err != nil {
		return nil, err
	}
	size := req.GetSizeBytes()
	if size < minVolumeBytes || size > maxVolumeBytes || size%volumeSizeUnit != 0 {
		return nil, status.Errorf(codes.InvalidArgument, "%s: not %d bytes", volumeSizeError, size)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.volumes[id]; v != nil {
		if v.SizeBytes == size && sameLabels(v.Labels, req.GetLabels()) {
			return cloneVolume(v), nil
		}
		return nil, status.Errorf(codes.AlreadyExists, "volume %s exists with another size or labels", id)
	}
	v := &volumev1.Volume{VolumeId: id, SizeBytes: size, Labels: req.GetLabels(), State: volumev1.VolumeState_VOLUME_STATE_AVAILABLE}
	if v.Labels == nil {
		v.Labels = map[string]string{}
	}
	s.volumes[id] = v
	return cloneVolume(v), nil
}

func (s *volumeService) DeleteVolume(_ context.Context, req *volumev1.DeleteVolumeRequest) (*volumev1.DeleteVolumeResponse, error) {
	id := req.GetVolumeId()
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.volumes[id]
	if v == nil {
		return nil, status.Errorf(codes.NotFound, "no volume %s", id)
	}
	if v.PodId != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is attached to pod %s", id, v.PodId)
	}
	delete(s.volumes, id)
	return &volumev1.DeleteVolumeResponse{}, nil
}

func (s *volumeService) GetVolume(_ context.Context, req *volumev1.GetVolumeRequest) (*volumev1.Volume, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.volumes[req.GetVolumeId()]
	if v == nil {
		return nil, status.Errorf(codes.NotFound, "no volume %s", req.GetVolumeId())
	}
	return cloneVolume(v), nil
}

func (s *volumeService) ListVolumes(context.Context, *volumev1.ListVolumesRequest) (*volumev1.ListVolumesResponse, error) {
	return &volumev1.ListVolumesResponse{Volumes: s.list()}, nil
}

// list returns the volumes' records, sorted by ID.
func (s *volumeService) list() []*volumev1.Volume {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*volumev1.Volume, 0, len(s.volumes))
	for _, v := range s.volumes {
		out = append(out, cloneVolume(v))
	}
	slices.SortFunc(out, func(a, b *volumev1.Volume) int { return strings.Compare(a.VolumeId, b.VolumeId) })
	return out
}

// claim attaches the volumes ids to the pod, all or none, as a create
// does: each must be on the node and held by no other pod. An available
// volume is attached; a checkpointed one stays checkpointed until the
// pod's first boot brings it back.
func (s *volumeService) claim(pod string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var claimed []*volumev1.Volume
	for _, id := range ids {
		v := s.volumes[id]
		if v == nil {
			return status.Errorf(codes.FailedPrecondition, "no volume %s on this node", id)
		}
		switch v.PodId {
		case "":
		case pod:
			continue
		default:
			return status.Errorf(codes.FailedPrecondition, "volume %s is attached to pod %s", id, v.PodId)
		}
		claimed = append(claimed, v)
	}
	for _, v := range claimed {
		v.PodId = pod
		if v.State == volumev1.VolumeState_VOLUME_STATE_AVAILABLE {
			v.State = volumev1.VolumeState_VOLUME_STATE_ATTACHED
		}
	}
	return nil
}

// release lets go of the pod's volumes, as a deletion of the pod does: an
// attached one is available again, a checkpointed one stays checkpointed.
func (s *volumeService) release(pod string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.volumes {
		if v.PodId != pod {
			continue
		}
		v.PodId = ""
		if v.State == volumev1.VolumeState_VOLUME_STATE_ATTACHED {
			v.State = volumev1.VolumeState_VOLUME_STATE_AVAILABLE
		}
	}
}

// booted marks the pod's volumes as its boot leaves them: attached, with
// their filesystem.
func (s *volumeService) booted(pod string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.volumes {
		if v.PodId == pod {
			v.State, v.Formatted = volumev1.VolumeState_VOLUME_STATE_ATTACHED, true
		}
	}
}

// checkpointed returns the pod's volumes' records as its checkpoint of
// epoch carries them: checkpointed, with their heads.
func (s *volumeService) checkpointed(pod string, epoch uint64) []*volumev1.Volume {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*volumev1.Volume
	for _, v := range s.volumes {
		if v.PodId != pod {
			continue
		}
		c := cloneVolume(v)
		c.State = volumev1.VolumeState_VOLUME_STATE_CHECKPOINTED
		c.Checkpoint = &volumev1.VolumeCheckpoint{HeadKey: volumeHeadKey(v.VolumeId, epoch), Epoch: epoch}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *volumev1.Volume) int { return strings.Compare(a.VolumeId, b.VolumeId) })
	return out
}

// checkpoint marks the pod's volumes checkpointed into its checkpoint of
// epoch, as a hibernation leaves them; the pod still has them.
func (s *volumeService) checkpoint(pod string, epoch uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.volumes {
		if v.PodId == pod {
			v.State = volumev1.VolumeState_VOLUME_STATE_CHECKPOINTED
			v.Checkpoint = &volumev1.VolumeCheckpoint{HeadKey: volumeHeadKey(v.VolumeId, epoch), Epoch: epoch}
		}
	}
}

// volumeHeadKey is the key of a volume's head in its pod's checkpoint.
func volumeHeadKey(volumeID string, epoch uint64) string {
	return "snapshots/volume-" + volumeID + "/" + strconv.FormatUint(epoch, 10) + "/head.json"
}

// restorable checks that the volumes of a pod's checkpoint can be taken
// over on the node, and changes nothing (check_restorable, which feosd's
// adopt_checkpoint runs before it records the pod): each must be one the
// node does not have, the pod's own, or checkpointed and held by no pod.
func (s *volumeService) restorable(pod string, volumes []*volumev1.Volume) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restorableLocked(pod, volumes)
}

func (s *volumeService) restorableLocked(pod string, volumes []*volumev1.Volume) error {
	for _, v := range volumes {
		had := s.volumes[v.VolumeId]
		if had == nil {
			continue
		}
		switch {
		case had.PodId == pod:
		case had.PodId == "" && had.State == volumev1.VolumeState_VOLUME_STATE_CHECKPOINTED:
		case had.PodId == "":
			return status.Errorf(codes.FailedPrecondition, "volume %s is on this node with data its checkpoint may not have: delete it, or restore the pod on another node", v.VolumeId)
		default:
			return status.Errorf(codes.FailedPrecondition, "volume %s is attached to pod %s", v.VolumeId, had.PodId)
		}
	}
	return nil
}

// takeOver takes the volumes of a pod's checkpoint over, as a restore
// does: each must be restorable; afterwards each is the pod's,
// checkpointed until it boots.
func (s *volumeService) takeOver(pod string, volumes []*volumev1.Volume) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.restorableLocked(pod, volumes); err != nil {
		return err
	}
	for _, v := range volumes {
		c := cloneVolume(v)
		c.PodId = pod
		c.State = volumev1.VolumeState_VOLUME_STATE_CHECKPOINTED
		if c.Labels == nil {
			c.Labels = map[string]string{}
		}
		s.volumes[c.VolumeId] = c
	}
	return nil
}

// Volumes returns the node's volumes' records, sorted by ID.
func (n *Node) Volumes() []*volumev1.Volume { return n.volumes.list() }
