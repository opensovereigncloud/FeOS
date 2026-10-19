// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	containerv1 "github.com/ironcore-dev/feos/go/gen/feos/container/v1"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
	"github.com/ironcore-dev/feos/go/internal/testnode"
)

// The process scripts' stop, by a line only it has.
const stopScriptMark = "os.killpg(pgid, signal.SIGTERM)"

// podProcesses is hostProcesses with each pod's records in a directory
// of its own (root/<pod>), as each container has its own, and with stops
// that fail while failStops is set (an exec cap, a node that does not
// answer).
type podProcesses struct {
	root      string
	failStops atomic.Bool
	stops     atomic.Int64
}

func (pp *podProcesses) dir(pod uuid.UUID) string { return filepath.Join(pp.root, pod.String()) }

func hostProcessesPerPod(n *testnode.Node, root string) *podProcesses {
	pp := &podProcesses{root: root}
	n.Script().SetExec(func(_ context.Context, pod uuid.UUID, req *containerv1.ExecRequest, out testnode.ExecOutput) (int32, bool, error) {
		argv := req.Command
		if len(argv) > 2 && argv[0] == "python3" {
			if strings.Contains(argv[2], stopScriptMark) {
				pp.stops.Add(1)
				if pp.failStops.Load() {
					return 1, true, out.Stderr([]byte("injected failure\n"))
				}
			}
			start := strings.Contains(argv[2], "no such working directory")
			argv[2] = strings.ReplaceAll(argv[2], processRoot, pp.dir(pod))
			if start && len(argv) > 6 && argv[6] != "" {
				argv[6] = filepath.Join(n.PodDir(pod), argv[6])
			}
		}
		return 0, false, nil
	})
	return pp
}

// hostsOf reads a pod's /etc/hosts.
func hostsOf(n *testnode.Node, id uuid.UUID) string {
	b, _ := os.ReadFile(filepath.Join(n.PodDir(id), "etc", "hosts"))
	return string(b)
}

// FeCode replaces a lost workspace sandbox under the same ID: the
// attached service in it comes back in the new incarnation (§13.6), with
// restarts, next_restart_at, probe_failures and hosts_written reset and
// the new incarnation recorded; its process starts there, its hosts block
// is written there and the new sandbox is a member of the environment's
// group.
func TestAttachedNewIncarnation(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	hostProcessesPerPod(n1, t.TempDir())
	sb := h.attachable()
	env := h.newEnv("dev")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"api":{"image":%q,"ports":[{"name":"http","port":8080}]},
		"web":{"runtime":"attached","sandbox":%q,"command":["sleep","120"]}}}`, testImage, sb.ID))
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 60*time.Second)
	a, err := h.st.GetAttached(bg, env.ID, "web")
	if err != nil || a.ProcessID == "" || a.SandboxIncarnation != sb.Incarnation {
		t.Fatalf("attached %+v %v", a, err)
	}
	// An exit, so that there is a restart to reset.
	if err := h.runner.StopProcessDetached(bg, lease(sb), a.ProcessID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the restart after the exit", func() bool {
		b, err := h.st.GetAttached(bg, env.ID, "web")
		return err == nil && b.Restarts == 1 && b.ProcessID != "" && b.ProcessID != a.ProcessID && b.Phase == model.PhaseReady
	})
	b, _ := h.st.GetAttached(bg, env.ID, "web")

	// The sandbox is lost and replaced under the same ID, by the same
	// tenant.
	if _, _, err := h.eng.Delete(bg, testTenant, sb.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the sandbox's pod gone", func() bool { return h.pod("n1", sb.ID) == nil })
	again := h.attachableAs(testTenant, sb.ID)
	if again.Incarnation == sb.Incarnation {
		t.Fatal("the same incarnation")
	}
	var c store.EnvAttached
	eventually(t, 60*time.Second, "the service back in the new incarnation", func() bool {
		c, err = h.st.GetAttached(bg, env.ID, "web")
		return err == nil && c.SandboxIncarnation == again.Incarnation && c.ProcessID != "" && c.ProcessID != b.ProcessID &&
			c.Phase == model.PhaseReady && c.HostsWritten != ""
	})
	if c.Restarts != 0 || c.ProbeFailures != 0 || !c.NextRestartAt.IsZero() || c.Detail != "" {
		t.Errorf("not reset: %+v", c)
	}
	if got := h.runningNamed(again, "ferun-env-dev-web"); !slices.Equal(got, []string{c.ProcessID}) {
		t.Errorf("running %v, the row records %s", got, c.ProcessID)
	}
	if !strings.Contains(hostsOf(n1, sb.ID), "127.0.0.1 web\n") {
		t.Errorf("the new sandbox's hosts\n%s", hostsOf(n1, sb.ID))
	}
	eventually(t, 30*time.Second, "the new sandbox in the env group", func() bool {
		h.c.notify(bg, HintGroups)
		g := h.group("n1", model.EnvGroupID(env.ID))
		return g != nil && slices.Contains(g.Members, again.Address)
	})
}

// seedRecords writes n records of processes that are over, named name,
// under dir, as the start script writes them.
func seedRecords(t *testing.T, dir, prefix, name string, n int) {
	t.Helper()
	for i := range n {
		d := filepath.Join(dir, fmt.Sprintf("%s%06d", prefix, i))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range map[string]string{"name": name, "cmd": strings.Repeat("node server.js --port 3000 ", 7), "pgid": "0",
			"timeout": "0", "started_at": strconv.Itoa(1000 + i), "exit": "1"} {
			if err := os.WriteFile(filepath.Join(d, k), []byte(v), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Process records pile up in a sandbox (the agent's own processes, and
// every restart of an attached service): the supervisor lists only its
// service's, so that it keeps supervising however many there are, and a
// start removes its name's records that are over but the newest few.
func TestAttachedSupervisedPastManyRecords(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	pp := hostProcessesPerPod(n1, t.TempDir())
	sb := h.attachable()
	env := h.newEnv("dev")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"web":{"runtime":"attached","sandbox":%q,"command":["sleep","120"]}}}`, sb.ID))
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 60*time.Second)
	a, err := h.st.GetAttached(bg, env.ID, "web")
	if err != nil || a.ProcessID == "" {
		t.Fatalf("attached %+v %v", a, err)
	}
	seedRecords(t, pp.dir(sb.ID), "000000-agent", "", 300)
	seedRecords(t, pp.dir(sb.ID), "000000-web", "ferun-env-dev-web", 10)

	if err := h.runner.StopProcessDetached(bg, lease(sb), a.ProcessID); err != nil {
		t.Fatal(err)
	}
	var b store.EnvAttached
	eventually(t, 30*time.Second, "the restart after the exit", func() bool {
		b, err = h.st.GetAttached(bg, env.ID, "web")
		return err == nil && b.Restarts == 1 && b.ProcessID != "" && b.ProcessID != a.ProcessID && b.Phase == model.PhaseReady
	})
	ps, err := h.runner.ProcessesDetached(bg, lease(sb), "ferun-env-dev-web")
	if err != nil {
		t.Fatal(err)
	}
	var over []string
	for _, p := range ps {
		if !p.Running {
			over = append(over, p.ID)
		}
	}
	// The newest three that are over: the process stopped above and the
	// two newest seeded ones.
	if want := []string{"000000-web000008", "000000-web000009", a.ProcessID}; !slices.Equal(over, want) || len(ps) != 4 {
		t.Fatalf("records of the service %+v, over %v, want %v", ps, over, want)
	}
	if all, err := h.runner.ProcessesDetached(bg, lease(sb), ""); err != nil || len(all) != 304 {
		t.Fatalf("every record: %d %v", len(all), err)
	}
}
