// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { existsSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// The build goes into ferund's tree, where internal/ui embeds it
// (//go:embed all:dist). Only dist/.keep is committed there, so that the
// Go module builds without npm; emptyOutDir removes it with the rest, and
// keepPlaceholder writes it back, so that a build leaves the checkout as
// it was.
const outDir = fileURLToPath(new URL('../go/internal/ui/dist', import.meta.url));

// keepPlaceholder writes .keep into the directory the build actually
// wrote (an --outDir overrides the config's), and only if it exists: a
// build that failed before writing leaves its error alone.
function keepPlaceholder() {
  let dir = '';
  return {
    name: 'ferun-keep-placeholder',
    apply: 'build',
    configResolved(config) {
      dir = resolve(config.root, config.build.outDir);
    },
    closeBundle() {
      if (dir && existsSync(dir)) writeFileSync(join(dir, '.keep'), '');
    },
  };
}

// The dev server (npm run dev) serves the UI on :5173 and passes the API,
// sign-in and MCP through to a ferund on :8100.
const ferund = 'http://localhost:8100';

export default defineConfig({
  plugins: [react(), keepPlaceholder()],
  server: {
    proxy: { '/api': ferund, '/auth': ferund, '/mcp': ferund },
  },
  build: { outDir, emptyOutDir: true },
  test: { environment: 'node' },
});
