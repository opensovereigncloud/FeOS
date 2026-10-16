// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

// The image check before a chat starts: is the sandbox image there? An
// anonymous OCI manifest lookup over the registry's HTTP API v2. Only
// metadata is read (the manifest, capped), never layer blobs. Only public
// images: nothing on the way to the sandbox carries registry credentials,
// so a private registry looks like "not found".
//
// GET /v2/<repo>/manifests/<ref> with the manifest Accept headers. On a
// 401 with a bearer challenge, fetch an anonymous token from the realm
// and try again. A 2xx with an image index is also checked for a
// linux/amd64 variant: the sandboxes run on amd64 only.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type imageCheckResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Warning string `json:"warning,omitempty"`
}

const manifestAccept = "application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json"

const maxManifestBytes = 8 << 20

// splitImageRef splits "registry/repo[:tag][@digest]" the way Docker
// does. The default registry is docker.io (registry-1.docker.io, with
// library/ in front of a single-component repo); the default ref is
// latest.
func splitImageRef(s string) (reg, repo, ref string, err error) {
	if s == "" {
		return "", "", "", fmt.Errorf("empty image reference")
	}
	if i := strings.Index(s, "@"); i >= 0 {
		ref, s = s[i+1:], s[:i]
	} else if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") {
		ref, s = s[i+1:], s[:i]
	} else {
		ref = "latest"
	}
	if parts := strings.SplitN(s, "/", 2); len(parts) == 2 &&
		(strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		reg, repo = parts[0], parts[1]
	} else {
		reg, repo = "registry-1.docker.io", s
		if !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	}
	if reg == "docker.io" || reg == "index.docker.io" {
		reg = "registry-1.docker.io"
	}
	if reg == "" || repo == "" || ref == "" {
		return "", "", "", fmt.Errorf("invalid image reference %q", s)
	}
	if strings.ContainsAny(repo+ref, " \t\n\r") {
		return "", "", "", fmt.Errorf("invalid image reference %q", s)
	}
	return reg, repo, ref, nil
}

// validImageRef validates the image of a request to /api/v1/chat: empty
// means ferund's default image, anything else must look like a reference.
func validImageRef(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > 512 || strings.ContainsAny(s, " \t\n\r\x00") {
		return fmt.Errorf("invalid image reference")
	}
	_, _, _, err := splitImageRef(s)
	return err
}

var authKV = regexp.MustCompile(`(\w+)="([^"]*)"`)

// errImageNotFound is what the token realm's and the manifest endpoint's
// refusals map to. Private registries cannot be used and look exactly
// like this.
var errImageNotFound = errors.New("not found (private registries are not supported and look like this)")

// bearerToken answers a WWW-Authenticate bearer challenge anonymously.
func bearerToken(ctx context.Context, client *http.Client, challenge string) (string, error) {
	params := map[string]string{}
	for _, m := range authKV.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("unsupported auth challenge (anonymous pulls only)")
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
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// ghcr and others refuse anonymous tokens for scopes that do not
		// exist with 403 or 404: to the caller that is "not found".
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			return "", errImageNotFound
		}
		return "", fmt.Errorf("token realm: HTTP %d", resp.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return "", err
	}
	t := tok.Token
	if t == "" {
		t = tok.AccessToken
	}
	if t == "" {
		return "", fmt.Errorf("token realm returned no token")
	}
	return t, nil
}

// checkImageAvailable checks whether the image can be pulled anonymously.
// It reads no more than the manifest (maxManifestBytes).
func checkImageAvailable(ctx context.Context, image string) imageCheckResult {
	reg, repo, ref, err := splitImageRef(image)
	if err != nil {
		return imageCheckResult{Message: err.Error()}
	}
	scheme := "https"
	if reg == "localhost" || strings.HasPrefix(reg, "localhost:") || strings.HasPrefix(reg, "127.0.0.1") {
		scheme = "http"
	}
	manifestURL := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, reg, repo, ref)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := &http.Client{}

	get := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", manifestAccept)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return client.Do(req)
	}

	resp, err := get("")
	if err != nil {
		return imageCheckResult{Message: "registry unreachable: " + err.Error()}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		token, terr := bearerToken(ctx, client, challenge)
		if terr != nil {
			return imageCheckResult{Message: terr.Error()}
		}
		if resp, err = get(token); err != nil {
			return imageCheckResult{Message: "registry unreachable: " + err.Error()}
		}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		// parse the manifest below
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized:
		return imageCheckResult{Message: errImageNotFound.Error()}
	case resp.StatusCode == http.StatusTooManyRequests:
		return imageCheckResult{Message: "registry rate-limited the check; the pull may still work"}
	default:
		io.Copy(io.Discard, io.LimitReader(resp.Body, 256))
		return imageCheckResult{Message: fmt.Sprintf("registry answered HTTP %d", resp.StatusCode)}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
	if err != nil {
		return imageCheckResult{Message: "manifest read: " + err.Error()}
	}
	var m struct {
		MediaType string `json:"mediaType"`
		Manifests []struct {
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return imageCheckResult{Message: "unparseable manifest"}
	}
	// An image index or manifest list: look for the platform's variant.
	if strings.HasSuffix(m.MediaType, "image.index.v1+json") ||
		strings.HasSuffix(m.MediaType, "manifest.list.v2+json") {
		for _, sub := range m.Manifests {
			if sub.Platform.OS == "linux" && sub.Platform.Architecture == "amd64" {
				return imageCheckResult{OK: true, Warning: "multi-arch, linux/amd64 present"}
			}
		}
		return imageCheckResult{Message: "no linux/amd64 variant in this image index"}
	}
	return imageCheckResult{OK: true}
}

func handleImageCheck() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Image string `json:"image"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Image == "" {
			// an empty field is ferund's default image, there by definition
			writeJSON(w, imageCheckResult{OK: true, Message: "default image"})
			return
		}
		res := checkImageAvailable(r.Context(), req.Image)
		if res.OK {
			log.Printf("image-check %q: ok (%s)", req.Image, res.Warning)
		} else {
			log.Printf("image-check %q: %s", req.Image, res.Message)
		}
		writeJSON(w, res)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
