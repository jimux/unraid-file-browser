# Unraid File Browser

A plugin for Unraid that adds array-wide file browsing, search, and in-browser
file viewing to the webGUI.  The entire feature set is read-only in v1; no
existing data is ever modified by the plugin.

## What it does

- **Browse** every Unraid share through a virtualized file list that handles
  directories with hundreds of thousands of entries without freezing the browser.
- **Search** by filename, file metadata (size, type, date), or file content.
  Content search is opt-in per root and backed by a SQLite FTS5 index built by
  a background crawler.
- **View any file** as text with a selectable character encoding (UTF-8, all
  ISO-8859 variants, Windows code pages, Shift-JIS, EUC-JP/KR, GBK, Big5,
  KOI8, and others), or as a hex dump.  Large files are windowed — only the
  requested range is transferred.
- **Browse inside archives** — zip, tar (all compressions), 7z, iso, rar, cab,
  and other formats supported by the bundled 7zz binary.  Archives may be nested
  up to three levels deep, and each layer is addressed with the same path API as
  a real directory.
- **Image preview** for common raster and vector formats; download always
  available.

## Architecture

```
Browser tab
    |
    | (authenticated HTTPS via Unraid webGUI / nginx)
    v
FileBrowser.page  (thin PHP shell; loads SPA inside iframe)
    |
proxy.php  (auth bridge — forwards requests to unix socket)
    |
    | (unix socket /var/run/filebrowserd.sock, root-owned)
    v
filebrowserd  (static Go binary, rc.d service)
    |-- REST API: browse, view, hex, search, index management
    |-- SQLite FTS5 index  (/mnt/user/appdata/filebrowser/index.db)
    `-- 7zz subprocess  (long-tail archive formats)
```

The daemon never opens a TCP port.  All traffic goes through the webGUI's own
authenticated HTTP layer, so no additional authentication mechanism is needed
and nothing is exposed on the local network.

## Features

| Area | Details |
|---|---|
| Directory listing | Sorted by name, size, mtime, or type; ascending or descending; directories-first option; paged (up to 10 000 entries per request) |
| File viewer | Text with encoding selection; windowed (256 KB per page) so 40 GB logs are fine; hex view; image preview |
| Encoding support | UTF-8/16/32, ISO-8859-1 through -16, Windows-125x, Shift-JIS, EUC-JP, EUC-KR, GBK, GB18030, Big5, KOI8-R/U, Mac-Roman, and more |
| Archive browsing | zip, tar.gz, tar.bz2, tar.xz, tar.zst, 7z, iso, rar, cab; nested archives up to depth 3 |
| Search — filename | FTS5 prefix match on name, extension, directory; filters: type, size range, mtime range |
| Search — content | Full-text search over indexed plain-text files; results include highlighted snippets |
| Index | Stat-only crawl (fast, always on); opt-in content extraction for text-like files up to 10 MB |
| Themes | Reads Unraid webGUI CSS variables; matches black, white, azure, and gray themes automatically |

## Repository layout

```
unraid_browser/
├── Makefile                       build, package, dev targets
├── DESIGN.md                      architecture and design decisions
├── API.md                         REST API contract
├── README.md                      this file
├── daemon/                        Go module (unraid-filebrowser, Go 1.25)
│   ├── go.mod / go.sum
│   ├── cmd/filebrowserd/          main package
│   └── internal/
│       ├── api/                   HTTP handlers, routing, SSE
│       ├── fsops/                 directory listing, stat, read, path safety
│       ├── view/                  encoding conversion, hex view
│       ├── archive/               virtual-path resolver, Go fast path, 7zz driver
│       ├── index/                 crawler, FTS5, scheduler, content policy
│       └── config/                config load/save
├── web/                           React + TypeScript SPA (Vite)
│   └── src/{api,components,views,hooks}/
└── plugin/
    ├── filebrowser.plg            Slackware package manifest (XML)
    └── source/filebrowser/        installed to /usr/local/emhttp/plugins/filebrowser/
        ├── FileBrowser.page       webGUI menu entry
        ├── Settings.page          index and service configuration UI
        ├── proxy.php              auth bridge: HTTP → unix socket
        ├── rc.filebrowserd        service init script
        └── event/                 array start/stop hooks
```

## Development quickstart

### Prerequisites

- Go 1.25 or later
- Node.js 20 or later with npm
- 7zz (optional, for testing archive formats locally):
  `brew install sevenzip` on macOS

### Running locally

```
make dev
```

This prints the commands to start both processes.  Run each in a separate
terminal:

**Terminal 1 — daemon (TCP dev mode, auth disabled):**
```sh
cd daemon && go run ./cmd/filebrowserd \
    -dev -listen 127.0.0.1:8384 -root "$HOME" -data /tmp/fbdata
```

**Terminal 2 — Vite dev server (proxies /api to the daemon):**
```sh
cd web && npm run dev
```

Open the URL that Vite prints (typically `http://localhost:5173`).  The daemon
and SPA hot-reload independently.

### Running tests

```
make test
```

Runs `go vet` and `go test ./...` on the daemon, then `npm run typecheck` on
the web package if it is present.  Daemon tests run on any platform (testdata
archives are included in the repo).

## Build and package

### Build the daemon (host architecture, for testing)

```
make daemon
```

Output: `daemon/filebrowserd`

### Build the Linux release binary

```
make daemon-linux
```

Output: `daemon/filebrowserd-linux-amd64` (static, CGO disabled)

### Build the web SPA

```
make web
```

Output: `web/dist/`

### Build the full Slackware package

```
make package [VERSION=YYYY.MM.DD]
```

`VERSION` defaults to today's date (`date +%Y.%m.%d`).

The target:
1. Cross-compiles the daemon for linux/amd64.
2. Builds the web SPA.
3. Stages a package tree under `build/pkg/` matching the Unraid filesystem layout.
4. Downloads a static 7zz binary (7-Zip 25.01 for Linux x64, SHA-256 verified) into `build/cache/`
   on the first run; subsequent runs reuse the cached copy.
5. Archives the tree to `dist/filebrowser-VERSION-x86_64-1.txz`.
6. Computes the MD5 of the txz, prints it, and writes a patched copy of
   `plugin/filebrowser.plg` to `dist/` with the `version` and `md5` XML
   entities updated.

The resulting `dist/` directory contains everything needed to release:
`filebrowser.plg` and `filebrowser-VERSION-x86_64-1.txz`.

**macOS note:** The build host needs GNU tar (`brew install gnu-tar`) to produce
a correctly-owned txz.  If only macOS bsdtar is available, `make package` falls
back to `COPYFILE_DISABLE=1 tar` to suppress `._` resource-fork files, but the
txz will not have root ownership on its entries; this is harmless for Unraid but
differs from a Linux-produced package.

### Clean

```
make clean
```

Removes `build/`, `dist/`, `daemon/filebrowserd`, `daemon/filebrowserd-linux-amd64`,
and `web/dist/`.

## Installing on Unraid

### Via plugin URL (once released)

In the Unraid webGUI go to **Plugins > Install Plugin** and paste the raw URL
of `dist/filebrowser.plg` from the GitHub release.  Unraid fetches and verifies
the txz automatically.

### Manual install (for development builds)

1. Copy `dist/filebrowser-VERSION-x86_64-1.txz` to the Unraid flash drive at
   `/boot/packages/`.
2. Copy `dist/filebrowser.plg` to `/boot/config/plugins/filebrowser/filebrowser.plg`.
3. From an Unraid terminal run:
   ```sh
   installpkg /boot/packages/filebrowser-VERSION-x86_64-1.txz
   ```
4. Start the service:
   ```sh
   /usr/local/emhttp/plugins/filebrowser/rc.filebrowserd start
   ```
5. The **File Browser** entry will appear in the webGUI apps menu.

To uninstall, use the Settings page in the webGUI or run:
```sh
removepkg filebrowser
```

This removes the service and emhttp files.  The index database and plugin config
under `/boot/config/plugins/filebrowser/` are left in place unless you tick
**Remove data** on the Settings page first.

## Configuration

Configuration is managed through the **Settings** page in the webGUI.  The
underlying config file lives on the flash drive at
`/boot/config/plugins/filebrowser/config.json` and is read by the daemon on
startup.

| Setting | Default | Notes |
|---|---|---|
| Index roots | `/mnt/user` | Directories the crawler traverses; multiple roots supported |
| Data directory | `/mnt/user/appdata/filebrowser` | Location of `index.db`; must be on the array, not the flash drive |
| Crawl schedule | `0 3 * * *` (nightly at 03:00) | Standard cron expression |
| Crawler parallelism | 2 | Stat-only threads; increase on fast storage, decrease on spinning disks |
| Content indexing | disabled | Enable per root; only plain-text-like files are extracted in v1 |
| Content include paths | (none) | Path prefixes that receive content indexing within a root |
| Content extensions | `txt md go log conf cfg ini csv js ts py sh` | Extension allowlist for content extraction |
| Content size cap | 10 MB | Files larger than this are stat-indexed but not content-indexed |

A rescan can be triggered at any time from the Settings page or via the API.
Progress is visible in the SPA's index-status panel via server-sent events.

## Keeping private details out of the repo

`make test` runs `scripts/leakcheck.sh`, which refuses to pass if the tree
contains private IPv4 addresses, MAC addresses, or LAN-style host names.
Add your own patterns (for example your server's hostname) one per line to
`.leakcheck.local` — that file is git-ignored, so the patterns themselves are
never published. To run it before every commit:

```sh
printf '#!/bin/sh\nexec scripts/leakcheck.sh\n' > .git/hooks/pre-commit
chmod +x .git/hooks/pre-commit
```

## Security

- **Read-only v1.** No REST endpoint mutates filesystem data.  The only writes
  the daemon performs are to its index database and config file.
- **Unix socket, not TCP.** The daemon binds to `/var/run/filebrowserd.sock`
  (root-owned, mode 0600).  Nothing is reachable from the LAN.
- **Auth via webGUI.** Every request passes through `proxy.php`, which sits
  inside the authenticated webGUI.  If Unraid's session is not established the
  request never reaches the daemon.
- **Path confinement.** Every requested path is cleaned (resolving `..` and
  symlinks) and verified to fall within a configured index root before any
  filesystem operation.  Paths that escape roots return `403 FORBIDDEN`.
- **7zz subprocess.** The bundled 7zz is invoked with a fixed argument list (no
  shell), a wall-clock timeout, and a stdout size cap.  Extracted temporary
  files are written to the data directory and cleaned up after each request.
- **Daemon runs as root** to allow browsing all shares including those owned by
  other users.  Path confinement and read-only operation are the primary
  mitigations.

## Roadmap

The following items are planned but not included in v1:

- **Write operations** (rename, move, delete, upload) behind a per-root opt-in
  toggle on the Settings page.
- **inotify-based freshness** so the index updates within seconds of a file
  change rather than waiting for the next scheduled crawl.
- **PDF and office document text extraction** (pdftotext, LibreOffice headless,
  or a pure-Go library) to make document content searchable.
- **Saved search bookmarks** and persistent sort/filter preferences in the SPA.

## License

MIT — see LICENSE file.

### Third-party software

The packaged plugin bundles software from other projects:

- **7-Zip** (`7zz`, downloaded at package time from https://www.7-zip.org and
  shipped in the txz). Licensed under the GNU LGPL v2.1 with the unRAR
  restriction (the code may not be used to develop a RAR compressor); see
  https://www.7-zip.org/license.txt. The daemon invokes it as a separate
  process and does not link against it.
- **Go modules** compiled into `filebrowserd` (modernc.org/sqlite and its
  dependencies, golang.org/x/text, klauspost/compress, ulikunitz/xz,
  robfig/cron, and others) under BSD and MIT licenses.
- **JavaScript packages** compiled into the web app (React, ReactDOM,
  @tanstack/react-virtual) under the MIT license.

Full license texts for the compiled-in dependencies are collected under
`third_party/licenses/`.
