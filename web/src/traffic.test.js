// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  dropReason, endpoint, flowRows, networkModel, qtypeName, rcodeName, trafficSummary, tupleText,
} from './traffic.js';

// Traffic events as GET …/activity?source=traffic returns them: the
// record's kind, and the whole TrafficRecord as protojson with the
// proto's field names, zero values left out, 64-bit counters as strings.

let seq = 0;
const ev = (kind, body, extra = {}) => {
  seq += 1;
  return {
    kind, source: 'traffic', category: 'network', v: 1, node: 'node-a', eventId: `node-a:s:${seq}`,
    tsNs: String(BigInt(Date.parse('2026-10-16T12:00:00Z') + seq) * 1000000n),
    detailJson: JSON.stringify({ time: '2026-10-16T12:00:00Z', pod_id: 'p', ...extra, [kind]: body }),
  };
};
const TUPLE = { protocol: 6, source_address: 'fd00::2', source_port: 40000, destination_address: '2606:4700::1', destination_port: 443 };

describe('rendering', () => {
  it('brackets IPv6 addresses with a port', () => {
    expect(endpoint('fd00::2', 443)).toBe('[fd00::2]:443');
    expect(endpoint('10.0.0.2', 53)).toBe('10.0.0.2:53');
    expect(endpoint('fd00::2', 0)).toBe('fd00::2');
    expect(endpoint('', undefined)).toBe('?');
  });

  it('renders a tuple, protocols by number', () => {
    expect(tupleText(TUPLE)).toBe('TCP [fd00::2]:40000 → [2606:4700::1]:443');
    expect(tupleText({ protocol: 58, source_address: 'fd00::2', destination_address: 'fd00::1' })).toBe('ICMPv6 fd00::2 → fd00::1');
    expect(tupleText(undefined)).toBe('IP ? → ?');
  });

  it('names DNS types and response codes, 0 left out by protojson', () => {
    expect(qtypeName(28)).toBe('AAAA');
    expect(qtypeName(undefined)).toBe('TYPE0');
    expect(rcodeName(undefined)).toBe('NOERROR');
    expect(rcodeName(3)).toBe('NXDOMAIN');
  });

  it('reads the drop reasons as enum names', () => {
    expect(dropReason('DROP_REASON_NO_RULE')).toBe('no rule of its security groups allowed it');
    expect(dropReason(undefined)).toBe('unspecified');
    expect(dropReason('DROP_REASON_NEW')).toBe('DROP_REASON_NEW');
  });

  it('summarises each kind', () => {
    expect(trafficSummary(ev('dns', { name: 'example.com', type: 28, addresses: ['2606:4700::1'] })))
      .toBe('example.com AAAA NOERROR: 2606:4700::1');
    expect(trafficSummary(ev('dns', { name: 'nx.example', type: 1, rcode: 3 }))).toBe('nx.example A NXDOMAIN: no addresses');
    expect(trafficSummary(ev('server_name', { tuple: TUPLE, source: 'SERVER_NAME_SOURCE_TLS', name: 'example.com' })))
      .toBe('example.com → [2606:4700::1]:443');
    expect(trafficSummary(ev('drop', { tuple: TUPLE, outbound: true, reason: 'DROP_REASON_DENIED_PREFIX' })))
      .toBe('TCP [fd00::2]:40000 → [2606:4700::1]:443: its destination is in a denied prefix');
    expect(trafficSummary(ev('flow', { tuple: TUPLE }))).toBe('TCP [fd00::2]:40000 → [2606:4700::1]:443 (in)');
  });
});

describe('flowRows', () => {
  it('pairs a flow’s beginning and end by tuple and start', () => {
    const start = '2026-10-16T12:00:00.250Z';
    const rows = flowRows([
      ev('flow', { tuple: TUPLE, outbound: true, start }),
      ev('flow', { tuple: { ...TUPLE, source_port: 40001 }, outbound: true, start }),
      ev('flow', { tuple: TUPLE, outbound: true, start, end: '2026-10-16T12:00:02.250Z', ended: true,
        bytes_sent: '2048', bytes_received: '512', packets_sent: '4', packets_received: '3' }),
    ]);
    expect(rows).toHaveLength(2);
    const [a, b] = rows;
    expect([a.ended, a.bytesSent, a.bytesReceived, a.packetsSent, a.packetsReceived]).toEqual([true, 2048, 512, 4, 3]);
    expect(a.durationMs).toBe(2000);
    expect(a.begin).not.toBeNull();
    expect(a.end).not.toBeNull();
    expect([b.ended, b.durationMs]).toEqual([false, null]);
  });

  it('shows a flow whose beginning fell out of the page from its end', () => {
    const rows = flowRows([ev('flow', { tuple: TUPLE, start: '2026-10-16T11:00:00Z', end: '2026-10-16T11:00:01Z', ended: true })]);
    expect(rows).toHaveLength(1);
    expect(rows[0].begin).toBeNull();
    expect(rows[0].durationMs).toBe(1000);
    expect(rows[0].outbound).toBe(false);
  });

  it('a second flow of the same tuple with a new start is a flow of its own', () => {
    const rows = flowRows([
      ev('flow', { tuple: TUPLE, start: '2026-10-16T12:00:00Z' }),
      ev('flow', { tuple: TUPLE, start: '2026-10-16T12:00:00Z', end: '2026-10-16T12:00:01Z', ended: true }),
      ev('flow', { tuple: TUPLE, start: '2026-10-16T12:00:05Z' }),
    ]);
    expect(rows.map((r) => r.ended)).toEqual([true, false]);
  });
});

describe('networkModel', () => {
  it('splits a page into the tables and sums the missed records', () => {
    const m = networkModel([
      ev('dns', { name: 'example.com', type: 1, addresses: ['93.184.216.34'] }, { missed: '2' }),
      ev('dns', { name: 'nx.example', type: 1, rcode: 3 }),
      ev('server_name', { tuple: TUPLE, source: 'SERVER_NAME_SOURCE_HTTP', name: 'example.com' }),
      ev('drop', { tuple: TUPLE, reason: 'DROP_REASON_SPOOFED' }, { missed: '1' }),
      ev('flow', { tuple: TUPLE, start: '2026-10-16T12:00:00Z' }),
      { kind: 'pod_state', source: 'feosd', eventId: 'x', detailJson: '{}' },
    ]);
    expect(m.dns.map((d) => [d.name, d.rcode, d.failed])).toEqual([['example.com', 'NOERROR', false], ['nx.example', 'NXDOMAIN', true]]);
    expect(m.names.map((s) => [s.name, s.source])).toEqual([['example.com', 'HTTP']]);
    expect(m.drops.map((d) => [d.reason, d.outbound])).toEqual([['its source address was not the pod’s', false]]);
    expect([m.flows.length, m.openFlows, m.missed]).toEqual([1, 1, 3]);
  });

  it('survives a detail that is not JSON', () => {
    const m = networkModel([{ kind: 'dns', source: 'traffic', eventId: 'y', detailJson: 'not json' }]);
    expect(m.dns).toHaveLength(1);
    expect(m.dns[0].name).toBe('');
  });
});
