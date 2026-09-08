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
busy/unavailable), `UNAVAILABLE` (an optional subsystem is not installed on
this host — media transcoding without ffmpeg; static until reinstall, do not
retry), `INTERNAL`. HTTP status mirrors the class (400/404/403/413/504/422/
503/503/500). Raw endpoints (`fs/raw`, HLS playlists and segments) stream
bytes and use plain HTTP statuses; their *errors* are still JSON envelopes.

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
`q` (**optional**, see below), `mode` = `name|content|both` (default `both`),
`path` (scope prefix, optional), `ext` (comma list, optional),
`minSize`/`maxSize` (bytes), `after`/`before` (unix seconds, mtime),
`meta` (repeatable metadata filter, see **Metadata search**),
`sort` = `relevance|name|size|mtime`, `dir` = `asc|desc`,
`limit` (default 100, max 1000), `offset`.
→ `{ hits: SearchHit[], total: number, indexFresh: boolean, tookMs: number }`
`q` supports FTS5-style quoting; bare terms are prefix-matched on names.
Paging is bounded: `offset + limit` must be ≤ **10 000** (`BAD_REQUEST`
beyond that — narrow the query instead). The substring fallback that runs
when prefix matching finds nothing requires every term to be at least
**3 characters**. Results are always confined to the current index roots
*and* the daemon's browse roots, regardless of what was indexed earlier.

**`q` is optional, but only when something else narrows the search.** A
request with no `q` (absent, empty, or whitespace) is valid as long as it
carries at least one of `path`, `ext`, `minSize`, `maxSize`, `after`, `before`
or `meta` — that is the "every file bigger than 20 GB" query, which needs no
search term. With neither `q` nor a filter the request is `BAD_REQUEST`
("q is required unless at least one filter is given …"): asking for the whole
index is a mistake, not a query. Whitespace-only `q` is normalised to empty
before the filter check, so `?q=%20&minSize=1` is a filters-only search.

`sort` defaults to **`relevance` when `q` is present and `name` ascending when
it is not**. Both parameters are applied by the index; when the client omits
them the HTTP layer forwards them unset rather than substituting a default of
its own, so the index's default is what a bare request gets. Unknown values are
`BAD_REQUEST` (note `type`, valid for `fs/list`, is **not** a search sort key).
`sort=relevance` on a filters-only search has nothing to rank and falls back to
the index's `name` ordering.

### `GET /search/fields`
No parameters. → `{ categories: MetaCategory[] }`

The metadata vocabulary: which categories exist, which fields each carries,
and — for `enum` fields — the fixed value list. Static for a given daemon
build, so the SPA may fetch it once and cache it. Arrays are never `null`.

### `GET /search/values`
`key` (**required**, a key from `/search/fields`), `prefix` (optional, narrows to
values starting with it — for type-ahead), `path` (optional scope prefix),
`limit` (default 200, max 500 — larger is clamped, not refused).
→ `{ values: MetaValue[] }`

The distinct values recorded for one metadata key, each with the number of
files carrying it, ordered by the index (most useful first) and truncated to
`limit`. A missing or malformed `key` is `BAD_REQUEST`; an unknown but
well-formed key is simply an empty list. `values` is never `null`.
This endpoint is called interactively while the user types, so
it shares the in-flight budget described under **Load shedding**; `/search/fields`
does not (it is a static read).

```ts
interface MetaValue { value: string; count: number }

interface MetaCategory {
  id: string;            // "common" | "image" | "video" | "audio" | "package"
  label: string;         // UI heading, e.g. "Photo"
  extensions: string[];  // the category's default file extensions, lowercase,
                         // no dot; [] for "common", which applies to every file
  fields: MetaFieldDef[];
}

interface MetaFieldDef {
  key: string;           // "image.cameraModel"; bare for the common category
  label: string;         // "Camera model"
  type: "text" | "number" | "bytes" | "date" | "enum" | "bool";
  unit?: string;         // display hint for numbers: "px", "s", "mm", "bit/s"
  values?: MetaEnumValue[]; // enum and bool fields only; absent otherwise
}

interface MetaEnumValue { value: string; label: string }
```

### Index management
- `GET /index/status` → `{ state: "idle"|"crawling"|"extracting"|"metadata", filesIndexed,
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

### Media (on-the-fly HLS)

See the **Transcoding** section for what these do and why. `ENCODING_ERROR`
(422) is the code for "ffmpeg/ffprobe could not read or convert this file";
`UNAVAILABLE` (503) means ffmpeg is not installed at all.

- `GET /media/capabilities` → never errors.
  ```ts
  { available: boolean;
    ffmpeg: string; ffprobe: string;          // versions, "" when missing
    ffmpegPath?: string; ffprobePath?: string;
    hwaccels: string[];                       // `ffmpeg -hwaccels` (informational)
    encoders: string[];                       // H.264/HEVC/AAC encoders this build offers, e.g. ["aac","libx264"]
    hwEncoder?: string;                       // hardware encoder the daemon will try first, absent = software only
    reason?: string }                         // why available is false
  ```
- `GET /media/probe?path=<p>` → `{ probe: Probe }`. Real files only: a
  virtual (archive) path is `BAD_REQUEST`. ffprobe has 20 s and 4 MB of JSON.
  ```ts
  interface Probe {
    container: string;               // ffprobe format_name, e.g. "matroska,webm"
    durationSec: number;             // 0 when unknown
    bitrate: number;                 // bits/s, 0 when unknown
    video: { index: number; codec: string; profile: string; width: number;
             height: number; fps: number; bitrate: number } | null;   // cover art is skipped
    audio: { index: number; codec: string; channels: number; lang: string;
             title: string; default: boolean }[];
    subtitles: { index: number; codec: string; lang: string; title: string }[];
  }
  ```
- `POST /media/session` body
  `{ path: string; can: string[]; audioIndex?: number; maxHeight?: number }`
  → `{ session: Session }`.
  `can` is the browser's codec list (`["h264","vp9","aac","opus","mp3"]`;
  aliases like `h.264`, `h265`, `mp4a` are understood). Empty = unknown →
  everything is re-encoded to H.264/AAC. `audioIndex` is a `Probe.audio[].index`
  (default: the track flagged default, else the first). `maxHeight` below the
  source height **forces a video transcode with scaling** even when the codec
  is playable (the user asked for less); at or above it never upscales.
  ```ts
  interface Session {
    id: string;                      // 32 hex chars
    mode: "remux" | "transcode";     // remux = every stream copied; transcode = at least one re-encoded
    reason: string;                  // human-readable justification, e.g. "AC-3 audio is not supported by this browser"
    durationSec: number; segmentSec: number; segmentCount: number;
    playlist: string;                // "/api/v1/media/hls/<id>/index.m3u8"
    video: { codec: string; width: number; height: number;
             bitrate: number;        // ceiling applied when transcoding (bits/s); 0 when copied
             copied: boolean } | null;
    audio: { codec: string; channels: number; lang: string; index: number; copied: boolean } | null;
  }
  ```
- `GET /media/hls/<id>/index.m3u8` → `application/vnd.apple.mpegurl`. A
  complete **VOD** playlist generated up front from the probed duration
  (`#EXT-X-PLAYLIST-TYPE:VOD`, `#EXT-X-TARGETDURATION`, one `#EXTINF` per
  segment, `#EXT-X-ENDLIST`), so the player can seek anywhere immediately.
  Segment URIs are **bare relative names** (`seg-00001.ts`); the SPA rewrites
  them for the bridge. Each request touches the session (keeps it alive).
- `GET /media/hls/<id>/seg-<NNNNN>.ts` → `video/mp2t`, produced on demand
  and streamed as it is made (chunked, no `Content-Length`). `NNNNN` is
  zero-padded, 5–7 digits, `0 ≤ N < segmentCount`; anything else is
  `NOT_FOUND`. Nothing throttles a session: a player may pull segments back
  to back as fast as ffmpeg produces them.
- `POST /media/session/<id>/close` → `{ ok: true }` (as `data`). Idempotent;
  the SPA calls it via `sendBeacon` on unload. Only GET and POST reach the
  daemon through the bridge — there is no DELETE.

Headers: every media response carries `X-Content-Type-Options: nosniff`.
Playlists and JSON carry `Cache-Control: private, no-store`; segments carry
`Cache-Control: private, max-age=3600` — a segment of a given session is
immutable and its id unguessable, so the browser's own cache may keep it
and a seek back does not cost a second transcode. Never a shared cache.

Limits: at most **3** simultaneous ffmpeg processes (segment producers,
probes and keyframe lookups share the pool; a request that cannot get a slot
within **10 s** is `TIMEOUT` 504 "server busy"); at most **8** live sessions
(the least recently used idle one is evicted for a new one; when all are
busy the new one is refused with `TIMEOUT`); sessions expire after **10 min**
idle; a segment has **60 s** of wall time; playlists are capped at
**100 000** segments (`TOO_LARGE`).

### `GET /healthz`
→ `{ version, uptimeSec, indexDb: "ok"|"missing"|"error", roots: string[] }`
`roots` are the browse roots the daemon was started with (`-roots`).

## Metadata search

Metadata keys are **`<category>.<field>`**: the category is lowercase
(`^[a-z]+$`), the field is a letters-only lowerCamelCase name (`^[A-Za-z]+$`)
— e.g. `image.cameraModel`, `video.hdr`, `audio.artist`, `image.iso`. The one
exception is the **`common`** category, whose keys are bare (`name`, `ext`,
`size`, `mtime`, `mime`): they are columns every indexed file already has
rather than extracted metadata. So the accepted key syntax is
`^[a-z]+(\.[A-Za-z]+)?$`, and every key `/search/fields` serves is a key
`/search` and `/search/values` accept — a dropdown never offers a field it
cannot then query. Anything else is rejected by both endpoints with
`BAD_REQUEST`, before it reaches the index.

A filter is one `meta` parameter of the form `<key><op><value>`, repeated for
each condition (they combine with AND). At most **16** `meta` parameters per
request; beyond that, `BAD_REQUEST`.

| Operator | Meaning | Example |
| --- | --- | --- |
| `=` | equals | `meta=image.cameraModel=DMC-GH5` |
| `!=` | not equal | `meta=video.hdr!=none` |
| `~` | contains (case-insensitive substring) | `meta=audio.artist~davis` |
| `>` | greater than | `meta=video.durationSec>3600` |
| `>=` | greater than or equal | `meta=image.iso>=800` |
| `<` | less than | `meta=image.iso<200` |
| `<=` | less than or equal | `meta=video.height<=1080` |

The ordering operators apply to numeric field types (`number`, `bytes`,
`date` — dates accept RFC3339 or unix seconds); `~` is a case-insensitive
substring match on text. `!=` means "this file has no value of this key equal
to *value*", so a file with two audio tracks, one AC-3 and one AAC, does **not**
match `meta=video.audioCodec!=ac3`.

Parsing rule: the daemon scans left to right for the **first** operator after
the key, testing the two-character operators (`>=`, `<=`, `!=`) before the
one-character ones at the same position. Everything before it is the key,
everything after it is the value — so a value may itself contain `=`, `~`,
`<` or `>` (`meta=audio.title=a=b` searches for the literal `a=b`). Whitespace
around the key, operator and value is trimmed. An empty value, a missing
operator, or a key that fails the pattern is `BAD_REQUEST` naming the offending
`meta` item.

Values are ordinary query-string values: percent-encode them (`+` and `%20`
both decode to a space) and let the daemon decode once — it never decodes
twice, so a literal `%2F` in a title survives as `%2F`. The value is forwarded
to the index verbatim and compared there according to the field's declared
`type` (`number`, `bytes` and `date` numerically, the rest as text); the HTTP
layer validates only the shape of the clause, never the value's contents.

**Categories are a client-side convenience.** Each `MetaCategory` carries the
default `extensions` for its kind of file, and it is the *client* that turns a
chosen category into a plain `ext=` parameter on `/search` (e.g. picking
"Photo" sends `ext=jpg,jpeg,png,heic,…`). The `common` category has no
extensions — its fields apply to every indexed file. The daemon therefore has no
`category` parameter: the category exists only to shape the dropdowns and to
pre-fill the extension list, which the user can then widen or narrow. Combining
`ext` with `meta` is the normal case — the extension list keeps the query on
the right files and the meta filters do the actual selecting.

Example — GH5 stills at ISO 800 or above, shot since 2024, in one share:

```
GET /api/v1/search?ext=jpg,jpeg,raw,rw2&path=/mnt/user/photos
   &meta=image.cameraModel%3DDMC-GH5&meta=image.iso%3E%3D800
   &after=1704067200&sort=mtime&dir=desc
```

(no `q` — the filters are the query.)

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

`fs/list`, `search`, `search/values` and `fs/view` on virtual (archive) paths
share a global in-flight budget of **8** concurrent requests.
(`search/fields` is a static read and is not in the budget.) A request that cannot get a slot
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

## Transcoding

The SPA can only play what the browser decodes natively. For anything else
(AVI, MKV with AC-3, HEVC, …) it asks the daemon for an HLS session and plays
`index.m3u8` with hls.js. The daemon picks the **cheapest** treatment:

- **remux** — every selected stream's codec is in `can` *and* can be carried
  in MPEG-TS (H.264, HEVC, MPEG-2/4 video; AAC, MP3, MP2, AC-3, E-AC-3, Opus,
  DTS audio): the container is the only problem, so streams are copied
  (`-c copy`) — I/O-bound, no CPU to speak of. A browser may decode VP9 or FLAC
  natively but TS has no mapping for them, so they are re-encoded regardless.
- **transcode** — at least one stream is re-encoded, and **only** that one.
  The very common MKV+H.264+AC-3 costs a single AAC encode; the video is
  copied. Video is encoded with libx264 (`veryfast`, CRF 23, High@4.1,
  yuv420p) under a bitrate ceiling from this ladder (`-maxrate`, `-bufsize` =
  2×), by output height: ≤480p 1.5 Mbps, ≤720p 3 Mbps, ≤1080p 6 Mbps, above
  12 Mbps. Audio is encoded to AAC 192 kbps stereo. The ceiling is reported in
  `session.video.bitrate`. Hardware encoding (VAAPI) is tried first only when
  `/dev/dri/renderD128` exists **and** the ffmpeg build lists `h264_vaapi`;
  the bundled static build has neither, so software is the tested path. A
  hardware failure falls back to software for the rest of the session.

`reason` explains the choice in one sentence for the UI.

**Segments are streamed, never stored.** A session is a small in-memory record
(path, probe, chosen treatment, segment length). Each segment is produced by
its own short-lived ffmpeg writing MPEG-TS to stdout, streamed straight to the
client and killed the moment the client disconnects — nothing is written to
the array or anywhere else, and aggressive prefetch costs only CPU. Every
segment carries absolute timestamps (`-copyts`, plus a constant 1 s offset so
segment 0's negative decode timestamps never make ffmpeg shift that one
segment differently from the rest), so hls.js needs no discontinuity handling
and a mid-file segment can be produced without any earlier one.

Segment length: **4 s** when video is transcoded (a slow CPU still starts
quickly); **10 s** when video is copied. Copied video can only be cut on
keyframes: segment *n* runs from the keyframe ffmpeg's seek lands on for
boundary *n* to the keyframe boundary *n+1* lands on, cut in decode order so
consecutive segments are exactly contiguous; the daemon asks ffmpeg itself
where each seek lands (a ~50 ms lookup, cached per session). Consequently
`#EXTINF` values are nominal and real copied segments may start up to one
segment early; hls.js realigns from the segments' own timestamps. At session
creation the first boundaries are sampled, and a file whose keyframes are
farther apart than the segment (GOP > 10 s) is transcoded instead — the
`reason` says so — because copying it would alternate copied and re-encoded
segments. Should a long GOP still turn up mid-file, only that one segment is
re-encoded so coverage stays gapless.

Security: the input is opened by the daemon's race-safe path walk and handed
to ffmpeg as an inherited descriptor (`/dev/fd/3`); a user-derived path is
never an ffmpeg argument. Fixed argv, no shell, `-nostdin`, and
`-protocol_whitelist file,fd,pipe` so a crafted file cannot make ffmpeg open
network URLs or other files.

## Dev mode

`filebrowserd -dev -listen 127.0.0.1:8384 -root <path>` — TCP instead of unix
socket, auth disabled, CORS `*`, `-root` overrides configured roots. The Vite
dev server proxies `/api` there. Production build embeds the SPA and serves it
at `/` on the unix socket.
