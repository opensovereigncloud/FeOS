// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	containerv1 "github.com/ironcore-dev/feos/go/gen/feos/container/v1"
	podv1 "github.com/ironcore-dev/feos/go/gen/feos/pod/v1"
)

func TestParseExitRule(t *testing.T) {
	for _, c := range []struct {
		in    string
		code  int
		after time.Duration
	}{{"job/*=0@3s", 0, 3 * time.Second}, {"service/web=1", 1, defaultExitAfter}, {"*/*=-1@0s", -1, 0}} {
		r, err := ParseExitRule(c.in)
		if err != nil || r.Code != c.code || r.After != c.after {
			t.Errorf("%s: %+v, %v", c.in, r, err)
		}
	}
	for _, bad := range []string{"", "job/*", "=0", "job/*=x", "job/*=256", "job/*=0@soon", "job/*=0@-1s", "job/[=0"} {
		if _, err := ParseExitRule(bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}

// A pod whose object matches an exit rule has its main container exit
// with the rule's code after every boot, and only after the boot it was
// scheduled for; StartLog writes a line at each boot and exit; a pod
// matching no rule runs on.
func TestContainerExitRules(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	jobRule, err := ParseExitRule("job/*=0@100ms")
	if err != nil {
		t.Fatal(err)
	}
	crash := ExitRule{Object: "service/crasher", Code: -1, After: 100 * time.Millisecond}
	e.start("n1", in, func(c *Config) {
		c.ContainerExits = []ExitRule{jobRule, crash}
		c.StartLog = true
	})
	pc := pods(t, in, "n1")
	podOf := func(object string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		spec := envSpec(id, "")
		spec.Labels[objectLabel] = object
		if _, err := create(t, pc, id.String(), spec); err != nil {
			t.Fatal(err)
		}
		waitState(t, pc, id.String(), podv1.PodState_POD_STATE_RUNNING)
		return id
	}
	exited := func(id uuid.UUID) *podv1.ContainerStatus {
		t.Helper()
		var st *podv1.ContainerStatus
		eventually(t, 5*time.Second, "pod "+id.String()+" exited", func() bool {
			info, err := pc.GetPod(bgctx, &podv1.GetPodRequest{PodId: id.String()})
			if err != nil || len(info.Containers) == 0 {
				return false
			}
			st = info.Containers[0]
			return st.State == podv1.ContainerState_CONTAINER_STATE_EXITED
		})
		return st
	}
	job, svc, crasher := podOf("job/migrate"), podOf("service/web"), podOf("service/crasher")
	if st := exited(job); st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("the job's container %v", st)
	}
	if st := exited(crasher); st.ExitCode != nil {
		t.Fatalf("the crasher exited with a code %v", st)
	}
	time.Sleep(300 * time.Millisecond)
	if info, _ := pc.GetPod(bgctx, &podv1.GetPodRequest{PodId: svc.String()}); info.Containers[0].State != podv1.ContainerState_CONTAINER_STATE_RUNNING {
		t.Fatalf("a pod no rule matches %v", info)
	}

	// A restart boots it anew: running, then the exit again.
	if _, err := pc.StopPod(bgctx, &podv1.StopPodRequest{PodId: crasher.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.StartPod(bgctx, &podv1.StartPodRequest{PodId: crasher.String()}); err != nil {
		t.Fatal(err)
	}
	info := waitState(t, pc, crasher.String(), podv1.PodState_POD_STATE_RUNNING)
	if info.Containers[0].State != podv1.ContainerState_CONTAINER_STATE_RUNNING {
		t.Fatalf("after the restart %v", info)
	}
	exited(crasher)

	cc := containerv1.NewContainerServiceClient(conn(t, in, "n1"))
	logs := func(id uuid.UUID) []string {
		t.Helper()
		s, err := cc.StreamContainerLogs(podCtx(id.String()), &containerv1.StreamContainerLogsRequest{TailLines: 100})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for {
			e, err := s.Recv()
			if err == io.EOF {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, string(e.Line))
		}
	}
	got := logs(crasher)
	if len(got) != 4 || !strings.Contains(got[0], "started") || !strings.HasSuffix(got[1], "exited without a code") ||
		!strings.Contains(got[2], "boot") || !strings.HasSuffix(got[3], "exited without a code") {
		t.Fatalf("the crasher's log %q", got)
	}
	if got := logs(job); len(got) != 2 || !strings.HasSuffix(got[1], "exited with code 0") {
		t.Fatalf("the job's log %q", got)
	}
	if got := logs(svc); len(got) != 1 || !strings.Contains(got[0], "container main of pod "+svc.String()+" started") {
		t.Fatalf("the service's log %q", got)
	}
}

// Config.PortAddrs sends a port's forwards to another address.
func TestConfigPortAddrs(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	n := e.start("n1", in, func(c *Config) { c.PortAddrs = map[uint32]string{8080: "127.0.0.1:18080"} })
	if got := n.Script().portAddr(uuid.New(), 8080); got != "127.0.0.1:18080" {
		t.Fatalf("port 8080 dials %s", got)
	}
	if got := n.Script().portAddr(uuid.New(), 8081); got != "127.0.0.1:8081" {
		t.Fatalf("port 8081 dials %s", got)
	}
}

// With FakeProcesses the process scripts are answered in memory: a start
// prints its handle, an exit rule "process/<name>" ends the process with
// its code, a new boot of the pod ends the others without one.
func TestFakeProcesses(t *testing.T) {
	e := newEnv(t)
	in := e.instance("i1", caIss)
	e.start("n1", in, func(c *Config) {
		c.FakeProcesses = true
		c.ContainerExits = []ExitRule{{Object: "process/crasher", Code: 2, After: 50 * time.Millisecond}}
	})
	pc := pods(t, in, "n1")
	id, _ := createRunning(t, pc)
	cc := containerv1.NewContainerServiceClient(conn(t, in, "n1"))
	run := func(script string, args ...string) (int32, string) {
		t.Helper()
		s, err := cc.Exec(podCtx(id.String()), &containerv1.ExecRequest{Command: append([]string{"python3", "-c", processRoot + "\n" + script}, args...)})
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		for {
			f, err := s.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if c, ok := f.Frame.(*containerv1.ExecStreamFrame_ExitCode); ok {
				return c.ExitCode, out.String()
			}
			out.Write(f.GetStdout())
		}
	}
	if code, out := run(markList); code != 0 || out != "no background processes\n" {
		t.Fatalf("list %d %q", code, out)
	}
	if code, out := run(markStart, "npm run dev", "0", "web", "/nowhere"); code != 3 || !strings.HasPrefix(out, "no such working directory") {
		t.Fatalf("start in a missing directory %d %q", code, out)
	}
	code, out := run(markStart, "npm run dev", "0", "web", "")
	var h struct{ ID, Name string }
	if err := json.Unmarshal([]byte(out), &h); code != 0 || err != nil || h.ID == "" || h.Name != "web" {
		t.Fatalf("start %d %q", code, out)
	}
	run(markStart, "false", "0", "crasher", "")
	var rows []processRow
	eventually(t, 5*time.Second, "the crasher exited", func() bool {
		_, out := run(markList)
		rows = nil
		return json.Unmarshal([]byte(out), &rows) == nil && len(rows) == 2 && !rows[1].Running
	})
	if !rows[0].Running || rows[1].ExitCode == nil || *rows[1].ExitCode != 2 {
		t.Fatalf("rows %+v", rows)
	}
	if code, out := run(markLogs, h.ID, "10"); code != 0 || out != "ferun-testnode: started npm run dev\n" {
		t.Fatalf("logs %d %q", code, out)
	}
	if code, out := run(markLogs, "nope", "10"); code != 1 || !strings.HasPrefix(out, "no such process") {
		t.Fatalf("logs of an unknown process %d %q", code, out)
	}
	// A new boot ends what ran, without a code.
	if _, err := pc.StopPod(bgctx, &podv1.StopPodRequest{PodId: id.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.StartPod(bgctx, &podv1.StartPodRequest{PodId: id.String()}); err != nil {
		t.Fatal(err)
	}
	waitState(t, pc, id.String(), podv1.PodState_POD_STATE_RUNNING)
	_, out = run(markList)
	if err := json.Unmarshal([]byte(out), &rows); err != nil || rows[0].Running || rows[0].ExitCode != nil {
		t.Fatalf("after the boot %q", out)
	}
	if code, out := run(markStop, h.ID); code != 0 || !strings.HasSuffix(out, "was not running any more\n") {
		t.Fatalf("stop %d %q", code, out)
	}
}
