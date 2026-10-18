// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	containerv1 "github.com/ironcore-dev/feos/go/gen/feos/container/v1"
	networkv1 "github.com/ironcore-dev/feos/go/gen/feos/network/v1"
	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
	"github.com/ironcore-dev/feos/go/internal/testnode"
)

// processRoot is where the process scripts keep their records in a
// container; the tests point it at a directory of their own (the test
// node runs commands on the host).
const processRoot = "/var/lib/sandbox-processes"

// hostProcesses makes the node run the process scripts with their records
// under root and their working directory beneath the pod's directory,
// which stands in for the container's root.
func hostProcesses(n *testnode.Node, root string) {
	n.Script().SetExec(func(_ context.Context, pod uuid.UUID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
		argv := req.Command
		if len(argv) > 2 && argv[0] == "python3" {
			start := strings.Contains(argv[2], "no such working directory")
			argv[2] = strings.ReplaceAll(argv[2], processRoot, root)
			if start && len(argv) > 6 && argv[6] != "" {
				argv[6] = filepath.Join(n.PodDir(pod), argv[6])
			}
		}
		return 0, false, nil
	})
}

// attachable makes an attachable sandbox of the tenant (the template the
// API gives it) and brings it up.
func (h *harness) attachable() store.Sandbox {
	h.t.Helper()
	id := uuid.New()
	if _, _, err := h.st.CreateSandbox(bg, store.NewSandbox{ID: id, Owner: testTenant, Image: testImage, ImageRef: testImage,
		VCPUs: 1, MemoryMiB: 512, PodTemplate: &model.PodTemplate{Command: []string{"sleep", "infinity"},
			SecurityGroups: []string{model.SandboxGroupID(id)}}}); err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	if _, err := h.eng.Ensure(ctx, id); err != nil {
		h.t.Fatalf("ensure the attachable sandbox: %v", err)
	}
	sb, err := h.st.GetSandbox(bg, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return sb
}

func attachedSpec(sandbox uuid.UUID, cwd, greeting string, withWeb bool) string {
	web := ""
	if withWeb {
		web = fmt.Sprintf(`,"web":{"runtime":"attached","sandbox":%q,"command":["sh","-c","echo started $GREETING $API; exec sleep 60"],
			"cwd":%q,"env":{"GREETING":%q,"API":{"service":"api","port":"http","key":"url"}},
			"ports":[{"name":"http","port":5173}]}`, sandbox, cwd, greeting)
	}
	return fmt.Sprintf(`{"services":{"api":{"image":%q,"ports":[{"name":"http","port":8080}]}%s}}`, testImage, web)
}

// An attached service is a supervised process in an attachable sandbox:
// started (once its working directory exists) with its env, named in the
// sandbox's hosts block beside the environment's image services, the
// sandbox in the environment's group and its group reaching that group;
// restarted on request and on a changed template; removed with its
// process and its hosts entries. The attachable sandbox's own group is put
// before its pod is created, also without an environment.
func TestAttachedService(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	hostProcesses(n1, t.TempDir())
	sb := h.attachable()
	sbx := h.group("n1", model.SandboxGroupID(sb.ID))
	if sbx == nil || len(sbx.Rules) != 2 || !slices.Equal(sbx.Members, []string{sb.Address}) {
		t.Fatalf("the sandbox's group %v", sbx)
	}

	env := h.newEnv("dev")
	r1 := h.apply(env.ID, attachedSpec(sb.ID, "/app", "hi", true))
	eventually(t, 30*time.Second, "the missing cwd", func() bool {
		a, err := h.st.GetAttached(bg, env.ID, "web")
		return err == nil && a.Phase == model.PhaseStarting && a.Detail == "cwd /app does not exist yet" && a.ProcessID == ""
	})
	if err := os.MkdirAll(filepath.Join(n1.PodDir(sb.ID), "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 60*time.Second)
	a, err := h.st.GetAttached(bg, env.ID, "web")
	if err != nil || a.Phase != model.PhaseReady || a.ProcessID == "" || a.SandboxIncarnation != sb.Incarnation ||
		a.HostsWritten == "" || a.Restarts != 0 {
		t.Fatalf("attached %+v %v", a, err)
	}
	ps, err := h.runner.ProcessesDetached(bg, lease(sb))
	if err != nil || len(ps) != 1 || ps[0].ID != a.ProcessID || ps[0].Name != "ferun-env-dev-web" || !ps[0].Running {
		t.Fatalf("processes %+v %v", ps, err)
	}
	var lines []string
	eventually(t, 10*time.Second, "the process's log", func() bool {
		var source string
		lines, source, err = h.c.LogTail(bg, env.ID, spec.KindService, "web", 0, 10)
		return err == nil && source == SourceProcess && len(lines) == 1
	})
	if lines[0] != "started hi http://api:8080" {
		t.Errorf("log %q", lines)
	}
	api := h.waitRows(env.ID, spec.KindService, "api", 1, 10*time.Second, "ready", ready)[0]
	hosts, err := os.ReadFile(filepath.Join(n1.PodDir(sb.ID), "etc", "hosts"))
	if err != nil {
		t.Fatal(err)
	}
	wantBlock := fmt.Sprintf("# ferun environment dev begin\n%s api\n127.0.0.1 web\n::1 web\n# ferun environment dev end\n",
		api.Sandbox.Address)
	if string(hosts) != hostsBase+wantBlock {
		t.Errorf("the sandbox's hosts\n%s", hosts)
	}
	eventually(t, 10*time.Second, "the image instance's hosts block", func() bool {
		b, err := os.ReadFile(filepath.Join(n1.PodDir(api.Sandbox.ID), "etc", "hosts"))
		return err == nil && string(b) == hostsBase+fmt.Sprintf("# ferun environment dev begin\n%s api\n# ferun environment dev end\n",
			api.Sandbox.Address)
	})
	// Groups: the sandbox is a member of the env group, and its group
	// reaches the env group.
	eventually(t, 30*time.Second, "the groups", func() bool {
		eg := h.group("n1", model.EnvGroupID(env.ID))
		sg := h.group("n1", model.SandboxGroupID(sb.ID))
		return eg != nil && sg != nil && slices.Contains(eg.Members, sb.Address) && slices.Contains(eg.Members, api.Sandbox.Address) &&
			slices.ContainsFunc(sg.Rules, func(r *networkv1.Rule) bool {
				return r.GetGroup() == model.EnvGroupID(env.ID) && r.Direction == networkv1.Direction_DIRECTION_EGRESS
			})
	})

	// A restart on request starts a new process.
	if err := h.c.RestartService(bg, env.ID, "web"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "a new process", func() bool {
		b, err := h.st.GetAttached(bg, env.ID, "web")
		return err == nil && b.ProcessID != "" && b.ProcessID != a.ProcessID && b.Phase == model.PhaseReady
	})
	// A changed template replaces the process.
	b, _ := h.st.GetAttached(bg, env.ID, "web")
	r2 := h.apply(env.ID, attachedSpec(sb.ID, "/app", "hello", true))
	h.waitRelease(env.ID, r2, model.ReleaseRealised, 60*time.Second)
	c, _ := h.st.GetAttached(bg, env.ID, "web")
	if c.ProcessID == b.ProcessID || c.Phase != model.PhaseReady {
		t.Fatalf("after the template changed %+v", c)
	}
	ps, _ = h.runner.ProcessesDetached(bg, lease(sb))
	runningProcs := 0
	for _, p := range ps {
		if p.Running {
			runningProcs++
			if p.ID != c.ProcessID {
				t.Errorf("an old process runs: %+v", p)
			}
		}
	}
	if runningProcs != 1 {
		t.Errorf("%d processes run: %+v", runningProcs, ps)
	}
	// A process that exits is restarted after its backoff.
	if err := h.runner.StopProcessDetached(bg, lease(sb), c.ProcessID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the restart after the exit", func() bool {
		d, err := h.st.GetAttached(bg, env.ID, "web")
		return err == nil && d.Restarts == 1 && d.ProcessID != "" && d.ProcessID != c.ProcessID
	})

	// Removed: the process stops, the hosts block goes, the row goes.
	d, _ := h.st.GetAttached(bg, env.ID, "web")
	r3 := h.apply(env.ID, attachedSpec(sb.ID, "/app", "", false))
	h.waitRelease(env.ID, r3, model.ReleaseRealised, 60*time.Second)
	if _, err := h.st.GetAttached(bg, env.ID, "web"); !errors.Is(err, store.ErrNoRow) {
		t.Errorf("the attached row: %v", err)
	}
	ps, _ = h.runner.ProcessesDetached(bg, lease(sb))
	for _, p := range ps {
		if p.Running {
			t.Errorf("a process still runs: %+v (last %s)", p, d.ProcessID)
		}
	}
	if hosts, _ := os.ReadFile(filepath.Join(n1.PodDir(sb.ID), "etc", "hosts")); string(hosts) != hostsBase {
		t.Errorf("the sandbox's hosts after the removal\n%s", hosts)
	}
	eventually(t, 30*time.Second, "the sandbox out of the env group", func() bool {
		eg := h.group("n1", model.EnvGroupID(env.ID))
		return eg != nil && !slices.Contains(eg.Members, sb.Address)
	})
}

// An attached service in a paused sandbox: the controller wakes it to
// start the process (and never for a probe).
func TestAttachedWakesPausedSandbox(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	hostProcesses(n1, t.TempDir())
	sb := h.attachable()
	if err := h.eng.Pause(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "paused", func() bool {
		s, _ := h.st.GetSandbox(bg, sb.ID)
		return s.ClientState == model.StatePaused && s.Op == ""
	})
	env := h.newEnv("dev")
	r1 := h.apply(env.ID, fmt.Sprintf(`{"services":{"web":{"runtime":"attached","sandbox":%q,
		"command":["sleep","60"]}}}`, sb.ID))
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 60*time.Second)
	a, err := h.st.GetAttached(bg, env.ID, "web")
	if err != nil || a.ProcessID == "" || a.Phase != model.PhaseReady {
		t.Fatalf("attached %+v %v", a, err)
	}
	if _, _, err := h.c.LogTail(bg, env.ID, spec.KindService, "web", 0, 5); err != nil {
		t.Errorf("logs of a running process: %v", err)
	}
	// Paused again, its logs are not read (it is not woken).
	if err := h.eng.Pause(bg, sb.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "paused again", func() bool {
		s, _ := h.st.GetSandbox(bg, sb.ID)
		return s.ClientState == model.StatePaused && s.Op == ""
	})
	if _, _, err := h.c.LogTail(bg, env.ID, spec.KindService, "web", 0, 5); !errors.Is(err, ErrPaused) {
		t.Errorf("logs of a paused sandbox: %v", err)
	}
	never(t, 3*time.Second, "the probes woke the sandbox", func() bool {
		s, _ := h.st.GetSandbox(bg, sb.ID)
		return s.ClientState != model.StatePaused
	})
}
