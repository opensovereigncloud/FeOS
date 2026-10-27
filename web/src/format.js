// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Formatting shared by the views. Times are shown in the browser's time
// zone, as 2026-10-16 14:03 and 14:03:22.123, the same in every locale.

const pad = (n, w = 2) => String(n).padStart(w, '0');

function dateOf(v) {
  const d = v instanceof Date ? v : new Date(v);
  return Number.isNaN(d.getTime()) ? null : d;
}

const ymd = (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
const hm = (d) => `${pad(d.getHours())}:${pad(d.getMinutes())}`;
const hms = (d) => `${hm(d)}:${pad(d.getSeconds())}`;

// fmtDate renders an RFC 3339 time as "2026-10-16 14:03"; "—" when unset.
export function fmtDate(s) {
  if (!s) return '—';
  const d = dateOf(s);
  return d ? `${ymd(d)} ${hm(d)}` : String(s);
}

// fmtDateTime renders an RFC 3339 time with seconds.
export function fmtDateTime(s) {
  if (!s) return '—';
  const d = dateOf(s);
  return d ? `${ymd(d)} ${hms(d)}` : String(s);
}

// fmtTime renders a time as "14:03:22".
export function fmtTime(s) {
  if (!s) return '';
  const d = dateOf(s);
  return d ? hms(d) : String(s);
}

// fmtMsTime renders milliseconds since the epoch as "14:03:22.123".
export function fmtMsTime(ms) {
  const d = dateOf(Number(ms));
  if (!d) return String(ms);
  return `${hms(d)}.${pad(Math.floor(((Number(ms) % 1000) + 1000) % 1000), 3)}`;
}

// nsToMs turns nanoseconds since the epoch (an int64 string or a number)
// into whole milliseconds. A string is divided exactly: as a float, a
// time in nanoseconds has lost its last digits.
export function nsToMs(tsNs) {
  if (typeof tsNs === 'string' && /^-?\d+$/.test(tsNs)) return Number(BigInt(tsNs) / 1000000n);
  return Math.floor(Number(tsNs) / 1e6);
}

// fmtNs renders nanoseconds since the epoch (an int64 string) as
// "14:03:22.123".
export function fmtNs(tsNs) {
  if (!tsNs || tsNs === '0') return '—';
  return fmtMsTime(nsToMs(tsNs));
}

// nsToDate renders nanoseconds since the epoch with the date, for titles.
export function nsToDate(tsNs) {
  if (!tsNs || tsNs === '0') return '';
  const ms = nsToMs(tsNs);
  const d = dateOf(ms);
  return d ? `${ymd(d)} ${fmtMsTime(ms)}` : '';
}

// nsToIso turns nanoseconds since the epoch into an RFC 3339 time, for
// the relative labels; '' for 0.
export function nsToIso(tsNs) {
  if (!tsNs || tsNs === '0') return '';
  const d = dateOf(nsToMs(tsNs));
  return d ? d.toISOString() : '';
}

function span(s) {
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h`;
  return `${Math.floor(h / 24)}d`;
}

// fmtAgo renders how long ago an RFC 3339 time was ("3m ago"); a time
// ahead reads "in 3m".
export function fmtAgo(iso, now = Date.now()) {
  if (!iso) return '—';
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return String(iso);
  const s = Math.round((now - then) / 1000);
  if (s < -4) return `in ${span(-s)}`;
  if (s < 5) return 'just now';
  return `${span(s)} ago`;
}

// fmtDurMs renders a duration in milliseconds.
export function fmtDurMs(ms) {
  const v = Number(ms);
  if (!Number.isFinite(v)) return '';
  if (v >= 3600000) return (v / 3600000).toFixed(1) + ' h';
  if (v >= 60000) return (v / 60000).toFixed(1) + ' min';
  if (v >= 1000) return (v / 1000).toFixed(1) + ' s';
  return v.toFixed(0) + ' ms';
}

// fmtDurNs renders a duration in nanoseconds (a number or an int64
// string).
export function fmtDurNs(ns) {
  if (ns === undefined || ns === null || ns === '') return '';
  return fmtDurMs(Number(ns) / 1e6);
}

// fmtSeconds renders a whole number of seconds as "1 h", "30 min", "45 s".
export function fmtSeconds(sec) {
  const v = Number(sec) || 0;
  if (v > 0 && v % 3600 === 0) return `${v / 3600} h`;
  if (v > 0 && v % 60 === 0) return `${v / 60} min`;
  return `${v} s`;
}

// prettyJson pretty-prints a JSON value that arrives either way: the
// activity log carries params and details as JSON text, the tool list
// carries objects.
export function prettyJson(v) {
  if (v && typeof v === 'object') return JSON.stringify(v, null, 2);
  try {
    return JSON.stringify(JSON.parse(v), null, 2);
  } catch {
    return v;
  }
}

// shortId is an ID to show where space is short: a long one (a UUID) by
// its first 8 characters, a short one (a pod's, 11 characters) whole.
export function shortId(id) {
  if (!id) return '—';
  return id.length > 12 ? id.slice(0, 8) : id;
}
