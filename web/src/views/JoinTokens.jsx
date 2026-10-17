// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, LiveBadge, Secret, useArm, useNow, usePoll } from '../components.jsx';
import { fmtDate, fmtDateTime } from '../format.js';
import { joinTokenState } from './fleet.js';

// Join tokens: how a node joins. A token is used once; pinned to a node
// name, it lets that name join again (a re-join replaces the node's
// identity), unpinned it joins a new name only. It is shown once, when it
// is made; the list holds no secrets.

const TTLS = [
  { label: 'default', seconds: 0 },
  { label: '10 min', seconds: 600 },
  { label: '1 h', seconds: 3600 },
  { label: '1 day', seconds: 86400 },
  { label: '7 days', seconds: 7 * 86400 },
];

export function JoinTokens() {
  const [tokens, setTokens] = useState(null);
  const [err, setErr] = useState('');
  const [node, setNode] = useState('');
  const [ttl, setTtl] = useState(0);
  const [fresh, setFresh] = useState(null);
  const [armed, arm, disarm] = useArm();
  useNow(5000);

  const refresh = useCallback(async () => {
    try {
      setTokens((await api.listJoinTokens()).joinTokens || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(refresh, 10000);

  return (
    <div className="single">
      <section className="pane">
        <div className="pane-toolbar">
          <h2><Icon name="ticket" size={15} /> Join tokens <span className="count">{tokens ? tokens.length : '…'}</span></h2>
          <span className="spacer" />
          <LiveBadge lastOk={lastOk} />
        </div>

        <form className="createpanel" onSubmit={async (e) => {
          e.preventDefault();
          try {
            const res = await api.createJoinToken({ ttlSeconds: ttl, node: node.trim() });
            setFresh(res);
            setNode('');
            await refresh();
          } catch (e2) {
            setErr(e2.message);
          }
        }}>
          <input placeholder="node name (empty: any new name)" value={node} onChange={(e) => setNode(e.target.value)} />
          <select value={ttl} onChange={(e) => setTtl(Number(e.target.value))} title="how long the token is valid">
            {TTLS.map((t) => <option key={t.seconds} value={t.seconds}>valid {t.label}</option>)}
          </select>
          <button type="submit" className="primary"><Icon name="plus" size={14} /> Make a join token</button>
        </form>
        {fresh && (
          <Secret value={fresh.token} title={fresh.node ? `Join token for node ${fresh.node}` : 'Join token for a new node'}
            onDismiss={() => setFresh(null)}>
            <p className="muted small">
              Valid until {fmtDateTime(fresh.expiresAt)}, for one join. It carries the pin of the platform's root
              certificate, so that the node can check whom it joins.
            </p>
          </Secret>
        )}
        <ErrBox error={err} />

        <div className="tblwrap"><table className="tbl">
          <thead>
            <tr>
              <th>ID</th><th>node</th><th>made by</th><th className="col-when">made</th><th>expires</th><th>use</th><th className="col-quick" />
            </tr>
          </thead>
          <tbody>
            {(tokens || []).map((t) => {
              const st = joinTokenState(t);
              return (
                <tr key={t.id} className={st !== 'open' ? 'dim' : ''}>
                  <td className="mono small" title={t.id}>{t.id.slice(0, 8)}</td>
                  <td className="mono">{t.node || <span className="muted">any new name</span>}</td>
                  <td className="small">{t.createdBy || '—'}</td>
                  <td className="col-when muted">{fmtDate(t.createdAt)}</td>
                  <td className="small">{st === 'expired' ? <span className="muted">expired</span> : <Ago iso={t.expiresAt} />}</td>
                  <td className="small">
                    {t.usedAt
                      ? <>used by <span className="mono">{t.usedName || '?'}</span> <Ago iso={t.usedAt} />
                        {t.usedPeer && <div className="muted mono">{t.usedPeer}</div>}</>
                      : <span className="muted">unused</span>}
                  </td>
                  <td className="col-quick">
                    {st === 'open' && (
                      <ArmButton armKey={t.id} armed={armed} arm={arm} disarm={disarm} icon="trash"
                        className="mini" confirmLabel="revoke?"
                        onConfirm={async () => {
                          try {
                            await api.revokeJoinToken(t.id);
                            await refresh();
                          } catch (e) {
                            setErr(e.message);
                          }
                        }}>Revoke</ArmButton>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table></div>
        {tokens && tokens.length === 0 && <p className="empty">No join tokens.</p>}
        {!tokens && !err && <p className="empty">Loading…</p>}
      </section>
    </div>
  );
}
