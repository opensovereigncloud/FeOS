// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

// The blueprints against the real images: each test below renders an
// addon, runs its image in docker the way FeOS runs a pod's container,
// waits for the rendered probe, runs the provisioning twice and connects
// with the rendered URL from a second container, as a pod of the
// environment would. They run only with FERUN_ADDON_DOCKER=1 (and a
// docker the user may use); they pull what is missing.
//
// How FeOS is emulated (see .claude/caas/spike-addons.md §0):
//   - the container runs as uid 0 (FeOS ignores the image's user);
//   - a rendered command replaces entrypoint and cmd (--entrypoint plus
//     arguments), an empty one runs the image's;
//   - the volume is a fresh directory owned by root, 0755, holding
//     lost+found (0700), as the guest's mke2fs leaves it, bind-mounted at
//     the mount path;
//   - the probe and the steps run through docker exec, which inherits the
//     container's env as FeOS's exec does, the step's env on top;
//   - pods talk over IPv6: a dual-stack network of its own, since
//     docker's default bridge has none, and every URL is the [ipv6] form;
//   - a stop is a SIGKILL (docker kill).
//
// Everything a test starts carries the label ferun-addon-test=1 and is
// removed when it ends; after a killed run:
//
//	docker rm -f $(docker ps -aq --filter label=ferun-addon-test=1)
//	docker network rm $(docker network ls -q --filter label=ferun-addon-test=1)
//
// The volume directories are temporary directories, emptied as root.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gardenlinux/feos/go/internal/model"
)

const (
	dockerGate  = "FERUN_ADDON_DOCKER"
	dockerLabel = "ferun-addon-test=1"
	// natsBox is the NATS client image (nats.go's CLI).
	natsBox = "natsio/nats-box:0.19.7"
)

// dockerLab is one test's docker network and what it started there.
type dockerLab struct {
	t       *testing.T
	network string
}

// newDockerLab skips t unless FERUN_ADDON_DOCKER=1, and makes a
// dual-stack network for it: 172.29.24<n>.0/24 and fd99:5b1e:ca7:<n>::/64.
func newDockerLab(t *testing.T, n int) *dockerLab {
	t.Helper()
	if os.Getenv(dockerGate) != "1" {
		t.Skip("set " + dockerGate + "=1 to run the addon images in docker")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("%s=1, but: %v", dockerGate, err)
	}
	l := &dockerLab{t: t, network: "ferun-addon-" + strings.ToLower(t.Name()) + "-" + randomHex(t, 3)}
	l.must(t, 2*time.Minute, "network", "create", "--label", dockerLabel, "--ipv6",
		"--subnet", fmt.Sprintf("172.29.24%d.0/24", n), "--subnet", fmt.Sprintf("fd99:5b1e:ca7:%d::/64", n), l.network)
	t.Cleanup(func() { l.docker(time.Minute, "network", "rm", l.network) })
	return l
}

// docker runs the docker CLI: stdout, stderr and the exit code (-1: it
// did not run to an exit).
func (l *dockerLab) docker(timeout time.Duration, args ...string) (string, string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			code = exit.ExitCode()
		} else {
			stderr.WriteString(err.Error())
		}
	}
	return stdout.String(), stderr.String(), code
}

// must runs docker and fails t unless it exits 0; its stdout.
func (l *dockerLab) must(t *testing.T, timeout time.Duration, args ...string) string {
	t.Helper()
	out, errOut, code := l.docker(timeout, args...)
	if code != 0 {
		t.Fatalf("docker %s: exit %d\n%s%s", strings.Join(redactArgs(args), " "), code, out, errOut)
	}
	return out
}

// redactArgs keeps generated credentials out of a failure's message.
func redactArgs(args []string) []string {
	out := slices.Clone(args)
	for i, a := range out {
		if k, _, ok := strings.Cut(a, "="); ok && i > 0 && out[i-1] == "-e" {
			out[i] = k + "=…"
		}
	}
	return out
}

// ensureImage pulls image unless docker has it.
func (l *dockerLab) ensureImage(t *testing.T, image string) {
	t.Helper()
	if _, _, code := l.docker(time.Minute, "image", "inspect", image); code != 0 {
		l.must(t, 10*time.Minute, "pull", "-q", image)
	}
}

// volume is a fresh volume as the node's guest formats one: a directory
// owned by root with lost+found, emptied again (as root, with image)
// when t ends.
func (l *dockerLab) volume(t *testing.T, image string) string {
	t.Helper()
	dir := t.TempDir()
	l.must(t, 2*time.Minute, "run", "--rm", "--label", dockerLabel, "--user", "0", "-v", dir+":/m",
		"--entrypoint", "sh", image, "-c", "chown 0:0 /m && chmod 755 /m && mkdir -m 700 /m/lost+found")
	t.Cleanup(func() {
		l.docker(2*time.Minute, "run", "--rm", "--label", dockerLabel, "--user", "0", "-v", dir+":/m",
			"--entrypoint", "sh", image, "-c",
			fmt.Sprintf("find /m -mindepth 1 -delete; chown %d:%d /m", os.Getuid(), os.Getgid()))
	})
	return dir
}

// start runs inst as FeOS would: uid 0, its command in place of the
// image's entrypoint and cmd (none: the image's), env resolved, the
// volume dir ("" none) at its mount, memory as the plan's. The
// container's name; it is removed when t ends.
func (l *dockerLab) start(t *testing.T, name string, inst Instance, env map[string]string, memoryMib int,
	dir string) string {
	t.Helper()
	name = l.network + "-" + name
	args := []string{"run", "-d", "--name", name, "--label", dockerLabel, "--network", l.network, "--user", "0",
		"--shm-size", "64m", "--memory", strconv.Itoa(memoryMib) + "m"}
	if inst.Mount != "" {
		if dir == "" {
			t.Fatal("the instance has a mount and no volume was made")
		}
		args = append(args, "-v", dir+":"+inst.Mount)
	}
	args = append(args, envArgs(env)...)
	if len(inst.Command) > 0 {
		args = append(args, "--entrypoint", inst.Command[0], inst.Image)
		args = append(args, inst.Command[1:]...)
	} else {
		args = append(args, inst.Image)
	}
	t.Cleanup(func() { l.docker(time.Minute, "rm", "-f", "-v", name) })
	l.must(t, 2*time.Minute, args...)
	return name
}

func envArgs(env map[string]string) []string {
	var args []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "-e", k+"="+env[k])
	}
	return args
}

// exec runs argv in container with env on top of its own: output
// (stdout and stderr) and exit code.
func (l *dockerLab) exec(container string, env map[string]string, argv ...string) (string, int) {
	args := append([]string{"exec"}, envArgs(env)...)
	args = append(args, container)
	out, errOut, code := l.docker(2*time.Minute, append(args, argv...)...)
	return out + errOut, code
}

// waitReady polls the probe until it exits 0, failing t when the
// container stops or a minute and a half passes.
func (l *dockerLab) waitReady(t *testing.T, container string, probe []string) time.Duration {
	t.Helper()
	begin := time.Now()
	for {
		out, code := l.exec(container, nil, probe...)
		if code == 0 {
			return time.Since(begin)
		}
		running, _, _ := l.docker(time.Minute, "inspect", "-f", "{{.State.Running}}", container)
		if strings.TrimSpace(running) != "true" || time.Since(begin) > 90*time.Second {
			logs, logErr, _ := l.docker(time.Minute, "logs", "--tail", "40", container)
			t.Fatalf("%s not ready (running %s, probe exit %d: %s)\n%s%s", container, strings.TrimSpace(running),
				code, out, logs, logErr)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// address is the container's IPv6 address on the lab's network.
func (l *dockerLab) address(t *testing.T, container string) string {
	t.Helper()
	addr := strings.TrimSpace(l.must(t, time.Minute, "inspect", "-f",
		`{{(index .NetworkSettings.Networks "`+l.network+`").GlobalIPv6Address}}`, container))
	if !strings.Contains(addr, ":") {
		t.Fatalf("%s has no IPv6 address: %q", container, addr)
	}
	return addr
}

// client runs a one-off container on the lab's network (a pod of the
// environment): output and exit code.
func (l *dockerLab) client(entrypoint, image string, args ...string) (string, int) {
	all := []string{"run", "--rm", "--label", dockerLabel, "--network", l.network, "--entrypoint", entrypoint, image}
	out, errOut, code := l.docker(2*time.Minute, append(all, args...)...)
	return out + errOut, code
}

// provision runs the addon's steps in container as the controller
// would, each with its env resolved; each must exit 0 without a word.
func (l *dockerLab) provision(t *testing.T, container string, steps []Step, creds map[string]string) {
	t.Helper()
	for _, st := range steps {
		out, code := l.exec(container, resolveEnv(t, st.Env, creds), st.Argv...)
		if code != 0 || out != "" {
			t.Fatalf("step %s: exit %d\n%s", st.Name, code, redact(out, creds))
		}
	}
}

// credentials generates each of a's credentials as the controller does:
// 32 hex characters.
func credentials(t *testing.T, b Blueprint, a Addon) map[string]string {
	creds := map[string]string{}
	for _, k := range b.Credentials(a) {
		creds[k] = randomHex(t, 16)
	}
	return creds
}

func randomHex(t *testing.T, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func resolveEnv(t *testing.T, env map[string]model.TemplateValue, creds map[string]string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = resolve(t, v, creds)
	}
	return out
}

// redact replaces the credentials' values in s, as ctl must before it
// stores a step's output.
func redact(s string, creds map[string]string) string {
	for _, v := range creds {
		s = strings.ReplaceAll(s, v, "<redacted>")
	}
	return s
}

// outputURL is the addon's rendered url reference, resolved.
func outputURL(t *testing.T, b Blueprint, a Addon, c Context, fields map[string]string, creds map[string]string) string {
	t.Helper()
	parts, err := b.Outputs(a, c, Ref{Key: "url", Fields: fields})
	if err != nil {
		t.Fatal(err)
	}
	u := resolveParts(t, parts, creds)
	if !strings.Contains(u, "@["+c.Address+"]:") {
		t.Fatalf("url %s is not the [ipv6] form", redact(u, creds))
	}
	return u
}

func TestPostgresDocker(t *testing.T) {
	lab := newDockerLab(t, 1)
	b, _ := Get("postgres")
	for _, version := range b.Versions() {
		t.Run(version, func(t *testing.T) {
			a := addon(t, "postgres", `{"type":"postgres","version":"`+version+`","plan":"dev",
				"options":{"databases":["expensa","reports"],"roles":["expense_svc","approval_svc"]}}`)
			if fs := b.Validate(nil, a); len(fs) != 0 {
				t.Fatalf("findings:\n%s", findingText(fs))
			}
			creds := credentials(t, b, a)
			// One password that needs every escape: the url's userinfo
			// must carry it to libpq intact. (Generated ones are hex.)
			creds["role.approval_svc.password"] = `p@ss:w/rd%?#&=+ "'$x\;` + randomHex(t, 4)
			c := Context{Env: "test", Name: a.Name}
			inst, err := b.Render(a, c)
			if err != nil {
				t.Fatal(err)
			}
			lab.ensureImage(t, inst.Image)
			pod := lab.start(t, "pg"+version, inst, resolveEnv(t, inst.Env, creds), b.Plans()[a.Plan].MemoryMib,
				lab.volume(t, inst.Image))
			t.Logf("ready after %s", lab.waitReady(t, pod, inst.Probe).Round(time.Millisecond))
			steps := b.Provision(a, c)
			lab.provision(t, pod, steps, creds)
			lab.provision(t, pod, steps, creds) // idempotent, silent

			c.Address = lab.address(t, pod)
			psql := func(url, sql string) (string, int) {
				out, code := lab.client("psql", inst.Image, url, "-X", "-q", "-At", "-v", "ON_ERROR_STOP=1", "-c", sql)
				return strings.TrimSpace(redact(out, creds)), code
			}
			expense := outputURL(t, b, a, c, map[string]string{"role": "expense_svc"}, creds)
			if out, code := psql(expense, "CREATE TABLE expenses (id int); INSERT INTO expenses VALUES (1); "+
				"SELECT current_user, current_database(), current_schema(), inet_server_addr()"); code != 0 ||
				out != "expense_svc|expensa|expense_svc|"+c.Address {
				t.Fatalf("expense_svc: exit %d: %s", code, out)
			}
			approval := outputURL(t, b, a, c, map[string]string{"role": "approval_svc", "database": "reports"}, creds)
			if out, code := psql(approval, "SELECT current_user, current_database()"); code != 0 ||
				out != "approval_svc|reports" {
				t.Fatalf("approval_svc: exit %d: %s", code, out)
			}
			approvalExpensa := outputURL(t, b, a, c, map[string]string{"role": "approval_svc"}, creds)
			if out, code := psql(approvalExpensa, "SELECT * FROM expense_svc.expenses"); code == 0 ||
				!strings.Contains(out, "permission denied for schema expense_svc") {
				t.Fatalf("approval_svc read expense_svc's schema: exit %d: %s", code, out)
			}
			super := outputURL(t, b, a, c, nil, creds)
			if out, code := psql(super, "SELECT current_user, current_database(), current_setting('shared_buffers')"); code != 0 ||
				out != "postgres|expensa|64MB" {
				t.Fatalf("superuser: exit %d: %s", code, out)
			}
			t.Logf("url %s", redact(approval, creds))
			wrong := outputURL(t, b, a, c, nil, map[string]string{"password": "wrong"})
			if out, code := psql(wrong, "SELECT 1"); code == 0 || !strings.Contains(out, "password authentication failed") {
				t.Fatalf("a wrong password: exit %d: %s", code, out)
			} else {
				t.Logf("a wrong password: exit %d: %s", code, out)
			}
			// Loopback TCP asks for the password too (a port-forward
			// dials 127.0.0.1 in the pod).
			if out, code := lab.exec(pod, nil, "psql", "-X", "-h", "127.0.0.1", "-U", "postgres", "-c", "SELECT 1"); code == 0 ||
				!strings.Contains(out, "no password supplied") {
				t.Fatalf("loopback without a password: exit %d: %s", code, out)
			} else {
				t.Logf("loopback without a password: exit %d: %s", code, strings.TrimSpace(out))
			}

			// A node-side stop is a kill: crash recovery, then the same
			// data, and provisioning still converges silently.
			lab.must(t, time.Minute, "kill", pod)
			lab.must(t, time.Minute, "start", pod)
			lab.waitReady(t, pod, inst.Probe)
			lab.provision(t, pod, steps, creds)
			c.Address = lab.address(t, pod)
			expense = outputURL(t, b, a, c, map[string]string{"role": "expense_svc"}, creds)
			if out, code := psql(expense, "SELECT count(*) FROM expenses"); code != 0 || out != "1" {
				t.Fatalf("after a restart: exit %d: %s", code, out)
			}

			// A role added later whose schema another role made first, open
			// to everyone: provisioning fails naming the schema rather than
			// adopt it (expense_svc would own what late_svc's unqualified
			// names resolve to).
			if out, code := psql(expense, "CREATE SCHEMA late_svc; GRANT USAGE, CREATE ON SCHEMA late_svc TO PUBLIC"); code != 0 {
				t.Fatalf("expense_svc makes late_svc's schema: exit %d: %s", code, out)
			}
			later := addon(t, "postgres", `{"type":"postgres","version":"`+version+`","plan":"dev",
				"options":{"databases":["expensa","reports"],"roles":["expense_svc","approval_svc","late_svc"]}}`)
			if fs := b.Validate(&a, later); len(fs) != 0 {
				t.Fatalf("findings:\n%s", findingText(fs))
			}
			creds["role.late_svc.password"] = randomHex(t, 16)
			for _, st := range b.Provision(later, c) {
				out, code := lab.exec(pod, resolveEnv(t, st.Env, creds), st.Argv...)
				out = redact(out, creds)
				if code == 0 || !strings.Contains(out, "schema late_svc in database expensa belongs to role expense_svc, not to role late_svc") {
					t.Fatalf("step %s past a squatted schema: exit %d\n%s", st.Name, code, out)
				}
				t.Logf("step %s past a squatted schema: exit %d: %s", st.Name, code, strings.TrimSpace(out))
			}
			if out, code := psql(super, "SELECT nspowner::regrole FROM pg_namespace WHERE nspname = 'late_svc'"); code != 0 ||
				out != "expense_svc" {
				t.Fatalf("late_svc's schema after the failed step: exit %d: %s", code, out)
			}
		})
	}
}

func TestValkeyDocker(t *testing.T) {
	lab := newDockerLab(t, 2)
	b, _ := Get("valkey")
	for _, version := range b.Versions() {
		t.Run(version, func(t *testing.T) {
			a := addon(t, "cache", `{"type":"valkey","version":"`+version+`","plan":"dev",
				"options":{"maxmemoryPolicy":"allkeys-lru"}}`)
			if fs := b.Validate(nil, a); len(fs) != 0 {
				t.Fatalf("findings:\n%s", findingText(fs))
			}
			creds := credentials(t, b, a)
			c := Context{Env: "test", Name: a.Name}
			inst, err := b.Render(a, c)
			if err != nil {
				t.Fatal(err)
			}
			lab.ensureImage(t, inst.Image)
			pod := lab.start(t, "valkey"+version, inst, resolveEnv(t, inst.Env, creds), b.Plans()[a.Plan].MemoryMib,
				lab.volume(t, inst.Image))
			t.Logf("ready after %s", lab.waitReady(t, pod, inst.Probe).Round(time.Millisecond))
			// The probe fails on a wrong password: valkey-cli exits 0 on
			// NOAUTH, the grep does not.
			if out, code := lab.exec(pod, map[string]string{"VALKEY_PASSWORD": "wrong"}, inst.Probe...); code == 0 {
				t.Fatalf("the probe passed with a wrong password: %s", out)
			} else {
				t.Logf("the probe with a wrong password: exit %d: %s", code, strings.TrimSpace(out))
			}
			lab.provision(t, pod, b.Provision(a, c), creds)
			lab.provision(t, pod, b.Provision(a, c), creds)

			c.Address = lab.address(t, pod)
			cli := func(url string, args ...string) (string, int) {
				out, code := lab.client("valkey-cli", inst.Image, append([]string{"--no-auth-warning", "-u", url}, args...)...)
				return strings.TrimSpace(redact(out, creds)), code
			}
			u := outputURL(t, b, a, c, nil, creds)
			t.Logf("url %s", redact(u, creds))
			for _, check := range []struct {
				args []string
				want string
			}{
				{[]string{"ping"}, "PONG"},
				{[]string{"set", "persisted", "yes"}, "OK"},
				{[]string{"get", "persisted"}, "yes"},
				{[]string{"config", "get", "maxmemory"}, "maxmemory\n" + strconv.Itoa(89<<20)},
				{[]string{"config", "get", "maxmemory-policy"}, "maxmemory-policy\nallkeys-lru"},
				{[]string{"config", "get", "appendonly"}, "appendonly\nyes"},
				{[]string{"config", "get", "dir"}, "dir\n/data"},
				{[]string{"acl", "whoami"}, "default"},
			} {
				if out, code := cli(u, check.args...); code != 0 || out != check.want {
					t.Fatalf("%v: exit %d: %q, want %q", check.args, code, out, check.want)
				}
			}
			if out, _ := cli(outputURL(t, b, a, c, nil, map[string]string{"password": "wrong"}), "ping"); out == "PONG" ||
				!strings.Contains(out, "WRONGPASS") {
				t.Fatalf("a wrong password: %s", out)
			} else {
				t.Logf("a wrong password: %s", strings.ReplaceAll(out, "\n", " / "))
			}
			// The entrypoint dropped to valkey and handed /data to it.
			if out, code := lab.exec(pod, nil, "stat", "-c", "%U", "/data/appendonlydir"); code != 0 ||
				strings.TrimSpace(out) != "valkey" {
				t.Fatalf("appendonlydir: exit %d: %s", code, out)
			}
		})
	}
}

func TestNATSDocker(t *testing.T) {
	lab := newDockerLab(t, 3)
	lab.ensureImage(t, natsBox)
	b, _ := Get("nats")
	for _, jetstream := range []bool{true, false} {
		t.Run("jetstream="+strconv.FormatBool(jetstream), func(t *testing.T) {
			a := addon(t, "bus", `{"type":"nats","version":"2","plan":"dev","options":{"jetstream":`+
				strconv.FormatBool(jetstream)+`}}`)
			if fs := b.Validate(nil, a); len(fs) != 0 {
				t.Fatalf("findings:\n%s", findingText(fs))
			}
			creds := credentials(t, b, a)
			c := Context{Env: "test", Name: a.Name}
			inst, err := b.Render(a, c)
			if err != nil {
				t.Fatal(err)
			}
			lab.ensureImage(t, inst.Image)
			dir := ""
			if inst.Mount != "" {
				dir = lab.volume(t, inst.Image)
			}
			pod := lab.start(t, "nats-"+strconv.FormatBool(jetstream), inst, resolveEnv(t, inst.Env, creds),
				b.Plans()[a.Plan].MemoryMib, dir)
			t.Logf("ready after %s", lab.waitReady(t, pod, inst.Probe).Round(time.Millisecond))
			lab.provision(t, pod, b.Provision(a, c), creds)
			lab.provision(t, pod, b.Provision(a, c), creds)

			c.Address = lab.address(t, pod)
			nats := func(url string, args ...string) (string, int) {
				out, code := lab.client("nats", natsBox, append([]string{"--server", url}, args...)...)
				return redact(out, creds), code
			}
			u := outputURL(t, b, a, c, nil, creds)
			t.Logf("url %s", redact(u, creds))
			if out, code := nats(u, "server", "check", "connection"); code != 0 || !strings.Contains(out, "OK Connection OK") {
				t.Fatalf("connect: exit %d: %s", code, out)
			}
			if out, code := nats(u, "pub", "orders.new", "hello"); code != 0 {
				t.Fatalf("publish: exit %d: %s", code, out)
			}
			if jetstream {
				if out, code := nats(u, "stream", "add", "ORDERS", "--subjects", "orders.>", "--storage", "file",
					"--defaults"); code != 0 {
					t.Fatalf("stream add: exit %d: %s", code, out)
				}
				if out, code := nats(u, "pub", "orders.new", "hello"); code != 0 {
					t.Fatalf("publish: exit %d: %s", code, out)
				}
				if out, code := nats(u, "stream", "info", "ORDERS", "--json"); code != 0 ||
					!strings.Contains(out, `"messages": 1`) {
					t.Fatalf("stream info: exit %d: %s", code, out)
				}
				if out, code := lab.exec(pod, nil, "ls", "/data/jetstream"); code != 0 {
					t.Fatalf("the store is not on the volume: exit %d: %s", code, out)
				}
			} else if out, code := nats(u, "stream", "ls"); code == 0 || !strings.Contains(out, "no responders") {
				// Nothing serves JetStream's API.
				t.Fatalf("streams without JetStream: exit %d: %s", code, out)
			} else {
				t.Logf("streams without JetStream: exit %d: %s", code, strings.TrimSpace(out))
			}
			if out, code := nats(outputURL(t, b, a, c, nil, map[string]string{"password": "wrong"}),
				"server", "check", "connection"); code == 0 || !strings.Contains(out, "Authorization Violation") {
				t.Fatalf("a wrong password: exit %d: %s", code, out)
			} else {
				t.Logf("a wrong password: exit %d: %s", code, strings.TrimSpace(out))
			}
			// The monitor (no authentication) is on loopback only.
			if out, code := lab.client("wget", inst.Image, "-qO-", "-T", "3",
				"http://["+c.Address+"]:8222/healthz"); code == 0 || !strings.Contains(out, "Connection refused") {
				t.Fatalf("the monitor answers env peers: exit %d: %s", code, out)
			} else {
				t.Logf("the monitor from a peer: exit %d: %s", code, strings.TrimSpace(out))
			}
		})
	}
}
