const SIZE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

/** Binary human size. -1 means "unknown" per the Entry contract. */
export function formatSize(bytes: number): string {
  if (bytes < 0 || !Number.isFinite(bytes)) return "—";
  if (bytes < 1024) return `${bytes} B`;
  let v = bytes;
  let u = 0;
  while (v >= 1024 && u < SIZE_UNITS.length - 1) {
    v /= 1024;
    u += 1;
  }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${SIZE_UNITS[u]}`;
}

/** Exact byte count with thousands separators, for tooltips. */
export function formatBytesExact(bytes: number): string {
  if (bytes < 0 || !Number.isFinite(bytes)) return "unknown";
  return `${bytes.toLocaleString()} bytes`;
}

/** Absolute local timestamp from unix seconds. 0 means unknown. */
export function formatAbsolute(unixSec: number): string {
  if (!unixSec) return "—";
  const d = new Date(unixSec * 1000);
  if (Number.isNaN(d.getTime())) return "—";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

const REL_STEPS: Array<[number, Intl.RelativeTimeFormatUnit]> = [
  [60, "second"],
  [3600, "minute"],
  [86400, "hour"],
  [604800, "day"],
  [2629800, "week"],
  [31557600, "month"],
  [Infinity, "year"],
];

const DIVISORS: Record<string, number> = {
  second: 1,
  minute: 60,
  hour: 3600,
  day: 86400,
  week: 604800,
  month: 2629800,
  year: 31557600,
};

const rtf = typeof Intl !== "undefined" && Intl.RelativeTimeFormat ? new Intl.RelativeTimeFormat(undefined, { numeric: "auto" }) : null;

/** "3 days ago" style. */
export function formatRelative(unixSec: number, nowSec = Date.now() / 1000): string {
  if (!unixSec) return "—";
  const delta = unixSec - nowSec;
  const abs = Math.abs(delta);
  const step = REL_STEPS.find(([limit]) => abs < limit);
  const unit = step ? step[1] : "year";
  const value = Math.round(delta / DIVISORS[unit]);
  if (!rtf) return formatAbsolute(unixSec);
  return rtf.format(value, unit);
}

/** Accepts unix seconds or an RFC3339 string (lastFullScan is loosely typed). */
export function toUnixSeconds(v: number | string | undefined | null): number {
  if (v === undefined || v === null || v === "") return 0;
  if (typeof v === "number") return v > 1e11 ? Math.floor(v / 1000) : v;
  const n = Number(v);
  if (Number.isFinite(n) && v.trim() !== "") return n > 1e11 ? Math.floor(n / 1000) : n;
  const t = Date.parse(v);
  return Number.isNaN(t) ? 0 : Math.floor(t / 1000);
}

/** Coarse label for the Type column. */
export function typeLabel(type: string, mime: string, name: string): string {
  if (type === "dir") return "Folder";
  if (type === "archive") return "Archive";
  if (type === "symlink") return "Link";
  if (mime) {
    // `mime` is whatever the daemon sniffed — do not assume it has a "/".
    const slash = mime.indexOf("/");
    const top = slash < 0 ? mime : mime.slice(0, slash);
    const sub = slash < 0 ? "" : mime.slice(slash + 1);
    if (top === "text") return sub === "plain" || !sub ? "Text" : sub.toUpperCase();
    if (top === "image" || top === "audio" || top === "video") return top[0].toUpperCase() + top.slice(1);
    if (sub) return sub.replace(/^x-/, "").toUpperCase().slice(0, 12);
  }
  const i = name.lastIndexOf(".");
  if (i > 0) return name.slice(i + 1).toUpperCase().slice(0, 8);
  return "File";
}

export function formatNumber(n: number): string {
  return Number.isFinite(n) ? n.toLocaleString() : "—";
}

export function formatDuration(sec: number): string {
  if (!Number.isFinite(sec) || sec < 0) return "—";
  const s = Math.floor(sec % 60);
  const m = Math.floor((sec / 60) % 60);
  const h = Math.floor((sec / 3600) % 24);
  const d = Math.floor(sec / 86400);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  if (m) return `${m}m ${s}s`;
  return `${s}s`;
}

/** Hex offset column, e.g. 00001000. */
export function formatOffsetHex(n: number): string {
  return n.toString(16).padStart(8, "0");
}
