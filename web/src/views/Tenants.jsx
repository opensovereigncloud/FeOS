// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useState } from 'react';
import { api, getTenantToken, setTenantToken, storedTokenHash } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, LiveBadge, Secret, useArm, useNow, usePoll } from '../components.jsx';
import { fmtDate } from '../format.js';
import { tenantDeleteText, tokenRows } from './fleet.js';

// The tenants: create, delete, and their tokens. ferund stores a tenant's
// tokens as hashes only, so a token is shown once, when it is made; this
// browser keeps the tokens it made, to work as the tenant (the tenant's
// pages call the API with it).

export function Tenants({ navigate, user }) {
  const [tenants, setTenants] = useState(null);
  const [err, setErr] = useState('');
  const [name, setName] = useState('');
  const [showCreate, setShowCreate] = useState(false);
  const [created, setCreated] = useState(null); // {name, token}
  const [open, setOpen] = useState('');
  const [armed, arm, disarm] = useArm();
  useNow(5000);

  const refresh = useCallback(async () => {
    try {
      setTenants((await api.listTenants()).tenants || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(refresh, 5000);

  const go = (t) => navigate(`/tenants/${encodeURIComponent(t)}`);

  return (
    <div className="tenants single">
      <section className="pane">
        <div className="pane-toolbar">
          <h2><Icon name="users" size={15} /> Tenants <span className="count">{tenants ? tenants.length : '…'}</span></h2>
          <span className="spacer" />
          <LiveBadge lastOk={lastOk} />
          <button className={'ghost mini' + (showCreate ? ' on' : '')} onClick={() => setShowCreate(!showCreate)}>
            <Icon name="plus" size={13} /> new
          </button>
        </div>

        {showCreate && (
          <form className="createpanel" onSubmit={async (e) => {
            e.preventDefault();
            try {
              const n = name.trim();
              const res = await api.createTenant(n);
              setTenantToken(n, res.token);
              setCreated({ name: n, token: res.token });
              setName('');
              setShowCreate(false);
              await refresh();
            } catch (e2) {
              setErr(e2.message);
            }
          }}>
            <input placeholder="tenant name: lowercase letters, digits, '.' and '-'" value={name} autoFocus
              onChange={(e) => setName(e.target.value)} />
            <button type="submit" className="primary" disabled={!name.trim()}><Icon name="plus" size={14} /> Create</button>
            <button type="button" className="ghost" onClick={() => setShowCreate(false)}><Icon name="x" size={14} /></button>
          </form>
        )}
        {created && (
          <Secret value={created.token} title={`Token of tenant ${created.name}`} onDismiss={() => setCreated(null)}>
            <p className="muted small">This browser keeps it, to work as the tenant.</p>
          </Secret>
        )}
        <ErrBox error={err} />

        <div className="tblwrap"><table className="tbl">
          <thead>
            <tr>
              <th>name</th>
              <th className="col-num">sandboxes</th>
              <th className="col-num">tokens</th>
              <th>this browser</th>
              <th className="col-when">created</th>
              <th className="col-quick" />
            </tr>
          </thead>
          <tbody>
            {(tenants || []).map((t) => {
              const held = getTenantToken(t.name);
              const heldHash = storedTokenHash(t.name);
              const valid = held && (t.tokens || []).some((tk) => tk.hash === heldHash);
              const del = tenantDeleteText(t);
              return (
                <React.Fragment key={t.name}>
                  <tr className={'clickable' + (open === t.name ? ' sel' : '')} onClick={() => go(t.name)}
                    title="work as this tenant">
                    <td className="strong">
                      {t.name}
                      {t.deleting && <span className="badge warn">being deleted</span>}
                    </td>
                    <td className="col-num">{t.sandboxCount}</td>
                    <td className="col-num">{(t.tokens || []).length}</td>
                    <td className="small">
                      {!held && <span className="muted">no token</span>}
                      {held && valid && <span className="ok">holds a token</span>}
                      {held && !valid && <span className="warn" title="the token this browser holds is not among the tenant's">holds a revoked token</span>}
                    </td>
                    <td className="col-when muted">{fmtDate(t.createdAt)}</td>
                    <td className="col-quick" onClick={(e) => e.stopPropagation()}>
                      <button className={'ghost mini' + (open === t.name ? ' on' : '')} title="the tenant's tokens"
                        onClick={() => setOpen(open === t.name ? '' : t.name)}>
                        <Icon name="key" size={13} /> tokens
                      </button>
                      <button className="iconbtn mini" title="work as this tenant" onClick={() => go(t.name)}>
                        <Icon name="chevronRight" size={13} /></button>
                      <ArmButton armKey={t.name} armed={armed} arm={arm} disarm={disarm} icon="trash"
                        className="mini" confirmLabel={del.confirm} title={del.title}
                        onConfirm={async () => {
                          try {
                            await api.deleteTenant(t.name);
                            setTenantToken(t.name, '');
                            await refresh();
                          } catch (e) {
                            setErr(e.message);
                          }
                        }}>Delete</ArmButton>
                    </td>
                  </tr>
                  {open === t.name && (
                    <tr className="subrow">
                      <td colSpan={6}><TenantTokens tenant={t.name} user={user} onChange={refresh} /></td>
                    </tr>
                  )}
                </React.Fragment>
              );
            })}
          </tbody>
        </table></div>
        {tenants && tenants.length === 0 && <p className="empty">No tenants yet.</p>}
        {!tenants && !err && <p className="empty">Loading…</p>}

        <p className="hint">
          A tenant's token authenticates <span className="mono">/api/v1/sandboxes</span> and{' '}
          <span className="mono">/api/v1/tools</span> as the tenant; FeCode's backend holds one. Deleting a
          tenant revokes its tokens at once and deletes all its sandboxes.
        </p>
      </section>
    </div>
  );
}

// TenantTokens lists, mints and revokes a tenant's tokens. A minted token
// is shown once; the browser keeps it when it holds none of the tenant's
// yet, and otherwise on request.
function TenantTokens({ tenant, user, onChange }) {
  const [tokens, setTokens] = useState(null);
  const [label, setLabel] = useState('');
  const [fresh, setFresh] = useState('');
  const [err, setErr] = useState('');
  const [, setTick] = useState(0);
  const [armed, arm, disarm] = useArm();

  const refresh = useCallback(async () => {
    try {
      setTokens((await api.listTenantTokens(tenant)).tokens || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [tenant]);
  useEffect(() => { refresh(); }, [refresh]);

  const rows = tokenRows(tokens, storedTokenHash(tenant));
  const heldValid = rows.some((r) => r.held);

  return (
    <div className="subpanel">
      <ErrBox error={err} />
      {fresh && (
        <Secret value={fresh} title={`New token of tenant ${tenant}`} onDismiss={() => setFresh('')}>
          {getTenantToken(tenant) === fresh
            ? <p className="muted small">This browser keeps it, to work as the tenant.</p>
            : (
              <button className="ghost mini" onClick={() => { setTenantToken(tenant, fresh); setTick((x) => x + 1); onChange(); }}>
                <Icon name="check" size={13} /> use it in this browser
              </button>)}
        </Secret>
      )}
      <div className="tblwrap"><table className="tbl">
        <thead>
          <tr><th>hash</th><th>label</th><th className="col-when">created</th><th className="col-when">last used</th><th /><th className="col-quick" /></tr>
        </thead>
        <tbody>
          {rows.map((t) => (
            <tr key={t.hash}>
              <td className="mono muted" title={t.hash}>{t.hashPrefix}…</td>
              <td>{t.label || <span className="muted">(no label)</span>}</td>
              <td className="col-when muted">{fmtDate(t.createdAt)}</td>
              <td className="col-when">{t.lastUsedAt ? <Ago iso={t.lastUsedAt} /> : <span className="muted">never</span>}</td>
              <td className="small">{t.held && <span className="badge ok">this browser</span>}</td>
              <td className="col-quick">
                <ArmButton armKey={t.hash} armed={armed} arm={arm} disarm={disarm} icon="trash"
                  className="mini" confirmLabel="revoke?"
                  onConfirm={async () => {
                    try {
                      await api.revokeTenantToken(tenant, t.hash);
                      if (t.held) setTenantToken(tenant, '');
                      await refresh();
                      onChange();
                    } catch (e) {
                      setErr(e.message);
                    }
                  }}>Revoke</ArmButton>
              </td>
            </tr>
          ))}
        </tbody>
      </table></div>
      {tokens && tokens.length === 0 && <p className="empty">No tokens: nothing can act as this tenant.</p>}
      <form className="createpanel" onSubmit={async (e) => {
        e.preventDefault();
        try {
          const res = await api.createTenantToken(tenant, label.trim());
          if (!heldValid) setTenantToken(tenant, res.token);
          setFresh(res.token);
          setLabel('');
          await refresh();
          onChange();
        } catch (e2) {
          setErr(e2.message);
        }
      }}>
        <input placeholder={`label (e.g. fecode, ${user ? 'ui ' + user : 'ui'})`} value={label}
          onChange={(e) => setLabel(e.target.value)} />
        <button type="submit" className="primary"><Icon name="key" size={14} /> Mint a token</button>
      </form>
    </div>
  );
}
