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
- **Image preview** for common raster formats; download always available.
- **Play video and audio** in a separate player window, streamed straight from
  the array with seeking. Arrow keys step to the previous/next media file in
  the directory's current sort order, with optional auto-advance. Playback is
  native to the browser (no transcoding): MP4/H.264, WebM, most MOV and MKV
  with H.264, and common audio formats play; others get a download prompt.
- **Transcode on the fly** for anything the browser cannot play natively —
  remuxed or transcoded to HLS with a quality selector in the player. This
  needs `ffmpeg`, which ships as a **separate companion plugin** (see
  [Two plugins](#two-plugins) below); everything else works without it.
  Supports software decoding of h264, hevc, mpeg4, mpeg2, vc1, vp9, av1, ac3,
  eac3, dts, aac and mp3; software encoding via libx264 and the native AAC
  encoder; mpegts and mp4 muxing. Hardware-accelerated encoding (VAAPI) is not
  available in this build because VAAPI requires runtime-linked libraries
  incompatible with a fully-static binary.

## Two plugins

The project ships as two independent Unraid plugins:

| Plugin | Package | Size | Contains |
|---|---|---|---|
| **File Browser** (`filebrowser`) | `filebrowser-<date>-x86_64-1.txz` | ~5 MB | webGUI pages, the SPA, `filebrowserd`, `7zz` |
| **File Browser ffmpeg** (`filebrowser-ffmpeg`) | `filebrowser-ffmpeg-7.0.2-x86_64-1.txz` | ~40 MB | `ffmpeg` and `ffprobe` only |

They are split because Unraid's plugin manager re-downloads the whole package
and rewrites it to the **USB boot flash** on every update. Bundling ffmpeg
would have meant ~190 MB of flash writes for every release of a 5 MB app. The
companion is versioned by **ffmpeg's own version** (`7.0.2`), not by the file
browser's date version, so it is only rewritten when ffmpeg itself is bumped.

The companion installs into a private directory:

```
/usr/local/filebrowser/bin/ffmpeg
/usr/local/filebrowser/bin/ffprobe
```

`filebrowserd` looks for the binaries in this order:

1. `/usr/local/filebrowser/bin` — the companion plugin
2. `/usr/local/sbin` — installs that predate the split
3. `$PATH` — anything the admin installed another way

A private directory means the companion can neither shadow nor be shadowed by
an ffmpeg installed some other way. Without any of them the daemon reports
`available: false` and the media endpoints answer `UNAVAILABLE`; browsing,
search, viewing, downloads and native playback are unaffected.

**Either order works.** The daemon probes for ffmpeg when it starts, and the
companion's install and remove scripts restart `filebrowserd` if it is already
running, so whichever plugin you install second is picked up immediately.
**Settings → User Utilities → File Browser Settings** shows a *Transcoding*
row with the detected ffmpeg version and path, or "Not installed" plus the URL
to install the companion.

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
    |-- 7zz subprocess  (long-tail archive formats)
    `-- ffmpeg/ffprobe subprocesses  (HLS transcoding; companion plugin, optional)
```

The daemon never opens a TCP port.  All traffic goes through the webGUI's own
authenticated HTTP layer, so no additional authentication mechanism is needed
and nothing is exposed on the local network.

## Features

| Area | Details |
|---|---|
| Directory listing | Sorted by name, size, mtime, or type; ascending or descending; directories-first option; paged (up to 10 000 entries per request) |
| File viewer | Text with encoding selection; windowed (256 KB per page) so 40 GB logs are fine; hex view; image preview |
| Media player | Separate window; HTTP Range streaming (seekable); ↑/↓ previous/next in the directory's sort order; auto-advance; native browser codecs, plus HLS transcoding when the `filebrowser-ffmpeg` companion plugin is installed |
| Context menu | Right-click any row: open, play, view as text or hex, details, download, copy path or name |
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
    ├── filebrowser-ffmpeg.plg     companion plugin manifest (ffmpeg/ffprobe only)
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
5. Archives the tree to `dist/filebrowser-VERSION-x86_64-1.txz` with every entry
   forced to root ownership, then re-reads the archive and fails the build if any
   entry is not `0/0`.
6. Computes the MD5 of the txz, prints it, and writes a patched copy of
   `plugin/filebrowser.plg` to `dist/` with the `version` and `md5` XML
   entities updated.

ffmpeg is **not** part of this package — see the next target.

### Build the ffmpeg companion package

```
make package-ffmpeg
```

Takes no `VERSION`: it is versioned by `FFMPEG_VERSION` in the Makefile (`7.0.2`).
The target:
1. Downloads static ffmpeg and ffprobe (johnvansickle.com GPL v3 build, SHA-256
   pinned and verified, ~40 MB compressed / ~76 MB each uncompressed) into
   `build/cache/` on the first run; subsequent runs reuse the cached copies.
2. Stages them under `build/pkg-ffmpeg/usr/local/filebrowser/bin/`, mode 0755.
3. Archives to `dist/filebrowser-ffmpeg-7.0.2-x86_64-1.txz` with the same forced
   root ownership and the same `0/0` assertion.
4. Computes the MD5 and writes `dist/filebrowser-ffmpeg.plg` with the `version`
   and `md5` entities stamped.

### Build both

```
make package-all [VERSION=YYYY.MM.DD]
```

Runs `package` then `package-ffmpeg`. The resulting `dist/` contains everything
needed to release both plugins:

```
filebrowser.plg                        filebrowser-VERSION-x86_64-1.txz
filebrowser-ffmpeg.plg                 filebrowser-ffmpeg-7.0.2-x86_64-1.txz
```

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
and `web/dist/`.  That includes both staging trees (`build/pkg`,
`build/pkg-ffmpeg`) and the download cache (`build/cache`), so the next
`make package-all` re-downloads and re-verifies 7zz and ffmpeg.

## Installing on Unraid

In the Unraid webGUI go to **Plugins → Install Plugin** and paste a plugin URL.
Install one or both; either order works.

**File Browser** (required):

```
https://github.com/jimux/unraid-file-browser/releases/latest/download/filebrowser.plg
```

**File Browser ffmpeg** (optional — only needed for transcoding):

```
https://github.com/jimux/unraid-file-browser/releases/latest/download/filebrowser-ffmpeg.plg
```

Unraid downloads each txz to the flash drive, verifies it against the MD5 in the
manifest, and runs the manifest's install script. **Settings → User Utilities →
File Browser Settings** shows a *Transcoding* row: "Available — ffmpeg &lt;version&gt;
(&lt;path&gt;)" once the companion is in, or "Not installed" with the URL above.

Without the companion plugin the file browser is fully functional; only
transcoding is unavailable, and files the browser can decode itself still play.

### Manual install (for development builds)

1. Copy the txz to the flash drive under the plugin's own directory:
   ```sh
   scp dist/filebrowser-VERSION-x86_64-1.txz       root@tower:/boot/config/plugins/filebrowser/
   scp dist/filebrowser-ffmpeg-7.0.2-x86_64-1.txz  root@tower:/boot/config/plugins/filebrowser-ffmpeg/
   ```
2. From an Unraid terminal run:
   ```sh
   installpkg /boot/config/plugins/filebrowser/filebrowser-VERSION-x86_64-1.txz
   installpkg /boot/config/plugins/filebrowser-ffmpeg/filebrowser-ffmpeg-7.0.2-x86_64-1.txz
   ```
   Use `removepkg <old-package-name>` first when replacing an installed version:
   Unraid's `upgradepkg` may silently skip a package it judges "not newer".
3. Start (or restart) the service:
   ```sh
   /usr/local/emhttp/plugins/filebrowser/rc.filebrowserd restart
   ```
4. The **File Browser** tab will appear in the webGUI.

To uninstall, use **Plugins → Installed Plugins** in the webGUI, or run:
```sh
removepkg filebrowser-VERSION-x86_64-1
removepkg filebrowser-ffmpeg-7.0.2-x86_64-1
```

Removing the file browser leaves the index database and the plugin config under
`/boot/config/plugins/filebrowser/` in place. Removing the companion deletes
`/usr/local/filebrowser/bin` and nothing else.

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

### RAM cost of bundled binaries

`/usr/local` on Unraid is a tmpfs RAM disk, so every byte of the installed
binaries counts against available RAM:

| Binary | Plugin | Uncompressed size |
|---|---|---|
| `filebrowserd` | File Browser | ~10 MB |
| `7zz` | File Browser | ~3 MB |
| **File Browser total** | | **~13 MB** |
| `ffmpeg` | File Browser ffmpeg | ~76 MB |
| `ffprobe` | File Browser ffmpeg | ~76 MB |
| **File Browser ffmpeg total** | | **~152 MB** |

The companion plugin therefore costs roughly **152 MB of RAM** while it is
installed — significantly above the ~80 MB originally estimated for it. Unraid
typically has several gigabytes free, so this is unlikely to matter in practice,
but it is worth noting on systems with less than 2 GB of RAM. If you never
transcode, simply do not install the companion.

### Third-party software

The **File Browser ffmpeg** companion plugin ships:

- **FFmpeg** 7.0.2 (`ffmpeg` and `ffprobe`, downloaded at package time from
  https://johnvansickle.com/ffmpeg/ and shipped in the companion txz at
  `/usr/local/filebrowser/bin/`). This is an unmodified fully-static GPL v3
  build. It includes software decoders for
  h264/hevc/mpeg4/mpeg2/vc1/vp9/av1/dts/ac3/eac3/aac/mp3 and encoders
  libx264 and aac. Licensed under the **GNU General Public License version 3**;
  see the license text at `third_party/licenses/ffmpeg-GPLv3.txt` and source
  code at https://git.ffmpeg.org/ffmpeg.git (tag n7.0.2). The daemon invokes
  these binaries as separate processes and does not link against them (mere
  aggregation under GPL v3). VAAPI hardware-accelerated encoding is absent
  because it requires runtime-linked libraries incompatible with a fully-static
  build.

The main **File Browser** plugin bundles software from other projects:

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
