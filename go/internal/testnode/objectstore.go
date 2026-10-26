// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
	volumev1 "github.com/ironcore-dev/feos/go/gen/feos/volume/v1"
	"github.com/ironcore-dev/feos/go/internal/podid"
)

// ObjectStore is a fake of the object storage that diskd hibernates pods
// into (ADR 0008), shared by the test nodes that are given it: a pod
// hibernated on one node restores on another that has the same store. A
// checkpoint holds the pod's spec, so that a node that does not have the
// pod adopts it, and the files of the pod's directory, its disk.
//
// Its ID, which the nodes report as their object_store_id, names what is
// shared, since the control plane restores a pod on another node only if
// both report the same: a store made by NewObjectStore lives in memory and
// has an ID of its own, which only the nodes of this process given that
// store report; a store opened by OpenObjectStore lives in a directory and
// has the ID kept there, which every process that opens the directory
// reports.
type ObjectStore struct {
	id  string
	dir string // "" in memory

	mu    sync.Mutex
	heads map[string]*checkpoint // in memory
}

// checkpoint is what a head names: the pod, its epoch, whether it holds
// memory, its spec, its disk, and its volumes' records, which a restore
// takes over.
type checkpoint struct {
	podID   string
	epoch   uint64
	memory  bool
	spec    *podv1.PodSpec
	files   []fileEntry
	bytes   uint64
	volumes []*volumev1.Volume
}

// fileEntry is one entry of a pod's directory: a directory, a regular file
// with its content, or a symbolic link.
type fileEntry struct {
	path string // slash-separated, relative to the pod's directory
	mode fs.FileMode
	data []byte
	link string
}

// storeID is an object_store_id: the lowercase hex SHA-256 of
// "<endpoint>\n<bucket>" (fleet.proto).
func storeID(endpoint, bucket string) string {
	sum := sha256.Sum256([]byte(endpoint + "\n" + bucket))
	return hex.EncodeToString(sum[:])
}

// NewObjectStore makes an empty object store in memory; name is its
// bucket. Nodes given the same store share it. Every store has an ID of
// its own, also two of the same name, since they share nothing.
func NewObjectStore(name string) *ObjectStore {
	return &ObjectStore{id: storeID("memory://testnode/"+uuid.NewString(), name), heads: map[string]*checkpoint{}}
}

// storeIDFile keeps the identity of a directory's store.
const storeIDFile = "store-id"

// OpenObjectStore opens the object store kept in dir, making it when dir
// holds none: test nodes in several processes that open the same
// directory share it and report the same ID. The ID is made from a UUID
// that the first open keeps in the directory.
func OpenObjectStore(dir string) (*ObjectStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("testnode: object store: %w", err)
	}
	name, err := storeName(dir)
	if err != nil {
		return nil, fmt.Errorf("testnode: object store in %s: %w", dir, err)
	}
	return &ObjectStore{id: storeID("file://testnode", name), dir: dir}, nil
}

// storeName reads the UUID kept in dir, or keeps a new one there. The file
// appears at once, written in full, so that a process opening the
// directory at the same time reads either nothing or all of it.
func storeName(dir string) (string, error) {
	path := filepath.Join(dir, storeIDFile)
	read := func() (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		u, err := uuid.ParseBytes(bytes.TrimSpace(data))
		if err != nil {
			return "", fmt.Errorf("%s: %w", storeIDFile, err)
		}
		return u.String(), nil
	}
	if name, err := read(); !errors.Is(err, os.ErrNotExist) {
		return name, err
	}
	name := uuid.NewString()
	tmp, err := writeTemp(dir, storeIDFile, []byte(name+"\n"))
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return read()
		}
		return "", err
	}
	return name, nil
}

// writeTemp writes data into a new temporary file in dir and returns its
// path.
func writeTemp(dir, pattern string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, "."+pattern+".*")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// ID is the store's identity as telemetry reports it (object_store_id).
func (o *ObjectStore) ID() string { return o.id }

// Heads lists the keys of the checkpoints' heads, sorted.
func (o *ObjectStore) Heads() []string {
	var keys []string
	if o.dir != "" {
		_ = filepath.WalkDir(filepath.Join(o.dir, "snapshots"), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if rel, err := filepath.Rel(o.dir, path); err == nil && validHeadKey(filepath.ToSlash(rel)) {
				keys = append(keys, filepath.ToSlash(rel))
			}
			return nil
		})
	} else {
		o.mu.Lock()
		for k := range o.heads {
			keys = append(keys, k)
		}
		o.mu.Unlock()
	}
	slices.Sort(keys)
	return keys
}

// headKey is the key of a checkpoint's head, as diskd writes it.
func headKey(podID string, epoch uint64) string {
	return fmt.Sprintf("snapshots/%s/%d/head.json", podID, epoch)
}

// validHeadKey reports whether key has the form of headKey: a key a
// request names is looked up only then, so that none reaches outside a
// store's directory.
func validHeadKey(key string) bool {
	parts := strings.Split(key, "/")
	if len(parts) != 4 || parts[0] != "snapshots" || parts[3] != "head.json" {
		return false
	}
	if _, err := podid.Parse(parts[1]); err != nil {
		return false
	}
	_, err := strconv.ParseUint(parts[2], 10, 64)
	return err == nil && filepath.IsLocal(filepath.FromSlash(key))
}

// checkpointJSON is a checkpoint in a store's directory.
type checkpointJSON struct {
	PodID  string     `json:"podId"`
	Epoch  uint64     `json:"epoch"`
	Memory bool       `json:"memory"`
	Spec   []byte     `json:"spec"` // the PodSpec in protobuf
	Files  []fileJSON `json:"files"`
	Bytes  uint64     `json:"bytes"`
	// Volumes are the volumes' records in protobuf.
	Volumes [][]byte `json:"volumes,omitempty"`
}

type fileJSON struct {
	Path string      `json:"path"`
	Mode fs.FileMode `json:"mode"`
	Data []byte      `json:"data,omitempty"`
	Link string      `json:"link,omitempty"`
}

// put keeps a checkpoint and returns its head's key. In a directory the
// head appears at once, written in full.
func (o *ObjectStore) put(c *checkpoint) (string, error) {
	key := headKey(c.podID, c.epoch)
	if o.dir == "" {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.heads[key] = c
		return key, nil
	}
	spec, err := proto.Marshal(c.spec)
	if err != nil {
		return "", err
	}
	j := checkpointJSON{PodID: c.podID, Epoch: c.epoch, Memory: c.memory, Spec: spec, Bytes: c.bytes}
	for _, f := range c.files {
		j.Files = append(j.Files, fileJSON{Path: f.path, Mode: f.mode, Data: f.data, Link: f.link})
	}
	for _, v := range c.volumes {
		b, err := proto.Marshal(v)
		if err != nil {
			return "", err
		}
		j.Volumes = append(j.Volumes, b)
	}
	data, err := json.Marshal(j)
	if err != nil {
		return "", err
	}
	path := filepath.Join(o.dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp, err := writeTemp(filepath.Dir(path), "head.json", data)
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return key, nil
}

// get reads the checkpoint whose head key names; false when there is
// none.
func (o *ObjectStore) get(key string) (*checkpoint, bool, error) {
	if o.dir == "" {
		o.mu.Lock()
		defer o.mu.Unlock()
		c, ok := o.heads[key]
		return c, ok, nil
	}
	if !validHeadKey(key) {
		return nil, false, nil
	}
	data, err := os.ReadFile(filepath.Join(o.dir, filepath.FromSlash(key)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var j checkpointJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, false, fmt.Errorf("%s: %w", key, err)
	}
	spec := &podv1.PodSpec{}
	if err := proto.Unmarshal(j.Spec, spec); err != nil {
		return nil, false, fmt.Errorf("%s: the spec: %w", key, err)
	}
	c := &checkpoint{podID: j.PodID, epoch: j.Epoch, memory: j.Memory, spec: spec, bytes: j.Bytes}
	for _, f := range j.Files {
		c.files = append(c.files, fileEntry{path: f.Path, mode: f.Mode, data: f.Data, link: f.Link})
	}
	for i, b := range j.Volumes {
		v := &volumev1.Volume{}
		if err := proto.Unmarshal(b, v); err != nil {
			return nil, false, fmt.Errorf("%s: volume %d: %w", key, i, err)
		}
		c.volumes = append(c.volumes, v)
	}
	return c, true, nil
}

// snapshotDir reads a pod's directory into entries: what its checkpoint
// carries as its disk.
func snapshotDir(dir string) ([]fileEntry, uint64, error) {
	var entries []fileEntry
	var total uint64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := fileEntry{path: filepath.ToSlash(rel), mode: info.Mode()}
		switch {
		case d.IsDir():
		case info.Mode()&fs.ModeSymlink != 0:
			if e.link, err = os.Readlink(path); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if e.data, err = os.ReadFile(path); err != nil {
				return err
			}
			total += uint64(len(e.data))
		default:
			return nil // sockets, pipes and devices stay behind
		}
		entries = append(entries, e)
		return nil
	})
	return entries, total, err
}

// restoreDir writes entries into dir, which it makes anew.
func restoreDir(dir string, entries []fileEntry) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		path := filepath.Join(dir, filepath.FromSlash(e.path))
		var err error
		switch {
		case e.mode.IsDir():
			err = os.MkdirAll(path, e.mode.Perm()|0o700)
		case e.mode&fs.ModeSymlink != 0:
			err = os.Symlink(e.link, path)
		default:
			err = os.WriteFile(path, e.data, e.mode.Perm())
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// cloneSpec copies a spec, so that a checkpoint never shares one with a
// pod.
func cloneSpec(s *podv1.PodSpec) *podv1.PodSpec {
	if s == nil {
		return &podv1.PodSpec{}
	}
	return proto.Clone(s).(*podv1.PodSpec)
}
