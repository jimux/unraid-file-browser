import type {
  ApiErrorCode,
  EncodingOption,
  EncodingsResult,
  HealthResult,
  HexParams,
  HexResult,
  IndexConfig,
  IndexConfigResult,
  IndexStatus,
  ListParams,
  ListResult,
  MediaCapabilities,
  MediaProbe,
  MediaSession,
  MediaSessionParams,
  SearchParams,
  SearchResult,
  StatResult,
  ViewParams,
  ViewResult,
} from "./types";

export const API_BASE = "/api/v1";

/**
 * The single place a daemon URL is constructed.
 *
 *  - dev:  Vite proxies /api → http://127.0.0.1:8384 (`filebrowserd -dev`).
 *  - prod: the SPA is served from /plugins/filebrowser/app/ and the PHP auth
 *          bridge sits one level up at /plugins/filebrowser/proxy.php, which
 *          forwards `p` (the urlencoded path+query) to the unix socket.
 *
 * `path` is an API path *including* its query string, e.g. "/fs/list?path=%2F".
 */
export function apiUrl(path: string): string {
  if (import.meta.env.DEV) return `${API_BASE}${path}`;
  return `../proxy.php?p=${encodeURIComponent(API_BASE + path)}`;
}

export class ApiError extends Error {
  readonly code: ApiErrorCode;
  readonly status: number;

  constructor(code: ApiErrorCode, message: string, status = 0) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.status = status;
  }
}

export function isApiError(e: unknown): e is ApiError {
  return e instanceof ApiError;
}

const STATUS_CODES: Record<number, ApiErrorCode> = {
  400: "BAD_REQUEST",
  403: "FORBIDDEN",
  404: "NOT_FOUND",
  413: "TOO_LARGE",
  422: "ENCODING_ERROR",
  503: "INDEXING",
  504: "TIMEOUT",
};

/**
 * 503 is overloaded: the index endpoints mean "busy" (`INDEXING`) by it and
 * the media endpoints mean "ffmpeg is not installed" (`UNAVAILABLE`). The
 * envelope's own `code` always wins — this map is only consulted when the
 * daemon (or the PHP bridge, or nginx) answered with something that is not a
 * parseable envelope, so the path is the only signal left to disambiguate.
 */
const STATUS_CODES_MEDIA: Record<number, ApiErrorCode> = { ...STATUS_CODES, 503: "UNAVAILABLE" };

function codeForStatus(status: number, path = ""): ApiErrorCode {
  const map = path.startsWith("/media/") ? STATUS_CODES_MEDIA : STATUS_CODES;
  return map[status] ?? "INTERNAL";
}

/** Serialize params, dropping undefined/null/"" so defaults stay server-side. */
export function qs(params: Record<string, string | number | boolean | undefined | null>): string {
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null || v === "") continue;
    sp.set(k, String(v));
  }
  const s = sp.toString();
  return s ? `?${s}` : "";
}

interface OkEnvelope<T> {
  ok: true;
  data: T;
}
interface ErrEnvelope {
  ok: false;
  error: { code: ApiErrorCode; message: string };
}

/**
 * Unraid's webGUI rejects any POST that lacks its csrf_token (enforced by an
 * auto_prepend before proxy.php runs). The plugin page appends `csrf=<token>`
 * to the iframe URL; we echo it back as the X-CSRF-TOKEN header, which the
 * prepend accepts. GETs are exempt.
 *
 * Search param only. The hash is attacker-writable from outside the frame
 * (a bare `location.hash = "...?csrf=…"` needs no same-origin access), so
 * reading a token from it would let a third party choose what we send.
 */
function csrfToken(): string {
  return new URLSearchParams(window.location.search).get("csrf") ?? "";
}

/** Fetch + unwrap the {ok,data} envelope. Rejects with ApiError. */
async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response;
  const headers: Record<string, string> = {
    Accept: "application/json",
    ...((init?.headers as Record<string, string>) ?? {}),
  };
  let method = init?.method ?? "GET";
  if (method !== "GET" && !import.meta.env.DEV) {
    const token = csrfToken();
    if (token) headers["X-CSRF-TOKEN"] = token;
    // proxy.php accepts only GET/POST from the browser so Unraid's CSRF check
    // (POST-only) always applies; it forwards the override method upstream.
    if (method !== "POST") {
      headers["X-HTTP-Method-Override"] = method;
      method = "POST";
    }
  }
  try {
    res = await fetch(apiUrl(path), {
      credentials: "same-origin",
      ...init,
      method,
      headers,
    });
  } catch (e) {
    if (e instanceof DOMException && e.name === "AbortError") throw e;
    throw new ApiError("NETWORK", e instanceof Error ? e.message : "network request failed");
  }

  let body: unknown;
  const text = await res.text();
  try {
    body = text ? JSON.parse(text) : null;
  } catch {
    throw new ApiError(
      codeForStatus(res.status, path),
      res.ok
        ? "malformed response from daemon (not JSON)"
        : `HTTP ${res.status}: ${text.slice(0, 200) || res.statusText}`,
      res.status,
    );
  }

  if (body && typeof body === "object" && "ok" in body) {
    const env = body as OkEnvelope<T> | ErrEnvelope;
    if (env.ok) return env.data;
    throw new ApiError(
      env.error?.code ?? codeForStatus(res.status, path),
      env.error?.message ?? "request failed",
      res.status,
    );
  }

  throw new ApiError(codeForStatus(res.status, path), `unexpected response shape (HTTP ${res.status})`, res.status);
}

async function post<T>(path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  return request<T>(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: body === undefined ? "{}" : JSON.stringify(body),
    signal,
  });
}

/* -------------------------------------------------------------- filesystem */

export function listDir(p: ListParams, signal?: AbortSignal): Promise<ListResult> {
  return request<ListResult>(
    `/fs/list${qs({
      path: p.path,
      offset: p.offset,
      limit: p.limit,
      sort: p.sort,
      dir: p.dir,
      dirsFirst: p.dirsFirst === undefined ? undefined : p.dirsFirst ? "1" : "0",
    })}`,
    { signal },
  );
}

export function stat(path: string, signal?: AbortSignal): Promise<StatResult> {
  return request<StatResult>(`/fs/stat${qs({ path })}`, { signal });
}

/** Raw bytes URL — used for <img src> and download links; never fetched as JSON. */
export function rawUrl(path: string, download = false): string {
  return apiUrl(`/fs/raw${qs({ path, download: download ? 1 : undefined })}`);
}

export function view(p: ViewParams, signal?: AbortSignal): Promise<ViewResult> {
  return request<{ view: ViewResult }>(
    `/fs/view${qs({ path: p.path, encoding: p.encoding, offset: p.offset, length: p.length })}`,
    { signal },
  ).then((d) => d.view);
}

export function hex(p: HexParams, signal?: AbortSignal): Promise<HexResult> {
  return request<HexResult>(`/fs/hex${qs({ path: p.path, offset: p.offset, length: p.length })}`, { signal });
}

export function encodings(signal?: AbortSignal): Promise<EncodingOption[]> {
  return request<EncodingsResult>("/encodings", { signal }).then((d) => d.encodings ?? []);
}

/* ------------------------------------------------------- media / transcode */

/**
 * Capabilities are a property of the *daemon host*, not of the file, and they
 * cannot change while a player window is open — one request per document.
 * A failed probe clears the memo so a retry can still succeed.
 */
let capsPromise: Promise<MediaCapabilities> | null = null;

export function mediaCapabilities(): Promise<MediaCapabilities> {
  if (!capsPromise) {
    capsPromise = request<MediaCapabilities>("/media/capabilities")
      .then((c) => ({
        ...c,
        available: !!c?.available,
        // Go marshals empty slices as null; never let one reach .map().
        hwaccels: Array.isArray(c?.hwaccels) ? c.hwaccels : [],
        encoders: Array.isArray(c?.encoders) ? c.encoders : [],
      }))
      .catch((e) => {
        capsPromise = null;
        throw e;
      });
  }
  return capsPromise;
}

export function mediaProbe(path: string, signal?: AbortSignal): Promise<MediaProbe> {
  return request<{ probe: MediaProbe }>(`/media/probe${qs({ path })}`, { signal }).then((d) => {
    const p = d.probe ?? ({} as MediaProbe);
    return {
      ...p,
      video: p.video ?? null,
      audio: Array.isArray(p.audio) ? p.audio : [],
      subtitles: Array.isArray(p.subtitles) ? p.subtitles : [],
    };
  });
}

export function createMediaSession(p: MediaSessionParams, signal?: AbortSignal): Promise<MediaSession> {
  return post<{ session: MediaSession }>(
    "/media/session",
    { path: p.path, can: p.can, audioIndex: p.audioIndex, maxHeight: p.maxHeight },
    signal,
  ).then((d) => d.session);
}

/** Path (not URL) of the close endpoint — `apiUrl()` it, or beacon it. */
export function mediaSessionClosePath(id: string): string {
  return `/media/session/${encodeURIComponent(id)}/close`;
}

export function closeMediaSession(id: string): Promise<unknown> {
  return post<unknown>(mediaSessionClosePath(id), {});
}

/**
 * Best-effort close for `pagehide`: a beacon survives the document going away,
 * which a fetch does not. It cannot carry the CSRF header, so the synchronous
 * close in the unmount path stays the primary mechanism and the daemon's TTL
 * sweeper is the backstop. Returns whether the beacon was queued.
 */
export function beaconCloseMediaSession(id: string): boolean {
  try {
    return navigator.sendBeacon?.(apiUrl(mediaSessionClosePath(id)), "") ?? false;
  } catch {
    return false;
  }
}

/**
 * The API path of a file inside a session's HLS directory.
 *
 * `name` is a *bare* playlist/segment name as it appears in the manifest
 * (`index.m3u8`, `seg-00001.ts`). Anything with a slash, a scheme or a `..` in
 * it is not something the daemon serves, so it is refused rather than
 * concatenated into a URL.
 */
const HLS_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

export function hlsPath(sessionId: string, name: string): string | null {
  if (!sessionId || !HLS_NAME.test(name) || name.includes("..")) return null;
  return `/media/hls/${encodeURIComponent(sessionId)}/${name}`;
}

/* ------------------------------------------------------------------ search */

export function search(p: SearchParams, signal?: AbortSignal): Promise<SearchResult> {
  return request<SearchResult>(
    `/search${qs({
      q: p.q,
      mode: p.mode,
      path: p.path,
      ext: p.ext,
      minSize: p.minSize,
      maxSize: p.maxSize,
      after: p.after,
      before: p.before,
      limit: p.limit,
      offset: p.offset,
    })}`,
    { signal },
  );
}

/* ------------------------------------------------------------------- index */

export function indexStatus(signal?: AbortSignal): Promise<IndexStatus> {
  return request<IndexStatus>("/index/status", { signal });
}

/** Normalise a possibly-old daemon that omits `allowedRoots`. */
function asConfigResult(d: IndexConfigResult): IndexConfigResult {
  // Older daemons serialise unset lists as null; never let a null reach .map().
  const c = d.config ?? ({} as IndexConfig);
  const content = c.content ?? ({} as IndexConfig["content"]);
  const config: IndexConfig = {
    ...c,
    roots: Array.isArray(c.roots) ? c.roots : [],
    schedule: c.schedule ?? "0 3 * * *",
    parallelism: c.parallelism ?? 2,
    content: {
      ...content,
      enabled: !!content.enabled,
      includePaths: Array.isArray(content.includePaths) ? content.includePaths : [],
      extensions: Array.isArray(content.extensions) ? content.extensions : [],
      maxFileBytes: content.maxFileBytes ?? 10485760,
    },
  };
  return { config, allowedRoots: Array.isArray(d.allowedRoots) ? d.allowedRoots : [] };
}

/** Returns the config *and* the browse roots it must stay inside. */
export function getIndexConfig(signal?: AbortSignal): Promise<IndexConfigResult> {
  return request<IndexConfigResult>("/index/config", { signal }).then(asConfigResult);
}

export function putIndexConfig(config: IndexConfig, signal?: AbortSignal): Promise<IndexConfigResult> {
  return request<IndexConfigResult>("/index/config", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ config }),
    signal,
  }).then(asConfigResult);
}

export function rescan(path?: string, signal?: AbortSignal): Promise<unknown> {
  return post<unknown>("/index/rescan", path ? { path } : {}, signal);
}

export function pauseIndex(signal?: AbortSignal): Promise<unknown> {
  return post<unknown>("/index/pause", {}, signal);
}

export function resumeIndex(signal?: AbortSignal): Promise<unknown> {
  return post<unknown>("/index/resume", {}, signal);
}

/* ------------------------------------------------------------------ health */

export function healthz(signal?: AbortSignal): Promise<HealthResult> {
  return request<HealthResult>("/healthz", { signal });
}
