// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ArmButton, ErrBox, LiveBadge, StateDot, useArm, useNow, usePoll } from '../components.jsx';
import { fmtNs, nsToDate } from '../format.js';
import { canDrain, fleetDefaults, fmtBytes, fmtBytesOf, memBar, nodeRows, stopping, vcpuCapacityText } from './fleet.js';

// The nodes: one row per node that joined, with its telemetry, what
// placement booked on it, its connection to this instance and its
// certificate. Refreshed every 5 s. A row opens the node.

export function Nodes({ navigate }) {
  const [nodes, setNodes] = useState(null);
  const [defaults, setDefaults] = useState(null);
  const [err, setErr] = useState('');
  const [lastOk, setLastOk] = useState(0);
  const [armed, arm, disarm] = useArm();
  const now = useNow(5000);

  const load = useCallback(async () => {
    try {
      const res = await api.listNodes();
      setNodes(res.nodes || []);
      setDefaults(res.defaults || null);
      setErr('');
      setLastOk(Date.now());
    } catch (e) {
      setErr(e.message);
    }
  }, []);
  usePoll(load, 5000);

  // A drain keeps placement off the node and moves its sandboxes into the
  // object store; an undrain ends it. The leader does the work: the row's
  // description shows the progress on the next polls.
  const drainAct = async (r) => {
    try {
      if (r.draining) await api.nodeUndrain(r.node);
      else await api.nodeDrain(r.node);
    } catch (e) {
      setErr(e.message);
    }
    await load();
  };

  const rows = nodeRows(nodes);
  const counts = {
    ready: rows.filter((r) => r.status === 'ready').length,
    down: rows.filter((r) => r.status === 'down').length,
    draining: rows.filter((r) => r.draining).length,
  };
  const open = (r) => navigate(`/nodes/${encodeURIComponent(r.node)}`);

  return (
    <div className="single">
      <section className="pane">
        <div className="pane-toolbar">
          <h2>
            <Icon name="server" size={15} /> Nodes
            <span className="count">{nodes ? rows.length : '…'}</span>
          </h2>
          <span className="mini-stats muted">
            <span className="ok" title="ready">● {counts.ready}</span>
            {counts.down > 0 && <span className="bad" title="down">● {counts.down}</span>}
            {counts.draining > 0 && <span className="warn" title="draining">▣ {counts.draining}</span>}
          </span>
          <span className="spacer" />
          <LiveBadge lastOk={lastOk} />
          <button className="ghost mini" onClick={() => navigate('/join-tokens')}>
            <Icon name="ticket" size={13} /> join a node
          </button>
        </div>
        <ErrBox error={err} />
        <NodesTable rows={rows} now={now} armed={armed} arm={arm} disarm={disarm} onDrain={drainAct} onOpen={open} />
        {nodes && rows.length === 0 && (
          <p className="empty">
            No node has joined yet. A node joins with a join token:{' '}
            <a href="/join-tokens" onClick={(e) => { e.preventDefault(); navigate('/join-tokens'); }}>make one →</a>
          </p>
        )}
        {!nodes && !err && <p className="empty">Loading…</p>}
        <NodesFootnote defaults={defaults} />
      </section>
    </div>
  );
}

// NodesTable is the nodes' table: rows from nodeRows, now in ms for the
// restart notes, the arm state of the drain buttons, and what a drain and
// a click on a row do. It fits a 1280 px window and scrolls sideways in
// a narrower one.
export function NodesTable({ rows, now, armed, arm, disarm, onDrain, onOpen }) {
  return (
    <div className="tblwrap"><table className="tbl nodes-tbl">
      <thead>
        <tr>
          <th className="col-state" title="status" />
          <th>node</th>
          <th title="the node's last telemetry; connected to the instance that answered, and its node connections there">seen</th>
          <th className="col-bar" title="the host's memory in use, of its total">memory</th>
          <th>load1</th>
          <th>disk free</th>
          <th>running / paused</th>
          <th title="what placement booked on the node, against what it may book: vCPUs, memory, sandboxes against slots">booked</th>
          <th>certificate</th>
          <th className="col-quick" />
        </tr>
      </thead>
      <tbody>
        {rows.map((r) => {
          const mem = memBar(r);
          return (
            <tr key={r.key} className={'clickable ' + r.cls} onClick={() => onOpen(r)}>
              <td className="col-state"><StateDot state={r.status} title={r.status} /></td>
              <td className="col-node">
                <span className="mono strong">{r.node}</span> <span className="muted small">{r.status}</span>
                {r.hostname && r.hostname !== r.node && <div className="muted small mono">{r.hostname}</div>}
                {r.draining && <div className="warn small">draining: no new sandboxes</div>}
                {r.identityForked && <div className="bad small" title="two holders of the node's identity were seen">identity forked</div>}
                {stopping(r, now) && <div className="muted small">feosd restarting</div>}
                {r.description && <div className="muted small">{r.description}</div>}
              </td>
              <td className="small nowrap">
                <div className="muted" title={nsToDate(r.lastSeenNs)}>{r.lastSeenNs ? fmtNs(r.lastSeenNs) : '—'}</div>
                {r.connected
                  ? <div className="ok" title={`connected to this instance, ${r.connections} node connection${r.connections === 1 ? '' : 's'}`}>connected{r.connections > 1 ? ` ×${r.connections}` : ''}</div>
                  : r.status === 'down' || r.status === 'removed'
                    ? <div className="muted">not connected</div>
                    : <div className="muted" title="not connected to this instance; it may be to another">—</div>}
              </td>
              <td className="col-bar">
                <div className="membar" title={mem.title}>
                  <div className="membar-fill" style={{ width: r.memPct + '%' }} />
                  <span className="membar-label mono">{mem.label}</span>
                </div>
              </td>
              <td className="mono nowrap">{r.load1.toFixed(2)}</td>
              <td className="mono nowrap">{fmtBytes(r.diskAvail)}</td>
              <td className="mono nowrap">{r.running} / {r.paused}</td>
              <td className="mono small nowrap">
                <div title={r.vcpusCapacity ? `${r.cpus} cores × ${r.cpuOvercommit}` : 'CPU not gated'}>
                  {r.vcpusBooked} / {vcpuCapacityText(r.vcpusCapacity)} vCPU
                </div>
                <div title={`total less ${fmtBytes(r.memHeadroom)} headroom`}>{fmtBytesOf(r.memBooked, r.memCapacity)}</div>
                <div title="sandboxes booked, against the node's slots">{r.booked} / {r.slots} slots</div>
              </td>
              <td className="small nowrap">
                {r.certSerial
                  ? <><span className="mono muted" title={r.certSerial}>{r.certSerial.slice(0, 8)}</span>
                    {r.status === 'removed'
                      ? <div className="muted">revoked</div>
                      : r.certNotAfter && <div>expires <Ago iso={r.certNotAfter} /></div>}</>
                  : <span className="muted">—</span>}
              </td>
              <td className="col-quick" onClick={(e) => e.stopPropagation()}>
                {canDrain(r) && (
                  <ArmButton armKey={'drain:' + r.node} armed={armed} arm={arm} disarm={disarm}
                    className="mini" onConfirm={() => onDrain(r)}
                    confirmLabel={r.draining ? 'undrain?' : 'drain?'}>
                    {r.draining ? 'undrain' : 'drain'}
                  </ArmButton>
                )}
                <button className="iconbtn mini" title="open the node" onClick={() => onOpen(r)}>
                  <Icon name="chevronRight" size={13} /></button>
              </td>
            </tr>
          );
        })}
      </tbody>
    </table></div>
  );
}

// Flag names one of ferund's flags with this instance's value, when the
// API told it.
function Flag({ name, value }) {
  return <><code>{name}</code>{value ? ` (${value})` : ''}</>;
}

// NodesFootnote explains the statuses, the bookings and a drain with the
// timings of the instance that answered (GET /api/v1/admin/nodes's
// defaults), not with built-in numbers.
export function NodesFootnote({ defaults }) {
  const fd = fleetDefaults(defaults);
  return (
    <p className="muted small footnote">
      A node is <code>joined</code> from its join until its first telemetry, then <code>ready</code>; it reports
      telemetry{fd.telemetryInterval ? ` every ${fd.telemetryInterval}` : ''} and goes <code>down</code> when its
      telemetry stops for longer than ferund's <Flag name="--node-ttl" value={fd.nodeTtl} />; once it has been
      down for <Flag name="--lost-grace" value={fd.lostGrace} />, its sandboxes with a checkpoint
      become <code>hibernated</code> and are restored elsewhere on their next use, those without one
      count as <code>lost</code>. It is{' '}
      <code>removed</code> when an operator removes it. Memory is the host's. <code>booked</code> is what
      placement promised the node's running and starting sandboxes, against floor(cores × CPU overcommit)
      vCPUs (∞: CPU not gated) and memory less the headroom; a paused sandbox books nothing. A drain
      hibernates every sandbox of the node into the object store and keeps placement off it; execs that still
      run after <Flag name="--drain-timeout" value={fd.drainTimeout} /> are cancelled. Drain, wait for the node
      to be empty, act, undrain.
    </p>
  );
}
