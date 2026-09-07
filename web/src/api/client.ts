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

function codeForStatus(status: number): ApiErrorCode {
  return STATUS_CODES[status] ?? "INTERNAL";
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
      codeForStatus(res.status),
      res.ok
        ? "malformed response from daemon (not JSON)"
        : `HTTP ${res.status}: ${text.slice(0, 200) || res.statusText}`,
      res.status,
    );
  }

  if (body && typeof body === "object" && "ok" in body) {
    const env = body as OkEnvelope<T> | ErrEnvelope;
    if (env.ok) return env.data;
    throw new ApiError(env.error?.code ?? codeForStatus(res.status), env.error?.message ?? "request failed", res.status);
  }

  throw new ApiError(codeForStatus(res.status), `unexpected response shape (HTTP ${res.status})`, res.status);
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
  return { config: d.config, allowedRoots: Array.isArray(d.allowedRoots) ? d.allowedRoots : [] };
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
