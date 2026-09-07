/**
 * Hand-rolled hash router.
 *
 *   #/browse/<encodeURIComponent(path)>
 *   #/view/<encodeURIComponent(path)>?tab=<text|hex|image|media|download>
 *                                         (viewer panel over the parent dir)
 *   #/play/<encodeURIComponent(path)>?sort=<key>&dir=<asc|desc>&force=1
 *                                         (standalone media player, own window)
 *   #/search?q=…&mode=…&…
 *   #/settings
 *
 * Paths are percent-encoded whole, so "/" and "!" inside a virtual path never
 * collide with the route grammar.
 */

import type { SortDir, SortKey } from "../api/types";

/**
 * The player's prev/next keys walk the file's siblings *in the order the
 * browser view is currently sorted in*, so the player window — a separate
 * document with no access to the browser's React state — has to be told that
 * order. It rides in the hash as `?sort=&dir=`; a link that omits them (the
 * viewer panel, search results) gets the daemon's own defaults.
 */
export interface PlaySort {
  sort: SortKey;
  dir: SortDir;
}

export const DEFAULT_PLAY_SORT: PlaySort = { sort: "name", dir: "asc" };

/**
 * `force` is the "Play as media…" escape hatch from the context menu: open the
 * player for a file neither the mime nor the extension calls media, because
 * the user knows better than both. It does *not* bypass the server's own
 * classification — `fs/raw` still refuses to stream a non-media type inline,
 * and the player says so.
 */
export interface PlayOptions extends Partial<PlaySort> {
  force?: boolean;
}

const SORT_KEYS: readonly string[] = ["name", "size", "mtime", "type"];

/** The viewer panel's tabs, in the order they render. */
export type ViewTab = "text" | "hex" | "image" | "media" | "download";

const VIEW_TABS: readonly string[] = ["text", "hex", "image", "media", "download"];

export type Route =
  | { name: "browse"; path: string }
  | { name: "view"; path: string; tab: ViewTab | null }
  | ({ name: "play"; path: string; force: boolean } & PlaySort)
  | { name: "search"; query: URLSearchParams }
  | { name: "settings" }
  | { name: "unknown" };

export function browseHref(path: string): string {
  return `#/browse/${encodeURIComponent(path)}`;
}

/**
 * `tab` is optional: omitting it keeps the viewer's own defaulting (image for
 * an inline image, media for audio/video, text otherwise). Naming one pins it,
 * which is what the context menu's "View as text"/"View as hex" want.
 */
export function viewHref(path: string, tab?: ViewTab | null): string {
  const base = `#/view/${encodeURIComponent(path)}`;
  return tab ? `${base}?tab=${tab}` : base;
}

/**
 * `sort` is optional: omitting it keeps the URL short and lets the player fall
 * back to name/asc, which is what a caller that has no sort of its own wants.
 */
export function playHref(path: string, opts?: PlayOptions | null): string {
  const base = `#/play/${encodeURIComponent(path)}`;
  if (!opts || (!opts.sort && !opts.dir && !opts.force)) return base;
  const sp = new URLSearchParams();
  if (opts.sort || opts.dir) {
    sp.set("sort", opts.sort ?? DEFAULT_PLAY_SORT.sort);
    sp.set("dir", opts.dir ?? DEFAULT_PLAY_SORT.dir);
  }
  if (opts.force) sp.set("force", "1");
  return `${base}?${sp.toString()}`;
}

export function searchHref(params: URLSearchParams | string): string {
  const s = typeof params === "string" ? params : params.toString();
  return s ? `#/search?${s}` : "#/search";
}

export const SETTINGS_HREF = "#/settings";

/**
 * Deepest path the router will hand to the rest of the app. Real Unraid share
 * paths are nowhere near this; a hash with thousands of segments is a crafted
 * URL whose only effect is to make the tree sidebar and breadcrumbs fan out
 * into one proxy.php request per segment.
 *
 * We collapse rather than refuse: landing on a real ancestor directory is far
 * less surprising than a blank screen or an error banner for what is, from the
 * user's point of view, just a very long path.
 */
export const MAX_PATH_SEGMENTS = 64;

export function clampPathDepth(path: string): string {
  const segs = path.split("/").filter(Boolean);
  if (segs.length <= MAX_PATH_SEGMENTS) return path;
  const lead = path.startsWith("/") ? "/" : "";
  // A cut landing right after an archive boundary would leave a dangling "!".
  return (lead + segs.slice(0, MAX_PATH_SEGMENTS).join("/")).replace(/!+$/, "");
}

export function parseRoute(hash: string): Route {
  const raw = hash.startsWith("#") ? hash.slice(1) : hash;
  if (!raw || raw === "/") return { name: "unknown" };

  const qIdx = raw.indexOf("?");
  const pathPart = qIdx >= 0 ? raw.slice(0, qIdx) : raw;
  const queryPart = qIdx >= 0 ? raw.slice(qIdx + 1) : "";
  const segs = pathPart.split("/").filter(Boolean);

  switch (segs[0]) {
    case "browse":
      return { name: "browse", path: clampPathDepth(decodeSafe(segs.slice(1).join("/"))) || "/" };
    case "view": {
      const p = clampPathDepth(decodeSafe(segs.slice(1).join("/")));
      if (!p) return { name: "unknown" };
      return { name: "view", path: p, tab: parseViewTab(new URLSearchParams(queryPart)) };
    }
    case "play": {
      const p = clampPathDepth(decodeSafe(segs.slice(1).join("/")));
      if (!p) return { name: "unknown" };
      const q = new URLSearchParams(queryPart);
      return { name: "play", path: p, force: q.get("force") === "1", ...parsePlaySort(q) };
    }
    case "search":
      return { name: "search", query: new URLSearchParams(queryPart) };
    case "settings":
      return { name: "settings" };
    default:
      return { name: "unknown" };
  }
}

/**
 * Anything unrecognised collapses to the defaults rather than being forwarded:
 * these values go straight into an `fs/list` query, and the daemon answers
 * BAD_REQUEST for a bogus `sort`, which would disable navigation for no reason.
 */
export function parsePlaySort(q: URLSearchParams): PlaySort {
  const sort = q.get("sort");
  const dir = q.get("dir");
  return {
    sort: sort && SORT_KEYS.includes(sort) ? (sort as SortKey) : DEFAULT_PLAY_SORT.sort,
    dir: dir === "desc" ? "desc" : "asc",
  };
}

/**
 * Same shape as `parsePlaySort`: anything unrecognised collapses, here to
 * `null`, which means "no opinion" — the viewer then picks its own default tab
 * for the file rather than being pinned to a bogus one.
 */
export function parseViewTab(q: URLSearchParams): ViewTab | null {
  const tab = q.get("tab");
  return tab && VIEW_TABS.includes(tab) ? (tab as ViewTab) : null;
}

function decodeSafe(s: string): string {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}

/* ---------------------------------------------------------------- the store */

type Listener = () => void;
const listeners = new Set<Listener>();
let current = typeof window === "undefined" ? "" : window.location.hash;

function emit() {
  current = window.location.hash;
  for (const l of listeners) l();
}

if (typeof window !== "undefined") {
  window.addEventListener("hashchange", emit);
}

export function subscribeRoute(l: Listener): () => void {
  listeners.add(l);
  return () => listeners.delete(l);
}

export function currentHash(): string {
  return current;
}

/** Navigate. `replace` swaps the history entry instead of pushing one. */
export function navigate(href: string, replace = false): void {
  const target = href.startsWith("#") ? href : `#${href}`;
  if (window.location.hash === target) return;
  if (replace) {
    history.replaceState(null, "", target);
    emit();
  } else {
    window.location.hash = target.slice(1);
  }
}

/**
 * Rewrite the URL *without* notifying subscribers — used by the search view to
 * keep the address bar in sync with its debounced form state without
 * re-mounting itself on every keystroke.
 */
export function syncUrlSilently(href: string): void {
  const target = href.startsWith("#") ? href : `#${href}`;
  if (window.location.hash === target) return;
  history.replaceState(null, "", target);
  current = target;
}
