// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { newest, poller } from './poll.js';

// A promise with its resolve outside, for answers that arrive when the
// test says.
function deferred() {
  let resolve;
  const promise = new Promise((r) => { resolve = r; });
  return { promise, resolve };
}

describe('newest', () => {
  it('an older answer that resolves last is not applied', async () => {
    const request = newest();
    const shown = [];
    const old = deferred();
    const fresh = deferred();
    // A view's refresh: apply the answer only while its check holds.
    const refresh = async (answer) => {
      const current = request();
      const v = await answer;
      if (current()) shown.push(v);
    };
    const a = refresh(old.promise); // the previous source's request
    const b = refresh(fresh.promise); // the new source's, started at once
    fresh.resolve('feosd');
    await b;
    old.resolve('all sources');
    await a;
    expect(shown).toEqual(['feosd']);
  });

  it('a check holds until a later request starts', () => {
    const request = newest();
    const first = request();
    expect(first()).toBe(true);
    const second = request();
    expect([first(), second()]).toEqual([false, true]);
  });
});

describe('poller', () => {
  it('skips a run while the previous one has not finished', async () => {
    let calls = 0;
    let done = 0;
    let answer = deferred();
    const p = poller(() => { calls++; return answer.promise; }, () => { done++; });
    const first = p.run();
    await p.run(); // the next tick, the first request still under way
    await p.run();
    expect(calls).toBe(1);
    answer.resolve();
    await first;
    expect(done).toBe(1);
    answer = deferred();
    const again = p.run();
    expect(calls).toBe(2);
    answer.resolve();
    await again;
    expect(done).toBe(2);
  });

  it('stops: no run starts and no finished one is reported', async () => {
    let calls = 0;
    let done = 0;
    const answer = deferred();
    const p = poller(() => { calls++; return answer.promise; }, () => { done++; });
    const first = p.run();
    p.stop();
    answer.resolve();
    await first;
    await p.run();
    expect([calls, done]).toEqual([1, 0]);
  });

  it('a failed run frees the poller for the next', async () => {
    let calls = 0;
    const p = poller(async () => { calls++; throw new Error('503'); }, () => {});
    await expect(p.run()).rejects.toThrow('503');
    await expect(p.run()).rejects.toThrow('503');
    expect(calls).toBe(2);
  });
});
