import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ApiError, search as apiSearch } from "../api/client";
import type { Entry, SearchMode, SearchResult } from "../api/types";
import { EmptyState, ErrorBanner, SkeletonRows, Spinner } from "../components/Feedback";
import { EntryIcon, Icon } from "../components/Icon";
import { ContextMenu, isMenuKey, useContextMenu } from "../components/ContextMenu";
import { useDebounced } from "../hooks/useAsync";
import { formatAbsolute, formatRelative, formatSize } from "../lib/format";
import { buildEntryMenu } from "../lib/entryMenu";
import { dirOf } from "../lib/paths";
import { SETTINGS_HREF, navigate, searchHref, syncUrlSilently, viewHref } from "../lib/router";
import { sanitizeSnippet } from "../lib/sanitize";

const PAGE = 100;

/** Hard byte-ish ceiling on one result snippet before sanitising/rendering. */
const SNIPPET_CHARS = 4096;

const SIZE_UNITS: Array<{ id: string; label: string; mult: number }> = [
  { id: "B", label: "B", mult: 1 },
  { id: "KiB", label: "KiB", mult: 1024 },
  { id: "MiB", label: "MiB", mult: 1024 ** 2 },
  { id: "GiB", label: "GiB", mult: 1024 ** 3 },
];

interface FormState {
  q: string;
  mode: SearchMode;
  scope: string;
  everywhere: boolean;
  ext: string;
  minSize: string;
  minUnit: string;
  maxSize: string;
  maxUnit: string;
  after: string; // yyyy-mm-dd
  before: string;
}

function initialForm(params: URLSearchParams, currentDir: string): FormState {
  const scopeParam = params.get("path");
  return {
    q: params.get("q") ?? "",
    mode: (params.get("mode") as SearchMode) || "both",
    scope: scopeParam ?? currentDir,
    everywhere: params.has("path") ? !scopeParam : !currentDir,
    ext: params.get("ext") ?? "",
    minSize: params.get("minSize") ? String(Number(params.get("minSize"))) : "",
    minUnit: "B",
    maxSize: params.get("maxSize") ? String(Number(params.get("maxSize"))) : "",
    maxUnit: "B",
    after: unixToDateInput(params.get("after")),
    before: unixToDateInput(params.get("before")),
  };
}

function unixToDateInput(v: string | null): string {
  if (!v) return "";
  const n = Number(v);
  if (!Number.isFinite(n) || n <= 0) return "";
  const d = new Date(n * 1000);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

function dateInputToUnix(v: string, endOfDay: boolean): number | undefined {
  if (!v) return undefined;
  const t = Date.parse(endOfDay ? `${v}T23:59:59` : `${v}T00:00:00`);
  return Number.isNaN(t) ? undefined : Math.floor(t / 1000);
}

function bytesOf(value: string, unit: string): number | undefined {
  if (!value.trim()) return undefined;
  const n = Number(value);
  if (!Number.isFinite(n) || n < 0) return undefined;
  const u = SIZE_UNITS.find((s) => s.id === unit) ?? SIZE_UNITS[0];
  return Math.round(n * u.mult);
}

export function SearchView({
  params,
  currentDir,
  onNavigate,
  onOpenFile,
}: {
  params: URLSearchParams;
  currentDir: string;
  onNavigate: (p: string) => void;
  onOpenFile: (p: string, mime?: string, name?: string) => void;
}) {
  const [form, setForm] = useState<FormState>(() => initialForm(params, currentDir));
  const [result, setResult] = useState<SearchResult | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [offset, setOffset] = useState(0);
  const { menu, openAt, openAtElement, close: closeMenu } = useContextMenu<Entry>();

  /**
   * The results have no sort of their own (relevance order is not something
   * `fs/list` can reproduce), so the player gets the daemon's name/asc default
   * for its ↑/↓ — the same thing a click on the hit name already does.
   */
  const openHit = useCallback(
    (e: Entry) => (e.type === "dir" || e.type === "archive" ? onNavigate(e.path) : onOpenFile(e.path, e.mime, e.name)),
    [onNavigate, onOpenFile],
  );

  const menuItems = useMemo(
    () =>
      menu ? buildEntryMenu(menu.target, { open: openHit, details: (e) => navigate(viewHref(e.path)) }) : [],
    [menu, openHit],
  );

  const set = <K extends keyof FormState>(k: K, v: FormState[K]) =>
    setForm((f) => ({ ...f, [k]: v }));

  const debounced = useDebounced(form, 300);

  // Debounced query params, memoised so the fetch effect keys off a string.
  const query = useMemo(() => {
    const f = debounced;
    const sp = new URLSearchParams();
    if (f.q.trim()) sp.set("q", f.q.trim());
    sp.set("mode", f.mode);
    if (!f.everywhere && f.scope.trim()) sp.set("path", f.scope.trim());
    if (f.ext.trim()) sp.set("ext", normaliseExts(f.ext));
    const min = bytesOf(f.minSize, f.minUnit);
    const max = bytesOf(f.maxSize, f.maxUnit);
    if (min !== undefined) sp.set("minSize", String(min));
    if (max !== undefined) sp.set("maxSize", String(max));
    const after = dateInputToUnix(f.after, false);
    const before = dateInputToUnix(f.before, true);
    if (after !== undefined) sp.set("after", String(after));
    if (before !== undefined) sp.set("before", String(before));
    return sp;
  }, [debounced]);

  const queryKey = query.toString();

  // Reset paging whenever the query itself changes.
  const lastKeyRef = useRef(queryKey);
  useEffect(() => {
    if (lastKeyRef.current !== queryKey) {
      lastKeyRef.current = queryKey;
      setOffset(0);
    }
  }, [queryKey]);

  // Keep the address bar shareable without remounting on every keystroke.
  useEffect(() => {
    syncUrlSilently(searchHref(queryKey));
  }, [queryKey]);

  useEffect(() => {
    const q = query.get("q");
    if (!q) {
      setResult(null);
      setError(null);
      setLoading(false);
      return;
    }
    const ac = new AbortController();
    let alive = true;
    setLoading(true);
    apiSearch(
      {
        q,
        mode: (query.get("mode") as SearchMode) || "both",
        path: query.get("path") ?? undefined,
        ext: query.get("ext") ?? undefined,
        minSize: numOrUndef(query.get("minSize")),
        maxSize: numOrUndef(query.get("maxSize")),
        after: numOrUndef(query.get("after")),
        before: numOrUndef(query.get("before")),
        limit: PAGE,
        offset,
      },
      ac.signal,
    )
      .then((r) => {
        if (!alive) return;
        setResult(r);
        setError(null);
      })
      .catch((e: unknown) => {
        if (!alive) return;
        if (e instanceof DOMException && e.name === "AbortError") return;
        setError(e instanceof ApiError ? e : new ApiError("INTERNAL", String(e)));
        setResult(null);
      })
      .finally(() => alive && setLoading(false));
    return () => {
      alive = false;
      ac.abort();
    };
  }, [queryKey, offset]); // eslint-disable-line react-hooks/exhaustive-deps

  const nowSec = Date.now() / 1000;
  const hits = result?.hits ?? [];
  const emptyIndex = error?.code === "INDEXING" || (result && result.total === 0 && !result.indexFresh);

  return (
    <section className="search" aria-label="Search">
      <div className="search-form">
        <div className="search-row">
          <label className="field grow">
            <span className="field-label">Query</span>
            <input
              className="input"
              autoFocus
              value={form.q}
              placeholder="filename or content terms — FTS5 quoting supported"
              onChange={(e) => set("q", e.target.value)}
            />
          </label>
          <label className="field">
            <span className="field-label">Mode</span>
            <select className="input select" value={form.mode} onChange={(e) => set("mode", e.target.value as SearchMode)}>
              <option value="both">Name + content</option>
              <option value="name">Name only</option>
              <option value="content">Content only</option>
            </select>
          </label>
        </div>

        <div className="search-row">
          <label className="field grow">
            <span className="field-label">Scope</span>
            <input
              className="input"
              value={form.everywhere ? "" : form.scope}
              disabled={form.everywhere}
              placeholder={form.everywhere ? "everywhere (all index roots)" : "/mnt/user/…"}
              onChange={(e) => set("scope", e.target.value)}
            />
          </label>
          <label className="field check-field">
            <input type="checkbox" checked={form.everywhere} onChange={(e) => set("everywhere", e.target.checked)} />
            Everywhere
          </label>
          <label className="field">
            <span className="field-label">Extensions</span>
            <input
              className="input"
              value={form.ext}
              placeholder="go, md, log"
              onChange={(e) => set("ext", e.target.value)}
            />
          </label>
        </div>

        <div className="search-row">
          <label className="field">
            <span className="field-label">Min size</span>
            <span className="input-group">
              <input
                className="input num"
                type="number"
                min="0"
                value={form.minSize}
                onChange={(e) => set("minSize", e.target.value)}
              />
              <select className="input select unit" value={form.minUnit} onChange={(e) => set("minUnit", e.target.value)}>
                {SIZE_UNITS.map((u) => (
                  <option key={u.id} value={u.id}>
                    {u.label}
                  </option>
                ))}
              </select>
            </span>
          </label>
          <label className="field">
            <span className="field-label">Max size</span>
            <span className="input-group">
              <input
                className="input num"
                type="number"
                min="0"
                value={form.maxSize}
                onChange={(e) => set("maxSize", e.target.value)}
              />
              <select className="input select unit" value={form.maxUnit} onChange={(e) => set("maxUnit", e.target.value)}>
                {SIZE_UNITS.map((u) => (
                  <option key={u.id} value={u.id}>
                    {u.label}
                  </option>
                ))}
              </select>
            </span>
          </label>
          <label className="field">
            <span className="field-label">Modified after</span>
            <input className="input" type="date" value={form.after} onChange={(e) => set("after", e.target.value)} />
          </label>
          <label className="field">
            <span className="field-label">Modified before</span>
            <input className="input" type="date" value={form.before} onChange={(e) => set("before", e.target.value)} />
          </label>
        </div>
      </div>

      <div className="search-status">
        {loading ? <Spinner label="searching…" /> : null}
        {result ? (
          <>
            <strong>{result.total.toLocaleString()}</strong>
            <span className="muted"> match{result.total === 1 ? "" : "es"} in {result.tookMs} ms</span>
            {!result.indexFresh ? (
              <span className="pill pill-warn" title="A crawl is pending or in progress — results may be stale">
                index may be stale
              </span>
            ) : null}
          </>
        ) : null}
      </div>

      {error ? <ErrorBanner error={error} /> : null}

      <div className="search-results">
        {loading && !result ? <SkeletonRows rows={10} cols={3} /> : null}

        {!loading && !form.q.trim() ? (
          <EmptyState title="Type to search">
            Name matching is prefix-based; content matching needs the content index enabled in{" "}
            <a href={SETTINGS_HREF}>Settings</a>.
          </EmptyState>
        ) : null}

        {!loading && result && hits.length === 0 && form.q.trim() ? (
          <EmptyState title="No matches">
            {emptyIndex ? (
              <>
                The index looks empty or unavailable. Configure roots and run a scan in{" "}
                <a href={SETTINGS_HREF}>Settings</a>.
              </>
            ) : (
              <>Try a broader scope, a different mode, or loosening the size/date filters.</>
            )}
          </EmptyState>
        ) : null}

        {hits.map((hit) => {
          const e = hit.entry;
          const dir = dirOf(e.path);
          return (
            <article
              className="hit"
              key={`${e.path}-${hit.score}`}
              onContextMenu={(ev) => {
                ev.preventDefault();
                openAt(ev, e);
              }}
              onKeyDown={(ev) => {
                if (!isMenuKey(ev)) return;
                ev.preventDefault();
                openAtElement(ev.currentTarget, e);
              }}
            >
              <div className="hit-main">
                <EntryIcon type={e.type} />
                <button
                  type="button"
                  className="hit-name"
                  title={e.path}
                  onClick={() => openHit(e)}
                >
                  {e.name}
                </button>
                <span className={`pill pill-muted matched-${hit.matchedIn}`}>{hit.matchedIn}</span>
              </div>
              <div className="hit-sub">
                <button type="button" className="hit-dir" title={`Browse ${dir}`} onClick={() => onNavigate(dir)}>
                  <Icon name="folder" />
                  {dir}
                </button>
                <span className="muted">{formatSize(e.size)}</span>
                <span className="muted" title={formatAbsolute(e.mtime)}>
                  {formatRelative(e.mtime, nowSec)}
                </span>
              </div>
              {hit.snippet ? (
                <div
                  className="hit-snippet"
                  // Sanitised server HTML: everything escaped except <mark>.
                  // The server bounds a snippet by tokens, not bytes, so a
                  // single-token file of megabytes can still arrive huge —
                  // clamp before sanitising and before it reaches the DOM.
                  dangerouslySetInnerHTML={{ __html: sanitizeSnippet(hit.snippet.slice(0, SNIPPET_CHARS)) }}
                />
              ) : null}
            </article>
          );
        })}
      </div>

      {result && result.total > PAGE ? (
        <div className="pager search-pager">
          <span className="pager-label">
            {offset + 1}–{Math.min(offset + hits.length, result.total)} of {result.total.toLocaleString()}
          </span>
          <button type="button" className="btn btn-sm" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE))}>
            ‹ Prev
          </button>
          <button
            type="button"
            className="btn btn-sm"
            disabled={offset + hits.length >= result.total}
            onClick={() => setOffset(offset + PAGE)}
          >
            Next ›
          </button>
        </div>
      ) : null}

      {menu ? (
        <ContextMenu
          x={menu.x}
          y={menu.y}
          items={menuItems}
          onClose={closeMenu}
          label={`Actions for ${menu.target.name}`}
        />
      ) : null}
    </section>
  );
}

function normaliseExts(raw: string): string {
  return raw
    .split(/[,\s]+/)
    .map((s) => s.trim().replace(/^\./, "").toLowerCase())
    .filter(Boolean)
    .join(",");
}

function numOrUndef(v: string | null): number | undefined {
  if (v === null || v === "") return undefined;
  const n = Number(v);
  return Number.isFinite(n) ? n : undefined;
}
