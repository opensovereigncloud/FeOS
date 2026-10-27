// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { shortId } from './format.js';

describe('shortId', () => {
  it('shows a short pod ID whole and a UUID by its first 8 characters', () => {
    expect(shortId('abcdefghjkm')).toBe('abcdefghjkm');
    expect(shortId('0199f0b7-1c2d-7e3f-8a00-00000000e2e0')).toBe('0199f0b7');
    expect(shortId('')).toBe('—');
  });
});
