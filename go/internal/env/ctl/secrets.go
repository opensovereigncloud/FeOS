// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/ironcore-dev/feos/go/internal/env/addons"
	"github.com/ironcore-dev/feos/go/internal/env/envdeps"
	"github.com/ironcore-dev/feos/go/internal/model"
	"github.com/ironcore-dev/feos/go/internal/store"
)

// ResolveSecret is the value of version version of the secret name of
// environment envID, a tenant's or an addon's system secret
// ("addon.<addon>.<key>"), unsealed with its AAD (envdeps.SecretAAD). A
// deleted version still resolves: a release pins it until a later one is
// realised. Without a Sealer it is an error naming --secret-key-file. It
// is the engine's secret resolver (through the environment service's) and
// the controller's own, for the env of attached services and of
// provisioning steps.
func (c *Controller) ResolveSecret(ctx context.Context, envID uuid.UUID, name string, version int) (string, error) {
	if c.d.Sealer == nil {
		return "", errNoSealer
	}
	sec, err := c.st.GetSecret(ctx, envID, name, version)
	if errors.Is(err, store.ErrNoRow) {
		return "", fmt.Errorf("no secret %s version %d in environment %s", name, version, envID)
	}
	if err != nil {
		return "", err
	}
	plain, err := c.d.Sealer.Open(sec.Sealed, envdeps.SecretAAD(envID, name, version))
	if err != nil {
		return "", fmt.Errorf("secret %s version %d: %w", name, version, err)
	}
	return string(plain), nil
}

// credentialBytes is how many random bytes a credential has: 32 hex
// characters, which survive word splitting (valkey's flags).
const credentialBytes = 16

// newCredential is a fresh credential value.
func newCredential() (string, error) {
	b := make([]byte, credentialBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ensureCredentials makes sure the addon's credentials exist as system
// secrets, version 1, generating each once (§4.3): a credential is never
// changed, so that the addon's data keeps opening with it. Without a
// Sealer it answers errNoSealer.
func (c *Controller) ensureCredentials(ctx context.Context, envID uuid.UUID, addon string, keys []string) error {
	var missing []string
	for _, k := range keys {
		name := addons.CredentialSecret(addon, k)
		_, err := c.st.GetSecret(ctx, envID, name, 1)
		switch {
		case errors.Is(err, store.ErrNoRow):
			missing = append(missing, name)
		case err != nil:
			return err
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if c.d.Sealer == nil {
		return errNoSealer
	}
	type sealed struct {
		name string
		blob []byte
	}
	var blobs []sealed
	for _, name := range missing {
		value, err := newCredential()
		if err != nil {
			return err
		}
		blob, err := c.d.Sealer.Seal([]byte(value), envdeps.SecretAAD(envID, name, 1))
		if err != nil {
			return fmt.Errorf("seal %s: %w", name, err)
		}
		blobs = append(blobs, sealed{name, blob})
	}
	// One transaction each: a credential made meanwhile (ErrExists) ends
	// its transaction, and stays as it is.
	for _, b := range blobs {
		err := c.fenced(ctx, func(tx *store.Store) error {
			_, err := tx.InsertSecret(ctx, store.EnvSecret{EnvID: envID, Name: b.name, Version: 1, Sealed: b.blob,
				System: true, CreatedBy: "ferund"})
			return err
		})
		if err != nil && !errors.Is(err, store.ErrExists) {
			return err
		}
	}
	return nil
}

// resolvedEnv is an env with its secrets' values, and those values (for
// redaction).
type resolvedEnv struct {
	vars    map[string]string
	secrets []string
}

// resolveEnv renders a template's env with the secrets' values, for an
// exec or a process: never written anywhere by the controller.
func (c *Controller) resolveEnv(ctx context.Context, envID uuid.UUID, env map[string]model.TemplateValue) (resolvedEnv, error) {
	out := resolvedEnv{vars: make(map[string]string, len(env))}
	seen := map[string]string{}
	secret := func(name string, version int, escape string) (string, error) {
		k := fmt.Sprintf("%s@%d", name, version)
		s, ok := seen[k]
		if !ok {
			var err error
			if s, err = c.ResolveSecret(ctx, envID, name, version); err != nil {
				return "", err
			}
			seen[k] = s
			out.secrets = append(out.secrets, s)
		}
		if escape == model.EscapeURL {
			return escapeUserinfo(s), nil
		}
		return s, nil
	}
	for k, v := range env {
		switch {
		case v.Parts != nil:
			var b strings.Builder
			for _, p := range v.Parts {
				switch {
				case p.Credential != "":
					return resolvedEnv{}, fmt.Errorf("env %s: an unmapped credential %q", k, p.Credential)
				case p.Secret != "":
					s, err := secret(p.Secret, p.Version, p.Escape)
					if err != nil {
						return resolvedEnv{}, fmt.Errorf("env %s: %w", k, err)
					}
					b.WriteString(s)
				default:
					b.WriteString(p.Value)
				}
			}
			out.vars[k] = b.String()
		case v.Secret != "":
			s, err := secret(v.Secret, v.Version, v.Escape)
			if err != nil {
				return resolvedEnv{}, fmt.Errorf("env %s: %w", k, err)
			}
			out.vars[k] = s
		default:
			out.vars[k] = v.Value
		}
	}
	return out, nil
}

// credentialValues are the values of an addon's credentials, for
// redaction; those that do not resolve are left out.
func (c *Controller) credentialValues(ctx context.Context, envID uuid.UUID, addon string, keys []string) []string {
	var out []string
	for _, k := range keys {
		if s, err := c.ResolveSecret(ctx, envID, addons.CredentialSecret(addon, k), 1); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// escapeUserinfo escapes s for the user or the password of a URL's
// userinfo (RFC 3986 §3.2.1), as the engine's pod spec does: the
// unreserved characters and the sub-delims stay, every other byte is
// percent-encoded in upper-case hex.
func escapeUserinfo(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' ||
			strings.IndexByte("-._~!$&'()*+,;=", ch) >= 0 {
			b.WriteByte(ch)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigits[ch>>4])
		b.WriteByte(hexDigits[ch&15])
	}
	return b.String()
}

// redactedMark replaces a secret's value in stored output.
const redactedMark = "[redacted]"

// redact removes every value of secrets from s (and its userinfo
// escaping), longest first: step and probe output never carries a
// credential into a detail, an incident or a log.
func redact(s string, secrets []string) string {
	vals := slices.Clone(secrets)
	for _, v := range secrets {
		if e := escapeUserinfo(v); e != v {
			vals = append(vals, e)
		}
	}
	slices.SortFunc(vals, func(a, b string) int { return len(b) - len(a) })
	for _, v := range vals {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, redactedMark)
		}
	}
	return s
}
