// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The image resolver pins an image to the digest of its linux/amd64
// manifest, so that a sandbox never silently changes image and a tag that
// moves never breaks CreatePod's idempotency (§9 "Image resolution"). It
// speaks the anonymous registry protocol only: private registries are not
// supported.
const (
	resolveTTL          = 60 * time.Second
	resolveTimeout      = 30 * time.Second
	resolveManifestMax  = 8 << 20
	dockerHub           = "docker.io"
	dockerHubRegistry   = "registry-1.docker.io"
	resolveManifestType = "application/vnd.oci.image.index.v1+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json, " +
		"application/vnd.oci.image.manifest.v1+json, " +
		"application/vnd.docker.distribution.manifest.v2+json"
)

// errImageNotFound is the answer of a registry that does not serve the
// image anonymously: it does not exist, or is private.
var errImageNotFound = errors.New("not found (private registries are not supported and look like this)")

var authParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// imageRef is an image reference split as Docker does.
type imageRef struct {
	// name is the registry as it is named in a pinned reference
	// ("docker.io" for Docker Hub); host is the one that serves it.
	name, host string
	repo       string
	// ref is the tag, or the digest ("sha256:…").
	ref string
}

func (r imageRef) digest() bool { return strings.HasPrefix(r.ref, "sha256:") }

// pinned is the reference with the manifest digest d.
func (r imageRef) pinned(d string) string { return r.name + "/" + r.repo + "@" + d }

// parseImageRef splits "registry/repo[:tag][@digest]": the registry is the
// first component when it has a dot or a colon or is localhost, else
// Docker Hub (with "library/" for a one-component repository); the tag is
// "latest" when none is given.
func parseImageRef(s string) (imageRef, error) {
	if strings.TrimSpace(s) == "" {
		return imageRef{}, errors.New("empty image reference")
	}
	var r imageRef
	rest := s
	if i := strings.Index(rest, "@"); i >= 0 {
		r.ref, rest = rest[i+1:], rest[:i]
		// A tag before the digest is ignored: the digest names the image.
		if j := strings.LastIndex(rest, ":"); j > strings.LastIndex(rest, "/") {
			rest = rest[:j]
		}
	} else if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		r.ref, rest = rest[i+1:], rest[:i]
	} else {
		r.ref = "latest"
	}
	if parts := strings.SplitN(rest, "/", 2); len(parts) == 2 &&
		(strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		r.name, r.repo = parts[0], parts[1]
	} else {
		r.name, r.repo = dockerHub, rest
	}
	if r.name == "index.docker.io" || r.name == dockerHubRegistry {
		r.name = dockerHub
	}
	if r.name == dockerHub && !strings.Contains(r.repo, "/") {
		r.repo = "library/" + r.repo
	}
	r.host = r.name
	if r.name == dockerHub {
		r.host = dockerHubRegistry
	}
	if r.repo == "" || r.ref == "" || strings.ContainsAny(r.repo+r.ref, " \t\r\n") {
		return imageRef{}, fmt.Errorf("invalid image reference %q", s)
	}
	if strings.Contains(s, "@") && !r.digest() {
		return imageRef{}, fmt.Errorf("invalid image reference %q: only sha256 digests", s)
	}
	return r, nil
}

// resolver resolves image references with a cache of tags.
type resolver struct {
	client *http.Client
	// scheme returns the URL scheme of a registry host: http for the
	// loopback, https for everything else.
	scheme func(host string) string
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]resolved
}

type resolved struct {
	pinned  string
	expires time.Time
}

func newResolver() *resolver {
	return &resolver{client: &http.Client{}, scheme: registryScheme, now: time.Now, cache: map[string]resolved{}}
}

func registryScheme(host string) string {
	if host == "localhost" || strings.HasPrefix(host, "localhost:") || strings.HasPrefix(host, "127.0.0.1") ||
		strings.HasPrefix(host, "[::1]") {
		return "http"
	}
	return "https"
}

// resolve returns "<registry>/<repository>@sha256:<manifest digest>" for
// image: for an index, the digest of its linux/amd64 manifest. A digest
// reference is passed through without asking the registry; a tag is
// cached for 60 s.
func (r *resolver) resolve(ctx context.Context, image string) (string, error) {
	ref, err := parseImageRef(image)
	if err != nil {
		return "", err
	}
	if ref.digest() {
		return ref.pinned(ref.ref), nil
	}
	now := r.now()
	r.mu.Lock()
	if c, ok := r.cache[image]; ok && now.Before(c.expires) {
		r.mu.Unlock()
		return c.pinned, nil
	}
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	body, digest, err := r.manifest(ctx, ref, ref.ref)
	if err != nil {
		return "", err
	}
	var probe struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return "", fmt.Errorf("%s/%s: unparseable manifest", ref.name, ref.repo)
	}
	if strings.HasSuffix(probe.MediaType, "image.index.v1+json") || strings.HasSuffix(probe.MediaType, "manifest.list.v2+json") ||
		(probe.MediaType == "" && len(probe.Manifests) > 0) {
		digest = ""
		for _, m := range probe.Manifests {
			if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
				digest = m.Digest
				break
			}
		}
		if digest == "" {
			return "", fmt.Errorf("%s/%s: no linux/amd64 variant in this image index", ref.name, ref.repo)
		}
	} else if digest == "" {
		digest = fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	}
	if !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("%s/%s: unsupported digest %q", ref.name, ref.repo, digest)
	}
	pinned := ref.pinned(digest)
	r.mu.Lock()
	r.cache[image] = resolved{pinned: pinned, expires: now.Add(resolveTTL)}
	for k, c := range r.cache {
		if !now.Before(c.expires) {
			delete(r.cache, k)
		}
	}
	r.mu.Unlock()
	return pinned, nil
}

// manifest fetches a manifest anonymously, answering a Bearer challenge
// with an anonymous token; it returns the body and the digest the
// registry names (Docker-Content-Digest), if any.
func (r *resolver) manifest(ctx context.Context, ref imageRef, tagOrDigest string) ([]byte, string, error) {
	u := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", r.scheme(ref.host), ref.host, ref.repo, tagOrDigest)
	get := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", resolveManifestType)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return r.client.Do(req)
	}
	resp, err := get("")
	if err != nil {
		return nil, "", fmt.Errorf("registry unreachable: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		token, err := r.token(ctx, challenge)
		if err != nil {
			return nil, "", err
		}
		if resp, err = get(token); err != nil {
			return nil, "", fmt.Errorf("registry unreachable: %w", err)
		}
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
		return nil, "", errImageNotFound
	case http.StatusTooManyRequests:
		return nil, "", errors.New("registry rate-limited the resolve; retry")
	default:
		return nil, "", fmt.Errorf("registry answered HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, resolveManifestMax))
	if err != nil {
		return nil, "", fmt.Errorf("manifest read: %w", err)
	}
	return body, resp.Header.Get("Docker-Content-Digest"), nil
}

// token answers a WWW-Authenticate Bearer challenge anonymously.
func (r *resolver) token(ctx context.Context, challenge string) (string, error) {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(challenge)), "bearer") {
		return "", errors.New("unsupported auth challenge (anonymous pulls only)")
	}
	params := map[string]string{}
	for _, m := range authParam.FindAllStringSubmatch(challenge, -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		return "", errors.New("unsupported auth challenge (anonymous pulls only)")
	}
	q := url.Values{}
	if v := params["service"]; v != "" {
		q.Set("service", v)
	}
	if v := params["scope"]; v != "" {
		q.Set("scope", v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token realm unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized {
			return "", errImageNotFound
		}
		return "", fmt.Errorf("token realm answered HTTP %d", resp.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", fmt.Errorf("token realm: %w", err)
	}
	if tok.Token != "" {
		return tok.Token, nil
	}
	if tok.AccessToken != "" {
		return tok.AccessToken, nil
	}
	return "", errors.New("token realm returned no token")
}
