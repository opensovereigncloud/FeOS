// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { NodesFootnote, NodesTable } from './Nodes.jsx';
import { nodeRows } from './fleet.js';

// The nodes view's footnote explains the statuses and a drain with the
// timings of the instance, from GET /api/v1/admin/nodes's defaults.

const DEFAULTS = {
  telemetryIntervalSeconds: 5, nodeTtlSeconds: 20, lostGraceSeconds: 420, drainTimeoutSeconds: 5400,
  defaultVcpus: 2, defaultMemoryMib: 512, defaultImage: 'ghcr.io/example/image:1',
  minVcpus: 1, maxVcpus: 32, minMemoryMib: 256, maxMemoryMib: 65536,
};

const render = (defaults) => renderToStaticMarkup(<NodesFootnote defaults={defaults} />);

describe('NodesFootnote', () => {
  it("names the instance's timings with their flags", () => {
    const out = render(DEFAULTS);
    expect(out).toContain('telemetry every 5 s');
    expect(out).toContain('<code>--node-ttl</code> (20 s)');
    expect(out).toContain('<code>--lost-grace</code> (7 min)');
    expect(out).toContain('<code>--drain-timeout</code> (1 h 30 min)');
  });

  it('says what lost grace does with and without a checkpoint', () => {
    const out = render(DEFAULTS).replace(/\s+/g, ' ');
    expect(out).toContain('with a checkpoint become <code>hibernated</code> and are restored elsewhere on their next use');
    expect(out).toContain('those without one count as <code>lost</code>');
  });

  it('shows other values when the instance runs with others', () => {
    const out = render({ ...DEFAULTS, nodeTtlSeconds: 15, lostGraceSeconds: 600, drainTimeoutSeconds: 3600 });
    expect(out).toContain('<code>--node-ttl</code> (15 s)');
    expect(out).toContain('<code>--lost-grace</code> (10 min)');
    expect(out).toContain('<code>--drain-timeout</code> (1 h)');
    expect(out).not.toContain('(20 s)');
  });

  it('without defaults (an older ferund, or before the first answer): the flags without numbers', () => {
    const out = render(null);
    expect(out).toContain('<code>--node-ttl</code>;');
    expect(out).toContain('<code>--lost-grace</code>,');
    expect(out).toContain('<code>--drain-timeout</code> are cancelled');
    expect(out).not.toMatch(/\(\d/);
    expect(out).not.toContain('telemetry every');
  });
});

// The nodes' table: compact enough for a 1280 px window, its memory bar
// labelled by what it fills by.

const NODE = {
  node: 'lab-1', status: 'ready', hostname: 'lab-1.example', cpuNumcores: 16, memoryTotalbytes: String(64 * 2 ** 30),
  memAvailBytes: String(40.5 * 2 ** 30), slots: 32, memHeadroomBytes: String(2 ** 30), vcpusCapacity: 64,
  memoryCapacityBytes: String(63 * 2 ** 30), cpuOvercommit: 4, diskAvailBytes: String(812 * 2 ** 30), load1: 1.23,
  sandboxesRunning: 12, sandboxesPaused: 3, lastSeenAtNs: '1760000005000000000', sandboxesBooked: 12, vcpusBooked: 24,
  memoryBookedBytes: String(12 * 2 ** 30), connected: true, connections: 2,
  certSerial: 'd76051b3b729497ad882eb42c04ca255', certNotAfter: '2026-10-23T20:08:44Z',
};

const table = (nodes) => renderToStaticMarkup(
  <NodesTable rows={nodeRows(nodes)} now={0} armed="" arm={() => {}} disarm={() => {}} onDrain={() => {}} onOpen={() => {}} />);

describe('NodesTable', () => {
  it('labels the memory bar with the memory in use of the total, the bar filled as much', () => {
    const out = table([NODE]);
    expect(out).toContain('width:37%');
    expect(out).toContain('>23.5 / 64.0 GiB</span>');
    expect(out).toContain('title="23.5 GiB in use of 64.0 GiB, 40.5 GiB available"');
    expect(out).not.toContain('40.5 GiB /');
  });

  it('books vCPUs, memory and slots against what the node may book, in one column', () => {
    const out = table([NODE]);
    expect(out).toContain('>24 / 64 vCPU</div>');
    expect(out).toContain('>12.0 / 63.0 GiB</div>');
    expect(out).toContain('>12 / 32 slots</div>');
  });

  it('has ten compact columns, the link under the last telemetry', () => {
    const out = table([NODE, { ...NODE, node: 'lab-2', connected: false, connections: 0 }]);
    expect(out).toContain('class="tbl nodes-tbl"');
    expect(out.match(/<th[ >]/g)).toHaveLength(10);
    expect(out).toContain('>connected ×2</div>');
  });

  // The armed button grows by one character, so that arming it does not
  // push the actions column out of a 1280 px window.
  it('asks briefly before a drain or an undrain', () => {
    const armed = (node) => renderToStaticMarkup(
      <NodesTable rows={nodeRows([node])} now={0} armed={'drain:' + node.node} arm={() => {}} disarm={() => {}}
        onDrain={() => {}} onOpen={() => {}} />);
    expect(armed(NODE)).toContain('>drain?</button>');
    expect(armed({ ...NODE, draining: true })).toContain('>undrain?</button>');
  });

  // The link is to the instance that answered: a ready node connected to
  // another replica is no fault, only a down or removed one is told so.
  it('names a missing link only for a node that is down or removed', () => {
    const elsewhere = table([{ ...NODE, connected: false, connections: 0 }]);
    expect(elsewhere).not.toContain('not connected</div>');
    expect(elsewhere).toContain('title="not connected to this instance; it may be to another">—</div>');
    for (const status of ['down', 'removed']) {
      expect(table([{ ...NODE, status, connected: false, connections: 0 }])).toContain('>not connected</div>');
    }
  });
});
