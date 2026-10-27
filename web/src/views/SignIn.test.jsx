// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { SignIn } from './SignIn.jsx';

// The sign-in screen offers what /auth/config says ferund offers.

const render = (config) => renderToStaticMarkup(
  <SignIn config={config} next="/tenants/acme/sandboxes" onSignedIn={() => {}} />,
);

describe('SignIn', () => {
  it('with OIDC: "Sign in" goes to /auth/login with the path to return to', () => {
    const out = render({ oidc: true, basic: false });
    expect(out).toContain('href="/auth/login?next=%2Ftenants%2Facme%2Fsandboxes"');
    expect(out).toContain('Sign in');
    expect(out).not.toContain('type="password"');
  });

  it('with Basic only: the user and password form, no OIDC link', () => {
    const out = render({ oidc: false, basic: true });
    expect(out).toContain('type="password"');
    expect(out).not.toContain('/auth/login');
  });

  it('with both: both', () => {
    const out = render({ oidc: true, basic: true });
    expect(out).toContain('/auth/login?next=');
    expect(out).toContain('type="password"');
  });

  it('with neither: says so', () => {
    const out = render({ oidc: false, basic: false });
    expect(out).toContain('offers no sign-in');
    expect(out).not.toContain('<form');
  });
});
