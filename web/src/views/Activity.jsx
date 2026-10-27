// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { usePoll } from '../components.jsx';
import { Icon } from '../Icon.jsx';
import { newest } from '../poll.js';
import { Timeline } from './Timeline.jsx';
import { eventDescription, eventTitle, keyOf, laneOf, lanesOf, levelOf } from '../events.jsx';
import { fmtNs, nsToDate, prettyJson } from '../format.js';

// The activity of a sandbox (or of a node): the waterfall timeline and
// the raw events. The table shows every event one to one, newest first;
// the timeline's spans are drawn from the same events, and a selection is
// a raw event. load(source) reads one page.

// The producers ferund's activity routes filter by.
export const SOURCES = [
  { key: '', label: 'all sources' },
  { key: 'ferund', label: 'ferund' },
  { key: 'feosd', label: 'feosd' },
  { key: 'authn', label: 'feos-authn' },
  { key: 'traffic', label: 'traffic' },
];

// The filter chips: all, the lanes of the page, and errors.
function filters(events) {
  return [{ key: 'all', label: 'all' }, ...lanesOf(events), { key: 'errors', label: 'errors' }];
}

export function Activity({ load, pollMs = 3000 }) {
  const [events, setEvents] = useState(null);
  const [err, setErr] = useState('');
  const [source, setSource] = useState('');
  const [laneFilter, setLaneFilter] = useState('all');
  const [q, setQ] = useState('');
  const [selKey, setSelKey] = useState(null); // the selected event's key survives the polls
  const sel = selKey ? (events || []).find((ev) => keyOf(ev) === selKey) || null : null;
  const tableRef = useRef(null);
  // A selection scrolls its table row into view; a poll does not.
  useEffect(() => {
    if (!selKey || !tableRef.current) return;
    const row = tableRef.current.querySelector(`[data-evkey="${CSS.escape(selKey)}"]`);
    row?.scrollIntoView({ block: 'center', behavior: 'smooth' });
  }, [selKey]);

  // A change of source starts a request at once while one for the old
  // source may still be under way; only the newest request's answer is
  // shown.
  const [request] = useState(() => newest());
  const refresh = useCallback(async () => {
    const current = request();
    try {
      const res = await load(source);
      if (!current()) return;
      setEvents(res.events || []);
      setErr('');
    } catch (e) {
      if (current()) setErr(e.message);
    }
  }, [load, source, request]);
  usePoll(refresh, pollMs);

  const chipdefs = useMemo(() => filters(events), [events]);

  const counts = useMemo(() => {
    const c = { all: 0, errors: 0 };
    for (const ev of events || []) {
      c.all++;
      const lane = laneOf(ev);
      c[lane] = (c[lane] || 0) + 1;
      if (levelOf(ev) === 'error') c.errors++;
    }
    return c;
  }, [events]);

  const filtered = useMemo(() => {
    let out = events || [];
    if (laneFilter === 'errors') out = out.filter((ev) => levelOf(ev) === 'error');
    else if (laneFilter !== 'all') out = out.filter((ev) => laneOf(ev) === laneFilter);
    if (q.trim()) {
      const needle = q.trim().toLowerCase();
      out = out.filter((ev) =>
        (ev.kind || '').toLowerCase().includes(needle)
        || (ev.name || '').toLowerCase().includes(needle)
        || (ev.tool || '').toLowerCase().includes(needle)
        || (ev.output || '').toLowerCase().includes(needle)
        || (ev.paramsJson || '').toLowerCase().includes(needle)
        || (ev.detailJson || '').toLowerCase().includes(needle));
    }
    return out;
  }, [events, laneFilter, q]);

  if (!events && !err) return <p className="empty">Loading the activity…</p>;

  return (
    <div className="activity">
      <div className="filterbar">
        {chipdefs.map((f) => (
          <button key={f.key}
            className={'chipbtn' + (laneFilter === f.key ? ' on' : '') + (f.key === 'errors' && counts.errors > 0 ? ' haserr' : '')}
            onClick={() => setLaneFilter(f.key)}>
            {f.label}
            {counts[f.key] != null && <span className="chipcount">{counts[f.key]}</span>}
          </button>
        ))}
        <span className="vsep" />
        <select className="selectbare" value={source} title="the producer of the events"
          onChange={(e) => { setSource(e.target.value); setSelKey(null); }}>
          {SOURCES.map((s) => <option key={s.key} value={s.key}>{s.label}</option>)}
        </select>
        <span className="searchwrap">
          <Icon name="search" size={13} />
          <input className="searchbare" placeholder="name, tool, kind, output…" value={q}
            onChange={(e) => setQ(e.target.value)} />
          {q && <button className="ghost mini icon-only" onClick={() => setQ('')}><Icon name="x" size={12} /></button>}
        </span>
        <span className="spacer" />
        <button className="ghost mini" onClick={refresh}><Icon name="refresh" size={13} /> refresh</button>
      </div>

      {err && <p className="error">{err}</p>}

      {filtered.length > 0 && (
        <Timeline events={filtered} selected={sel}
          onSelect={(ev) => setSelKey(ev ? keyOf(ev) : null)} />
      )}

      <div className="evtable-wrap" ref={tableRef}>
        <table className="tbl evtable">
          <colgroup>
            <col className="w-time" /><col className="w-src" /><col className="w-kind" /><col />
          </colgroup>
          <thead>
            <tr>
              <th className="col-time">time</th>
              <th className="col-src">source</th>
              <th className="col-kind">event</th>
              <th>description</th>
            </tr>
          </thead>
          <tbody>
            {[...filtered].reverse().map((ev) => {
              const level = levelOf(ev);
              const isSel = selKey === keyOf(ev);
              return (
                <React.Fragment key={keyOf(ev)}>
                  <tr
                    data-evkey={keyOf(ev)}
                    className={(level !== 'info' ? level + ' ' : '') + (isSel ? 'sel' : '')}
                    title={nsToDate(ev.tsNs)}
                    onClick={() => setSelKey(isSel ? null : keyOf(ev))}>
                    <td className="mono col-time">{fmtNs(ev.tsNs)}</td>
                    <td className="muted mono col-src">{sourceText(ev)}</td>
                    <td className={'col-kind' + (level === 'error' ? ' errtext' : level === 'warn' ? ' warntext' : '')}>
                      <span className="kindlabel" title={eventTitle(ev)}>{eventTitle(ev)}</span>
                    </td>
                    <td className="evdesc">{eventDescription(ev)}</td>
                  </tr>
                  {isSel && (
                    <tr className="evdetail-row">
                      <td colSpan={4}><EventDetail ev={ev} /></td>
                    </tr>
                  )}
                </React.Fragment>
              );
            })}
          </tbody>
        </table>
        {filtered.length === 0 && <p className="empty">No events match this filter.</p>}
      </div>
    </div>
  );
}

// sourceText is the source column: the producer and, for a node's
// records, the node.
export function sourceText(ev) {
  const src = ev.source || 'feosd';
  return ev.node ? `${src}@${ev.node}` : src;
}

// EventDetail is the expansion below a clicked row: the raw event.
function EventDetail({ ev }) {
  return (
    <div className="evdetail-body" onClick={(e) => e.stopPropagation()}>
      {ev.paramsJson && <><h4>params</h4><pre>{prettyJson(ev.paramsJson)}</pre></>}
      {ev.output && <><h4>output{ev.outputTruncated ? ' (truncated)' : ''}</h4><pre>{ev.output}</pre></>}
      {ev.detailJson && ev.detailJson !== '{}' && <><h4>detail</h4><pre>{prettyJson(ev.detailJson)}</pre></>}
      <details>
        <summary className="muted small">the raw event as JSON</summary>
        <pre>{JSON.stringify(ev, null, 2)}</pre>
      </details>
      <p className="muted small mono">
        {ev.node ? `node ${ev.node}` : 'ferund'} · source {ev.source || 'feosd'} · kind {ev.kind}
        {ev.eventId && <> · <IdChip label="event" id={ev.eventId} /></>}
        {ev.podId && <> · <IdChip label="pod" id={ev.podId} /></>}
        {ev.requestId && <> · <IdChip label="req" id={ev.requestId} /></>}
        {ev.spanId && <> · <IdChip label="span" id={ev.spanId} /></>}
        {ev.parentSpanId && <> · <IdChip label="parent" id={ev.parentSpanId} /></>}
      </p>
    </div>
  );
}

// IdChip is an ID, verbatim; a click copies it.
function IdChip({ label, id }) {
  const [copied, setCopied] = useState(false);
  return (
    <span className="muted mono small reqid" title={`${label} ID: click to copy`}
      onClick={() => {
        if (!navigator.clipboard) return;
        navigator.clipboard.writeText(id).then(() => {
          setCopied(true);
          setTimeout(() => setCopied(false), 1200);
        }, () => {});
      }}>
      {copied ? 'copied' : `${label} ${id}`}
    </span>
  );
}
