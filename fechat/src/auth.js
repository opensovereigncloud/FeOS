// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Sign-in with a FeCode ID: the backend runs it (backend/auth.go) and keeps
// the session in a cookie; the SPA only asks who is signed in, and sends
// the browser to sign in when nobody is.

// Goes to the sign-in, back to this page afterwards.
export function signIn() {
  const back = window.location.pathname + window.location.search
  window.location.assign('/api/v1/auth/login?return=' + encodeURIComponent(back))
}

// Who is signed in: the user, or null while the browser goes to sign in.
// A backend without sign-in (development) answers 404: anonymous then.
export async function currentUser() {
  const resp = await fetch('/api/v1/auth/me')
  if (resp.status === 401) {
    signIn()
    return null
  }
  if (resp.status === 404) {
    return { login: '', name: '' }
  }
  if (!resp.ok) {
    throw new Error(`sign-in check failed: HTTP ${resp.status}`)
  }
  return resp.json()
}

// Ends the session, and the IdP's, which would sign the browser in again.
export async function signOut() {
  const resp = await fetch('/api/v1/auth/logout', { method: 'POST' })
  const { redirect } = resp.ok ? await resp.json() : { redirect: '/' }
  window.location.assign(redirect || '/')
}
