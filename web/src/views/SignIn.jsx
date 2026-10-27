// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useState } from 'react';
import { auth, loginURL } from '../api.js';
import { Icon } from '../Icon.jsx';

// The sign-in screen. With OIDC, "Sign in" goes to the provider through
// /auth/login, which brings the browser back to next; with Basic
// (--admin-users), a user and password, kept for this tab only. ferund's
// /auth/config says which of them it offers.

export function SignIn({ config, next, notice, onSignedIn }) {
  const [user, setUser] = useState('');
  const [pass, setPass] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);
  const oidc = !!(config && config.oidc);
  const basic = !!(config && config.basic);

  return (
    <div className="login">
      <div className="login-card">
        <div className="login-brand"><Icon name="box" size={22} /> FeRun</div>
        <p className="muted">The control plane of FeOS: sandboxes, tenants and nodes.</p>
        {notice && <p className="notice small">{notice}</p>}
        {oidc && (
          <a className="button primary block" href={loginURL(next)}>
            <Icon name="login" size={15} /> Sign in
          </a>
        )}
        {oidc && basic && <div className="or muted small">or with an operator's password</div>}
        {basic && (
          <form onSubmit={async (e) => {
            e.preventDefault();
            setBusy(true);
            setErr('');
            try {
              const me = await auth.signInBasic(user, pass);
              onSignedIn(me);
            } catch (e2) {
              setErr(e2.message);
            } finally {
              setBusy(false);
            }
          }}>
            <input value={user} autoFocus={!oidc} placeholder="user" autoComplete="username"
              onChange={(e) => setUser(e.target.value)} />
            <input type="password" value={pass} placeholder="password" autoComplete="current-password"
              onChange={(e) => setPass(e.target.value)} />
            <button type="submit" className={oidc ? '' : 'primary'} disabled={busy || !user}>
              {busy ? 'checking…' : 'Sign in with the password'}
            </button>
          </form>
        )}
        {!oidc && !basic && (
          <p className="error">
            This instance offers no sign-in: ferund runs without --oidc-issuer and without --admin-users.
          </p>
        )}
        {err && <p className="error">{err}</p>}
      </div>
    </div>
  );
}
