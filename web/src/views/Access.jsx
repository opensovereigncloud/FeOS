// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, CopyBtn, ErrBox, LiveBadge, useNow, usePoll } from '../components.jsx';
import { fmtDateTime } from '../format.js';

// What a sandbox can reach beyond itself: the Kubernetes clusters and SSH
// servers bound to it, each with what the audit log says it was used for,
// and the newest refusals. Read with the tenant's token; the sets are
// changed with the sandbox's token, by the client that owns them, not
// here. No credential reaches this view: fingerprints and public keys
// only. Where ferund runs without bindings, kube and ssh are null.

export function Access({ sandboxId }) {
  const [data, setData] = useState(null);
  const [err, setErr] = useState('');
  useNow(5000);

  const refresh = useCallback(async () => {
    try {
      setData(await api.getAccess(sandboxId));
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [sandboxId]);
  const lastOk = usePoll(refresh, 5000);

  if (!data) return err ? <ErrBox error={err} /> : <p className="empty">Loading…</p>;
  const denies = data.denies || [];

  return (
    <div className="access">
      <div className="access-head">
        <span className="muted small">
          The credentials stay on the node; the sandbox sees a local endpoint only.
        </span>
        <span className="spacer" />
        <LiveBadge lastOk={lastOk} />
      </div>
      <ErrBox error={err} />

      <h3 className="sect"><Icon name="server" size={13} /> Kubernetes clusters</h3>
      {!data.kube && <Disabled />}
      {data.kube && (data.kube.bindings || []).length === 0 && <p className="empty">No cluster bound.</p>}
      {data.kube && (data.kube.bindings || []).length > 0 && (
        <table className="tbl">
          <thead>
            <tr><th>name</th><th>server</th><th>credential</th><th>in the sandbox</th><th>use</th></tr>
          </thead>
          <tbody>
            {data.kube.bindings.map((b) => (
              <tr key={b.name}>
                <td className="mono">{b.name}</td>
                <td className="mono small trunc" title={b.server}>
                  {b.server}
                  {b.serverName && <div className="muted">TLS name {b.serverName}</div>}
                  {b.hasCa && <div className="muted">own CA</div>}
                </td>
                <td className="small">
                  {b.authType === 'client_cert' ? 'client certificate' : 'bearer token'}
                  <div className="mono muted" title="the fingerprint of the stored credential">{b.fingerprint}</div>
                </td>
                <td className="mono small">http://127.0.0.1:{b.port}</td>
                <td><Use use={b.use} unit="request" /></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h3 className="sect"><Icon name="terminal" size={13} /> SSH servers</h3>
      {!data.ssh && <Disabled />}
      {data.ssh && (data.ssh.bindings || []).length === 0 && <p className="empty">No server bound.</p>}
      {data.ssh && (data.ssh.bindings || []).length > 0 && (
        <table className="tbl">
          <thead>
            <tr><th>name</th><th>login</th><th>key</th><th>host keys</th><th>use</th></tr>
          </thead>
          <tbody>
            {data.ssh.bindings.map((b) => (
              <tr key={b.name}>
                <td className="mono">{b.name}</td>
                <td className="mono small">
                  {b.anyServer
                    ? <span title="signs a login to any server that trusts its public key: where the key is registered decides">any server</span>
                    : `${b.user}@${(b.server || '').replace(/^ssh:\/\//, '')}`}
                </td>
                <td className="small">
                  {b.authType === 'generate' ? 'generated' : 'uploaded'} {b.keyType}
                  <div className="access-key">
                    <span className="mono muted" title={b.publicKey}>{b.fingerprint}</span>
                    <CopyBtn text={b.publicKey} title="copy the public key (for authorized_keys)" mini />
                  </div>
                </td>
                <td className="mono small">
                  {b.anyServer && <span className="muted">—</span>}
                  {(b.hostKeys || []).map((h, i) => (
                    <div key={i} className="muted" title={h.type}>{h.fingerprint || 'unreadable key'}</div>
                  ))}
                </td>
                <td><Use use={b.use} unit="login" /></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h3 className="sect"><Icon name="x" size={13} /> Refusals</h3>
      {denies.length === 0 && <p className="empty">Nothing refused.</p>}
      {denies.length > 0 && (
        <table className="tbl">
          <thead>
            <tr><th className="col-when">when</th><th>kind</th><th>binding</th><th>reason</th><th>detail</th></tr>
          </thead>
          <tbody>
            {denies.map((d, i) => (
              <tr key={i}>
                <td className="col-when" title={fmtDateTime(d.at)}><Ago iso={d.at} /></td>
                <td className="small">{d.kind === 'kube' ? 'Kubernetes' : 'SSH'}</td>
                <td className="mono small">{d.binding || <span className="muted">—</span>}</td>
                <td className="mono small bad">{d.reason}</td>
                <td className="small muted trunc" title={d.detail}>{d.detail}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function Use({ use, unit }) {
  if (!use) return <span className="muted small">never used</span>;
  return (
    <div className="small">
      {use.uses} {unit}{use.uses === 1 ? '' : 's'}
      {use.errors > 0 && <span className="bad"> · {use.errors} failed</span>}
      {' · '}<Ago iso={use.lastUsedAt} />
      <div className="mono muted trunc" title={use.lastUse}>{use.lastUse}</div>
    </div>
  );
}

function Disabled() {
  return <p className="empty">Not enabled on this platform.</p>;
}
