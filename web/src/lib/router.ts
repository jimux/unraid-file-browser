/**
 * Hand-rolled hash router.
 *
 *   #/browse/<encodeURIComponent(path)>
 *   #/view/<encodeURIComponent(path)>     (viewer panel over the parent dir)
 *   #/search?q=…&mode=…&…
 *   #/settings
 *
 * Paths are percent-encoded whole, so "/" and "!" inside a virtual path never
 * collide with the route grammar.
 */

export type Route =
  | { name: "browse"; path: string }
  | { name: "view"; path: string }
  | { name: "search"; query: URLSearchParams }
  | { name: "settings" }
  | { name: "unknown" };

export function browseHref(path: string): string {
  return `#/browse/${encodeURIComponent(path)}`;
}

export function viewHref(path: string): string {
  return `#/view/${encodeURIComponent(path)}`;
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
      return p ? { name: "view", path: p } : { name: "unknown" };
    }
    case "search":
      return { name: "search", query: new URLSearchParams(queryPart) };
    case "settings":
      return { name: "settings" };
    default:
      return { name: "unknown" };
  }
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
