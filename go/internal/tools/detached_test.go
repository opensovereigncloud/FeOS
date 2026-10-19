// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// WriteFileDetached and ReadFileDetached go through the guest's tools; a
// file that does not exist is ErrNoFile.
func TestFileDetached(t *testing.T) {
	env := toolstest.New(t, toolstest.Options{})
	r := newRunner(env)
	lease := detachedLease(t, env)
	content := "127.0.0.1 localhost\n::1 localhost\n# ferun environment e begin\nfd00::5 postgres\n# ferun environment e end\n"
	if err := r.WriteFileDetached(bg, lease, "/etc/hosts", []byte(content)); err != nil {
		t.Fatal(err)
	}
	got, err := r.ReadFileDetached(bg, lease, "/etc/hosts")
	if err != nil || got != content {
		t.Fatalf("read %q, %v", got, err)
	}
	// Longer than a page of the read tool.
	var long strings.Builder
	for i := range 4500 {
		long.WriteString(strings.Repeat("x", i%7) + "\n")
	}
	if err := r.WriteFileDetached(bg, lease, "/long", []byte(long.String())); err != nil {
		t.Fatal(err)
	}
	if got, err := r.ReadFileDetached(bg, lease, "/long"); err != nil || got != long.String() {
		t.Errorf("a long file read back as %d bytes (want %d), %v", len(got), long.Len(), err)
	}
	if _, err := r.ReadFileDetached(bg, lease, "/nope"); !errors.Is(err, ErrNoFile) {
		t.Errorf("a missing file: %v", err)
	}
	if got, err := r.ReadFileDetached(bg, lease, "/empty"); !errors.Is(err, ErrNoFile) {
		t.Errorf("a missing file: %q %v", got, err)
	}
	if err := r.WriteFileDetached(bg, lease, "/empty", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := r.ReadFileDetached(bg, lease, "/empty"); err != nil || got != "" {
		t.Errorf("an empty file: %q %v", got, err)
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
	ps, err := r.ProcessesDetached(bg, lease)
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
	ps, err = r.ProcessesDetached(bg, lease)
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
		ps, err = r.ProcessesDetached(bg, lease)
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
	if ps, err := r.ProcessesDetached(bg, lease); err != nil || len(ps) != 0 {
		t.Fatalf("no processes yet: %+v, %v", ps, err)
	}
	id, err := r.StartProcessDetached(bg, lease, ProcessStart{Argv: []string{"npm", "run", "dev"}, Cwd: "/app", Name: "ferun-env-stage-web"})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := r.ProcessesDetached(bg, lease)
	if err != nil || len(ps) != 1 || ps[0].ID != id || ps[0].Name != "ferun-env-stage-web" || !ps[0].Running || ps[0].ExitCode != nil {
		t.Fatalf("processes %+v, %v", ps, err)
	}
	if lines, err := r.ProcessLogsDetached(bg, lease, id, 10); err != nil || len(lines) != 1 || !strings.Contains(lines[0], "npm run dev") {
		t.Fatalf("logs %q, %v", lines, err)
	}
	if err := r.StopProcessDetached(bg, lease, id); err != nil {
		t.Fatal(err)
	}
	if ps, err = r.ProcessesDetached(bg, lease); err != nil || len(ps) != 1 || ps[0].Running || ps[0].ExitCode != nil {
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
