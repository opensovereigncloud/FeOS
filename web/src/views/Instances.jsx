// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { Ago, ErrBox, LiveBadge, StateDot, useNow, usePoll } from '../components.jsx';
import { fmtDateTime } from '../format.js';

// The control plane: its instances (every ferund that serves, or served
// recently) and which of them leads, running the loops that only one may
// run.

// instanceState is an instance's dot: running while it is live; stopping
// while it is stopping but still writes its row, for it serves its
// requests and ops until its last step; gone once its row aged past the
// TTL. The database decides fresh and live, never the browser's clock.
export function instanceState(i) {
  if (i.live) return { state: 'running', title: 'live' };
  if (i.fresh) return { state: 'paused', title: 'stopping' };
  return { state: 'absent', title: 'gone' };
}

// InstancesTable lists the instances; a row is dimmed once the instance is
// gone.
export function InstancesTable({ instances, leader }) {
  return (
    <div className="tblwrap">
      <table className="tbl">
        <thead>
          <tr>
            <th className="col-state">live</th><th>instance</th><th>address</th><th>nodes' address</th>
            <th>version</th><th>started</th><th>last seen</th><th>stopping</th>
          </tr>
        </thead>
        <tbody>
          {instances.map((i) => {
            const st = instanceState(i);
            return (
              <tr key={i.id} className={st.state === 'absent' ? 'dim' : ''}>
                <td className="col-state"><StateDot state={st.state} title={st.title} /></td>
                <td className="mono">
                  {i.id}
                  {leader && leader.id === i.id && <span className="badge acc"><Icon name="crown" size={11} /> leader</span>}
                </td>
                <td className="mono small">{i.address || '—'}</td>
                <td className="mono small">{i.nodeAddress || <span className="muted" title="not listed to nodes">—</span>}</td>
                <td className="mono small">{i.version || '—'}</td>
                <td className="small">{fmtDateTime(i.startedAt)}</td>
                <td className="small"><Ago iso={i.lastSeen} /></td>
                <td className="small">{i.stopping ? <span className="warn">since <Ago iso={i.stopping} /></span> : <span className="muted">—</span>}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

export function Instances() {
  const [data, setData] = useState(null);
  const [err, setErr] = useState('');
  useNow(2000);

  const refresh = useCallback(async () => {
    try {
      setData(await api.listInstances());
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, []);
  const lastOk = usePoll(refresh, 5000);

  const leader = data && data.leader;
  const instances = (data && data.instances) || [];

  return (
    <div className="single">
      <section className="pane">
        <div className="pane-toolbar">
          <h2><Icon name="layers" size={15} /> Instances <span className="count">{data ? instances.length : '…'}</span></h2>
          <span className="spacer" />
          <LiveBadge lastOk={lastOk} />
        </div>
        <ErrBox error={err} />
        {data && (
          <div className="kv-grid">
            <span className="k">leader</span>
            <span className="v">
              {leader
                ? <><span className="mono strong">{leader.id}</span> <span className="muted">epoch {leader.epoch}, since {fmtDateTime(leader.since)} (<Ago iso={leader.since} />)</span></>
                : <span className="warn">none: no instance holds the lease</span>}
            </span>
          </div>
        )}
        <InstancesTable instances={instances} leader={leader} />
        {data && instances.length === 0 && <p className="empty">No instances.</p>}
        {!data && !err && <p className="empty">Loading…</p>}
        <p className="muted small footnote">
          An instance writes its row every few seconds and counts as live while the row is fresh and it is not
          stopping. A stopping instance finishes its requests and ops, and stays listed to the nodes until it
          closes their connections; it is gone once its row is older than 15 s.
        </p>
      </section>
    </div>
  );
}
