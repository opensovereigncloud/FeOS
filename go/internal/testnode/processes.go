// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The control plane's background processes (process_start and the
// environment's attached services) are Python scripts it execs into a
// container, which keep their records under processRoot. With
// Config.FakeProcesses the test node answers those scripts itself and
// keeps the processes in memory, without running anything: a process
// runs until it is stopped, its pod boots anew, or an ExitRule
// "process/<name glob>" ends it. A detached start forgets the processes of
// its name that are over but the newest few, as its script removes their
// records.

// processRoot is where the control plane's process scripts keep their
// records (tools.processRoot); it tells the scripts apart from other
// commands.
const processRoot = `root = "/var/lib/sandbox-processes"`

// The scripts, by a line only each of them has.
const (
	markStart = "subprocess.Popen("
	markList  = `print(json.dumps(out, indent=2) if out else "no background processes")`
	markNamed = `print(json.dumps(out,separators=(",",":")))`
	markLogs  = "tail=f.readlines()"
	markStop  = "os.killpg(pgid, signal.SIGTERM)"
	// markPrune: a start (the detached one) that first removes the
	// records of its name's processes that are over, but the newest
	// argv[5].
	markPrune = "shutil.rmtree("
)

// fakeProc is a process of FakeProcesses.
type fakeProc struct {
	id, name, cmd string
	pgid          int
	running       bool
	exitCode      *int
	startedAt     int64
	timeout       int
	log           []string
}

// processService keeps the fake processes by pod ID.
type processService struct {
	n    *Node
	mu   sync.Mutex
	pods map[string][]*fakeProc
	seq  int
}

func newProcessService(n *Node) *processService {
	return &processService{n: n, pods: map[string][]*fakeProc{}}
}

// isProcessScript reports whether argv is one of the control plane's
// process scripts.
func isProcessScript(argv []string) bool {
	return len(argv) >= 3 && argv[0] == "python3" && argv[1] == "-c" && strings.Contains(argv[2], processRoot)
}

// exec answers a process script for the pod of g: its exit code, and
// false when argv is no such script.
func (s *processService) exec(g *guest, argv []string, out *frameSender) (int32, bool, error) {
	if !isProcessScript(argv) {
		return 0, false, nil
	}
	script, args := argv[2], argv[3:]
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	say := func(code int32, format string, a ...any) (int32, bool, error) {
		return code, true, out.Stdout(fmt.Appendf(nil, format, a...))
	}
	switch {
	case strings.Contains(script, markStart):
		cmd, name, cwd := arg(0), arg(2), arg(3)
		timeout, _ := strconv.Atoi(arg(1))
		if cwd != "" {
			rel, err := toRel(cwd)
			st, serr := os.Stat(filepath.Join(g.dir, filepath.FromSlash(rel)))
			if err != nil || serr != nil || !st.IsDir() {
				return say(3, "no such working directory: %s\n", cwd)
			}
		}
		if keep, err := strconv.Atoi(arg(4)); err == nil && name != "" && strings.Contains(script, markPrune) {
			s.prune(g.id, name, keep)
		}
		p := s.start(g.id, cmd, name, timeout)
		h, err := json.Marshal(map[string]any{"id": p.id, "pgid": p.pgid, "log": "/var/lib/sandbox-processes/" + p.id + "/log",
			"timeout": timeout, "name": name})
		if err != nil {
			return 0, true, err
		}
		return say(0, "%s\n", h)
	case strings.Contains(script, markList):
		rows := s.list(g.id)
		if len(rows) == 0 {
			return say(0, "no background processes\n")
		}
		b, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return 0, true, err
		}
		return say(0, "%s\n", b)
	case strings.Contains(script, markNamed):
		rows := []namedRow{}
		for _, r := range s.list(g.id) {
			if arg(0) == "" || r.Name == arg(0) {
				rows = append(rows, namedRow{ID: r.ID, Name: r.Name, Running: r.Running, ExitCode: r.ExitCode})
			}
		}
		b, err := json.Marshal(rows)
		if err != nil {
			return 0, true, err
		}
		return say(0, "%s\n", b)
	case strings.Contains(script, markLogs):
		n, _ := strconv.Atoi(arg(1))
		lines, ok := s.logs(g.id, arg(0), n)
		switch {
		case !ok:
			return say(1, "no such process: %s\n", arg(0))
		case len(lines) == 0:
			return say(0, "(no output yet)\n")
		}
		return say(0, "%s\n", strings.Join(lines, "\n"))
	case strings.Contains(script, markStop):
		was, ok := s.stop(g.id, arg(0))
		switch {
		case !ok:
			return say(1, "no such process: %s\n", arg(0))
		case !was:
			return say(0, "process %s was not running any more\n", arg(0))
		}
		return say(0, "process %s stopped\n", arg(0))
	}
	return say(1, "ferun-testnode: a process script it does not know\n")
}

// start records a running process, and schedules its exit when an exit
// rule names it.
func (s *processService) start(pod, cmd, name string, timeout int) *fakeProc {
	s.mu.Lock()
	s.seq++
	p := &fakeProc{
		id:   time.Now().UTC().Format("150405") + fmt.Sprintf("-%06x", s.seq),
		name: name, cmd: cmd, pgid: 10000 + s.seq, running: true, startedAt: time.Now().Unix(), timeout: timeout,
		log: []string{"ferun-testnode: started " + cmd},
	}
	s.pods[pod] = append(s.pods[pod], p)
	s.mu.Unlock()
	if r, ok := s.n.exitRuleOf("process/" + name); ok {
		s.n.goPods(func(ctx context.Context) {
			if sleep(ctx, r.After) {
				s.exit(p, r.Code)
			}
		})
	}
	return p
}

// exit ends a running process with code (negative: none).
func (s *processService) exit(p *fakeProc, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !p.running {
		return
	}
	p.running = false
	if code >= 0 {
		p.exitCode = &code
		p.log = append(p.log, fmt.Sprintf("ferun-testnode: exited with code %d", code))
	}
}

// processRow is a row of the list script's output.
type processRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Command   string `json:"command"`
	Running   bool   `json:"running"`
	StartedAt int64  `json:"started_at"`
	Timeout   int    `json:"timeout"`
	ExitCode  *int   `json:"exit_code,omitempty"`
}

// namedRow is a row of the detached list script's output.
type namedRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Running  bool   `json:"running"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// prune forgets the processes named name that are over, but the newest
// keep, as the detached start script removes their records.
func (s *processService) prune(pod, name string, keep int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	over := 0
	for _, p := range s.pods[pod] {
		if p.name == name && !p.running {
			over++
		}
	}
	s.pods[pod] = slices.DeleteFunc(s.pods[pod], func(p *fakeProc) bool {
		if p.name != name || p.running || over <= keep {
			return false
		}
		over--
		return true
	})
}

func (s *processService) list(pod string) []processRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []processRow
	for _, p := range s.pods[pod] {
		cmd := p.cmd
		if len(cmd) > 200 {
			cmd = cmd[:200]
		}
		rows = append(rows, processRow{ID: p.id, Name: p.name, Command: cmd, Running: p.running, StartedAt: p.startedAt,
			Timeout: p.timeout, ExitCode: p.exitCode})
	}
	return rows
}

func (s *processService) find(pod, id string) *fakeProc {
	i := slices.IndexFunc(s.pods[pod], func(p *fakeProc) bool { return p.id == id })
	if i < 0 {
		return nil
	}
	return s.pods[pod][i]
}

func (s *processService) logs(pod, id string, n int) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.find(pod, id)
	if p == nil {
		return nil, false
	}
	if n <= 0 {
		n = 100
	}
	return slices.Clone(p.log[max(0, len(p.log)-n):]), true
}

// stop ends a process as the stop script's TERM does: no exit code (the
// wrapper that records it ends with it). It reports whether the process
// was running, and false, false for one the pod does not have.
func (s *processService) stop(pod, id string) (was, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.find(pod, id)
	if p == nil {
		return false, false
	}
	was, p.running = p.running, false
	return was, true
}

// booted ends the processes of a pod that boots anew: their records stay
// on its disk, without an exit code.
func (s *processService) booted(pod string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pods[pod] {
		p.running = false
	}
}

// deleted forgets the processes of a deleted pod.
func (s *processService) deleted(pod string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pods, pod)
}
