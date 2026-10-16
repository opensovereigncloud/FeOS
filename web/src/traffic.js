// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Traffic records (ADR 0013): what a pod did on the network, as feosd
// records them and ferund stores them. The activity routes return them with
// source=traffic as events whose kind is the record's kind (flow, drop, dns,
// server_name) and whose detailJson is the whole feos.network.v1.
// TrafficRecord as protojson with the proto's field names:
//
//   {"time": "<RFC 3339>", "pod_id": "…", "missed": "3",
//    "flow": {"tuple": {"protocol": 6, "source_address": "…", "source_port": 40000,
//             "destination_address": "…", "destination_port": 443},
//             "outbound": true, "start": "…", "end": "…", "ended": true,
//             "bytes_sent": "512", "bytes_received": "2048",
//             "packets_sent": "4", "packets_received": "5"}}
//   {"drop": {"tuple": {…}, "outbound": true, "reason": "DROP_REASON_NO_RULE"}}
//   {"dns": {"name": "example.com", "type": 1, "rcode": 0, "addresses": ["…"]}}
//   {"server_name": {"tuple": {…}, "source": "SERVER_NAME_SOURCE_TLS", "name": "…"}}
//
// protojson leaves out fields at their zero value (rcode 0, outbound false,
// ended false, a port 0), and writes 64-bit counters as strings. This
// module is pure, so that the tests load it without a browser.

export const TRAFFIC_KINDS = ['flow', 'drop', 'dns', 'server_name'];

export function isTraffic(ev) {
  return ev && ev.source === 'traffic' && TRAFFIC_KINDS.includes(ev.kind);
}

// recordOf is the TrafficRecord of an event, {} when it does not parse.
export function recordOf(ev) {
  try {
    const r = ev && ev.detailJson ? JSON.parse(ev.detailJson) : {};
    return r && typeof r === 'object' ? r : {};
  } catch {
    return {};
  }
}

// bodyOf is the record's body: the member of its oneof that its kind
// names.
export function bodyOf(ev) {
  const b = recordOf(ev)[ev.kind];
  return b && typeof b === 'object' ? b : {};
}

const PROTOCOLS = { 1: 'ICMP', 6: 'TCP', 17: 'UDP', 58: 'ICMPv6' };

export function protocolName(n) {
  const v = Number(n) || 0;
  return PROTOCOLS[v] || (v ? `proto ${v}` : 'IP');
}

// endpoint renders an address and port: [2001:db8::1]:443, 10.0.0.1:443,
// or the address alone without a port.
export function endpoint(address, port) {
  const a = address || '?';
  if (!Number(port)) return a;
  return a.includes(':') ? `[${a}]:${port}` : `${a}:${port}`;
}

// tupleText renders a tuple as "TCP [2001:db8::2]:40000 → [2606:4700::1]:443".
export function tupleText(t) {
  const tu = t || {};
  return `${protocolName(tu.protocol)} ${endpoint(tu.source_address, tu.source_port)} → ${endpoint(tu.destination_address, tu.destination_port)}`;
}

const tupleKey = (t) => {
  const tu = t || {};
  return [Number(tu.protocol) || 0, tu.source_address || '', Number(tu.source_port) || 0,
    tu.destination_address || '', Number(tu.destination_port) || 0].join('|');
};

export const DROP_REASONS = {
  DROP_REASON_UNSPECIFIED: 'unspecified',
  DROP_REASON_NO_RULE: 'no rule of its security groups allowed it',
  DROP_REASON_DENIED_PREFIX: 'its destination is in a denied prefix',
  DROP_REASON_SPOOFED: 'its source address was not the pod’s',
};

export function dropReason(r) {
  if (r === undefined || r === null || r === '' || r === 0) return DROP_REASONS.DROP_REASON_UNSPECIFIED;
  return DROP_REASONS[r] || String(r);
}

export const SERVER_NAME_SOURCES = {
  SERVER_NAME_SOURCE_UNSPECIFIED: '',
  SERVER_NAME_SOURCE_TLS: 'TLS',
  SERVER_NAME_SOURCE_HTTP: 'HTTP',
};

export function serverNameSource(s) {
  if (s === undefined || s === null || s === '' || s === 0) return '';
  return SERVER_NAME_SOURCES[s] ?? String(s);
}

const QTYPES = {
  1: 'A', 2: 'NS', 5: 'CNAME', 6: 'SOA', 12: 'PTR', 15: 'MX', 16: 'TXT', 28: 'AAAA',
  33: 'SRV', 64: 'SVCB', 65: 'HTTPS', 255: 'ANY', 257: 'CAA',
};
const RCODES = {
  0: 'NOERROR', 1: 'FORMERR', 2: 'SERVFAIL', 3: 'NXDOMAIN', 4: 'NOTIMP', 5: 'REFUSED',
};

export function qtypeName(t) {
  const v = Number(t) || 0;
  return QTYPES[v] || `TYPE${v}`;
}

export function rcodeName(r) {
  const v = Number(r) || 0;
  return RCODES[v] || `RCODE${v}`;
}

const msOfTime = (s) => {
  const v = s ? Date.parse(s) : NaN;
  return Number.isNaN(v) ? null : v;
};

// trafficLabel is the event column's label of a record.
export function trafficLabel(ev) {
  const b = bodyOf(ev);
  switch (ev.kind) {
    case 'flow': return b.ended ? 'flow ended' : 'flow began';
    case 'drop': return 'packet dropped';
    case 'dns': return 'DNS query';
    case 'server_name': {
      const src = serverNameSource(b.source);
      return src ? `server name (${src})` : 'server name';
    }
    default: return ev.kind;
  }
}

// trafficSummary is a record's one-line description.
export function trafficSummary(ev) {
  const b = bodyOf(ev);
  switch (ev.kind) {
    case 'flow': {
      const dir = b.outbound ? 'out' : 'in';
      let s = `${tupleText(b.tuple)} (${dir})`;
      if (b.ended) s += ` ↑${fmtCount(b.bytes_sent)} ↓${fmtCount(b.bytes_received)}`;
      return s;
    }
    case 'drop':
      return `${tupleText(b.tuple)}: ${dropReason(b.reason)}`;
    case 'dns': {
      const addrs = Array.isArray(b.addresses) ? b.addresses : [];
      const answer = addrs.length ? addrs.join(', ') : 'no addresses';
      return `${b.name || '?'} ${qtypeName(b.type)} ${rcodeName(b.rcode)}: ${answer}`;
    }
    case 'server_name':
      return `${b.name || '?'} → ${endpoint(b.tuple?.destination_address, b.tuple?.destination_port)}`;
    default:
      return '';
  }
}

// fmtCount renders a byte count (an int64 string or a number).
export function fmtCount(n) {
  const v = Number(n) || 0;
  if (v >= 2 ** 30) return (v / 2 ** 30).toFixed(1) + ' GiB';
  if (v >= 2 ** 20) return (v / 2 ** 20).toFixed(1) + ' MiB';
  if (v >= 2 ** 10) return (v / 2 ** 10).toFixed(1) + ' KiB';
  return v + ' B';
}

// flowRows pairs the records of each flow: feosd records a flow when it
// begins (ended false) and again when it ends (ended true), both with the
// same tuple and start. A flow whose beginning fell out of the page is
// shown from its end alone; one without an end yet is open.
export function flowRows(events) {
  const rows = [];
  const open = new Map();
  for (const ev of events || []) {
    if (ev.kind !== 'flow') continue;
    const b = bodyOf(ev);
    const key = `${tupleKey(b.tuple)}|${b.start || ''}`;
    const row = open.get(key);
    if (b.ended) {
      if (row && !row.ended) {
        row.ended = true;
        row.end = ev;
        row.body = b;
        open.delete(key);
      } else {
        rows.push({ key: `${key}|${ev.eventId}`, tuple: b.tuple, outbound: !!b.outbound, startMs: msOfTime(b.start),
          ended: true, begin: null, end: ev, body: b });
      }
      continue;
    }
    if (row) continue; // a repeated beginning
    const r = { key: `${key}|${ev.eventId}`, tuple: b.tuple, outbound: !!b.outbound,
      startMs: msOfTime(b.start) ?? msOfTime(recordOf(ev).time), ended: false, begin: ev, end: null, body: b };
    rows.push(r);
    open.set(key, r);
  }
  for (const r of rows) {
    const b = r.body;
    r.endMs = r.ended ? msOfTime(b.end) : null;
    r.bytesSent = Number(b.bytes_sent) || 0;
    r.bytesReceived = Number(b.bytes_received) || 0;
    r.packetsSent = Number(b.packets_sent) || 0;
    r.packetsReceived = Number(b.packets_received) || 0;
    r.durationMs = r.ended && r.startMs != null && r.endMs != null ? Math.max(0, r.endMs - r.startMs) : null;
  }
  return rows;
}

// networkModel splits a page of traffic events into the Network tab's
// tables, each in time order, and sums the records feosd reported missed.
export function networkModel(events) {
  const dns = [];
  const names = [];
  const drops = [];
  let missed = 0;
  for (const ev of events || []) {
    if (!TRAFFIC_KINDS.includes(ev.kind)) continue;
    missed += Number(recordOf(ev).missed) || 0;
    const b = bodyOf(ev);
    if (ev.kind === 'dns') {
      dns.push({ ev, name: b.name || '', type: qtypeName(b.type), rcode: rcodeName(b.rcode),
        failed: (Number(b.rcode) || 0) !== 0, addresses: Array.isArray(b.addresses) ? b.addresses : [] });
    } else if (ev.kind === 'server_name') {
      names.push({ ev, name: b.name || '', source: serverNameSource(b.source), tuple: b.tuple || {} });
    } else if (ev.kind === 'drop') {
      drops.push({ ev, reason: dropReason(b.reason), outbound: !!b.outbound, tuple: b.tuple || {} });
    }
  }
  const flows = flowRows(events);
  return { flows, dns, names, drops, missed, openFlows: flows.filter((f) => !f.ended).length };
}
