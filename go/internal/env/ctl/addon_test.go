// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	containerv1 "github.com/ironcore-dev/feos/go/gen/feos/container/v1"
	volumev1 "github.com/ironcore-dev/feos/go/gen/feos/volume/v1"
	"github.com/ironcore-dev/feos/go/internal/env/envdeps"
	"github.com/ironcore-dev/feos/go/internal/env/spec"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
	"github.com/ironcore-dev/feos/go/internal/testnode"
)

// execLog records the Execs of the environments' pods, which exit 0.
type execLog struct {
	mu    sync.Mutex
	calls []*containerv1.ExecRequest
	pods  []uuid.UUID
}

func (l *execLog) fn(_ context.Context, pod uuid.UUID, req *containerv1.ExecRequest, _ testnode.ExecOutput) (int32, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, req)
	l.pods = append(l.pods, pod)
	return 0, false, nil
}

// find returns the recorded Execs whose command's first word is argv0.
func (l *execLog) find(argv0 string) []*containerv1.ExecRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*containerv1.ExecRequest
	for _, c := range l.calls {
		if len(c.Command) > 0 && c.Command[0] == argv0 {
			out = append(out, c)
		}
	}
	return out
}

const postgresSpec = `{"addons":{"postgres":{"type":"postgres","version":"17","plan":"dev",
	"options":{"databases":["app"],"roles":["svc"]}}}}`

// An addon (the postgres blueprint) comes up with its credentials as
// system secrets, its volume created once on its node with the plan's
// size, its row pinned there, its env group put before its pod, its probe
// passing and its provisioning run with the credentials in its env; a
// plan change recreates it on the same node with the same volume.
func TestAddonLifecycle(t *testing.T) {
	h := newHarness(t, options{})
	n1 := h.addNode("n1")
	execs := &execLog{}
	n1.Script().SetExec(execs.fn)
	env := h.newEnv("stage")
	r1 := h.apply(env.ID, postgresSpec)
	h.waitRelease(env.ID, r1, model.ReleaseRealised, 60*time.Second)

	// Credentials: 32 hex characters, version 1, system.
	var creds []string
	for _, name := range []string{"addon.postgres.password", "addon.postgres.role.svc.password"} {
		sec, err := h.st.GetSecret(bg, env.ID, name, 1)
		if err != nil || !sec.System {
			t.Fatalf("secret %s: %+v %v", name, sec, err)
		}
		plain, err := h.sealer.Open(sec.Sealed, envdeps.SecretAAD(env.ID, name, 1))
		if err != nil || len(plain) != 32 || strings.Trim(string(plain), "0123456789abcdef") != "" {
			t.Fatalf("credential %s: %q %v", name, plain, err)
		}
		creds = append(creds, string(plain))
	}
	if tenant, _ := h.st.ListSecrets(bg, env.ID, false); len(tenant) != 0 {
		t.Errorf("the credentials are listed as the tenant's: %+v", tenant)
	}

	rows := h.rowsOf(env.ID, spec.KindAddon, "postgres")
	if len(rows) != 1 || !ready(rows[0]) {
		t.Fatalf("rows %+v", rows)
	}
	first := rows[0]
	if first.Sandbox.PinNode != "n1" || first.Sandbox.IdleAfterS == nil || *first.Sandbox.IdleAfterS != 0 ||
		first.Instance.ProvisionedHash == "" || first.Instance.Release != r1 || first.Sandbox.Address == "" {
		t.Errorf("instance %+v / %+v", first.Instance, first.Sandbox)
	}
	tmpl := first.Sandbox.PodTemplate
	if tmpl == nil || len(tmpl.Volumes) != 1 || tmpl.Mounts[0].Path != "/var/lib/postgresql/data" ||
		tmpl.Env["POSTGRES_PASSWORD"].Parts[0].Secret != "addon.postgres.password" || tmpl.HasCredential() ||
		tmpl.Labels[model.LabelObject] != "addon/postgres" || tmpl.Labels[model.LabelTemplate] != first.Instance.TemplateHash {
		t.Fatalf("template %+v", tmpl)
	}
	vol, err := h.st.GetEnvVolume(bg, env.ID, "addon/postgres")
	if err != nil || vol.SizeMiB != 1024 || !vol.Provisioned || vol.Node != "n1" || vol.Holder != first.Sandbox.ID ||
		vol.VolumeID != tmpl.Volumes[0].VolumeID {
		t.Fatalf("volume %+v %v", vol, err)
	}
	nv := n1.Volumes()
	if len(nv) != 1 || nv[0].VolumeId != vol.VolumeID.String() || nv[0].SizeBytes != 1024<<20 ||
		nv[0].PodId != first.Sandbox.ID.String() || nv[0].State != volumev1.VolumeState_VOLUME_STATE_ATTACHED ||
		nv[0].Labels[model.LabelObject] != "addon/postgres" {
		t.Fatalf("the node's volumes %v", nv)
	}
	pod := h.pod("n1", first.Sandbox.ID)
	if pod == nil || !slices.Equal(pod.Spec.GetNetwork().GetSecurityGroups(), []string{model.EnvGroupID(env.ID)}) {
		t.Fatalf("pod %v", pod)
	}
	if pod.Spec.Containers[0].Env["POSTGRES_PASSWORD"] != creds[0] {
		t.Error("the pod's env has not the credential")
	}
	g := h.group("n1", model.EnvGroupID(env.ID))
	if g == nil || !slices.Contains(g.Members, first.Sandbox.Address) {
		t.Fatalf("env group %v", g)
	}
	// The probe ran; the provisioning ran with the credentials in its env,
	// not in its argv.
	if len(execs.find("pg_isready")) == 0 {
		t.Error("the probe never ran")
	}
	prov := execs.find("sh")
	if len(prov) == 0 {
		t.Fatal("the provisioning never ran")
	}
	p := prov[0]
	if p.Env["PGPASSWORD"] != creds[0] || p.Env["FERUN_ROLE_PASSWORD_svc"] != creds[1] {
		t.Errorf("provisioning env %v", p.Env)
	}
	for _, a := range p.Command {
		if strings.Contains(a, creds[0]) || strings.Contains(a, creds[1]) {
			t.Error("a credential is in the provisioning's argv")
		}
	}

	// A plan change recreates it, on the same node, with the same volume.
	r2 := h.apply(env.ID, strings.Replace(postgresSpec, `"dev"`, `"small"`, 1))
	h.waitRelease(env.ID, r2, model.ReleaseRealised, 60*time.Second)
	rows = h.rowsOf(env.ID, spec.KindAddon, "postgres")
	if len(rows) != 1 || rows[0].Sandbox.ID == first.Sandbox.ID || !ready(rows[0]) {
		t.Fatalf("after the recreate %+v", rows)
	}
	second := rows[0]
	if second.Sandbox.Node != "n1" || second.Sandbox.PinNode != "n1" || second.Sandbox.MemoryMiB != 1024 ||
		second.Sandbox.PodTemplate.Volumes[0].VolumeID != vol.VolumeID || second.Instance.ProvisionedHash == "" {
		t.Fatalf("second %+v %+v", second.Instance, second.Sandbox)
	}
	vol2, err := h.st.GetEnvVolume(bg, env.ID, "addon/postgres")
	if err != nil || vol2.SizeMiB != 1024 || vol2.Node != "n1" || vol2.Holder != second.Sandbox.ID {
		t.Fatalf("volume after the recreate %+v %v", vol2, err)
	}
	if _, err := h.st.GetSandbox(bg, first.Sandbox.ID); err == nil {
		t.Error("the old instance's row stays")
	}
	nv = n1.Volumes()
	if len(nv) != 1 || nv[0].PodId != second.Sandbox.ID.String() || nv[0].SizeBytes != 1024<<20 {
		t.Fatalf("the node's volumes after the recreate %v", nv)
	}
	// The credentials stayed.
	for i, name := range []string{"addon.postgres.password", "addon.postgres.role.svc.password"} {
		if v, err := h.c.ResolveSecret(bg, env.ID, name, 1); err != nil || v != creds[i] {
			t.Errorf("credential %s changed: %v", name, err)
		}
	}
	// A role option alone reprovisions it, without a recreate.
	r3 := h.apply(env.ID, strings.Replace(strings.Replace(postgresSpec, `"dev"`, `"small"`, 1), `["svc"]`, `["svc","ops"]`, 1))
	h.waitRelease(env.ID, r3, model.ReleaseRealised, 60*time.Second)
	rows = h.rowsOf(env.ID, spec.KindAddon, "postgres")
	if len(rows) != 1 || rows[0].Sandbox.ID != second.Sandbox.ID || rows[0].Instance.ProvisionedHash == second.Instance.ProvisionedHash {
		t.Fatalf("after a role was added %+v", rows)
	}
	if _, err := h.st.GetSecret(bg, env.ID, "addon.postgres.role.ops.password", 1); err != nil {
		t.Errorf("the new role's credential: %v", err)
	}
	if rel := h.release(env.ID, r1); rel.State != model.ReleaseSuperseded {
		t.Errorf("release %d is %s", r1, rel.State)
	}
	if e, _ := h.st.GetEnvironment(bg, env.ID); e.Realised != r3 {
		t.Errorf("realised %d", e.Realised)
	}

	// Removed, the addon loses its instance, its volume and its credentials.
	r4 := h.apply(env.ID, `{}`)
	h.waitRelease(env.ID, r4, model.ReleaseRealised, 60*time.Second)
	eventually(t, 60*time.Second, "the volume gone", func() bool {
		_, err := h.st.GetEnvVolume(bg, env.ID, "addon/postgres")
		return err != nil && len(n1.Volumes()) == 0
	})
	if secs, _ := h.st.ListSecrets(bg, env.ID, true); len(secs) != 0 {
		t.Errorf("credentials stay: %+v", secs)
	}
}

// Without --secret-key-file an addon is not realised: its release stays
// rolling with the reason, and no incident opens.
func TestAddonWithoutSealer(t *testing.T) {
	h := newHarness(t, options{noSealer: true})
	h.addNode("n1")
	env := h.newEnv("stage")
	r := h.apply(env.ID, postgresSpec)
	eventually(t, 30*time.Second, "the reason", func() bool {
		rel := h.release(env.ID, r)
		return rel.State == model.ReleaseRolling && rel.Reason == "secrets need --secret-key-file"
	})
	if rows := h.rows(env.ID); len(rows) != 0 {
		t.Errorf("rows %+v", rows)
	}
	if is := h.incidents(env.ID, false); len(is) != 0 {
		t.Errorf("incidents %+v", is)
	}
	if _, err := h.c.ResolveSecret(bg, env.ID, "x", 1); err == nil || err.Error() != "secrets need --secret-key-file" {
		t.Errorf("resolve without a sealer: %v", err)
	}
}

var _ = store.ErrNoRow
