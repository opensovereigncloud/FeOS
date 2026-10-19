// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	containerv1 "github.com/ironcore-dev/feos/go/gen/feos/container/v1"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/testnode"
	"github.com/ironcore-dev/feos/go/internal/tools/toolstest"
)

// detachedLease brings the sandbox's pod up through the fake engine and
// gives its hold back at once: the lease a caller of the detached calls
// read from the row.
func detachedLease(t *testing.T, env *toolstest.Env) model.Lease {
	t.Helper()
	sb := env.NewSandbox()
	ctx, cancel := context.WithTimeout(bg, 30*time.Second)
	defer cancel()
	lease, release, err := env.Engine.Acquire(ctx, sb.ID, model.HoldExec)
	if err != nil {
		t.Fatal(err)
	}
	release()
	return lease
}

// noBookkeeping checks that the detached calls took no hold, wrote no
// activity row and asked the engine for nothing beyond the one Acquire of
// detachedLease.
func noBookkeeping(t *testing.T, env *toolstest.Env, id uuid.UUID) {
	t.Helper()
	if n := env.Engine.Calls("Acquire", id); n != 1 {
		t.Errorf("%d Acquires", n)
	}
	if n := env.Engine.Holds(id); n != 0 {
		t.Errorf("%d holds", n)
	}
	for _, what := range []string{"HoldProcess", "ReleaseProcess", "Sync", "WaitRunning"} {
		if n := env.Engine.Calls(what, id); n != 0 {
			t.Errorf("%d calls of %s", n, what)
		}
	}
	if rows := env.Activity(id, 0); len(rows) != 0 {
		t.Errorf("%d activity rows: %+v", len(rows), rows)
	}
}

// ExecDetached runs argv with the env added, in cwd, and answers its exit
// code and both outputs apart; past its timeout it is a TimeoutError.
func TestExecDetached(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	lease := detachedLease(t, env)
	if err := os.MkdirAll(filepath.Join(env.Node.PodDir(lease.SandboxID), "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr, err := r.ExecDetached(bg, lease,
		[]string{"sh", "-c", `echo out; echo err >&2; echo "$FOO"; basename "$(pwd)"; exit 3`},
		map[string]string{"FOO": "bar baz"}, "/work", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 || stdout != "out\nbar baz\nwork\n" || stderr != "err\n" {
		t.Errorf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	_, _, _, err = r.ExecDetached(bg, lease, []string{"sleep", "5"}, nil, "", 300*time.Millisecond)
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Errorf("a call past its timeout: %v", err)
	}
	if _, _, _, err := r.ExecDetached(bg, lease, nil, nil, "", 0); err == nil {
		t.Error("an exec without a command ran")
	}
	// A pod the lease does not name on the node: the node's answer.
	other := model.Lease{SandboxID: uuid.New(), Incarnation: uuid.New(), Node: lease.Node}
	_, _, _, err = r.ExecDetached(bg, other, []string{"true"}, nil, "", time.Second)
	var ne *NodeError
	if !errors.As(err, &ne) {
		t.Errorf("an exec in a pod the node does not have: %v", err)
	}
	noBookkeeping(t, env, lease.SandboxID)
}

// detachedRoot makes the node run the process scripts with their records
// under root and the working directory taken beneath the pod's directory,
// which stands in for the container's root (the test node runs commands
// on the host).
func detachedRoot(env *toolstest.Env, root string) {
	env.Node.Script().SetExec(func(_ context.Context, pod uuid.UUID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
		argv := req.Command
		if len(argv) > 2 && argv[0] == "python3" {
			argv[2] = strings.ReplaceAll(argv[2], processRoot, root)
			if argv[2] == strings.ReplaceAll(detachedStartScript, processRoot, root) && len(argv) > 6 && argv[6] != "" {
				argv[6] = filepath.Join(env.Node.PodDir(pod), argv[6])
			}
		}
		return 0, false, nil
	})
}

// A process started detached runs with its env (HOME=/root added), none
// of which is written to disk; it lists, logs and stops; a working
// directory that does not exist is ErrNoCwd, an unknown id ErrNoProcess.
func TestProcessDetached(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	lease := detachedLease(t, env)
	root := t.TempDir()
	detachedRoot(env, root)
	if err := os.MkdirAll(filepath.Join(env.Node.PodDir(lease.SandboxID), "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	const secret = "s3cr3t-value"
	id, err := r.StartProcessDetached(bg, lease, ProcessStart{
		Argv: []string{"sh", "-c", `echo "$HOME $SECRET $1"; basename "$(pwd)"; exec sleep 30`, "sh", "it's"},
		Cwd:  "/app", Env: map[string]string{"SECRET": secret}, Name: "ferun-env-stage-web",
	})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := r.ProcessesDetached(bg, lease, "")
	if err != nil || len(ps) != 1 || ps[0].ID != id || ps[0].Name != "ferun-env-stage-web" || !ps[0].Running ||
		ps[0].ExitCode != nil {
		t.Fatalf("processes %+v, %v", ps, err)
	}
	var lines []string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if lines, err = r.ProcessLogsDetached(bg, lease, id, 10); err == nil && len(lines) == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if want := []string{"/root " + secret + " it's", "app"}; !slices.Equal(lines, want) {
		t.Fatalf("logs %q (%v), want %q", lines, err, want)
	}
	// The env stays off the disk: the record has the command, not the
	// values.
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Base(path) == "log" {
			return err
		}
		b, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(b), secret) {
			t.Errorf("%s holds the secret", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.StopProcessDetached(bg, lease, id); err != nil {
		t.Fatal(err)
	}
	ps, err = r.ProcessesDetached(bg, lease, "")
	if err != nil || len(ps) != 1 || ps[0].Running {
		t.Fatalf("after the stop: %+v, %v", ps, err)
	}
	// A second stop finds it not running: nil.
	if err := r.StopProcessDetached(bg, lease, id); err != nil {
		t.Errorf("a second stop: %v", err)
	}
	// One that exits by itself has its code.
	short, err := r.StartProcessDetached(bg, lease, ProcessStart{Argv: []string{"sh", "-c", "exit 7"}, Name: "short"})
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	var code *int
	for time.Now().Before(deadline) && code == nil {
		ps, err = r.ProcessesDetached(bg, lease, "")
		for _, p := range ps {
			if p.ID == short && !p.Running {
				code = p.ExitCode
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code == nil || *code != 7 {
		t.Errorf("the exit code of a process that ended: %v (%+v %v)", code, ps, err)
	}
	if _, err := r.StartProcessDetached(bg, lease, ProcessStart{Argv: []string{"true"}, Cwd: "/not-yet", Name: "x"}); !errors.Is(err, ErrNoCwd) {
		t.Errorf("a missing working directory: %v", err)
	}
	if _, err := r.ProcessLogsDetached(bg, lease, "000000-abcdef", 5); !errors.Is(err, ErrNoProcess) {
		t.Errorf("the logs of an unknown process: %v", err)
	}
	if err := r.StopProcessDetached(bg, lease, "000000-abcdef"); !errors.Is(err, ErrNoProcess) {
		t.Errorf("the stop of an unknown process: %v", err)
	}
	noBookkeeping(t, env, lease.SandboxID)
}

func TestShellCommand(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"npm", "run", "dev", "--", "--host", "::", "--port", "5173"}, "npm run dev -- --host :: --port 5173"},
		{[]string{"echo", "a b", "it's", "$HOME", ""}, `echo 'a b' 'it'"'"'s' '$HOME' ''`},
		{[]string{"go", "run", "./cmd/x"}, "go run ./cmd/x"},
		{[]string{"sh", "-c", "a; b"}, "sh -c 'a; b'"},
	} {
		if got := shellCommand(tc.argv); got != tc.want {
			t.Errorf("%q: %s, want %s", tc.argv, got, tc.want)
		}
		if _, err := exec.LookPath("sh"); err == nil {
			// The shell reads back the words.
			script := "for a in " + tc.want[strings.IndexByte(tc.want+" ", ' '):] + `; do printf '[%s]' "$a"; done`
			got, err := exec.Command("sh", "-c", script).Output()
			if err != nil {
				t.Fatal(err)
			}
			var want strings.Builder
			for _, a := range tc.argv[1:] {
				want.WriteString("[" + a + "]")
			}
			if string(got) != want.String() {
				t.Errorf("%q: the shell read %s, want %s", tc.argv, got, want.String())
			}
		}
	}
}

// A test node with FakeProcesses answers the detached process calls as
// the scripts do, without running anything: start (a missing working
// directory is ErrNoCwd), list, logs and stop.
func TestProcessDetachedFakeProcesses(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{FakeProcesses: true})
	r := newRunner(env)
	lease := detachedLease(t, env)
	if _, err := r.StartProcessDetached(bg, lease, ProcessStart{Argv: []string{"npm", "run", "dev"}, Cwd: "/app", Name: "web"}); !errors.Is(err, ErrNoCwd) {
		t.Fatalf("a missing working directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(env.Node.PodDir(lease.SandboxID), "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ps, err := r.ProcessesDetached(bg, lease, ""); err != nil || len(ps) != 0 {
		t.Fatalf("no processes yet: %+v, %v", ps, err)
	}
	id, err := r.StartProcessDetached(bg, lease, ProcessStart{Argv: []string{"npm", "run", "dev"}, Cwd: "/app", Name: "ferun-env-stage-web"})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := r.ProcessesDetached(bg, lease, "")
	if err != nil || len(ps) != 1 || ps[0].ID != id || ps[0].Name != "ferun-env-stage-web" || !ps[0].Running || ps[0].ExitCode != nil {
		t.Fatalf("processes %+v, %v", ps, err)
	}
	if lines, err := r.ProcessLogsDetached(bg, lease, id, 10); err != nil || len(lines) != 1 || !strings.Contains(lines[0], "npm run dev") {
		t.Fatalf("logs %q, %v", lines, err)
	}
	if err := r.StopProcessDetached(bg, lease, id); err != nil {
		t.Fatal(err)
	}
	if ps, err = r.ProcessesDetached(bg, lease, ""); err != nil || len(ps) != 1 || ps[0].Running || ps[0].ExitCode != nil {
		t.Fatalf("after the stop: %+v, %v", ps, err)
	}
	if err := r.StopProcessDetached(bg, lease, id); err != nil {
		t.Errorf("a second stop: %v", err)
	}
	if _, err := r.ProcessLogsDetached(bg, lease, "000000-abcdef", 5); !errors.Is(err, ErrNoProcess) {
		t.Errorf("the logs of an unknown process: %v", err)
	}
	if err := r.StopProcessDetached(bg, lease, "000000-abcdef"); !errors.Is(err, ErrNoProcess) {
		t.Errorf("the stop of an unknown process: %v", err)
	}
}

// seedRecord writes a process record under root as the start script
// does, for a process that is over: files holds the record's files.
func seedRecord(t *testing.T, root, id string, files map[string]string) {
	t.Helper()
	d := filepath.Join(root, id)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range files {
		if err := os.WriteFile(filepath.Join(d, k), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A detached start removes the records of the processes of its name that
// are over (exited, or stopped without an exit code) but the newest
// keptExited, by start time: never a running one's, one whose start is
// not fully recorded, or another name's. The listing by name stays short
// however many records the container holds, and parses.
func TestProcessDetachedPrunes(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is not installed; the process scripts need it")
	}
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	lease := detachedLease(t, env)
	root := t.TempDir()
	detachedRoot(env, root)
	const name = "ferun-env-dev-web"
	first, err := r.StartProcessDetached(bg, lease, ProcessStart{Argv: []string{"sleep", "30"}, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	defer r.StopProcessDetached(bg, lease, first)
	var newest []string
	for i := range 300 {
		id := fmt.Sprintf("%06d-%06x", i, i)
		files := map[string]string{"name": name, "cmd": strings.Repeat("npm run dev ", 15), "pgid": "0",
			"started_at": strconv.Itoa(1000 + i), "timeout": "0"}
		if i%2 == 0 {
			files["exit"] = "1"
		}
		seedRecord(t, root, id, files)
		if i >= 300-keptExited {
			newest = append(newest, id)
		}
	}
	for i := range 40 {
		seedRecord(t, root, fmt.Sprintf("%06d-other", i), map[string]string{"name": "agent-tool", "pgid": "0",
			"started_at": "5", "exit": "0"})
	}
	seedRecord(t, root, "999999-unrecorded", map[string]string{"name": name, "pgid": "0"})

	second, err := r.StartProcessDetached(bg, lease, ProcessStart{Argv: []string{"sleep", "30"}, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	defer r.StopProcessDetached(bg, lease, second)
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	others := 0
	for _, e := range ents {
		b, _ := os.ReadFile(filepath.Join(root, e.Name(), "name"))
		switch string(b) {
		case name:
			left = append(left, e.Name())
		case "agent-tool":
			others++
		}
	}
	want := append(slices.Clone(newest), first, second, "999999-unrecorded")
	slices.Sort(want)
	if !slices.Equal(left, want) || others != 40 {
		t.Fatalf("records of %s left %v (want %v), %d of another name", name, left, want, others)
	}
	ps, err := r.ProcessesDetached(bg, lease, name)
	if err != nil || len(ps) != len(want) {
		t.Fatalf("processes named %s: %+v %v", name, ps, err)
	}
	for _, p := range ps {
		if p.Name != name || p.Running != (p.ID == first || p.ID == second) {
			t.Errorf("process %+v", p)
		}
	}
	if all, err := r.ProcessesDetached(bg, lease, ""); err != nil || len(all) != len(want)+40 {
		t.Fatalf("every process: %d %v", len(all), err)
	}
	if none, err := r.ProcessesDetached(bg, lease, "nope"); err != nil || len(none) != 0 {
		t.Fatalf("processes of a name none has: %+v %v", none, err)
	}
}

// A test node with FakeProcesses forgets what a detached start's script
// removes, and lists by name.
func TestProcessDetachedFakePrunes(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{FakeProcesses: true})
	r := newRunner(env)
	lease := detachedLease(t, env)
	start := func(name string) string {
		t.Helper()
		id, err := r.StartProcessDetached(bg, lease, ProcessStart{Argv: []string{"npm", "run", "dev"}, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	other := start("other")
	if err := r.StopProcessDetached(bg, lease, other); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for range 6 {
		id := start("web")
		ids = append(ids, id)
		if err := r.StopProcessDetached(bg, lease, id); err != nil {
			t.Fatal(err)
		}
	}
	last := start("web")
	ps, err := r.ProcessesDetached(bg, lease, "web")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range ps {
		got = append(got, p.ID)
	}
	if want := append(slices.Clone(ids[len(ids)-keptExited:]), last); !slices.Equal(got, want) {
		t.Fatalf("processes named web %v, want %v", got, want)
	}
	if all, err := r.ProcessesDetached(bg, lease, ""); err != nil || len(all) != keptExited+2 {
		t.Fatalf("every process %+v %v", all, err)
	}
}
