// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, CopyBtn, ErrBox, LiveBadge, StateLabel, useArm, useNow, usePoll } from '../components.jsx';
import { fmtDateTime, nsToIso } from '../format.js';
import { Activity } from './Activity.jsx';
import {
  canDrain, certState, fmtBytes, nodeRows, settingsForm, settingsPatch, stopping, vcpuCapacityText,
} from './fleet.js';

// One node: everything ferund knows of it, its capacity settings, drain
// and removal, its certificates (the renewal history) and its activity
// (the node's own records, and those about pods that are not ferund's
// or not on it). The tab is the route's: /nodes/:node/:tab.

const TABS = [
  { key: 'overview', label: 'Overview', icon: 'gauge' },
  { key: 'certs', label: 'Certificates', icon: 'shield' },
  { key: 'activity', label: 'Activity', icon: 'activity' },
];

export function NodeDetail({ node, tab, navigate }) {
  const [raw, setRaw] = useState(null);
  const [err, setErr] = useState('');
  const base = `/nodes/${encodeURIComponent(node)}`;
  const activeTab = TABS.some((t) => t.key === tab) ? tab : 'overview';
  useNow(5000);

  const load = useCallback(async () => {
    try {
      const all = (await api.listNodes()).nodes || [];
      setRaw(all.find((n) => n.node === node) || false);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [node]);
  const lastOk = usePoll(load, 5000);
  const loadActivity = useCallback((source) => api.nodeActivity(node, { source }), [node]);

  if (raw === false) {
    return (
      <section className="pane">
        <p className="error">No node {node}.</p>
        <button className="ghost" onClick={() => navigate('/nodes')}><Icon name="arrowLeft" size={14} /> the nodes</button>
      </section>
    );
  }
  const row = raw ? nodeRows([raw])[0] : null;

  return (
    <section className="pane pane-detail">
      <div className="detail-head">
        <button className="iconbtn" title="back to the nodes" onClick={() => navigate('/nodes')}>
          <Icon name="arrowLeft" size={15} /></button>
        <Icon name="server" size={16} />
        <div className="detail-title">
          <span className="mono uuid">{node}</span>
          <span className="muted small">{raw ? `${raw.hostname || '—'} · ${raw.arch || '—'} · ${raw.status}` : 'loading…'}</span>
        </div>
        <LiveBadge lastOk={lastOk} />
      </div>
      <div className="tabbar">
        {TABS.map((t) => (
          <button key={t.key} className={'tab' + (activeTab === t.key ? ' on' : '')} onClick={() => navigate(`${base}/${t.key}`)}>
            <Icon name={t.icon} size={14} /> {t.label}
          </button>
        ))}
      </div>
      <ErrBox error={err} />
      {activeTab === 'overview' && raw && <NodeOverview raw={raw} row={row} reload={load} />}
      {activeTab === 'certs' && <NodeCerts node={node} current={raw ? raw.certSerial : ''} />}
      {activeTab === 'activity' && <div className="tabbody"><Activity load={loadActivity} /></div>}
    </section>
  );
}

function versionsOf(v) {
  if (!v || typeof v !== 'object') return [];
  return Object.entries(v).map(([k, x]) => [k, typeof x === 'string' ? x : JSON.stringify(x)]);
}

function NodeOverview({ raw, row, reload }) {
  const [armed, arm, disarm] = useArm();
  const [err, setErr] = useState('');
  const [needsForce, setNeedsForce] = useState(false);
  const [busy, setBusy] = useState(false);

  const act = async (fn) => {
    setBusy(true);
    setErr('');
    try {
      await fn();
      await reload();
    } catch (e) {
      setErr(e.message);
      return e;
    } finally {
      setBusy(false);
    }
    return null;
  };

  const versions = versionsOf(raw.versions);
  const removed = row.status === 'removed';

  return (
    <div className="tabbody">
      <ErrBox error={err} />
      <div className="kv-grid">
        <span className="k">status</span>
        <span className="v"><StateLabel state={row.status} />{row.description && <span className="muted small"> {row.description}</span>}</span>
        {row.draining && <><span className="k">draining</span><span className="v warn">since <Ago iso={row.drainStartedAt} /></span></>}
        {row.identityForked && <><span className="k">identity</span><span className="v bad">forked: two holders of the node's key were seen; re-join it with a pinned token</span></>}
        {stopping(row) && <><span className="k">feosd restarting</span><span className="v">until {fmtDateTime(row.stoppingUntil)}</span></>}
        {removed && <><span className="k">removed</span><span className="v">{fmtDateTime(row.removedAt)} <Ago iso={row.removedAt} /></span></>}
        <span className="k">link</span>
        <span className="v">{row.connected ? `connected to this instance (${row.connections} connection${row.connections === 1 ? '' : 's'})` : 'not connected to this instance'}</span>
        <span className="k">hostname</span><span className="v mono">{raw.hostname || '—'}</span>
        <span className="k">CPU</span><span className="v">{row.cpus} cores · {raw.cpuModelname || '—'} · {raw.arch || '—'}</span>
        <span className="k">memory</span><span className="v mono">{fmtBytes(row.memAvail)} available of {fmtBytes(row.memTotal)}</span>
        <span className="k">disk</span><span className="v mono">{fmtBytes(row.diskAvail)} free of {fmtBytes(raw.diskTotalbytes)}</span>
        <span className="k">load</span><span className="v mono">{row.load1.toFixed(2)}</span>
        <span className="k">pods</span><span className="v">{row.running} running, {row.paused} paused</span>
        <span className="k">booked</span>
        <span className="v mono">
          {row.booked} sandboxes · {row.vcpusBooked} / {vcpuCapacityText(row.vcpusCapacity)} vCPU · {fmtBytes(row.memBooked)} / {fmtBytes(row.memCapacity)}
        </span>
        <span className="k">object store</span>
        <span className="v">{row.objectStore ? <span className="mono">{row.objectStoreId || 'yes'}</span> : <span className="muted">none: its sandboxes cannot hibernate</span>}</span>
        <span className="k">pod prefix</span><span className="v mono">{raw.podPrefix || '—'}</span>
        <span className="k">inventory</span><span className="v mono muted">{row.inventoryHash || '—'}</span>
        <span className="k">feosd start</span>
        <span className="v mono">{raw.startId || '—'}{raw.startId && <CopyBtn text={raw.startId} mini />}</span>
        <span className="k">versions</span>
        <span className="v mono small">{versions.length ? versions.map(([k, v]) => <div key={k}>{k} {v}</div>) : '—'}</span>
        <span className="k">certificate</span>
        <span className="v mono small">
          {row.certSerial || '—'}
          {row.certNotAfter && <div className="muted">valid until {fmtDateTime(row.certNotAfter)} (<Ago iso={row.certNotAfter} />)</div>}
        </span>
        <span className="k">joined</span><span className="v">{fmtDateTime(nsToIso(raw.registeredAtNs))}</span>
        <span className="k">last seen</span><span className="v">{fmtDateTime(nsToIso(raw.lastSeenAtNs))} <Ago iso={nsToIso(raw.lastSeenAtNs)} /></span>
        {Number(raw.downSinceNs) > 0 && <><span className="k">down since</span><span className="v bad">{fmtDateTime(nsToIso(raw.downSinceNs))}</span></>}
      </div>

      {!removed && <Settings row={row} act={act} busy={busy} />}

      <div className="actionrow">
        {canDrain(row) && (
          <ArmButton armKey="drain" armed={armed} arm={arm} disarm={disarm} disabled={busy}
            confirmLabel={row.draining ? 'really undrain?' : 'really drain?'}
            onConfirm={() => act(() => (row.draining ? api.nodeUndrain(row.node) : api.nodeDrain(row.node)))}>
            {row.draining ? 'Undrain' : 'Drain'}
          </ArmButton>
        )}
        <span className="spacer" />
        {!removed && !needsForce && (
          <ArmButton armKey="remove" armed={armed} arm={arm} disarm={disarm} icon="trash" disabled={busy}
            title="revoke the node's certificates and close its connections; it must join again to come back"
            confirmLabel="really remove?"
            onConfirm={async () => {
              const e = await act(() => api.removeNode(row.node, false));
              if (e && e.status === 409) setNeedsForce(true);
            }}>Remove</ArmButton>
        )}
        {!removed && needsForce && (
          <ArmButton armKey="force" armed={armed} arm={arm} disarm={disarm} icon="trash" disabled={busy}
            title="its sandboxes are marked lost at once and placed again elsewhere"
            confirmLabel="remove with its sandboxes?"
            onConfirm={() => act(() => api.removeNode(row.node, true))}>Remove anyway</ArmButton>
        )}
      </div>
    </div>
  );
}

// Settings edits the node's capacity settings: slots, the memory placement
// leaves free, and the CPU overcommit (0: CPU not gated). The form keeps
// the values it was filled from (base); a save sends the fields that
// differ from them.
function Settings({ row, act, busy }) {
  const [base, setBase] = useState(() => settingsForm(row));
  const [form, setForm] = useState(base);
  const [msg, setMsg] = useState('');
  const [dirty, setDirty] = useState(false);
  // The polls bring new values; an untouched form follows them, and a
  // saved or reset one takes the node's current values.
  useEffect(() => {
    if (dirty) return;
    const f = settingsForm(row);
    setBase(f);
    setForm(f);
  }, [dirty, row.slots, row.memHeadroom, row.cpuOvercommit]);
  const set = (k) => (e) => { setDirty(true); setForm({ ...form, [k]: e.target.value }); };

  return (
    <form className="settings" onSubmit={async (e) => {
      e.preventDefault();
      setMsg('');
      let patch;
      try {
        patch = settingsPatch(base, form);
      } catch (e2) {
        setMsg(e2.message);
        return;
      }
      if (!Object.keys(patch).length) { setMsg('nothing changed'); setDirty(false); return; }
      const failed = await act(() => api.patchNode(row.node, patch));
      if (!failed) { setDirty(false); setMsg('saved'); }
    }}>
      <h3 className="sect"><Icon name="settings" size={13} /> Capacity</h3>
      <div className="settings-grid">
        <label>slots<input className="num" value={form.slots} onChange={set('slots')} inputMode="numeric" /></label>
        <label>memory headroom (MiB)<input className="num" value={form.memHeadroomMiB} onChange={set('memHeadroomMiB')} inputMode="numeric" /></label>
        <label>CPU overcommit<input className="num" value={form.cpuOvercommit} onChange={set('cpuOvercommit')} inputMode="decimal" /></label>
        <button type="submit" className="primary" disabled={busy}>Save</button>
        {dirty && <button type="button" className="ghost" onClick={() => { setDirty(false); setMsg(''); }}>Reset</button>}
        {msg && <span className="muted small">{msg}</span>}
      </div>
      <p className="muted small">
        Placement books at most <code>slots</code> sandboxes, floor(cores × CPU overcommit) vCPUs (0: CPU not
        gated) and the memory less the headroom.
      </p>
    </form>
  );
}

function NodeCerts({ node, current }) {
  const [certs, setCerts] = useState(null);
  const [err, setErr] = useState('');
  useEffect(() => {
    let alive = true;
    api.nodeCerts(node)
      .then((res) => { if (alive) { setCerts(res.certs || []); setErr(''); } })
      .catch((e) => { if (alive) setErr(e.message); });
    return () => { alive = false; };
  }, [node, current]);

  return (
    <div className="tabbody">
      <ErrBox error={err} />
      <p className="muted small">The node's certificates, newest first: each renewal names the one it replaced.</p>
      <div className="tblwrap">
        <table className="tbl">
          <thead>
            <tr><th>serial</th><th>kind</th><th>issued</th><th>valid until</th><th>first seen</th><th>replaces</th><th>peer</th><th>state</th></tr>
          </thead>
          <tbody>
            {(certs || []).map((c) => {
              const st = certState(c);
              return (
                <tr key={c.serial} className={c.serial === current ? 'sel' : ''}>
                  <td className="mono small" title={c.serial}>
                    {c.serial.slice(0, 12)}…{c.serial === current && <span className="badge ok">current</span>}
                  </td>
                  <td className="small">{c.kind}</td>
                  <td className="small">{fmtDateTime(c.issuedAt)}</td>
                  <td className="small">{fmtDateTime(c.notAfter)}</td>
                  <td className="small">{c.firstSeenAt ? <Ago iso={c.firstSeenAt} /> : <span className="muted">never used</span>}</td>
                  <td className="mono small muted" title={c.previousSerial}>{c.previousSerial ? c.previousSerial.slice(0, 12) + '…' : '—'}</td>
                  <td className="mono small muted">{c.peer || '—'}</td>
                  <td className="small">
                    {st === 'revoked' && <span className="bad" title={fmtDateTime(c.revokedAt)}>revoked: {c.revokedReason || '—'}</span>}
                    {st === 'expired' && <span className="muted">expired</span>}
                    {st === 'expiring' && <span className="warn">expires <Ago iso={c.notAfter} /></span>}
                    {st === 'valid' && <span className="ok">valid</span>}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {certs && certs.length === 0 && <p className="empty">No certificates.</p>}
      {!certs && !err && <p className="empty">Loading…</p>}
    </div>
  );
}
