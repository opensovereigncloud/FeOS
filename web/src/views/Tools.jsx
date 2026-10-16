// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import React, { useEffect, useMemo, useState } from 'react';
import { api } from '../api.js';
import { Icon } from '../Icon.jsx';
import { ErrBox } from '../components.jsx';
import { fmtDurMs, prettyJson } from '../format.js';

// The tools a sandbox offers its agents, over MCP and REST alike: the
// list on the left, a tool's description and input schema on the right.

export function Tools() {
  const [tools, setTools] = useState(null);
  const [waitMs, setWaitMs] = useState(0);
  const [err, setErr] = useState('');
  const [sel, setSel] = useState('');
  const [q, setQ] = useState('');

  useEffect(() => {
    api.listTools()
      .then((res) => {
        const list = res.tools || [];
        setTools(list);
        setWaitMs(Number(res.preparationWaitMs || 0));
        if (list.length) setSel((cur) => cur || list[0].name);
      })
      .catch((e) => setErr(e.message));
  }, []);

  const list = useMemo(() => {
    let out = tools || [];
    if (q.trim()) {
      const n = q.trim().toLowerCase();
      out = out.filter((t) => t.name.toLowerCase().includes(n) || (t.description || '').toLowerCase().includes(n));
    }
    return out;
  }, [tools, q]);

  const cur = (tools || []).find((t) => t.name === sel);

  return (
    <div className="split">
      <section className="pane pane-list">
        <div className="pane-toolbar">
          <h2><Icon name="terminal" size={15} /> Tools <span className="count">{tools ? tools.length : '…'}</span></h2>
          {waitMs > 0 && (
            <span className="muted small" title="the longest a tool call waits for its sandbox to be ready">
              preparation up to {fmtDurMs(waitMs)}
            </span>
          )}
        </div>
        <ErrBox error={err} />
        <div className="filterbar slim">
          <span className="searchwrap grow">
            <Icon name="search" size={13} />
            <input className="searchbare" placeholder="search the tools…" value={q} onChange={(e) => setQ(e.target.value)} />
            {q && <button className="ghost mini icon-only" onClick={() => setQ('')}><Icon name="x" size={12} /></button>}
          </span>
        </div>
        <div className="toollist">
          {list.map((t) => (
            <button key={t.name} className={'toolrow' + (sel === t.name ? ' sel' : '')} onClick={() => setSel(t.name)}>
              <span className="mono">{t.name}</span>
              <span className="muted trunc">{(t.description || '').slice(0, 72)}{(t.description || '').length > 72 ? '…' : ''}</span>
            </button>
          ))}
          {tools && list.length === 0 && <p className="empty">Nothing found.</p>}
          {!tools && !err && <p className="empty">Loading…</p>}
        </div>
      </section>

      <section className="pane pane-detail">
        {cur ? (
          <>
            <div className="detail-head">
              <Icon name="terminal" size={16} />
              <div className="detail-title"><span className="mono uuid">{cur.name}</span></div>
            </div>
            <div className="tabbody">
              <p className="tooldesc">{cur.description}</p>
              <h3 className="sect">input schema</h3>
              <pre className="schemapre">{prettyJson(cur.inputSchema)}</pre>
            </div>
          </>
        ) : <p className="empty">Select a tool on the left.</p>}
      </section>
    </div>
  );
}
