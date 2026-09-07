# web/ — File Browser SPA

React + TypeScript + Vite single-page app for the Unraid file-browser plugin.
It is the only UI: the plugin page loads `app/index.html` in an iframe and the
SPA talks to `filebrowserd` exclusively over the REST contract in
[`../API.md`](../API.md).

Runtime dependencies are deliberately three: `react`, `react-dom`,
`@tanstack/react-virtual`. Routing (hash-based), fetching, theming and the
snippet sanitizer are hand-rolled — no router, no state library, no CSS
framework.

## Scripts

| command             | what it does                                              |
| ------------------- | --------------------------------------------------------- |
| `npm run dev`       | Vite dev server on :5173, proxying `/api` → the daemon      |
| `npm run typecheck` | `tsc --noEmit`                                             |
| `npm run build`     | typecheck, then a production bundle into `dist/`            |
| `npm run preview`   | serve `dist/` locally (API calls need a `proxy.php` — see below) |

## Dev workflow (no Unraid box needed)

Two processes:

```sh
# 1. daemon in dev mode — TCP loopback instead of the unix socket, auth off,
#    CORS *, and -root to sandbox it to a scratch tree
cd ../daemon && go run ./cmd/filebrowserd -dev -listen 127.0.0.1:8384 -root /tmp/fbtest

# 2. the SPA
cd ../web && npm run dev     # http://localhost:5173
```

`vite.config.ts` proxies `/api` → `http://127.0.0.1:8384`, so in dev
`apiUrl("/fs/list?…")` resolves to `/api/v1/fs/list?…` and Vite forwards it.
SSE (`/api/v1/index/events`) passes through the dev proxy unbuffered.

Themes are switchable in dev via the query string:
`http://localhost:5173/?theme=black` (also `white`, `azure`, `gray`).

If the daemon is not running, every panel degrades to an error banner with a
`NETWORK` code, and the Settings page's live-status stream falls back to 2 s
polling — that is the intended behaviour, not a crash.

## Production build

`npm run build` emits `dist/` with `base: "./"`, i.e. every asset URL is
relative to the document. `make web` (repo root) copies `dist/` to
`plugin/source/filebrowser/app/`, which installs to:

```
/usr/local/emhttp/plugins/filebrowser/
├── app/                 # this build: index.html + assets/
├── proxy.php            # auth bridge → /var/run/filebrowserd.sock
├── FileBrowser.page
└── …
```

### What the SPA expects from the packaging side

1. **Iframe URL.** The page must load `app/index.html` with the webGUI's
   current theme as a query parameter:
   `plugins/filebrowser/app/index.html?theme=<white|black|azure|gray>`.
   The value maps to a `body.theme-*` class; anything unrecognised falls back
   to `white`. A theme carried in the hash query
   (`…/index.html#/browse/%2Fmnt%2Fuser?theme=black`) is also honoured.
   To retheme without reloading the iframe, `postMessage` to it:
   `{ type: "filebrowser:theme", theme: "black" }`.
2. **`proxy.php` location.** In a production build every request is
   `../proxy.php?p=<urlencoded "/api/v1" + path + query>` relative to
   `app/index.html`, i.e. `/plugins/filebrowser/proxy.php`. There is exactly
   one URL builder (`apiUrl()` in `src/api/client.ts`); nothing else constructs
   daemon URLs.
3. **What `proxy.php` must forward.**
   - Method (`GET`, `PUT`, `POST`) and the JSON request body verbatim
     (`PUT /index/config`, `POST /index/rescan|pause|resume`).
   - Response status **and** body verbatim: the SPA reads the `{ok,data}` /
     `{ok:false,error:{code,message}}` envelope and shows `error.code` in the
     UI, so an HTML error page or a rewritten status hides the real failure.
   - `Content-Type` for raw bytes (`fs/raw` feeds `<img src>` and the download
     link) plus `Content-Disposition` when `download=1`. `Range` should be
     passed through for real files; archive entries stream without it.
   - **SSE**: `/index/events` must stream unbuffered (no output buffering, no
     gzip, no `Content-Length`) or the browser never sees a frame. If it does
     buffer, the UI notices within 6 s and switches to 2 s polling of
     `/index/status`, so a buffering bridge degrades rather than breaks.
   - Cookies/session are the webGUI's own; requests are sent
     `credentials: "same-origin"`.
4. **No CSP surprises.** Assets are same-origin and the bundle has no external
   requests. Snippets from `/search` are rendered with everything except
   `<mark>` stripped client-side (`src/lib/sanitize.ts`), so the bridge does not
   need to sanitize them.

## Layout

```
src/
├── api/          client.ts (apiUrl + typed endpoint wrappers + ApiError)
│                 events.ts (SSE with polling fallback), types.ts (API.md mirror)
├── components/   TopBar, Breadcrumbs (archive chips), TreeSidebar, Icon, Feedback
├── hooks/        useRoute, useDirListing (paged), useAsync/useDebounced, useTheme
├── lib/          router.ts (hash routes), paths.ts (virtual "!/" paths),
│                 format.ts, sanitize.ts
├── views/        BrowserView (virtualized table), ViewerPanel (text/hex/image/
│                 download), SearchView, SettingsView
└── styles.css    all four palettes as CSS variables
```

Routes: `#/browse/<encoded path>`, `#/view/<encoded path>`, `#/search?…`,
`#/settings`. Paths are percent-encoded whole, so the `!/` archive separator
never collides with the route grammar and stays an opaque string owned by the
daemon.
