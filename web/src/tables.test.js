// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { readdirSync, readFileSync } from 'node:fs';

// Every table of the views sits in a container that scrolls it
// sideways, so that a window too narrow for it scrolls the table, not
// the page or nothing at all.

const dir = new URL('./views/', import.meta.url);

describe('the tables', () => {
  it('each scroll in a container of their own', () => {
    const bare = [];
    for (const f of readdirSync(dir).filter((f) => f.endsWith('.jsx') && !f.includes('.test.'))) {
      const src = readFileSync(new URL(f, dir), 'utf8');
      for (const m of src.matchAll(/<table className="tbl[^"]*">/g)) {
        const before = src.slice(Math.max(0, m.index - 120), m.index);
        if (!/className="(tblwrap|evtable-wrap)"[^<]*>\s*$/.test(before)) bare.push(`${f}: ${m[0]}`);
      }
    }
    expect(bare).toEqual([]);
  });

  it('the scrolling containers scroll sideways', () => {
    const css = readFileSync(new URL('./styles.css', import.meta.url), 'utf8');
    expect(css).toMatch(/\.tblwrap \{[^}]*overflow-x: auto/);
    expect(css).toMatch(/\.evtable-wrap \{[^}]*overflow-y: auto/);
  });
});
