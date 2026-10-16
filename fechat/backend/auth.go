// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

// Sign-in with a FeCode ID: FeChat is an OpenID Connect client of
// id.fecode.dev, where FeCode, FeRun and FeChat share their users, who sign
// in with GitHub and get in by invitation. The backend stays stateless:
// the session is a signed cookie with the user's claims.
//
//	GET  /api/v1/auth/login?return=/path   to the IdP (code flow, PKCE, nonce)
//	GET  /api/v1/auth/callback             back from it: the session cookie
//	GET  /api/v1/auth/me                   who is signed in, or 401
//	POST /api/v1/auth/logout               ends the session; answers where
//	                                       to end the IdP's too

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	loginCookie   = "__Host-fechat_login"
	sessionCookie = "__Host-fechat_session"
	loginTTL      = 10 * time.Minute
	// A sign-in again after this is silent while the IdP remembers the
	// person, and refused once they were disabled there.
	sessionTTL = 24 * time.Hour
)

// User is who a session belongs to: the IdP's claims.
type User struct {
	Subject string `json:"sub"`
	Login   string `json:"login"`
	Name    string `json:"name"`
	Email   string `json:"email"`
	Picture string `json:"picture"`
	Admin   bool   `json:"admin"`
}

type session struct {
	User    User  `json:"u"`
	Expires int64 `json:"e"`
}

// pendingLogin is a sign-in in flight, in its cookie.
type pendingLogin struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Return   string `json:"r"`
	Expires  int64  `json:"e"`
}

type auth struct {
	issuer       string
	clientID     string
	clientSecret string
	publicURL    string
	key          []byte

	// The IdP's discovery, fetched when first needed and kept: the backend
	// starts while the IdP is down, too.
	mu       sync.Mutex
	provider *oidc.Provider
	endURL   string
}

func newAuth(issuer, clientID, clientSecret, publicURL, secret string) (*auth, error) {
	if clientID == "" || clientSecret == "" || publicURL == "" {
		return nil, errors.New("OIDC_ISSUER needs OIDC_CLIENT_ID, OIDC_CLIENT_SECRET and PUBLIC_URL")
	}
	if len(secret) < 32 {
		return nil, errors.New("SESSION_SECRET must be at least 32 bytes")
	}
	key := sha256.Sum256([]byte("fechat sessions\x00" + secret))
	return &auth{
		issuer: issuer, clientID: clientID, clientSecret: clientSecret,
		publicURL: strings.TrimRight(publicURL, "/"), key: key[:],
	}, nil
}

func (a *auth) discover(ctx context.Context) (*oidc.Provider, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.provider != nil {
		return a.provider, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(ctx, a.issuer)
	if err != nil {
		return nil, err
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := p.Claims(&meta); err == nil {
		a.endURL = meta.EndSession
	}
	a.provider = p
	return p, nil
}

func (a *auth) oauth2(p *oidc.Provider) *oauth2.Config {
	// The client is registered for client_secret_basic; without saying so,
	// the library picks from what the IdP supports.
	ep := p.Endpoint()
	ep.AuthStyle = oauth2.AuthStyleInHeader
	return &oauth2.Config{
		ClientID:     a.clientID,
		ClientSecret: a.clientSecret,
		Endpoint:     ep,
		RedirectURL:  a.publicURL + "/api/v1/auth/callback",
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}
}

func (a *auth) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/auth/login", a.login)
	mux.HandleFunc("GET /api/v1/auth/callback", a.callback)
	mux.HandleFunc("GET /api/v1/auth/me", a.me)
	mux.HandleFunc("POST /api/v1/auth/logout", a.logout)
}

// require lets only signed-in users through, and only same-origin
// requests, against cross-site forms.
func (a *auth) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && !a.sameOrigin(r) {
			writeAuthError(w, http.StatusForbidden, "cross-origin request")
			return
		}
		u, ok := a.user(r)
		if !ok {
			writeAuthError(w, http.StatusUnauthorized, "sign-in required")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	}
}

type userKey struct{}

// userFrom returns who made a request that require let through.
func userFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey{}).(User)
	return u, ok
}

func (a *auth) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	return origin == "" || origin == a.publicURL
}

func (a *auth) user(r *http.Request) (User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return User{}, false
	}
	var s session
	if err := a.verify(c.Value, &s); err != nil || time.Now().Unix() > s.Expires {
		return User{}, false
	}
	return s.User, true
}

func (a *auth) login(w http.ResponseWriter, r *http.Request) {
	p, err := a.discover(r.Context())
	if err != nil {
		log.Printf("auth: discovery of %s: %v", a.issuer, err)
		http.Error(w, "the sign-in service is unreachable; try again", http.StatusBadGateway)
		return
	}
	pl := pendingLogin{
		State:    randomString(),
		Nonce:    randomString(),
		Verifier: oauth2.GenerateVerifier(),
		Return:   safeReturn(r.URL.Query().Get("return")),
		Expires:  time.Now().Add(loginTTL).Unix(),
	}
	if err := a.setCookie(w, loginCookie, pl, loginTTL); err != nil {
		http.Error(w, "sign-in failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, a.oauth2(p).AuthCodeURL(pl.State, oidc.Nonce(pl.Nonce),
		oauth2.S256ChallengeOption(pl.Verifier)), http.StatusFound)
}

func (a *auth) callback(w http.ResponseWriter, r *http.Request) {
	var pl pendingLogin
	c, err := r.Cookie(loginCookie)
	clearCookie(w, loginCookie)
	q := r.URL.Query()
	if err != nil || a.verify(c.Value, &pl) != nil || time.Now().Unix() > pl.Expires ||
		q.Get("state") == "" || q.Get("state") != pl.State {
		signInFailed(w, http.StatusBadRequest, "The sign-in took too long or came from elsewhere.")
		return
	}
	if e := q.Get("error"); e != "" {
		signInFailed(w, http.StatusForbidden, firstNonEmpty(q.Get("error_description"), e))
		return
	}
	p, err := a.discover(r.Context())
	if err != nil {
		log.Printf("auth: discovery of %s: %v", a.issuer, err)
		signInFailed(w, http.StatusBadGateway, "The sign-in service is unreachable.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tok, err := a.oauth2(p).Exchange(ctx, q.Get("code"), oauth2.VerifierOption(pl.Verifier))
	if err != nil {
		log.Printf("auth: code exchange: %v", err)
		signInFailed(w, http.StatusBadGateway, "The sign-in could not be completed.")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := p.Verifier(&oidc.Config{ClientID: a.clientID}).Verify(ctx, raw)
	if err != nil || idt.Nonce != pl.Nonce {
		log.Printf("auth: ID token: %v (nonce match %v)", err, idt != nil && idt.Nonce == pl.Nonce)
		signInFailed(w, http.StatusBadGateway, "The sign-in could not be verified.")
		return
	}
	var cl struct {
		Login   string   `json:"preferred_username"`
		Name    string   `json:"name"`
		Email   string   `json:"email"`
		Picture string   `json:"picture"`
		Groups  []string `json:"groups"`
	}
	if err := idt.Claims(&cl); err != nil {
		signInFailed(w, http.StatusBadGateway, "The sign-in could not be verified.")
		return
	}
	u := User{Subject: idt.Subject, Login: cl.Login, Name: cl.Name, Email: cl.Email, Picture: cl.Picture}
	for _, g := range cl.Groups {
		if g == "admins" {
			u.Admin = true
		}
	}
	if err := a.setCookie(w, sessionCookie, session{User: u, Expires: time.Now().Add(sessionTTL).Unix()}, sessionTTL); err != nil {
		signInFailed(w, http.StatusInternalServerError, "The session could not be made.")
		return
	}
	log.Printf("auth: %s signed in", firstNonEmpty(u.Login, u.Subject))
	http.Redirect(w, r, pl.Return, http.StatusFound)
}

func (a *auth) me(w http.ResponseWriter, r *http.Request) {
	u, ok := a.user(r)
	if !ok {
		writeAuthError(w, http.StatusUnauthorized, "sign-in required")
		return
	}
	writeJSON(w, u)
}

// logout ends the session here and answers where the browser ends the
// IdP's, which would otherwise sign it in again at once.
func (a *auth) logout(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(r) {
		writeAuthError(w, http.StatusForbidden, "cross-origin request")
		return
	}
	clearCookie(w, sessionCookie)
	to := "/"
	if _, err := a.discover(r.Context()); err == nil && a.endURL != "" {
		to = a.endURL
	}
	writeJSON(w, map[string]string{"redirect": to})
}

// safeReturn keeps a return path on this site: "/x", never "//host" or a
// URL.
func safeReturn(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") ||
		strings.HasPrefix(p, "/api/") {
		return "/"
	}
	return p
}

func signInFailed(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>FeChat</title>
<body style="font-family:sans-serif;background:#0b0d12;color:#e6e6e6;padding:3rem">
<h1 style="font-size:1.2rem">Sign-in failed</h1><p>%s</p>
<p><a style="color:#9fd0ff" href="/api/v1/auth/login">Try again</a></p>`, htmlEscape(text))
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}

func writeAuthError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func randomString() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on Linux
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// --- signed cookies: HMAC-SHA256 over JSON ---

func (a *auth) sign(v any) (string, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

var errBadCookie = errors.New("invalid cookie")

func (a *auth) verify(value string, v any) error {
	p, m, ok := strings.Cut(value, ".")
	if !ok {
		return errBadCookie
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	sum, err2 := base64.RawURLEncoding.DecodeString(m)
	if err1 != nil || err2 != nil {
		return errBadCookie
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write(payload)
	if !hmac.Equal(sum, mac.Sum(nil)) || json.Unmarshal(payload, v) != nil {
		return errBadCookie
	}
	return nil
}

func (a *auth) setCookie(w http.ResponseWriter, name string, v any, ttl time.Duration) error {
	value, err := a.sign(v)
	if err != nil {
		return err
	}
	// __Host-: Secure, Path=/, no Domain; Lax, so that it comes along when
	// the IdP sends the browser back.
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	return nil
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}
