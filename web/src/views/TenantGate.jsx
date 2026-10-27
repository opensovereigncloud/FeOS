// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useState } from 'react';
import { api, setTenantToken } from '../api.js';
import { sha256Hex } from '../sha256.js';
import { Icon } from '../Icon.jsx';

// TenantGate stands in for a tenant's pages while this browser holds no
// token of the tenant. ferund keeps tenant tokens as hashes only, so none
// can be read back: the operator mints one for this browser, or pastes
// one they have.

export function TenantGate({ tenant, user }) {
  const [tok, setTok] = useState('');
  const [err, setErr] = useState('');
  const [busy, setBusy] = useState(false);

  const mint = async () => {
    setBusy(true);
    setErr('');
    try {
      const res = await api.createTenantToken(tenant, `FeRun UI${user ? ` (${user})` : ''}`);
      setTenantToken(tenant, res.token);
    } catch (e) {
      setErr(e.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="pane gate">
      <h2><Icon name="key" size={15} /> A token of tenant “{tenant}”</h2>
      <p className="muted">
        This browser holds no token of the tenant. ferund stores tenant tokens as hashes, so a token cannot be
        read back: mint one for this browser (it shows in the tenant's tokens, and can be revoked there), or
        paste one you have.
      </p>
      <div className="actionrow">
        <button className="primary" disabled={busy} onClick={mint}>
          <Icon name="key" size={14} /> Mint a token for this browser
        </button>
      </div>
      <form className="createpanel" onSubmit={async (e) => {
        e.preventDefault();
        const t = tok.trim();
        if (!t) return;
        setBusy(true);
        setErr('');
        try {
          // The tenant's tokens are listed by hash: a token of another
          // tenant, or a revoked one, is not among them.
          const hash = sha256Hex(t);
          const { tokens } = await api.listTenantTokens(tenant);
          if (!(tokens || []).some((x) => x.hash === hash)) throw new Error(`that is not a token of tenant ${tenant}`);
          setTenantToken(tenant, t);
        } catch (e2) {
          setErr(e2.message);
        } finally {
          setBusy(false);
        }
      }}>
        <input type="password" value={tok} placeholder="or paste a tenant token" autoComplete="off"
          onChange={(e) => setTok(e.target.value)} />
        <button type="submit" disabled={busy || !tok.trim()}>Use it</button>
      </form>
      {err && <p className="error">{err}</p>}
    </section>
  );
}
