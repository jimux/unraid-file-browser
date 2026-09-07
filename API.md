# filebrowserd REST API v1 — Contract

This file is the **binding contract** between the daemon (`daemon/`), the SPA
(`web/`), and the PHP bridge (`plugin/`). Changes here require updating all
three. Base path: `/api/v1`. In production the SPA reaches the daemon through
`proxy.php?p=<urlencoded path+query>`; in dev mode the daemon serves TCP
loopback directly. The SPA must build URLs through a single `apiUrl()` helper
so the bridge is one code path.

## Envelope

JSON endpoints:

```json
{ "ok": true,  "data": { ... } }
{ "ok": false, "error": { "code": "NOT_FOUND", "message": "human readable" } }
```

Error codes: `BAD_REQUEST`, `NOT_FOUND`, `FORBIDDEN` (outside roots),
`TOO_LARGE`, `TIMEOUT`, `ARCHIVE_ERROR`, `ENCODING_ERROR`, `INDEXING` (index
busy/unavailable), `INTERNAL`. HTTP status mirrors the class (400/404/403/
413/504/422/503/500). Raw endpoints (`fs/raw`) stream bytes and use plain HTTP
statuses.

## Types

```ts
interface Entry {
  name: string;
  path: string;        // full (possibly virtual) path
  type: "file" | "dir" | "symlink" | "archive"; // archive = browsable-as-dir file
  size: number;        // bytes; -1 if unknown (some archive entries)
  mtime: number;       // unix seconds; 0 if unknown
  mime: string;        // sniffed/derived, "" if unknown
  target?: string;     // symlink target
}

interface ViewResult {
  text: string;        // decoded UTF-8 window
  encoding: string;    // encoding actually used
  sniffed: string;     // what detection suggested
  mime: string;
  size: number;        // total file size in bytes
  offset: number;      // byte offset of this window
  length: number;      // byte length consumed from source
  truncated: boolean;  // more bytes exist after this window
  lossy: boolean;      // replacement chars were produced
}

interface SearchHit {
  entry: Entry;
  score: number;
  snippet?: string;    // HTML with <mark> tags, content hits only
  matchedIn: "name" | "content";
}
```

## Endpoints

### `GET /fs/list`
`path` (required), `offset` (default 0), `limit` (default 1000, max 10000),
`sort` = `name|size|mtime|type` (default `name`), `dir` = `asc|desc`,
`dirsFirst` (default true).
→ `{ entries: Entry[], total: number, path: string }`
Works on real dirs and on virtual paths inside archives.
A directory with more than **250 000** entries is refused with `TOO_LARGE`
(413, message "directory too large to list …; narrow with search") — `total`
counts the whole directory, so paging cannot be used to escape the cap.

### `GET /fs/stat`
`path` → `{ entry: Entry }`

### `GET /fs/raw`
`path` → raw bytes. Supports `Range` for real files; virtual (archive) paths
stream without Range support (`Accept-Ranges: none`). `download=1` forces an
attachment. The served `Content-Type` follows the **raw content policy** below
— it is *not* simply the sniffed type.

### `GET /fs/view`
`path`, `encoding` (default `auto`), `offset` (default 0), `length` (default
262144, max 1048576).
→ `{ view: ViewResult }`
`auto`: BOM sniff → UTF-8 validity → fallback `windows-1252`. Any file may be
viewed with any supported encoding — this is the "binary as text" path.
Multi-byte windows are cut on code-point boundaries where possible.

### `GET /fs/hex`
`path`, `offset`, `length` (default 4096, max 65536)
→ `{ rows: [{offset:number, hex:string, ascii:string}], size, truncated }`
(16 bytes per row; hex is space-separated pairs.)

### `GET /encodings`
→ `{ encodings: [{ id: "utf-8", label: "UTF-8" }, ...] }` — order = picker order.

### `GET /search`
`q` (required), `mode` = `name|content|both` (default `both`),
`path` (scope prefix, optional), `ext` (comma list, optional),
`minSize`/`maxSize` (bytes), `after`/`before` (unix seconds, mtime),
`limit` (default 100, max 1000), `offset`.
→ `{ hits: SearchHit[], total: number, indexFresh: boolean, tookMs: number }`
`q` supports FTS5-style quoting; bare terms are prefix-matched on names.
Paging is bounded: `offset + limit` must be ≤ **10 000** (`BAD_REQUEST`
beyond that — narrow the query instead). The substring fallback that runs
when prefix matching finds nothing requires every term to be at least
**3 characters**. Results are always confined to the current index roots
*and* the daemon's browse roots, regardless of what was indexed earlier.

### Index management
- `GET /index/status` → `{ state: "idle"|"crawling"|"extracting", filesIndexed,
  contentIndexed, dbBytes, lastFullScan, current?: string, progress?: number }`
- `GET /index/events` → SSE stream of status objects (same shape), min 1/s max.
  The daemon also emits a comment frame `: keepalive` every **15 s** when no
  status change is pending, so both the PHP bridge and the browser can detect a
  dead peer on an otherwise silent stream; `EventSource` ignores comment
  frames. At most **32** concurrent subscribers: beyond that the request is
  refused with `INDEXING` (503) and clients should fall back to polling
  `/index/status`.
- `GET /index/config` / `PUT /index/config` →
  `{ config: IndexConfig, allowedRoots: string[] }`

`allowedRoots` are the daemon's boot-time **browse roots** (`filebrowserd
-roots`). Every entry in `config.roots` must lie inside one of them; a `PUT`
that violates this is rejected with `BAD_REQUEST`. `PUT` takes either
`{ "config": IndexConfig }` or a bare `IndexConfig` and answers with the same
shape as `GET`. Request bodies larger than **1 MiB** are rejected with
`TOO_LARGE` (413).

```ts
interface IndexConfig {
  roots: string[];               // default ["/mnt/user"]
  schedule: string;              // cron, default "0 3 * * *"
  parallelism: number;           // default 2
  content: {
    enabled: boolean;
    includePaths: string[];      // prefixes that get content indexing
    extensions: string[];        // allowlist, e.g. ["txt","md","go","log",...]
    maxFileBytes: number;        // default 10485760
  };
}
```

- `POST /index/rescan` body `{ path?: string }` — full rescan, or subtree only.
- `POST /index/pause`, `POST /index/resume`

### `GET /healthz`
→ `{ version, uptimeSec, indexDb: "ok"|"missing"|"error", roots: string[] }`
`roots` are the browse roots the daemon was started with (`-roots`).

## Raw content policy (`fs/raw`)

The SPA is served inside the Unraid webGUI origin, so anything the browser
renders from `fs/raw` renders with the logged-in admin (root) session. A file
planted on a share — or inside an archive and reached through a virtual path —
must therefore never be able to execute. Every `fs/raw` response, inline or
attachment, real path or virtual, carries:

```
X-Content-Type-Options: nosniff
Content-Security-Policy: default-src 'none'; sandbox
Cache-Control: private, no-store
```

Only an allowlist of non-scriptable media types is served **inline with its
real `Content-Type`**:

`image/png`, `image/jpeg`, `image/gif`, `image/webp`, `image/bmp`,
`image/avif`, `application/pdf`, `text/plain`, and everything under `audio/*`
and `video/*`. (`text/plain` is safe only because of `nosniff` + the sandbox
CSP above.)

**Everything else** — `text/html`, `image/svg+xml`, `application/xml`,
`text/xml`, `application/xhtml+xml`, `text/javascript`,
`application/javascript`, `text/css`, `application/json`, and any unknown or
empty type — is served as `application/octet-stream` with
`Content-Disposition: attachment; filename="<sanitised>"`, regardless of the
sniffed type. `download=1` forces the attachment disposition for allowlisted
types too (their `Content-Type` stays honest).

Consequence for the SPA: it must not rely on `fs/raw` to preview HTML, SVG or
XML — use `fs/view` (text) or `fs/hex` for those.

## Load shedding

`fs/list`, `search` and `fs/view` on virtual (archive) paths share a global
in-flight budget of **8** concurrent requests. A request that cannot get a slot
within **5 s** is answered `TIMEOUT` (504) with the message `server busy` — it
never ran, so `INDEXING` would be a lie. Clients should retry with backoff.

The 30 s per-request timeout does **not** apply to `fs/raw` or
`/index/events`: a multi-gigabyte download and a long-lived event stream are
streams by nature. Their contexts carry no deadline.

## Virtual paths (archives)

Separator: **`!/`**. Example:
`/mnt/user/bk/site.tar.gz!/www/config.zip!/app/settings.php`

Resolution is **filesystem-first, longest literal match**: the resolver first
checks whether the full literal string exists on disk; otherwise it splits on
the last `!/`, resolves the left side (recursively, same rule), requires it to
be an archive file, and looks up the right side as an entry path inside it.
Consequently a real directory literally named `foo!` never breaks, and
archive-in-archive nests arbitrarily (depth cap: 3, configurable).

Entry paths inside archives always use `/`, no leading slash. Archive entries
that are themselves archives (by extension) get `type: "archive"` so the UI
lets you drill in. Compressed single-file formats (`.gz`, `.bz2`, `.xz`,
`.zst` without `.tar`) list as one synthetic entry (the decompressed name).

Limits (all return `ARCHIVE_ERROR`/`TOO_LARGE`): nesting depth 3 (a path
with more `!/` separators than that is refused before any lookup); at most
**250 000** entries per archive (larger archives are refused, not truncated);
at most **4** concurrent archive operations (excess wait up to the request
deadline, then `TIMEOUT`); an aggregate **2 GB** budget for nested-layer temp
files; per-request wall clock 30s; decompressed bytes per entry read 256 MB (view/hex windows
stream-and-stop, so viewing the head of a huge entry is still fine); nested
archive layers larger than 512 MB compressed are extracted to a temp file in
the data dir rather than memory.

## Dev mode

`filebrowserd -dev -listen 127.0.0.1:8384 -root <path>` — TCP instead of unix
socket, auth disabled, CORS `*`, `-root` overrides configured roots. The Vite
dev server proxies `/api` there. Production build embeds the SPA and serves it
at `/` on the unix socket.
