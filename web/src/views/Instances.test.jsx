// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { InstancesTable, instanceState } from './Instances.jsx';

// The instances' table, from GET /api/v1/admin/instances: a stopping
// instance that still writes its row serves on and is shown stopping,
// not gone; the database decides fresh and live.

const BASE = {
  address: ':8100', nodeAddress: '10.0.0.1:7443', version: 'v1', startedAt: '2026-10-17T08:00:00Z',
  lastSeen: '2026-10-17T09:00:00Z', stopping: '', fresh: true, live: true,
};
const RUNNING = { ...BASE, id: 'ferund-a' };
const STOPPING = { ...BASE, id: 'ferund-b', stopping: '2026-10-17T08:59:00Z', live: false };
const GONE = { ...BASE, id: 'ferund-c', stopping: '2026-10-17T08:30:00Z', fresh: false, live: false };

const table = (instances, leader = null) => renderToStaticMarkup(<InstancesTable instances={instances} leader={leader} />);

describe('instanceState', () => {
  it('is running while live, stopping while fresh, gone after', () => {
    expect(instanceState(RUNNING)).toEqual({ state: 'running', title: 'live' });
    expect(instanceState(STOPPING)).toEqual({ state: 'paused', title: 'stopping' });
    expect(instanceState(GONE)).toEqual({ state: 'absent', title: 'gone' });
  });

  it('takes an older ferund without fresh as gone once not live', () => {
    const { fresh, ...older } = STOPPING;
    expect(fresh).toBe(true);
    expect(instanceState(older).state).toBe('absent');
  });
});

describe('InstancesTable', () => {
  it('shows a stopping instance that still serves as stopping, undimmed', () => {
    const out = table([STOPPING]);
    expect(out).toContain('class="sdot paused" title="stopping"');
    expect(out).not.toContain('class="dim"');
    expect(out).not.toContain('title="gone"');
  });

  it('dims only an instance that is gone', () => {
    const out = table([RUNNING, GONE]);
    expect(out).toContain('class="sdot running" title="live"');
    expect(out).toContain('class="sdot absent" title="gone"');
    expect(out.match(/class="dim"/g)).toHaveLength(1);
  });

  it('marks the leader', () => {
    expect(table([RUNNING], { id: 'ferund-a' })).toContain('leader</span>');
  });
});
