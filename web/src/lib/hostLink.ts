/**
 * The deep-link seam between the SPA and the document that hosts it.
 *
 * The SPA normally runs in an `<iframe>` on an Unraid webGUI page
 * (`FileBrowser.page`, reached at `https://<server>/FileBrowser`). The URL the
 * user sees, bookmarks and refreshes is therefore the *parent's*, not ours, so
 * a `?path=` on our own iframe URL would be invisible and unbookmarkable. The
 * feature is consequently split across two documents:
 *
 *   SPA  → parent   `postMessage({type:"filebrowser:location", path}, origin)`
 *                   every time the browse location settles. The parent writes
 *                   it into its own address bar with `history.replaceState`.
 *
 *   parent → SPA    on load only, and not as a message: the parent re-encodes
 *                   its own `?path=` into the iframe `src` alongside the
 *                   `theme`/`csrf`/`v` params it already passes. We read it
 *                   here at boot (`readDeepLinkPath`).
 *
 * `replaceState` (never `pushState`) is deliberate on the parent side: the
 * iframe drives its own hash history, and a parent history entry per navigation
 * would interleave with it and make Back behave erratically.
 *
 * Standalone (no iframe — the Vite dev server, or someone opening
 * `app/index.html` directly) is handled by the same call: we rewrite our *own*
 * `location.search` instead. `window.parent !== window` is the only test.
 *
 * The player window is a standalone document on the `#/play/` route and is
 * explicitly *not* part of this: App never publishes from it, and `playerUrl()`
 * strips `path` out of the search string it inherits.
 */

import { clampPathDepth } from "./router";

/** Query-string parameter, on both the parent page and the iframe URL. */
export const PATH_PARAM = "path";

/** postMessage `type` for SPA → host location updates. */
export const LOCATION_MESSAGE = "filebrowser:location";

/**
 * Longest path we will emit or accept. Real share paths are nowhere near this;
 * the cap exists because the value crosses two documents and ends up in a URL,
 * and the PHP page applies the identical limit on the way back in.
 */
export const MAX_PATH_LEN = 4096;

/** Control characters have no business in a path and break URLs and logs. */
const CONTROL_CHARS = /[\u0000-\u001f\u007f]/;

/**
 * Minimum spacing between publishes. Holding ↓ in the tree, or arrowing through
 * directories, would otherwise post a message and rewrite the parent's address
 * bar per keystroke. Leading edge fires immediately (a single navigation feels
 * instant), and a trailing flush guarantees the *final* state is always the one
 * the host ends up with.
 */
const THROTTLE_MS = 250;

/**
 * A path is publishable/acceptable if it is absolute, bounded, and free of
 * control characters. Everything past that — existence, confinement to the
 * browse roots, archive resolution — is the daemon's job, and deliberately not
 * duplicated here: this is a *transport* check, not an authorization one.
 */
export function isValidLinkPath(path: string | null | undefined): path is string {
  if (typeof path !== "string") return false;
  if (path.length === 0 || path.length > MAX_PATH_LEN) return false;
  if (!path.startsWith("/")) return false;
  return !CONTROL_CHARS.test(path);
}

/**
 * The `?path=` we were booted with, or null.
 *
 * `URLSearchParams` does the percent-decoding and never throws: a broken
 * sequence (`%zz`, a lone `%`) decodes to U+FFFD rather than blowing up, and
 * the result then simply fails `isValidLinkPath` or reaches the daemon as a
 * path that does not exist — either way a `?path=` a user mangled by hand
 * degrades to the default root, never to a blank screen.
 */
export function readDeepLinkPath(search?: string): string | null {
  if (typeof window === "undefined") return null;
  let raw: string | null = null;
  try {
    raw = new URLSearchParams(search ?? window.location.search).get(PATH_PARAM);
  } catch {
    return null;
  }
  if (raw === null) return null;
  const path = clampPathDepth(raw);
  return isValidLinkPath(path) ? path : null;
}

/* --------------------------------------------------------------- publishing */

let lastSent: string | null = null;
let lastSentAt = 0;
let pending: string | null = null;
let timer: ReturnType<typeof setTimeout> | null = null;

/**
 * Tell the host where we are. Safe to call on every render: identical values
 * are dropped, and bursts collapse to one leading and one trailing send.
 */
export function publishLocation(path: string): void {
  if (typeof window === "undefined") return;
  if (!isValidLinkPath(path)) return;
  if (path === lastSent) return;

  pending = path;
  // A trailing flush is already scheduled — it will pick up this newer value.
  if (timer !== null) return;

  const wait = THROTTLE_MS - (Date.now() - lastSentAt);
  if (wait <= 0) {
    flush();
    return;
  }
  timer = setTimeout(() => {
    timer = null;
    flush();
  }, wait);
}

function flush(): void {
  const path = pending;
  pending = null;
  if (path === null || path === lastSent) return;
  lastSent = path;
  lastSentAt = Date.now();
  send(path);
}

function send(path: string): void {
  const origin = window.location.origin;

  if (window.parent !== window) {
    try {
      // Never "*": the payload is the user's directory layout, and the only
      // document entitled to it is the plugin page on our own origin. If the
      // host is somewhere else the browser drops the message, which is exactly
      // the outcome we want.
      window.parent.postMessage({ type: LOCATION_MESSAGE, path }, origin);
    } catch {
      /* opaque origin (file://), or a parent that went away mid-flight */
    }
    return;
  }

  // Standalone: we *are* the bookmarkable document, so rewrite our own search
  // string. replaceState leaves the hash (our real router) untouched and emits
  // no hashchange, so nothing re-renders because of this.
  try {
    const url = new URL(window.location.href);
    url.searchParams.delete(PATH_PARAM);
    const rest = url.searchParams.toString();
    const qs = `?${PATH_PARAM}=${encodeURIComponent(path)}${rest ? `&${rest}` : ""}`;
    history.replaceState(history.state, "", `${url.pathname}${qs}${url.hash}`);
  } catch {
    /* a document.location we may not rewrite is not worth breaking over */
  }
}
