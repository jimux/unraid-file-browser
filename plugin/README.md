# Unraid plugin packaging

Everything in this directory is the Unraid side of the project: the `.plg`
install manifest and the tree that becomes
`/usr/local/emhttp/plugins/filebrowser/` on the server.

```
plugin/
├── filebrowser.plg                 install manifest (XML) - shipped to users
├── README.md                       this file
└── source/filebrowser/             -> /usr/local/emhttp/plugins/filebrowser/
    ├── FileBrowser.page            top-level "File Browser" tab (hosts the SPA)
    ├── FileBrowserSettings.page    Settings > User Utilities > File Browser Settings
    ├── proxy.php                   auth bridge: webGUI -> unix socket
    ├── rc.filebrowserd             start|stop|restart|status
    ├── default.cfg                 plugin defaults (SERVICE, DATA_DIR, BROWSE_ROOTS)
    ├── include/
    │   ├── settings.php            shared config/status helpers (no exec)
    │   └── exec.php                Start/Stop/Restart handler (the only exec)
    └── event/
        ├── disks_mounted           start after the array mounts (if autostart)
        └── stopping_svcs           stop before the array unmounts
```

## txz layout contract

`make package` (repo root Makefile) stages a tree and tars it as
`dist/filebrowser-<VERSION>-x86_64-1.txz`. Every entry is forced to `0/0`
(root:root) - GNU tar via `--owner=0 --group=0 --numeric-owner`, bsdtar via
`--uid 0 --gid 0 --uname root --gname root` - and the build fails if neither is
available or if any entry in the finished archive is not `0/0`. `installpkg`
preserves the ownership recorded in the archive, so a package built on macOS
without those flags would leave root-executed PHP and rc files owned (and
writable) by the build host's uid. The staged tree - which is exactly what
lands on the server - is:

| Path in the txz | Source | Notes |
| --- | --- | --- |
| `usr/local/emhttp/plugins/filebrowser/` | `plugin/source/filebrowser/` | pages, proxy, rc script, events |
| `usr/local/emhttp/plugins/filebrowser/app/` | `web/dist/` | built React SPA, served by emhttp's nginx |
| `usr/local/sbin/filebrowserd` | `daemon/filebrowserd-linux-amd64` | static linux/amd64, mode 0755 |
| `usr/local/sbin/7zz` | build cache | bundled static 7-Zip, mode 0755 |

Runtime paths that are *not* in the package:

| Path | Purpose |
| --- | --- |
| `/boot/config/plugins/filebrowser/settings.cfg` | persisted settings (survives update and uninstall) |
| `/boot/config/plugins/filebrowser/filebrowser-<version>-x86_64-1.txz` | the downloaded package; reinstalled from flash on every boot |
| `/var/run/filebrowserd.sock` | daemon listener (unix socket, no TCP port) |
| `/var/run/filebrowserd.pid` | pidfile written by `rc.filebrowserd` |
| `/var/log/filebrowserd.log` | daemon stdout/stderr |
| `<DATA_DIR>` (default `/mnt/user/appdata/filebrowser`) | index database + archive scratch |

The daemon is started as

```sh
/usr/local/sbin/filebrowserd -socket /var/run/filebrowserd.sock \
                             -data   "$DATA_DIR" \
                             -roots  "$BROWSE_ROOTS"
```

`-roots` takes the **comma-separated** list verbatim from `BROWSE_ROOTS` (one
argv element, quoted; embedded spaces in share names are fine). It is the
daemon's confinement boundary and is deliberately boot-time only - see below.

## Settings

`default.cfg` ships the defaults; the live copy is
`/boot/config/plugins/filebrowser/settings.cfg`, written by the webGUI's
`/update.php` (`#file=filebrowser/settings.cfg`). Both files are plain
`KEY="value"` ini - **no comments**, because PHP's `parse_ini_file()` rejects
`#` comment lines.

**These files are never `source`d.** `/update.php` copies POST field names and
values into `settings.cfg` verbatim, so a value like `DATA_DIR="/x$(id)"` - or
an injected `BIN=...` line - would be arbitrary code execution as root at the
next array start. Everything that needs a value reads it with the same literal
`sed` extraction instead:

* `rc.filebrowserd` defines `fb_cfg_get KEY FILE...` and validates every value;
* `event/disks_mounted` and the `.plg` install script duplicate the same
  three-line reader (neither can assume the rc script is already installed);
* the PHP pages use `parse_ini_file()` in `include/settings.php`.

Keys the plugin does not read are inert. A value that fails validation is
logged to `/var/log/filebrowserd.log`, replaced with the shipped default, and
flagged on the settings page.

| Key | Values | Meaning |
| --- | --- | --- |
| `SERVICE` | `enable` / `disable` | start with the array (and after a plugin install) |
| `DATA_DIR` | one absolute path, `^/[A-Za-z0-9._@/ -]+$`, not `/` | index database location |
| `BROWSE_ROOTS` | comma-separated absolute paths, each `^/[A-Za-z0-9._@/ -]*$` | everything the browser, viewer and indexer may reach (`filebrowserd -roots`) |

`BROWSE_ROOTS` is **not** editable from inside the app, and that is the point:
the daemon runs as root, so its confinement boundary must not be reachable
from the same HTTP surface it protects. `PUT /api/v1/index/config` may narrow
the *indexed* roots but every entry has to lie inside `-roots`; widening to `/`
from the API is impossible. Changes take effect on the next service restart
(the settings form restarts the daemon when it is running).

The crawl schedule, content-indexing rules and size caps are *daemon* config
(`GET/PUT /api/v1/index/config`), edited inside the app, stored in `DATA_DIR` -
not on the flash drive.

## SPA contract

`FileBrowser.page` embeds the SPA in an iframe:

```
/plugins/filebrowser/app/index.html?theme=<black|white|azure|gray>&csrf=<token>
```

* `theme` is `$display['theme']` narrowed the way the webGUI narrows it itself
  (`strtok($theme,'-')`, then whitelisted against the four theme names).
* `csrf` is the webGUI's `csrf_token`. It is required because Unraid enforces
  the token on **every POST** from its `auto_prepend_file`
  (`plugins/dynamix/include/local_prepend.php`) before `proxy.php` ever runs.
  The SPA must send it as the **`X-CSRF-TOKEN` header**, not as a form field:
  `local_prepend` removes a `csrf_token` field from `$_POST` but cannot remove
  it from `php://input`, which would leave the forwarded body and its
  `Content-Length` disagreeing. GET requests need no token.

All API traffic goes through
`/plugins/filebrowser/proxy.php?p=<urlencoded /api/v1 path + query>`; the
allowed grammar is `^/api/v1/[A-Za-z0-9/._-]*(\?.*)?$` and every byte of `p`
must be printable ASCII with no space (`\x21-\x7e`) - the SPA always
percent-encodes. GET and POST only.

`fs/raw` `Range` requests and the `index/events` SSE stream both work through
it: Range/Accept/Content-Type/Content-Length go in; Content-Type,
Content-Length, Content-Disposition, Content-Range, Accept-Ranges,
Cache-Control, Content-Security-Policy and X-Content-Type-Options come back.
Nothing else is relayed (upstream `Set-Cookie` and `Location` are dropped).

Four rules the bridge enforces on its own, so that a version skew between
plugin and daemon cannot open a hole:

* **POST bodies must be `application/json`** (415 otherwise). The SPA sends
  nothing else, and a cross-origin `<form>` cannot produce that content type.
* **The csrf_token is verified in `proxy.php` too**, against `csrf_token` in
  `/var/local/emhttp/var.ini` with `hash_equals()` - not only by Unraid's
  `auto_prepend_file`. Missing or wrong token → 403 JSON envelope.
* **`fs/raw` is hardened independently of the daemon**: every response gets
  `X-Content-Type-Options: nosniff` and
  `Content-Security-Policy: default-src 'none'; sandbox`, and anything whose
  Content-Type is outside the inline allowlist (`image/png|jpeg|gif|webp|bmp|
  avif`, `audio/*`, `video/*`, `application/pdf`, `text/plain`) is rewritten to
  `application/octet-stream` + `Content-Disposition: attachment`. This mirrors
  the daemon's own raw content policy (API.md); either side alone is enough.
  A file served inline on the webGUI origin would be stored XSS with the
  logged-in admin's (root) session, so both sides enforce it.
* **`X-HTTP-Method-Override` is validated before the socket is opened**, so a
  rejected request never reaches the daemon. Only `PUT` is accepted.

### SSE budgets

An event stream pins one php-fpm worker for its whole life, so `proxy.php`
owns its own lifetime rather than waiting on the daemon:

| Budget | Value | Why |
| --- | --- | --- |
| poll / ping interval | 5 s | `stream_select()`; on a timeout it writes `: ping` (a comment frame `EventSource` ignores) - the write is what makes `connection_aborted()` true for a closed tab |
| silence cutoff | 60 s | no bytes from the daemon at all (its own `: keepalive` is every 15 s) |
| hard lifetime cap | 600 s | `EventSource` reconnects transparently |

A closed tab is detected within two ping intervals (~10 s), because the first
write after the client goes away is what triggers the peer reset and the second
one fails. The socket is closed on every exit path.

## Version bump procedure

1. Nothing in this directory needs editing for a normal release. The version is
   whatever `make package` is given:

   ```sh
   make package                      # VERSION defaults to $(date +%Y.%m.%d)
   make package VERSION=2026.09.01a  # explicit (letter suffix for a same-day respin)
   ```

   `make package` rewrites the two entity lines in a *copy* of the manifest and
   writes `dist/filebrowser.plg`; the checked-in `plugin/filebrowser.plg` keeps
   its placeholder values. The sed patterns are literal, so these two lines must
   keep exactly one space and double quotes:

   ```xml
   <!ENTITY version "2026.09.01">
   <!ENTITY md5 "00000000000000000000000000000000">
   ```

2. Add a `###<version>` block at the top of `<CHANGES>` in
   `plugin/filebrowser.plg` describing the release.

3. The `github` entity points at `jimux/unraid-file-browser`; if the repo
   ever moves, change that one entity. The manifest resolves:

   * `pluginURL` -> `https://github.com/jimux/unraid-file-browser/releases/latest/download/filebrowser.plg`
     (what Unraid re-downloads for "check for updates" - it must always point at
     the newest manifest, hence `/releases/latest/`)
   * `packageURL` -> `https://github.com/jimux/unraid-file-browser/releases/download/v<version>/filebrowser-<version>-x86_64-1.txz`

4. Publish a GitHub release tagged `v<version>` with **both**
   `dist/filebrowser-<version>-x86_64-1.txz` and `dist/filebrowser.plg`
   attached.

## Manual install for testing

Without a release, install straight from the flash drive:

```sh
# on the build host
make package VERSION=2026.09.01
scp dist/filebrowser-<version>-x86_64-1.txz root@tower:/boot/config/plugins/filebrowser/
scp dist/filebrowser.plg                    root@tower:/boot/config/plugins/

# on the server
removepkg filebrowser-<old-version>-x86_64-1   # Unraid's upgradepkg may silently skip; see the .plg install script
installpkg /boot/config/plugins/filebrowser/filebrowser-<version>-x86_64-1.txz
/usr/local/emhttp/plugins/filebrowser/rc.filebrowserd restart
```

The webGUI picks up new `.page` files immediately - just reload the browser.
The top-level `File Browser` tab and `Settings -> User Utilities -> File Browser
Settings` should both appear.

Faster loop while iterating on the PHP/pages only (no repackaging):

```sh
rsync -a plugin/source/filebrowser/ root@tower:/usr/local/emhttp/plugins/filebrowser/
ssh root@tower chmod 755 /usr/local/emhttp/plugins/filebrowser/rc.filebrowserd \
                          /usr/local/emhttp/plugins/filebrowser/event/*
```

Note this is overwritten by the next package install, and `/usr/local/emhttp` is
on tmpfs - it does not survive a reboot. Anything permanent has to go through
the txz.

To exercise the full `.plg` path (download + MD5 + install script) point the
`packageURL` entity at a reachable URL, or install the local manifest with:

```sh
plugin install /boot/config/plugins/filebrowser.plg
plugin remove filebrowser.plg
```

Useful checks on the server:

```sh
/usr/local/emhttp/plugins/filebrowser/rc.filebrowserd status
tail -f /var/log/filebrowserd.log
curl -s --unix-socket /var/run/filebrowserd.sock http://localhost/api/v1/healthz
```

## Uninstall behaviour

`plugin remove filebrowser.plg` stops the daemon, `removepkg`s every installed
`filebrowser-*` package and deletes
`/usr/local/emhttp/plugins/filebrowser/`. It deliberately keeps
`/boot/config/plugins/filebrowser/` (settings) and the index in `DATA_DIR`, so
reinstalling picks up where the user left off. Removing those is a manual `rm`.
