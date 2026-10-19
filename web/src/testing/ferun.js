// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { readFileSync } from 'node:fs';
import { vi } from 'vitest';

// The tests' answers of ferund: the fixtures of its REST API
// (go/internal/api/testdata/ferun, one file per route and answer:
// {method, path, query, status, request, response}). ferund's API tests
// check its answers against the same files, so the views are tested
// against what ferund serves. For the tests only: it reads them with
// node:fs.

const dir = new URL('../../../go/internal/api/testdata/ferun/', import.meta.url);

// fixture reads one, by its file name without .json.
export function fixture(name) {
  return JSON.parse(readFileSync(new URL(`${name}.json`, dir), 'utf8'));
}

// response is a fixture's answer body.
export function response(name) {
  return fixture(name).response;
}

// serveFixtures stubs fetch to answer each request with the next of the
// given fixtures (status and body), and returns the requests made.
export function serveFixtures(...names) {
  const calls = [];
  const queue = names.map(fixture);
  vi.stubGlobal('fetch', vi.fn(async (path, init) => {
    calls.push({ path, init });
    const fx = queue.shift();
    if (!fx) throw new Error(`no fixture left for ${init && init.method} ${path}`);
    const text = fx.response === null ? '' : typeof fx.response === 'string' ? fx.response : JSON.stringify(fx.response);
    return { ok: fx.status >= 200 && fx.status < 300, status: fx.status, text: async () => text };
  }));
  return calls;
}

// requestOf splits a request's path into the fixture's shape: the path
// and its query as name -> values.
export function requestOf(call) {
  const u = new URL(call.path, 'http://ferund');
  const query = {};
  for (const k of new Set(u.searchParams.keys())) query[k] = u.searchParams.getAll(k);
  return { method: (call.init && call.init.method) || 'GET', path: u.pathname, query };
}
