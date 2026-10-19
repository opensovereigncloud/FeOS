// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package addons

import (
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ironcore-dev/feos/go/internal/model"
)

func TestPostgresVersionsPlans(t *testing.T) {
	var p postgres
	if b, ok := Get("postgres"); !ok || b != (postgres{}) {
		t.Fatalf("Get(postgres) = %v, %v", b, ok)
	}
	if v := p.Versions(); !slices.Equal(v, []string{"16", "17"}) {
		t.Fatal(v)
	}
	want := map[string]Plan{
		"dev":    {VCPUs: 1, MemoryMib: 256, VolumeMib: 1024},
		"small":  {VCPUs: 1, MemoryMib: 1024, VolumeMib: 10240},
		"medium": {VCPUs: 2, MemoryMib: 4096, VolumeMib: 51200},
		"large":  {VCPUs: 4, MemoryMib: 16384, VolumeMib: 204800},
	}
	if got := p.Plans(); !reflect.DeepEqual(got, want) {
		t.Fatalf("plans %v, want %v", got, want)
	}
	p.Plans()["dev"] = Plan{} // a caller's change stays its own
	if p.Plans()["dev"].MemoryMib != 256 {
		t.Fatal("Plans shares its map")
	}
	if p.DefaultPlan() != "small" {
		t.Fatal(p.DefaultPlan())
	}
	if k := p.OutputKeys(); !slices.Equal(k, []string{"host", "port", "user", "password", "database", "url"}) {
		t.Fatal(k)
	}
}

func TestPostgresValidate(t *testing.T) {
	const full = `{"type":"postgres","version":"17","plan":"small",
		"options":{"databases":["expensa","reports"],"roles":["expense_svc","approval_svc"]}}`
	long := strings.Repeat("a", 63)
	cases := []struct {
		name     string
		prev     *Addon
		next     string
		addon    string // the addon's name; "" is "postgres"
		want     []string
		problems []string
	}{
		{name: "minimal", next: `{"type":"postgres","version":"17"}`},
		{name: "full", next: full},
		{name: "16 dev", next: `{"type":"postgres","version":"16","plan":"dev"}`},
		{name: "options null", next: `{"type":"postgres","version":"17","options":null}`},
		{name: "lists null", next: `{"type":"postgres","version":"17","options":{"databases":null,"roles":null}}`},
		{name: "databases only", next: `{"type":"postgres","version":"17","options":{"databases":["a","b_2"]}}`},
		{name: "no version", next: `{"type":"postgres"}`,
			want: []string{"error version"}, problems: []string{"needs a version"}},
		{name: "unknown version", next: `{"type":"postgres","version":"15"}`,
			want: []string{"error version"}, problems: []string{`no version "15"`}},
		{name: "unknown plan", next: `{"type":"postgres","version":"17","plan":"huge"}`,
			want: []string{"error plan"}, problems: []string{`no plan "huge"`}},
		{name: "unknown option", next: `{"type":"postgres","version":"17","options":{"users":["x"],"database":"y"}}`,
			want: []string{"error options.database", "error options.users"}},
		{name: "databases not a list", next: `{"type":"postgres","version":"17","options":{"databases":"expensa"}}`,
			want: []string{"error options.databases"}, problems: []string{"want a list of names"}},
		{name: "element not a string",
			next: `{"type":"postgres","version":"17","options":{"databases":["a",5,true],"roles":[{"x":1}]}}`,
			want: []string{"error options.databases[1]", "error options.databases[2]", "error options.roles[0]"}},
		{name: "bad names",
			next: `{"type":"postgres","version":"17","options":{"databases":["Expensa","1db","my-db","` + long + `x",""],
				"roles":["Svc","a.b"]}}`,
			want: []string{"error options.databases[0]", "error options.databases[1]", "error options.databases[2]",
				"error options.databases[3]", "error options.databases[4]", "error options.roles[0]",
				"error options.roles[1]"},
			problems: []string{`database name "Expensa" is not a lowercase identifier`, `role name "a.b"`}},
		{name: "longest name", next: `{"type":"postgres","version":"17","options":{"databases":["` + long + `"]}}`},
		{name: "reserved databases",
			next:     `{"type":"postgres","version":"17","options":{"databases":["template0","template1","postgres","ok"]}}`,
			want:     []string{"error options.databases[0]", "error options.databases[1]", "error options.databases[2]"},
			problems: []string{"database name template0 is reserved"}},
		{name: "reserved roles",
			next: `{"type":"postgres","version":"17","options":{"databases":["d"],"roles":["postgres","public","none",
				"current_user","current_role","session_user","information_schema","pg_monitor","pg_x","ok","pgx"]}}`,
			want: []string{"error options.roles[0]", "error options.roles[1]", "error options.roles[2]",
				"error options.roles[3]", "error options.roles[4]", "error options.roles[5]", "error options.roles[6]",
				"error options.roles[7]", "error options.roles[8]"},
			problems: []string{"role name postgres is reserved", "role name pg_monitor is reserved"}},
		{name: "duplicates",
			next:     `{"type":"postgres","version":"17","options":{"databases":["a","b","a"],"roles":["r","r"]}}`,
			want:     []string{"error options.databases[2]", "error options.roles[1]"},
			problems: []string{"database a is listed twice", "role r is listed twice"}},
		{name: "indexes count every element",
			next:     `{"type":"postgres","version":"17","options":{"databases":["a",5,"a","B"]}}`,
			want:     []string{"error options.databases[1]", "error options.databases[2]", "error options.databases[3]"},
			problems: []string{"want a database name, not a number", "database a is listed twice"}},
		{name: "roles without databases", next: `{"type":"postgres","version":"17","options":{"roles":["svc"]}}`,
			want: []string{"error options.roles"}, problems: []string{"roles need a database"}},
		{name: "roles with empty databases",
			next: `{"type":"postgres","version":"17","options":{"databases":[],"roles":["svc"]}}`,
			want: []string{"error options.roles"}},
		{name: "credential too long", addon: long,
			next: `{"type":"postgres","version":"17","options":{"databases":["d"],"roles":["` + long[:43] + `","` + long[:44] + `"]}}`,
			want: []string{"error options.roles[1]"}, problems: []string{"more than 127"}},
		{name: "major upgrade", prev: ptr(addon(t, "postgres", `{"type":"postgres","version":"16"}`)),
			next: `{"type":"postgres","version":"17"}`,
			want: []string{"error version"}, problems: []string{"from 16 to 17 needs a data migration"}},
		{name: "major downgrade", prev: ptr(addon(t, "postgres", `{"type":"postgres","version":"17"}`)),
			next: `{"type":"postgres","version":"16"}`,
			want: []string{"error version"}, problems: []string{"from 17 to 16"}},
		{name: "unchanged", prev: ptr(addon(t, "postgres", full)), next: full},
		{name: "to an unknown version", prev: ptr(addon(t, "postgres", `{"type":"postgres","version":"17"}`)),
			next: `{"type":"postgres","version":"18"}`, want: []string{"error version"}},
		{name: "from another type", prev: ptr(addon(t, "postgres", `{"type":"valkey","version":"8"}`)),
			next: `{"type":"postgres","version":"17","plan":"dev"}`},
		{name: "plan change", prev: ptr(addon(t, "postgres", `{"type":"postgres","version":"17","plan":"small"}`)),
			next: `{"type":"postgres","version":"17","plan":"medium"}`,
			want: []string{"warning plan"}, problems: []string{"from small to medium", "medium's 51200 MiB is not applied"}},
		{name: "default plan is small", prev: ptr(addon(t, "postgres", `{"type":"postgres","version":"17"}`)),
			next: `{"type":"postgres","version":"17","plan":"small"}`},
		{name: "plan change and major", prev: ptr(addon(t, "postgres", `{"type":"postgres","version":"16","plan":"dev"}`)),
			next: `{"type":"postgres","version":"17"}`, want: []string{"warning plan", "error version"}},
		{name: "options grow", prev: ptr(addon(t, "postgres", `{"type":"postgres","version":"17","options":{"databases":["expensa"]}}`)),
			next: full},
		{name: "options shrink", prev: ptr(addon(t, "postgres", full)),
			next:     `{"type":"postgres","version":"17","plan":"small","options":{"databases":["expensa"],"roles":["approval_svc"]}}`,
			want:     []string{"warning options.databases", "warning options.roles"},
			problems: []string{"database reports is no longer listed", "role expense_svc is no longer listed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := c.addon
			if name == "" {
				name = "postgres"
			}
			checkFindings(t, postgres{}.Validate(c.prev, addon(t, name, c.next)), c.want, c.problems...)
		})
	}
}

func TestPostgresRender(t *testing.T) {
	env := `{"PGDATA":{"value":"/var/lib/postgresql/data/pgdata"},` +
		`"POSTGRES_INITDB_ARGS":{"value":"--auth-host=scram-sha-256"},` +
		`"POSTGRES_PASSWORD":{"parts":[{"credential":"password"}]}}`
	probe := []string{"pg_isready", "-U", "postgres", "-h", "127.0.0.1"}
	cases := []struct {
		addon, image, buffers string
	}{
		{`{"type":"postgres","version":"16","plan":"dev"}`, "postgres:16-alpine", "64MB"},
		{`{"type":"postgres","version":"17"}`, "postgres:17-alpine", "256MB"},
		{`{"type":"postgres","version":"17","plan":"medium"}`, "postgres:17-alpine", "1024MB"},
		{`{"type":"postgres","version":"17","plan":"large","options":{"databases":["d"],"roles":["r"]}}`,
			"postgres:17-alpine", "4096MB"},
	}
	for _, c := range cases {
		t.Run(c.addon, func(t *testing.T) {
			a := addon(t, "postgres", c.addon)
			got, err := postgres{}.Render(a, Context{Env: "stage", Name: "postgres", Address: "fd6b::5"})
			if err != nil {
				t.Fatal(err)
			}
			want := Instance{
				Image:   c.image,
				Command: []string{"docker-entrypoint.sh", "postgres", "-c", "shared_buffers=" + c.buffers},
				Env: map[string]model.TemplateValue{
					"PGDATA":               model.Literal("/var/lib/postgresql/data/pgdata"),
					"POSTGRES_PASSWORD":    model.CredentialValue("password"),
					"POSTGRES_INITDB_ARGS": model.Literal("--auth-host=scram-sha-256"),
				},
				Port:  5432,
				Mount: "/var/lib/postgresql/data",
				Probe: probe,
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("render\n%#v\nwant\n%#v", got, want)
			}
			if j := jsonOf(t, got.Env); j != env {
				t.Fatalf("env %s, want %s", j, env)
			}
			// Options are provisioned, never rendered: adding a role
			// must not recreate the pod.
			a.Options = nil
			bare, _ := postgres{}.Render(a, Context{})
			if !reflect.DeepEqual(bare, got) {
				t.Fatal("options or the context changed the rendering")
			}
		})
	}
	for _, bad := range []string{`{"type":"postgres"}`, `{"type":"postgres","version":"15"}`,
		`{"type":"postgres","version":"17","plan":"huge"}`} {
		if _, err := (postgres{}).Render(addon(t, "postgres", bad), Context{}); err == nil {
			t.Errorf("Render(%s) did not fail", bad)
		}
	}
}

func TestPostgresCredentials(t *testing.T) {
	cases := map[string][]string{
		`{"type":"postgres","version":"17"}`: {"password"},
		`{"type":"postgres","version":"17","options":{"databases":["d"],"roles":["expense_svc","approval_svc"]}}`: {
			"password", "role.expense_svc.password", "role.approval_svc.password"},
		`{"type":"postgres","version":"17","options":{"databases":["d"],"roles":["r","r"]}}`: {"password", "role.r.password"},
	}
	for raw, want := range cases {
		if got := (postgres{}).Credentials(addon(t, "postgres", raw)); !slices.Equal(got, want) {
			t.Errorf("Credentials(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestPostgresOutputs(t *testing.T) {
	full := addon(t, "db", `{"type":"postgres","version":"17",
		"options":{"databases":["expensa","reports"],"roles":["expense_svc","approval_svc"]}}`)
	bare := addon(t, "db", `{"type":"postgres","version":"17"}`)
	v6 := Context{Env: "stage", Name: "db", Address: "fd6b:59f2:b4a2:3::5"}
	none := Context{Env: "stage", Name: "db"}
	lit := func(s string) []model.TemplatePart { return []model.TemplatePart{{Value: s}} }
	cred := func(k string) []model.TemplatePart { return []model.TemplatePart{{Credential: k}} }
	urlOf := func(user, key, rest string) []model.TemplatePart {
		return []model.TemplatePart{{Value: "postgres://" + user + ":"}, {Credential: key, Escape: model.EscapeURL},
			{Value: rest}}
	}
	cases := []struct {
		name   string
		a      Addon
		c      Context
		key    string
		fields map[string]string
		want   []model.TemplatePart
	}{
		{"host", full, v6, "host", nil, lit("fd6b:59f2:b4a2:3::5")},
		{"host without address", full, none, "host", nil, lit("db")},
		{"port", full, v6, "port", nil, lit("5432")},
		{"user", full, v6, "user", nil, lit("postgres")},
		{"role's user", full, v6, "user", map[string]string{"role": "expense_svc"}, lit("expense_svc")},
		{"password", full, v6, "password", nil, cred("password")},
		{"role's password", full, v6, "password", map[string]string{"role": "approval_svc"},
			cred("role.approval_svc.password")},
		{"database is the first", full, v6, "database", nil, lit("expensa")},
		{"database field", full, v6, "database", map[string]string{"database": "reports"}, lit("reports")},
		{"database without any", bare, v6, "database", nil, lit("postgres")},
		{"url", full, v6, "url", nil,
			urlOf("postgres", "password", "@[fd6b:59f2:b4a2:3::5]:5432/expensa?sslmode=disable")},
		{"role's url", full, v6, "url", map[string]string{"role": "expense_svc", "database": "reports"},
			urlOf("expense_svc", "role.expense_svc.password", "@[fd6b:59f2:b4a2:3::5]:5432/reports?sslmode=disable")},
		{"url without databases", bare, v6, "url", nil,
			urlOf("postgres", "password", "@[fd6b:59f2:b4a2:3::5]:5432/postgres?sslmode=disable")},
		{"superuser's postgres database", full, v6, "url", map[string]string{"database": "postgres"},
			urlOf("postgres", "password", "@[fd6b:59f2:b4a2:3::5]:5432/postgres?sslmode=disable")},
		{"url without address", full, none, "url", map[string]string{"role": "approval_svc"},
			urlOf("approval_svc", "role.approval_svc.password", "@db:5432/expensa?sslmode=disable")},
		{"options with problems", addon(t, "db", `{"type":"postgres","version":"17","options":{"roles":["svc","pg_x"]}}`),
			v6, "user", map[string]string{"role": "pg_x"}, lit("pg_x")},
		{"url with an IPv4 address", full, Context{Name: "db", Address: "10.0.0.5"}, "url", nil,
			urlOf("postgres", "password", "@10.0.0.5:5432/expensa?sslmode=disable")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := postgres{}.Outputs(c.a, c.c, Ref{Key: c.key, Fields: c.fields})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("outputs %s, want %s", jsonOf(t, got), jsonOf(t, c.want))
			}
		})
	}
	bad := []struct {
		name   string
		a      Addon
		key    string
		fields map[string]string
		says   string
	}{
		{"no key", full, "", nil, "needs a key"},
		{"unknown key", full, "uri", nil, `no key "uri"`},
		{"unknown field", full, "url", map[string]string{"schema": "x"}, `no field "schema"`},
		{"unknown role", full, "url", map[string]string{"role": "nobody"}, `no role "nobody"`},
		{"empty role", full, "user", map[string]string{"role": ""}, `no role ""`},
		{"unknown database", full, "url", map[string]string{"database": "nope"}, `no database "nope"`},
		{"role in postgres", full, "url", map[string]string{"role": "expense_svc", "database": "postgres"},
			`gives role expense_svc no database "postgres"`},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := postgres{}.Outputs(c.a, v6, Ref{Key: c.key, Fields: c.fields})
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("error %v, want one that says %q", err, c.says)
			}
		})
	}
}

// TestPostgresURLParses resolves the url with a password that needs
// escaping and reads it back with net/url.
func TestPostgresURLParses(t *testing.T) {
	a := addon(t, "db", `{"type":"postgres","version":"17","options":{"databases":["expensa"],"roles":["svc"]}}`)
	parts, err := postgres{}.Outputs(a, Context{Name: "db", Address: "fd6b::5"},
		Ref{Key: "url", Fields: map[string]string{"role": "svc"}})
	if err != nil {
		t.Fatal(err)
	}
	const pw = "p@ss:w/rd%?#&=+ ü[]'\"$"
	raw := resolveParts(t, parts, map[string]string{"role.svc.password": pw})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	got, _ := u.User.Password()
	if u.Scheme != "postgres" || u.User.Username() != "svc" || got != pw || u.Hostname() != "fd6b::5" ||
		u.Port() != "5432" || u.Path != "/expensa" || u.Query().Get("sslmode") != "disable" {
		t.Fatalf("%s parses to %#v (password %q)", raw, u, got)
	}
}

// The script of the spike's verified run (.claude/caas/spike-addons.md
// §1): databases expensa, reports; roles expense_svc, approval_svc. It is
// kept as the report has it; provisioning runs it with the owner checks
// added (withOwnerChecks).
const postgresSpikeScript = `exec psql -X -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -p 5432 -U postgres -d postgres -v d0=expensa -v d1=reports -v r0=expense_svc -v r1=approval_svc <<'SQL'
\getenv p0 FERUN_ROLE_PASSWORD_expense_svc
\getenv p1 FERUN_ROLE_PASSWORD_approval_svc
SELECT format('CREATE DATABASE %I', :'d0') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'d0') \gexec
SELECT format('CREATE DATABASE %I', :'d1') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'d1') \gexec
SELECT format('CREATE ROLE %I', :'r0') WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'r0') \gexec
ALTER ROLE :"r0" WITH LOGIN PASSWORD :'p0';
SELECT format('CREATE ROLE %I', :'r1') WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'r1') \gexec
ALTER ROLE :"r1" WITH LOGIN PASSWORD :'p1';
GRANT CONNECT, CREATE ON DATABASE :"d0" TO :"r0", :"r1";
GRANT CONNECT, CREATE ON DATABASE :"d1" TO :"r0", :"r1";
\connect :"d0"
CREATE SCHEMA IF NOT EXISTS :"r0" AUTHORIZATION :"r0";
CREATE SCHEMA IF NOT EXISTS :"r1" AUTHORIZATION :"r1";
\connect :"d1"
CREATE SCHEMA IF NOT EXISTS :"r0" AUTHORIZATION :"r0";
CREATE SCHEMA IF NOT EXISTS :"r1" AUTHORIZATION :"r1";
SQL
`

// postgresOwnerCheck is the line provisioning has run since the review of
// S1 (finding C4) before the CREATE SCHEMA IF NOT EXISTS of each role
// (r<J>) in each database: it fails the step when a schema of the role's
// name exists there and another role owns it.
const postgresOwnerCheck = `SELECT format('DO %L', format('BEGIN RAISE EXCEPTION USING MESSAGE = %L; END', ` +
	`format('schema %I in database %I belongs to role %I, not to role %I', nspname, current_database(), ` +
	`pg_get_userbyid(nspowner), nspname))) FROM pg_namespace WHERE nspname = :'r<J>' ` +
	`AND nspowner <> (SELECT oid FROM pg_roles WHERE rolname = :'r<J>') \gexec`

// withOwnerChecks is a script of the spike's form with the owner check
// inserted before each CREATE SCHEMA line and nothing else changed. The
// spike's report stays as it was verified and written, so the spike case
// compares against its script plus the checks; TestPostgresDocker runs
// the checks against the real images.
func withOwnerChecks(script string) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(script, "\n") {
		if rest, ok := strings.CutPrefix(line, `CREATE SCHEMA IF NOT EXISTS :"r`); ok {
			j, _, _ := strings.Cut(rest, `"`)
			b.WriteString(strings.ReplaceAll(postgresOwnerCheck, "<J>", j) + "\n")
		}
		b.WriteString(line)
	}
	return b.String()
}

func TestPostgresProvision(t *testing.T) {
	pgoptions := model.Literal("-c client_min_messages=warning -c log_min_error_statement=panic")
	cases := []struct {
		name   string
		addon  string
		script string
		env    map[string]model.TemplateValue
	}{
		{
			name:   "spike",
			addon:  `{"type":"postgres","version":"17","options":{"databases":["expensa","reports"],"roles":["expense_svc","approval_svc"]}}`,
			script: withOwnerChecks(postgresSpikeScript),
			env: map[string]model.TemplateValue{
				"PGPASSWORD":                       model.CredentialValue("password"),
				"PGOPTIONS":                        pgoptions,
				"FERUN_ROLE_PASSWORD_expense_svc":  model.CredentialValue("role.expense_svc.password"),
				"FERUN_ROLE_PASSWORD_approval_svc": model.CredentialValue("role.approval_svc.password"),
			},
		},
		{
			name:  "databases only",
			addon: `{"type":"postgres","version":"16","options":{"databases":["expensa"]}}`,
			script: `exec psql -X -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -p 5432 -U postgres -d postgres -v d0=expensa <<'SQL'
SELECT format('CREATE DATABASE %I', :'d0') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'d0') \gexec
SQL
`,
			env: map[string]model.TemplateValue{"PGPASSWORD": model.CredentialValue("password"), "PGOPTIONS": pgoptions},
		},
		{
			name:  "one of each",
			addon: `{"type":"postgres","version":"17","options":{"roles":["app"],"databases":["app"]}}`,
			script: `exec psql -X -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -p 5432 -U postgres -d postgres -v d0=app -v r0=app <<'SQL'
\getenv p0 FERUN_ROLE_PASSWORD_app
SELECT format('CREATE DATABASE %I', :'d0') WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = :'d0') \gexec
SELECT format('CREATE ROLE %I', :'r0') WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = :'r0') \gexec
ALTER ROLE :"r0" WITH LOGIN PASSWORD :'p0';
GRANT CONNECT, CREATE ON DATABASE :"d0" TO :"r0";
\connect :"d0"
SELECT format('DO %L', format('BEGIN RAISE EXCEPTION USING MESSAGE = %L; END', format('schema %I in database %I belongs to role %I, not to role %I', nspname, current_database(), pg_get_userbyid(nspowner), nspname))) FROM pg_namespace WHERE nspname = :'r0' AND nspowner <> (SELECT oid FROM pg_roles WHERE rolname = :'r0') \gexec
CREATE SCHEMA IF NOT EXISTS :"r0" AUTHORIZATION :"r0";
SQL
`,
			env: map[string]model.TemplateValue{"PGPASSWORD": model.CredentialValue("password"), "PGOPTIONS": pgoptions,
				"FERUN_ROLE_PASSWORD_app": model.CredentialValue("role.app.password")},
		},
	}
	if n := strings.Count(withOwnerChecks(postgresSpikeScript), "\\gexec\n") - strings.Count(postgresSpikeScript, "\\gexec\n"); n != 4 {
		t.Fatalf("%d owner checks added to the spike's script, want 4 (2 databases, 2 roles)", n)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := addon(t, "postgres", c.addon)
			steps := postgres{}.Provision(a, Context{Env: "stage", Name: "postgres", Address: "fd6b::5"})
			want := []Step{{Name: "provision", Argv: []string{"sh", "-c", c.script}, Env: c.env}}
			if !reflect.DeepEqual(steps, want) {
				t.Fatalf("steps\n%#v\nwant\n%#v", steps, want)
			}
			// Deterministic, so that its hash decides re-runs: the same
			// addon gives the same steps, in the same JSON.
			again := postgres{}.Provision(a, Context{})
			if jsonOf(t, again) != jsonOf(t, steps) {
				t.Fatal("Provision is not deterministic")
			}
		})
	}
	for _, raw := range []string{`{"type":"postgres","version":"17"}`,
		`{"type":"postgres","version":"17","options":{"databases":[],"roles":[]}}`} {
		if steps := (postgres{}).Provision(addon(t, "postgres", raw), Context{}); steps != nil {
			t.Errorf("Provision(%s) = %v, want none", raw, steps)
		}
	}
}

// TestPostgresScriptQuotes: provisioning quotes a name the shell would
// read otherwise; Validate refuses such names, so this is only a guard.
func TestPostgresScriptQuotes(t *testing.T) {
	got := postgresScript([]string{"a b", "it's"}, nil)
	want := `exec psql -X -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -p 5432 -U postgres -d postgres -v d0='a b' -v d1='it'\''s' <<'SQL'` + "\n"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("script %q, want it to start with %q", got, want)
	}
}
