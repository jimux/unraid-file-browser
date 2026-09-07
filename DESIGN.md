# Unraid File Browser Plugin — Design

A file-browser plugin for Unraid with array-wide search (filename, metadata, and
opt-in content search), in-browser file viewing with selectable text encodings,
and read-only browsing *inside* archive files (zip, tar.*, 7z, iso, rar, …).

**v1 is strictly read-only.** The API is namespaced so write operations
(rename/move/delete/upload) can be added later behind a settings toggle, but no
mutating filesystem endpoint ships in v1.

## Architecture

```
┌─ Unraid webGUI (emhttp, PHP, nginx) ─────────────────────────┐
│  FileBrowser.page  — thin PHP shell, loads SPA, mints token  │
│  Settings page     — daemon/index configuration              │
└──────────────────────────────┬───────────────────────────────┘
                               │ HTTP via nginx/PHP bridge (auth'd)
                               ▼
┌─ filebrowserd — single static Go binary, rc.d service ───────┐
│  REST API │ FS ops │ Encoding/hex views │ Archive reader     │
│  Indexer/crawler → SQLite FTS5 on /boot-configured data dir  │
│  Shells out to bundled static 7zz for long-tail formats      │
└──────────────────────────────────────────────────────────────┘
```

### Components

1. **`filebrowserd`** (Go, `daemon/`): static binary (CGO off,
   `GOOS=linux GOARCH=amd64`). Serves the REST API on a **unix socket**
   (`/var/run/filebrowserd.sock`) — never a TCP port, so nothing is exposed on
   the LAN and auth rides on the webGUI's own session.
2. **Web SPA** (React + TypeScript + Vite, `web/`): built to static assets
   installed under `/usr/local/emhttp/plugins/filebrowser/app/` and served by
   emhttp's own (authenticated) nginx; the plugin page hosts it in an iframe.
   The SPA talks to the daemon exclusively through `proxy.php`. In dev mode
   the Vite dev server proxies `/api` to the daemon's TCP listener instead.
3. **Plugin packaging** (`plugin/`): `.plg` manifest, `.page` files, PHP auth
   bridge, `rc.filebrowserd` init script, settings UI, txz package build.

### Auth bridge (PHP ⇄ daemon)

The daemon listens only on a root-owned unix socket. The plugin installs a
small PHP endpoint (`proxy.php`) inside the authenticated webGUI: any request
reaching it has already passed Unraid's login. It streams requests/responses
to/from the unix socket (including ranged/streamed file bodies). No tokens, no
open ports, no CORS surface. WebSocket-ish needs (index progress) are handled
with SSE through the same proxy, with polling fallback.

## Repository layout

```
unraid_browser/
├── DESIGN.md, API.md, README.md
├── Makefile                  # build daemon, build web, package .plg, dev targets
├── daemon/
│   ├── go.mod                # module `unraid-filebrowser` (repo: github.com/jimux/unraid-file-browser)
│   ├── cmd/filebrowserd/main.go
│   └── internal/
│       ├── api/              # HTTP handlers, routing, SSE, error envelope
│       ├── fsops/            # list/stat/read, path safety, MIME sniffing
│       ├── view/             # encoding conversion (x/text), hex view
│       ├── archive/          # virtual-path resolver, Go fast path, 7zz driver
│       ├── index/            # crawler, SQLite FTS5 store, scheduler, policy
│       └── config/           # config file load/save, defaults
├── web/                      # React SPA (Vite, TS)
│   └── src/{api,components,views,hooks}/
└── plugin/
    ├── filebrowser.plg       # install manifest (XML)
    └── source/filebrowser/   # → /usr/local/emhttp/plugins/filebrowser/
        ├── FileBrowser.page  # menu entry, loads SPA
        ├── FileBrowserSettings.page
        ├── proxy.php         # auth bridge to unix socket
        ├── rc.filebrowserd
        └── event/            # array start/stop hooks
```

## REST API (see API.md for full contract)

All endpoints return a JSON envelope `{ok, data}` / `{ok:false, error:{code,message}}`
except raw-content endpoints which stream bytes with correct Content-Type.

- `GET  /api/v1/fs/list?path=&sort=&dir=&offset=&limit=` — paged directory listing
- `GET  /api/v1/fs/stat?path=`
- `GET  /api/v1/fs/raw?path=` — streamed download (Range supported)
- `GET  /api/v1/fs/view?path=&encoding=&offset=&length=` — windowed text view,
  bytes decoded server-side from `encoding` to UTF-8; returns text + detected
  info (`{text, encoding, sniffedMime, size, truncated}`). Works on *any* file:
  "view binary as text" is this endpoint with a user-chosen encoding.
- `GET  /api/v1/fs/hex?path=&offset=&length=` — hex+ASCII window
- `GET  /api/v1/search?q=&mode=name|content|both&path=&type=&minSize=&maxSize=&after=&before=&limit=&offset=`
  — FTS-backed; content hits include highlighted snippets
- `GET  /api/v1/index/status` (+ SSE `/api/v1/index/events`) — crawl progress
- `GET/PUT /api/v1/index/config` — roots, content-index rules
- `POST /api/v1/index/rescan?path=`
- `GET  /api/v1/encodings` — supported encoding list for the picker

### Virtual paths (archive drill-down)

Archives are addressed with a `!` separator so every fs endpoint works
uniformly inside them, nested arbitrarily:

```
/mnt/user/backups/site.tar.gz!/var/www/config.zip!/app/settings.php
```

`fs/list`, `fs/stat`, `fs/raw`, `fs/view`, `fs/hex` all accept virtual paths.
The archive resolver splits the path, opens each layer (Go fast path for
zip/tar/compressions; 7zz subprocess for 7z/iso/rar/cab/…), and streams the
target entry. Literal `!` in real filenames is handled by an escaping rule
defined in API.md. Guardrails: per-request timeout, decompressed-size caps,
nesting depth limit (default 3), and 7zz always invoked with an output cap
(zip-bomb defense).

## Search & indexing

- **Store**: SQLite via `modernc.org/sqlite` (pure Go — keeps CGO off) with
  FTS5. DB lives in the configured data dir (default
  `/mnt/user/appdata/filebrowser/index.db`), *not* on the flash drive.
- **Metadata index (always on)**: name, dir, ext, size, mtime, MIME class,
  owner bits for every file under the configured roots (default `/mnt/user`).
  Crawl is stat-only — fast, no content reads.
- **Content index (opt-in)**: per-root include rules decide which files get
  their text extracted into FTS5. Policy = path prefixes + extension/MIME
  allowlist + size cap (default 10 MB). Plain-text-like files only in v1
  (code, logs, configs, markdown, csv, …) — no PDF/office extraction yet, but
  the extractor is an interface so it can grow.
- **Freshness**: full stat-crawl on schedule (default nightly) + incremental
  re-crawl triggered from the UI; mtime/size comparison skips unchanged files.
  inotify hot-watching is a possible v2, not in v1.
- **Resource limits**: single-threaded-ish crawl (configurable parallelism,
  default 2), io-niced, pausable; content extraction respects size caps.

## Viewing & encodings

- Encoding conversion server-side via `golang.org/x/text/encoding` (UTF-8/16/32,
  ISO-8859 family, Windows-125x, Shift-JIS, EUC-JP/KR, GBK/GB18030, Big5,
  KOI8, Mac-Roman, EBCDIC/IBM037…). UTF-8 validity check + BOM sniff pick the
  default; the SPA has an encoding dropdown that re-requests the view.
- Large files are windowed (default 256 KB per request) with next/prev paging;
  the viewer never loads a 40 GB file into the tab.
- Hex view for anything, image preview for images, download always available.

## Frontend (React SPA)

- Vite + React + TypeScript. Virtualized file list (@tanstack/react-virtual)
  — must handle 100k-entry directories. Tree sidebar, breadcrumbs, sortable
  columns (name/size/mtime/type).
- Archive entries browse exactly like folders (virtual paths are transparent).
- Viewer panel: text (encoding picker, wrap toggle, windowed paging), hex,
  image; "open as text" available on any file regardless of type.
- Search view: query + filters (scope path, name/content/both, type, size,
  date), results with snippets, click-through to viewer.
- Theming: reads Unraid webGUI CSS variables so it matches black/white/azure/
  gray themes.

## Plugin packaging & lifecycle

- `.plg` XML manifest: installs txz to `/usr/local/emhttp/plugins/filebrowser/`,
  daemon binary + bundled `7zz` to `/usr/local/bin` equivalents, persists
  config under `/boot/config/plugins/filebrowser/`.
- `rc.filebrowserd start|stop|restart|status`; started on array-start event
  (index lives on the array, so daemon starts degraded-but-alive before that).
- Settings page: index roots, content rules, schedule, data dir, service
  control, index stats.
- Uninstall cleans service + emhttp files, leaves index/config unless the user
  ticks "remove data".

## Security posture

- Daemon runs as root (required to browse all shares) → path handling is
  defense-critical: every request path is cleaned and confined to configured
  roots; symlinks resolved and re-checked; `..` rejected post-normalization.
- Unix socket + PHP bridge means auth is exactly the webGUI's auth.
- 7zz subprocess: fixed argv (no shell), timeouts, stdout caps, temp dirs under
  the data dir with cleanup.
- Read-only v1: no endpoint mutates user data; the only writes are the index
  DB and config files.

## Build & test

- `make daemon` (cross-compiles linux/amd64 static), `make web`, `make plg`.
- Daemon unit tests run anywhere (fsops, archive, index, view are pure Go +
  testdata archives). `make dev` runs daemon locally (TCP loopback dev mode +
  Vite dev server) so the whole app is testable on macOS without an Unraid box.
