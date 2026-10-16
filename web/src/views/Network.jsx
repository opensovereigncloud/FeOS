// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useMemo, useState } from 'react';
import { api } from '../api.js';
import { ErrBox, LiveBadge, usePoll } from '../components.jsx';
import { fmtDurMs, fmtMsTime, fmtNs, nsToDate } from '../format.js';
import { endpoint, fmtCount, networkModel, protocolName, tupleText } from '../traffic.js';

// The network of one sandbox: its pod's traffic records (ADR 0013) as
// feosd records them — flows, the DNS queries the pod's resolver
// answered, the server names of its connections (TLS SNI, HTTP Host), and
// the packets its security groups dropped. Read from the activity route
// with source=traffic; the same records are in the Activity tab's network
// lane.

export function Network({ sandboxId }) {
  const [events, setEvents] = useState(null);
  const [err, setErr] = useState('');

  const refresh = useCallback(async () => {
    try {
      const res = await api.listActivity(sandboxId, { source: 'traffic' });
      setEvents(res.events || []);
      setErr('');
    } catch (e) {
      setErr(e.message);
    }
  }, [sandboxId]);
  const lastOk = usePoll(refresh, 3000);

  const model = useMemo(() => networkModel(events || []), [events]);

  if (!events && !err) return <p className="empty">Loading the traffic records…</p>;
  const { flows, dns, names, drops, missed, openFlows } = model;

  return (
    <div className="network">
      <div className="access-head">
        <span className="muted small">The newest records of the pod, as its node recorded them.</span>
        <span className="spacer" />
        <LiveBadge lastOk={lastOk} />
      </div>
      <ErrBox error={err} />
      <div className="kv-grid">
        <span className="k">flows</span>
        <span className="v">{flows.length} ({openFlows} open)</span>
        <span className="k">DNS</span>
        <span className="v">{dns.length} queries ({dns.filter((d) => d.failed).length} failed)</span>
        <span className="k">server names</span>
        <span className="v">{names.length}</span>
        <span className="k">drops</span>
        <span className={'v' + (drops.length ? ' warntext' : '')}>{drops.length} packets</span>
        <span className="k">missed</span>
        <span className={'v' + (missed ? ' warntext' : '')}>
          {missed} records{missed ? ' lost by the kernel or a slow reader on the node' : ''}
        </span>
      </div>

      <Sect title="Flows" n={flows.length}>
        <table className="tbl">
          <thead><tr><th>started</th><th>connection</th><th>dir</th><th>sent / received</th><th>state</th></tr></thead>
          <tbody>
            {[...flows].reverse().map((f) => (
              <tr key={f.key}>
                <td className="mono" title={f.startMs != null ? new Date(f.startMs).toISOString() : ''}>
                  {f.startMs != null ? fmtMsTime(f.startMs) : '—'}
                </td>
                <td className="mono">{tupleText(f.tuple)}</td>
                <td className="muted">{f.outbound ? 'out' : 'in'}</td>
                <td className="mono muted">
                  {f.ended
                    ? `${fmtCount(f.bytesSent)} / ${fmtCount(f.bytesReceived)} (${f.packetsSent}/${f.packetsReceived} packets)`
                    : ''}
                </td>
                <td>{f.ended
                  ? <span className="muted">ended{f.durationMs != null ? ` after ${fmtDurMs(f.durationMs)}` : ''}</span>
                  : <span className="ok">open</span>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </Sect>

      <Sect title="DNS" n={dns.length}>
        <table className="tbl">
          <thead><tr><th>time</th><th>name</th><th>type</th><th>answer</th><th>addresses</th></tr></thead>
          <tbody>
            {[...dns].reverse().map((d) => (
              <tr key={d.ev.eventId}>
                <td className="mono" title={nsToDate(d.ev.tsNs)}>{fmtNs(d.ev.tsNs)}</td>
                <td className="mono">{d.name}</td>
                <td className="muted mono">{d.type}</td>
                <td className={'mono' + (d.failed ? ' errtext' : ' muted')}>{d.rcode}</td>
                <td className="mono muted small">
                  {d.addresses.slice(0, 4).join(', ')}{d.addresses.length > 4 ? ` +${d.addresses.length - 4}` : ''}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </Sect>

      <Sect title="Server names" n={names.length}>
        <table className="tbl">
          <thead><tr><th>time</th><th>name</th><th>from</th><th>server</th></tr></thead>
          <tbody>
            {[...names].reverse().map((s) => (
              <tr key={s.ev.eventId}>
                <td className="mono" title={nsToDate(s.ev.tsNs)}>{fmtNs(s.ev.tsNs)}</td>
                <td className="mono">{s.name}</td>
                <td className="muted">{s.source || '—'}</td>
                <td className="mono muted">
                  {protocolName(s.tuple.protocol)} {endpoint(s.tuple.destination_address, s.tuple.destination_port)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </Sect>

      <Sect title="Drops" n={drops.length}>
        <table className="tbl">
          <thead><tr><th>time</th><th>packet</th><th>dir</th><th>reason</th></tr></thead>
          <tbody>
            {[...drops].reverse().map((d) => (
              <tr key={d.ev.eventId}>
                <td className="mono" title={nsToDate(d.ev.tsNs)}>{fmtNs(d.ev.tsNs)}</td>
                <td className="mono">{tupleText(d.tuple)}</td>
                <td className="muted">{d.outbound ? 'out' : 'in'}</td>
                <td className="warntext">{d.reason}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </Sect>
    </div>
  );
}

function Sect({ title, n, children }) {
  return (
    <div className="netsect">
      <h3>{title} <span className="muted small">{n}</span></h3>
      {n ? children : <p className="empty">None in this page.</p>}
    </div>
  );
}
