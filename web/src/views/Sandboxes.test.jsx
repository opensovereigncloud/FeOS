// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { CreatePanel } from './Sandboxes.jsx';

// The create form names the platform's default image and size and the
// bounds ferund accepts, from GET /api/v1/admin/nodes's defaults.

const DEFAULTS = {
  telemetryIntervalSeconds: 5, nodeTtlSeconds: 15, lostGraceSeconds: 600, drainTimeoutSeconds: 3600,
  defaultVcpus: 4, defaultMemoryMib: 2048, defaultImage: 'ghcr.io/example/image:1',
  minVcpus: 1, maxVcpus: 16, minMemoryMib: 512, maxMemoryMib: 32768,
};

const render = (defaults) => renderToStaticMarkup(
  <CreatePanel act={() => {}} busy="" defaults={defaults} onDone={() => {}} onCancel={() => {}} />,
);

describe('CreatePanel', () => {
  it("names the instance's defaults and bounds", () => {
    const out = render(DEFAULTS);
    expect(out).toContain('placeholder="image (empty: ghcr.io/example/image:1)"');
    expect(out).toContain('placeholder="vCPUs (4)"');
    expect(out).toContain('title="vCPUs, 1–16 (empty: 4)"');
    expect(out).toContain('min="1" max="16"');
    expect(out).toContain('placeholder="MiB (2048)"');
    expect(out).toContain('title="memory in MiB, 512–32768 (empty: 2048)"');
    expect(out).toContain('min="512" max="32768"');
  });

  it('without defaults: "the default", and no bounds of its own', () => {
    const out = render(null);
    expect(out).toContain('placeholder="image (empty: the default)"');
    expect(out).toContain('placeholder="vCPUs"');
    expect(out).toContain('title="vCPUs (empty: the default)"');
    expect(out).toContain('placeholder="MiB"');
    expect(out).not.toContain('min="');
    expect(out).not.toContain('max="');
  });
});
