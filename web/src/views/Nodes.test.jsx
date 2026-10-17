// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { NodesFootnote } from './Nodes.jsx';

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
