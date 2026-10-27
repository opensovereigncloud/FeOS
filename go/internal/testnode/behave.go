// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package testnode

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	podv1 "github.com/gardenlinux/feos/go/gen/feos/pod/v1"
	"github.com/gardenlinux/feos/go/internal/podid"
)

// objectLabel is the label naming an environment's object of a pod
// (model.LabelObject): "<kind>/<name>".
const objectLabel = "ferun.object"

// ExitRule makes the main container of every pod whose ferun.object
// label matches Object exit with Code, After its boot: a job that
// finishes, or a service that crashes after every start. It is how a
// control plane's environments are driven end to end on a test node,
// whose containers otherwise run until their pod ends. With
// Config.FakeProcesses, "process/<name glob>" ends a fake process After
// its start (an attached service that crashes).
type ExitRule struct {
	// Object is a glob over the label's value, "job/*" or
	// "service/web", or "process/<name>"; "*" does not match "/".
	Object string
	// Code is the exit code; a negative one exits without a code (the
	// node reports no exit status).
	Code int
	// After is the time from the boot (create, start, restore) to the
	// exit.
	After time.Duration
	re    *regexp.Regexp
}

// defaultExitAfter is an exit rule's delay when it names none.
const defaultExitAfter = 2 * time.Second

// ParseExitRule reads a rule written <glob>=<code>[@<duration>]:
// "job/*=0@3s", "service/crasher=1".
func ParseExitRule(s string) (ExitRule, error) {
	glob, rest, ok := strings.Cut(s, "=")
	if !ok || glob == "" {
		return ExitRule{}, fmt.Errorf("exit rule %q: want <object glob>=<code>[@<duration>]", s)
	}
	codeText, afterText, hasAfter := strings.Cut(rest, "@")
	code, err := strconv.Atoi(codeText)
	if err != nil || code > 255 {
		return ExitRule{}, fmt.Errorf("exit rule %q: the code is an integer up to 255, or negative for none", s)
	}
	r := ExitRule{Object: glob, Code: code, After: defaultExitAfter}
	if hasAfter {
		if r.After, err = time.ParseDuration(afterText); err != nil || r.After < 0 {
			return ExitRule{}, fmt.Errorf("exit rule %q: the delay is a duration, 3s", s)
		}
	}
	if err := r.compile(); err != nil {
		return ExitRule{}, err
	}
	return r, nil
}

func (r *ExitRule) compile() error {
	if r.re != nil {
		return nil
	}
	re, err := globRegexp(r.Object, false)
	if err != nil {
		return fmt.Errorf("exit rule %q: %w", r.Object, err)
	}
	r.re = re
	return nil
}

// exitRuleOf returns the first rule that matches an object, a pod's
// ferun.object label or "process/<name>" of a fake process.
func (n *Node) exitRuleOf(object string) (ExitRule, bool) {
	if object == "" {
		return ExitRule{}, false
	}
	for _, r := range n.cfg.ContainerExits {
		if r.re != nil && r.re.MatchString(object) {
			return r, true
		}
	}
	return ExitRule{}, false
}

// bootedLocked is what a boot of pd sets going (p.mu held): the start
// lines of Config.StartLog, and the exit an ExitRule schedules; a cold
// boot (not a restore of the memory) ends the pod's fake processes. It
// returns the schedule, which the caller runs once p.mu is released;
// nil when nothing is scheduled.
func (p *podService) bootedLocked(pd *pod, cold bool) func() {
	spec := pd.info.GetSpec()
	if cold {
		p.n.procs.booted(pd.info.PodId)
	}
	if p.n.cfg.StartLog {
		if pd.logs == nil {
			pd.logs = map[string][]string{}
		}
		for _, c := range spec.GetContainers() {
			pd.logs[c.Name] = append(pd.logs[c.Name], fmt.Sprintf("ferun-testnode: container %s of pod %s started (image %s, boot %d)",
				c.Name, pd.info.PodId, c.GetImageRef(), pd.pid))
		}
	}
	r, ok := p.n.exitRuleOf(spec.GetLabels()[objectLabel])
	if !ok {
		return nil
	}
	uid, pid := pd.uid, pd.pid
	return func() {
		p.n.goPods(func(ctx context.Context) {
			if sleep(ctx, r.After) {
				p.exitBoot(uid, pid, "main", r.Code)
			}
		})
	}
}

// exitBoot makes a container of a pod exit, if the pod still runs the
// boot pid (a restart or restore since then schedules its own exit).
func (p *podService) exitBoot(uid podid.ID, pid int64, container string, code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pd := p.byUIDLocked(uid)
	if pd == nil || pd.pid != pid || pd.info.State != podv1.PodState_POD_STATE_RUNNING {
		return
	}
	if pd.exited == nil {
		pd.exited = map[string]containerExit{}
	}
	e := containerExit{code: int32(code), hasCode: code >= 0}
	pd.exited[container] = e
	if p.n.cfg.StartLog {
		if pd.logs == nil {
			pd.logs = map[string][]string{}
		}
		line := "ferun-testnode: container " + container + " exited without a code"
		if e.hasCode {
			line = fmt.Sprintf("ferun-testnode: container %s exited with code %d", container, code)
		}
		pd.logs[container] = append(pd.logs[container], line)
	}
}
