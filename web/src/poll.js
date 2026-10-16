// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The ordering of the views' polls: pure, with no React or browser
// imports, so that the tests load it as it is.

// poller runs fn without overlap: a run asked for while the previous one
// has not finished is skipped, so that a slow API does not pile up
// requests. After each run, onDone is called until stop.
export function poller(fn, onDone) {
  let alive = true;
  let busy = false;
  return {
    run: async () => {
      if (!alive || busy) return;
      busy = true;
      try {
        await fn();
      } finally {
        busy = false;
        if (alive) onDone();
      }
    },
    stop: () => { alive = false; },
  };
}

// newest orders the requests of a view whose answers can resolve out of
// order (a filter changed, a manual refresh during a poll): each call of
// the function it returns starts a request and returns a check that
// holds while no later request has started. A view applies an answer
// only while its check holds, so that an older answer never overwrites a
// newer one.
export function newest() {
  let latest = 0;
  return () => {
    const mine = ++latest;
    return () => mine === latest;
  };
}
