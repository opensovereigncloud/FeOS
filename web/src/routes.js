// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// The UI's routes. ferund answers every path that is not a file of the
// build with index.html, so deep links work.
//
//   /, /tenants                         the tenants
//   /tenants/:t                         a tenant's overview
//   /tenants/:t/sandboxes[/:id[/:tab]]  its sandboxes; tab: overview, activity,
//                                       tokens, access, network
//   /tenants/:t/tools                   the tools of its sandboxes
//   /nodes[/:node[/:tab]]               the nodes; tab: overview, certs, activity
//   /join-tokens                        join tokens
//   /instances                          the control plane's instances

function decode(s) {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}

// parsePath maps a path to {view, tenant?, sel?, tab?, node?}.
export function parsePath(pathname) {
  const seg = String(pathname || '/').split('/').filter(Boolean).map(decode);
  switch (seg[0]) {
    case 'nodes':
      return seg[1] ? { view: 'node', node: seg[1], tab: seg[2] || 'overview' } : { view: 'nodes' };
    case 'join-tokens':
      return { view: 'joinTokens' };
    case 'instances':
      return { view: 'instances' };
    case 'tenants':
      break;
    default:
      return { view: 'tenants' };
  }
  if (seg.length === 1) return { view: 'tenants' };
  const tenant = seg[1];
  if (seg[2] === 'tools') return { view: 'tools', tenant };
  if (seg[2] === 'sandboxes') return { view: 'sandboxes', tenant, sel: seg[3] || '', tab: seg[4] || 'overview' };
  return { view: 'overview', tenant };
}

// TENANT_VIEWS are the views that act as a tenant: they need its token.
export const TENANT_VIEWS = new Set(['overview', 'sandboxes', 'tools']);
