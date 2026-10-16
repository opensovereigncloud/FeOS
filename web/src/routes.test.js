// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { TENANT_VIEWS, parsePath } from './routes.js';
import { fmtAgo, fmtDurNs, fmtMsTime, fmtNs, fmtSeconds, prettyJson } from './format.js';
import { ADMIN_KEY, readItem, writeItem } from './storage.js';

describe('parsePath', () => {
  it('maps the routes', () => {
    expect(parsePath('/')).toEqual({ view: 'tenants' });
    expect(parsePath('/tenants')).toEqual({ view: 'tenants' });
    expect(parsePath('/tenants/acme')).toEqual({ view: 'overview', tenant: 'acme' });
    expect(parsePath('/tenants/acme/tools')).toEqual({ view: 'tools', tenant: 'acme' });
    expect(parsePath('/tenants/acme/sandboxes')).toEqual({ view: 'sandboxes', tenant: 'acme', sel: '', tab: 'overview' });
    expect(parsePath('/tenants/acme/sandboxes/0199f0b7-1c2d-7e3f-8a00-00000000e2e0/network'))
      .toEqual({ view: 'sandboxes', tenant: 'acme', sel: '0199f0b7-1c2d-7e3f-8a00-00000000e2e0', tab: 'network' });
    expect(parsePath('/nodes')).toEqual({ view: 'nodes' });
    expect(parsePath('/nodes/node-a')).toEqual({ view: 'node', node: 'node-a', tab: 'overview' });
    expect(parsePath('/nodes/node-a/certs')).toEqual({ view: 'node', node: 'node-a', tab: 'certs' });
    expect(parsePath('/join-tokens')).toEqual({ view: 'joinTokens' });
    expect(parsePath('/instances')).toEqual({ view: 'instances' });
    expect(parsePath('/no/such/page')).toEqual({ view: 'tenants' });
  });

  it('decodes segments and survives a bad escape', () => {
    expect(parsePath('/tenants/a%2Eb').tenant).toBe('a.b');
    expect(parsePath('/tenants/%E0%A4%A').tenant).toBe('%E0%A4%A');
  });

  it('the tenant views are those that need a tenant token', () => {
    expect([...TENANT_VIEWS].sort()).toEqual(['overview', 'sandboxes', 'tools']);
  });
});

describe('format', () => {
  it('relative times, past and ahead', () => {
    const now = Date.parse('2026-10-16T12:00:00Z');
    expect(fmtAgo('2026-10-16T11:59:58Z', now)).toBe('just now');
    expect(fmtAgo('2026-10-16T11:59:00Z', now)).toBe('1m ago');
    expect(fmtAgo('2026-10-16T09:00:00Z', now)).toBe('3h ago');
    expect(fmtAgo('2026-10-23T12:00:00Z', now)).toBe('in 7d');
    expect(fmtAgo('', now)).toBe('—');
  });

  it('times without a locale', () => {
    const ms = new Date(2026, 9, 16, 9, 5, 7, 42).getTime();
    expect(fmtMsTime(ms)).toBe('09:05:07.042');
    expect(fmtNs(String(ms * 1e6))).toBe('09:05:07.042');
    expect(fmtNs('0')).toBe('—');
  });

  it('durations', () => {
    expect(fmtDurNs('1500000000')).toBe('1.5 s');
    expect(fmtDurNs(250e6)).toBe('250 ms');
    expect(fmtDurNs(undefined)).toBe('');
    expect(fmtSeconds(3600)).toBe('1 h');
    expect(fmtSeconds(600)).toBe('10 min');
  });

  it('pretty-prints JSON text and objects', () => {
    expect(prettyJson('{"a":1}')).toBe('{\n  "a": 1\n}');
    expect(prettyJson({ a: 1 })).toBe('{\n  "a": 1\n}');
    expect(prettyJson('not json')).toBe('not json');
  });
});

describe('storage', () => {
  it('keeps a value in memory where storage refuses', () => {
    // No sessionStorage in this environment: the value lives in memory.
    writeItem('sessionStorage', ADMIN_KEY, 'ops:pw');
    expect(readItem('sessionStorage', ADMIN_KEY)).toBe('ops:pw');
    writeItem('sessionStorage', ADMIN_KEY, '');
    expect(readItem('sessionStorage', ADMIN_KEY)).toBe('');
  });
});
