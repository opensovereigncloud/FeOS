// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ironcore-dev/feos/go/internal/bindings"
	"github.com/ironcore-dev/feos/go/internal/db/dbtest"
	"github.com/ironcore-dev/feos/go/internal/model"
)

func TestParseImageRef(t *testing.T) {
	cases := []struct {
		in, name, host, repo, ref string
	}{
		{"alpine", "docker.io", "registry-1.docker.io", "library/alpine", "latest"},
		{"alpine:3.20", "docker.io", "registry-1.docker.io", "library/alpine", "3.20"},
		{"maltej/debian-ai", "docker.io", "registry-1.docker.io", "maltej/debian-ai", "latest"},
		{"docker.io/library/alpine:3", "docker.io", "registry-1.docker.io", "library/alpine", "3"},
		{"index.docker.io/x/y:1", "docker.io", "registry-1.docker.io", "x/y", "1"},
		{"ghcr.io/maltej/debian-ai:latest", "ghcr.io", "ghcr.io", "maltej/debian-ai", "latest"},
		{"localhost:5000/a/b", "localhost:5000", "localhost:5000", "a/b", "latest"},
		{"ghcr.io/a/b:tag@sha256:abc", "ghcr.io", "ghcr.io", "a/b", "sha256:abc"},
		{"alpine@sha256:abc", "docker.io", "registry-1.docker.io", "library/alpine", "sha256:abc"},
	}
	for _, c := range cases {
		r, err := parseImageRef(c.in)
		if err != nil || r.name != c.name || r.host != c.host || r.repo != c.repo || r.ref != c.ref {
			t.Errorf("%s: %+v %v", c.in, r, err)
		}
	}
	for _, bad := range []string{"", " ", "a b", "x@md5:abc"} {
		if _, err := parseImageRef(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// registry is an anonymous OCI registry with a Bearer token flow, over
// https.
type registry struct {
	srv   *httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	token string
	// manifests by repo and reference: content type and body.
	manifests map[string][2]string
	digests   map[string]string // Docker-Content-Digest per reference ("" = none)
	// realm, when set, is the challenge's realm instead of the registry's
	// own /token; redirects answer a reference with a 307 to their URL.
	realm     string
	redirects map[string]string
	// badToken: /token answers something that is not JSON.
	badToken bool
}

func newRegistry(t *testing.T) *registry {
	r := &registry{hits: map[string]int{}, token: "anon-token", manifests: map[string][2]string{}, digests: map[string]string{},
		redirects: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		bad := r.badToken
		r.mu.Unlock()
		if bad {
			fmt.Fprint(w, "<html>not a token</html>")
			return
		}
		if req.URL.Query().Get("scope") == "" || req.URL.Query().Get("service") != "test-registry" {
			http.Error(w, "bad token request", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, `{"token": %q}`, r.token)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, req *http.Request) {
		key := strings.TrimPrefix(req.URL.Path, "/v2/")
		r.mu.Lock()
		r.hits[key]++
		m, ok := r.manifests[key]
		d := r.digests[key]
		realm, redirect := r.realm, r.redirects[key]
		r.mu.Unlock()
		if redirect != "" {
			http.Redirect(w, req, redirect, http.StatusTemporaryRedirect)
			return
		}
		if req.Header.Get("Authorization") != "Bearer "+r.token {
			repo := strings.Split(key, "/manifests/")[0]
			if realm == "" {
				realm = r.srv.URL + "/token"
			}
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s",service="test-registry",scope="repository:%s:pull"`,
				realm, repo))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !strings.Contains(req.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			http.Error(w, "no accept", http.StatusBadRequest)
			return
		}
		if key == "limited/x/manifests/latest" {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", m[0])
		if d != "" {
			w.Header().Set("Docker-Content-Digest", d)
		}
		fmt.Fprint(w, m[1])
	})
	r.srv = httptest.NewTLSServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

func (r *registry) host() string { return strings.TrimPrefix(r.srv.URL, "https://") }

// resolver returns a resolver that trusts the registry's certificate,
// under the destination rule deny (none: every IPv4 address).
func (r *registry) resolver(deny ...netip.Prefix) *resolver {
	res := newResolver(deny)
	res.transport.TLSClientConfig = r.srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	return res
}

func (r *registry) set(f func(r *registry)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f(r)
}

func (r *registry) put(key, mediaType, body, digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manifests[key] = [2]string{mediaType, body}
	r.digests[key] = digest
}

func (r *registry) hitsOf(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[key]
}

func TestResolver(t *testing.T) {
	reg := newRegistry(t)
	const amd = "sha256:" + "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	reg.put("team/app/manifests/v1", "application/vnd.oci.image.index.v1+json", `{"mediaType":"application/vnd.oci.image.index.v1+json",
		"manifests":[{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}},
		{"digest":"`+amd+`","platform":{"os":"linux","architecture":"amd64"}}]}`, "sha256:index")
	manifest := `{"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:cfg"}}`
	reg.put("team/plain/manifests/v1", "application/vnd.oci.image.manifest.v1+json", manifest, "")
	reg.put("team/arm/manifests/v1", "application/vnd.oci.image.index.v1+json",
		`{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}}]}`, "")

	now := time.Unix(1_700_000_000, 0)
	r := reg.resolver()
	r.now = func() time.Time { return now }
	host := reg.host()

	// An index: the linux/amd64 member's digest, after the anonymous
	// token flow.
	got, err := r.resolve(bg, host+"/team/app:v1")
	if err != nil || got != host+"/team/app@"+amd {
		t.Fatalf("index: %q %v", got, err)
	}
	if reg.hitsOf("team/app/manifests/v1") != 2 {
		t.Fatalf("hits %d (challenge and answer)", reg.hitsOf("team/app/manifests/v1"))
	}
	// Cached for 60 s.
	if again, err := r.resolve(bg, host+"/team/app:v1"); err != nil || again != got || reg.hitsOf("team/app/manifests/v1") != 2 {
		t.Fatalf("cached: %q %v, hits %d", again, err, reg.hitsOf("team/app/manifests/v1"))
	}
	now = now.Add(61 * time.Second)
	if _, err := r.resolve(bg, host+"/team/app:v1"); err != nil || reg.hitsOf("team/app/manifests/v1") != 4 {
		t.Fatalf("after the TTL: %v, hits %d", err, reg.hitsOf("team/app/manifests/v1"))
	}

	// A manifest without Docker-Content-Digest: the digest of its body.
	got, err = r.resolve(bg, host+"/team/plain:v1")
	if want := fmt.Sprintf("%s/team/plain@sha256:%x", host, sha256.Sum256([]byte(manifest))); err != nil || got != want {
		t.Fatalf("manifest: %q %v, want %q", got, err, want)
	}

	// A digest reference is passed through without asking the registry.
	d := "sha256:" + strings.Repeat("b", 64)
	if got, err := r.resolve(bg, host+"/team/other@"+d); err != nil || got != host+"/team/other@"+d || reg.hitsOf("team/other/manifests/"+d) != 0 {
		t.Fatalf("digest: %q %v", got, err)
	}
	if got, _ := r.resolve(bg, "alpine@"+d); got != "docker.io/library/alpine@"+d {
		t.Fatalf("docker hub digest: %q", got)
	}

	// Failures.
	if _, err := r.resolve(bg, host+"/team/arm:v1"); err == nil || !strings.Contains(err.Error(), "no linux/amd64 variant") {
		t.Fatalf("arm only: %v", err)
	}
	if _, err := r.resolve(bg, host+"/team/missing:v1"); err == nil || !strings.Contains(err.Error(), "private registries are not supported") {
		t.Fatalf("missing: %v", err)
	}
	if _, err := r.resolve(bg, host+"/limited/x"); err == nil || !strings.Contains(err.Error(), "rate-limited") {
		t.Fatalf("rate limited: %v", err)
	}
	if _, err := r.resolve(bg, "127.0.0.1:1/nothing/here:v1"); err == nil || err.Error() != "registry 127.0.0.1:1 unreachable" {
		t.Fatalf("unreachable: %v", err)
	}
	reg.set(func(r *registry) { r.badToken = true })
	if _, err := r.resolve(bg, host+"/team/plain:v2"); err == nil || err.Error() != "token realm returned no token" {
		t.Fatalf("a token realm that answers no JSON: %v", err)
	}
}

// The resolver reaches what a tenant's image names under the destination
// rule of bindings, on the address it dials: a registry, the target of a
// redirect and a token realm alike, each over https only; it tells which
// address it refused, never what a dial found there.
func TestResolverDestinationRule(t *testing.T) {
	reg := newRegistry(t)
	reg.put("team/app/manifests/v1", "application/vnd.oci.image.manifest.v1+json", `{"config":{}}`, "sha256:"+strings.Repeat("d", 64))
	host := reg.host()
	// A plain-http server that nothing may reach.
	var plainHits atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainHits.Add(1)
		fmt.Fprint(w, `{"token": "leaked"}`)
	}))
	defer plain.Close()

	// The default rule refuses the loopback, an address as a name, and a
	// cluster address, before anything is sent.
	def := reg.resolver(bindings.DefaultDenyPrefixes()...)
	port := strings.Split(host, ":")[1]
	for image, want := range map[string]string{
		host + "/team/app:v1": "registry " + host + " is not reachable from the control plane: " +
			"127.0.0.1 is an internal address (127.0.0.0/8)",
		"localhost:" + port + "/team/app:v1": "registry localhost:" + port + " is not reachable from the control plane: " +
			"127.0.0.1 is an internal address (127.0.0.0/8)",
		"10.43.0.1:5432/x/y:z": "registry 10.43.0.1:5432 is not reachable from the control plane: " +
			"10.43.0.1 is an internal address (10.0.0.0/8)",
	} {
		if _, err := def.resolve(bg, image); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", image, err, want)
		}
	}
	if reg.hitsOf("team/app/manifests/v1") != 0 {
		t.Fatal("the registry was reached")
	}

	// A rule that allows the registry's address: the realm and the
	// redirects are judged on their own.
	r := reg.resolver(netip.MustParsePrefix("10.0.0.0/8"))
	for _, c := range []struct {
		name, realm, redirect, want string
	}{
		{"realm over http", plain.URL + "/token", "",
			"unsupported auth challenge: the token realm is not an https URL"},
		{"realm on a denied address", "https://10.255.0.1/token", "",
			"token realm 10.255.0.1 is not reachable from the control plane: 10.255.0.1 is an internal address (10.0.0.0/8)"},
		{"redirect to http", "", plain.URL + "/v2/team/app/manifests/v1",
			"registry " + host + " redirected to a URL that is not https"},
		{"redirect to a denied address", "", "https://10.1.2.3/v2/team/app/manifests/v1",
			"registry " + host + " is not reachable from the control plane: 10.1.2.3 is an internal address (10.0.0.0/8)"},
	} {
		reg.set(func(r *registry) {
			r.realm = c.realm
			r.redirects["team/app/manifests/v1"] = c.redirect
		})
		r.cache = map[string]resolved{}
		if _, err := r.resolve(bg, host+"/team/app:v1"); err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if n := plainHits.Load(); n != 0 {
		t.Fatalf("the plain-http server was reached %d times", n)
	}
	reg.set(func(r *registry) {
		r.realm = ""
		r.redirects = map[string]string{}
	})
	if got, err := r.resolve(bg, host+"/team/app:v1"); err != nil || got != host+"/team/app@sha256:"+strings.Repeat("d", 64) {
		t.Fatalf("allowed: %q %v", got, err)
	}
}

// The engine's resolver applies the default rule when --deny-prefix names
// none.
func TestEngineResolverDefaultRule(t *testing.T) {
	h := newHarness(t)
	e, err := New(Config{InstanceID: "resolver", Log: dbtest.Logger(t)}, Deps{DB: h.d, Conns: h.ins[0].srv})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.resolveImage(bg, "127.0.0.1:1/x/y:z")
	if want := "registry 127.0.0.1:1 is not reachable from the control plane: 127.0.0.1 is an internal address (127.0.0.0/8)"; err == nil ||
		err.Error() != want {
		t.Fatalf("%v, want %q", err, want)
	}
}

func TestEnsureResolvesAndPinsImage(t *testing.T) {
	h := newHarness(t)
	h.addNode("n1")
	e := h.eng()
	reg := newRegistry(t)
	const amd = "sha256:" + "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	reg.put("team/app/manifests/v1", "application/vnd.oci.image.index.v1+json",
		`{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"`+amd+`","platform":{"os":"linux","architecture":"amd64"}}]}`, "")
	e.resolveImage = reg.resolver().resolve
	image := reg.host() + "/team/app:v1"
	sb := h.sandbox(512)
	h.exec("UPDATE sandboxes SET image = $2 WHERE id = $1", sb.ID, image)
	h.ensure(sb.ID)
	row := h.row(sb.ID)
	if row.ImageRef != reg.host()+"/team/app@"+amd || row.Image != image {
		t.Fatalf("image %q pinned to %q", row.Image, row.ImageRef)
	}
	info, err := h.pods("n1").GetPod(bg, podReq(sb.ID))
	if err != nil || info.GetSpec().GetContainers()[0].GetImageRef() != row.ImageRef {
		t.Fatalf("pod image %v %v", info.GetSpec().GetContainers(), err)
	}
	// A tag that moves later does not change the sandbox's image.
	reg.put("team/app/manifests/v1", "application/vnd.oci.image.index.v1+json",
		`{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"digest":"sha256:moved","platform":{"os":"linux","architecture":"amd64"}}]}`, "")
	h.startOp(sb.ID, OpIntent{Kind: OpPause, Trigger: TriggerExplicit, Target: "n1"})
	h.ensure(sb.ID)
	if h.row(sb.ID).ImageRef != row.ImageRef {
		t.Fatal("the pinned image changed")
	}
	// An image that cannot be resolved fails the Ensure.
	bad := h.sandbox(512)
	h.exec("UPDATE sandboxes SET image = $2 WHERE id = $1", bad.ID, reg.host()+"/team/missing:v1")
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	_, err = e.Ensure(ctx, bad.ID)
	wantErr(t, err, nil, "resolve image "+reg.host()+"/team/missing:v1: not found")
	wantState(t, h.row(bad.ID), model.StatePending)
}
