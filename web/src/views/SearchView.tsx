import { useCallback, useEffect, useMemo, useState } from "react";
import {
  ApiError,
  search as apiSearch,
  searchFields,
  searchQuery,
  searchValues,
} from "../api/client";
import type {
  Entry,
  SearchCategory,
  SearchField,
  SearchMode,
  SearchParams,
  SearchResult,
  SearchSort,
  SearchValueCount,
  SortDir,
} from "../api/types";
import { EmptyState, ErrorBanner, SkeletonRows, Spinner } from "../components/Feedback";
import { EntryIcon, Icon } from "../components/Icon";
import { ContextMenu, isMenuKey, useContextMenu } from "../components/ContextMenu";
import { formatAbsolute, formatRelative, formatSize } from "../lib/format";
import { buildEntryMenu } from "../lib/entryMenu";
import { dirOf } from "../lib/paths";
import { SETTINGS_HREF, navigate, searchHref, syncUrlSilently, viewHref } from "../lib/router";
import { sanitizeSnippet } from "../lib/sanitize";
import {
  BYTE_UNITS,
  DEFAULT_BYTE_UNIT,
  FALLBACK_CATEGORIES,
  OPS,
  buildSearchParams,
  canSearch,
  defaultOp,
  effectiveSort,
  findCategory,
  findField,
  MAX_CONDS,
  newCond,
  parseSearchForm,
  preFilterExtensions,
  typeCategoriesInUse,
  type Cond,
  type SearchForm,
} from "../lib/searchQuery";

const PAGE = 100;

/** Hard byte-ish ceiling on one result snippet before sanitising/rendering. */
const SNIPPET_CHARS = 4096;

/** How many extensions the pre-filter label names before it says "…". */
const EXT_PREVIEW = 4;

/** One search that has actually been issued: its wire query and its params. */
interface RunState {
  key: string;
  params: SearchParams;
  /** Bumped by a re-run of an unchanged query, so the effect refires. */
  nonce: number;
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
  const [cats, setCats] = useState<SearchCategory[] | null>(null);
  const [form, setForm] = useState<SearchForm | null>(null);
  const [run, setRun] = useState<RunState | null>(null);
  const [result, setResult] = useState<SearchResult | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [offset, setOffset] = useState(0);
  const [extOpen, setExtOpen] = useState(false);
  const { menu, openAt, openAtElement, close: closeMenu } = useContextMenu<Entry>();

  /**
   * The metadata schema, once, at mount. The form cannot be parsed out of the
   * URL before it arrives — a `meta=image.cameraModel=…` triple is only
   * editable if we know which category and which type that key belongs to —
   * so hydration, and the auto-run of a bookmarked search, both wait for it.
   *
   * Deriving the first run from the *hydrated form* rather than from the URL
   * verbatim is what keeps the "unrun changes" indicator honest: the query we
   * ran is by construction the query the form describes.
   */
  useEffect(() => {
    const ac = new AbortController();
    let alive = true;
    searchFields(ac.signal)
      .catch((e: unknown) => {
        if (e instanceof DOMException && e.name === "AbortError") throw e;
        return FALLBACK_CATEGORIES; // older daemon: degrade, do not break
      })
      .then((cs) => {
        if (!alive) return;
        const list = cs.length ? cs : FALLBACK_CATEGORIES;
        const f = parseSearchForm(params, list, currentDir);
        setCats(list);
        setForm(f);
        if (canSearch(f, list)) {
          const p = buildSearchParams(f, list);
          setRun({ key: searchQuery(p), params: p, nonce: 0 });
        }
      })
      .catch(() => {
        /* aborted */
      });
    return () => {
      alive = false;
      ac.abort();
    };
    // Mount-only: App remounts this view (keyed on the hash query) when the
    // route changes, so `params` is frozen for the lifetime of the component.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  /** The query the form currently describes — compared against the one we ran. */
  const wireKey = useMemo(
    () => (form && cats ? searchQuery(buildSearchParams(form, cats)) : ""),
    [form, cats],
  );

  const runnable = !!form && !!cats && canSearch(form, cats);
  const dirty = !!run && !!form && wireKey !== run.key;

  const doSearch = useCallback(() => {
    if (!form || !cats || !canSearch(form, cats)) return;
    const p = buildSearchParams(form, cats);
    setOffset(0);
    setRun((r) => ({ key: searchQuery(p), params: p, nonce: (r?.nonce ?? 0) + 1 }));
  }, [form, cats]);

  /**
   * The only place a `/search` request is made. It keys off `run`, which only
   * `doSearch` (and the bookmark hydration above) ever sets — editing the form
   * changes nothing here, which is the whole point on a spinning-rust array.
   */
  useEffect(() => {
    if (!run?.key) return;
    const ac = new AbortController();
    let alive = true;
    setLoading(true);
    apiSearch({ ...run.params, limit: PAGE, offset }, ac.signal)
      .then((r) => {
        if (!alive) return;
        setResult(r);
        setError(null);
      })
      .catch((e: unknown) => {
        if (!alive) return;
        if (e instanceof DOMException && e.name === "AbortError") return;
        setError(e instanceof ApiError ? e : new ApiError("INTERNAL", String(e)));
        // Results stay on screen: the banner says the *new* search failed, and
        // wiping the old hits would throw away the only thing still useful.
      })
      .finally(() => alive && setLoading(false));
    return () => {
      alive = false;
      ac.abort();
    };
  }, [run, offset]);

  // Keep the address bar on the query we actually ran, so a bookmark reproduces
  // these results. Silent, so it never remounts the view mid-search.
  useEffect(() => {
    if (run?.key) syncUrlSilently(searchHref(run.key));
  }, [run?.key]);

  const setField = useCallback(<K extends keyof SearchForm>(k: K, v: SearchForm[K]) => {
    setForm((f) => (f ? { ...f, [k]: v } : f));
  }, []);

  const updateCond = useCallback((c: Cond) => {
    setForm((f) => (f ? { ...f, conds: f.conds.map((x) => (x.id === c.id ? c : x)) } : f));
  }, []);

  const removeCond = useCallback((id: number) => {
    setForm((f) => (f ? { ...f, conds: f.conds.filter((x) => x.id !== id) } : f));
  }, []);

  const addCond = useCallback(() => {
    setForm((f) =>
      f && cats?.length && f.conds.length < MAX_CONDS ? { ...f, conds: [...f.conds, newCond(cats[0].id)] } : f,
    );
  }, [cats]);

  /**
   * The results have no sort of their own that `fs/list` could reproduce, so
   * the player gets the daemon's name/asc default for its ↑/↓ — the same thing
   * a click on the hit name already does.
   */
  const openHit = useCallback(
    (e: Entry) => (e.type === "dir" || e.type === "archive" ? onNavigate(e.path) : onOpenFile(e.path, e.mime, e.name)),
    [onNavigate, onOpenFile],
  );

  const menuItems = useMemo(
    () => (menu ? buildEntryMenu(menu.target, { open: openHit, details: (e) => navigate(viewHref(e.path)) }) : []),
    [menu, openHit],
  );

  if (!form || !cats) {
    return (
      <section className="search" aria-label="Search">
        <div className="search-form">
          <Spinner label="loading searchable fields…" />
        </div>
      </section>
    );
  }

  const hasQ = !!form.q.trim();
  const typeCats = typeCategoriesInUse(form, cats);
  const allPreExts = preFilterExtensions({ ...form, extFilter: true }, cats);
  const typeNames = typeCats.map((c) => c.label.toLowerCase()).join(" or ");
  const scopeForValues = form.everywhere ? "" : form.scope.trim();

  const nowSec = Date.now() / 1000;
  const hits = result?.hits ?? [];
  const emptyIndex = error?.code === "INDEXING" || (result && result.total === 0 && !result.indexFresh);

  return (
    <section className="search" aria-label="Search">
      <form
        className="search-form"
        onSubmit={(e) => {
          e.preventDefault();
          doSearch();
        }}
      >
        <div className="search-row">
          <label className="field grow">
            <span className="field-label">Query</span>
            <input
              className="input"
              data-testid="search-q"
              autoFocus
              value={form.q}
              placeholder="filename or content terms — optional; FTS5 quoting supported"
              onChange={(e) => setField("q", e.target.value)}
            />
          </label>
          <label className="field">
            <span className="field-label">Mode</span>
            <select
              className="input select"
              data-testid="search-mode"
              value={form.mode}
              disabled={!hasQ}
              title={hasQ ? "Where the query text is matched" : "Only applies to query text"}
              onChange={(e) => setField("mode", e.target.value as SearchMode)}
            >
              <option value="both">Name + content</option>
              <option value="name">Name only</option>
              <option value="content">Content only</option>
            </select>
          </label>
          <div className="field search-go">
            <span className="field-label">&nbsp;</span>
            <button
              type="submit"
              className={`btn btn-primary search-btn${dirty ? " is-dirty" : ""}`}
              data-testid="search-run"
              disabled={!runnable}
              title={
                runnable
                  ? dirty
                    ? "Run the search with your changes"
                    : "Run the search"
                  : "Enter query text, or add a condition, before searching"
              }
            >
              <Icon name="search" />
              Search
            </button>
          </div>
        </div>

        <div className="search-row">
          <label className="field grow">
            <span className="field-label">Scope</span>
            <input
              className="input"
              data-testid="search-scope"
              value={form.everywhere ? "" : form.scope}
              disabled={form.everywhere}
              placeholder={form.everywhere ? "everywhere (all index roots)" : "/mnt/user/…"}
              onChange={(e) => setField("scope", e.target.value)}
            />
          </label>
          <label className="field check-field">
            <input
              type="checkbox"
              data-testid="search-everywhere"
              checked={form.everywhere}
              onChange={(e) => setField("everywhere", e.target.checked)}
            />
            Everywhere
          </label>
          <label className="field">
            <span className="field-label">Extensions</span>
            <input
              className="input"
              data-testid="search-ext"
              value={form.ext}
              placeholder="go, md, log"
              title="Comma list, case-insensitive; added to any type pre-filter below"
              onChange={(e) => setField("ext", e.target.value)}
            />
          </label>
          <label className="field">
            <span className="field-label">Sort</span>
            <select
              className="input select"
              data-testid="search-sort"
              value={effectiveSort(form)}
              onChange={(e) => setField("sort", e.target.value as SearchSort)}
            >
              <option value="relevance" disabled={!hasQ} title={hasQ ? "" : "Needs query text"}>
                Relevance{hasQ ? "" : " (needs query text)"}
              </option>
              <option value="name">Name</option>
              <option value="size">Size</option>
              <option value="mtime">Modified</option>
            </select>
          </label>
          <label className="field">
            <span className="field-label">Direction</span>
            <select
              className="input select"
              data-testid="search-dir"
              value={form.dir}
              onChange={(e) => setField("dir", e.target.value as SortDir)}
            >
              <option value="asc">Ascending</option>
              <option value="desc">Descending</option>
            </select>
          </label>
        </div>

        <div className="search-conds" data-testid="search-conds">
          {form.conds.map((c, i) => (
            <ConditionRow
              key={c.id}
              index={i}
              cond={c}
              cats={cats}
              scope={scopeForValues}
              onChange={updateCond}
              onRemove={removeCond}
            />
          ))}

          <div className="search-cond-actions">
            <button
              type="button"
              className="btn btn-sm"
              data-testid="cond-add"
              disabled={form.conds.length >= MAX_CONDS}
              title={form.conds.length >= MAX_CONDS ? `At most ${MAX_CONDS} conditions per search` : "Add a condition"}
              onClick={addCond}
            >
              + Add condition
            </button>
            {form.conds.length > 1 ? <span className="muted">all conditions must match</span> : null}
          </div>

          {typeCats.length ? (
            <div className="ext-prefilter" data-testid="ext-prefilter">
              <label className="check-field">
                <input
                  type="checkbox"
                  data-testid="ext-prefilter-check"
                  checked={form.extFilter}
                  onChange={(e) => setField("extFilter", e.target.checked)}
                />
                <span data-testid="ext-prefilter-label" title={allPreExts.join(", ")}>
                  Only files with {typeNames} extensions ({allPreExts.slice(0, EXT_PREVIEW).join(", ")}
                  {allPreExts.length > EXT_PREVIEW ? ", …" : ""})
                </span>
              </label>
              {allPreExts.length > EXT_PREVIEW ? (
                <button
                  type="button"
                  className="link-btn"
                  data-testid="ext-prefilter-expand"
                  aria-expanded={extOpen}
                  onClick={() => setExtOpen((v) => !v)}
                >
                  {extOpen ? "hide list" : `show all ${allPreExts.length}`}
                </button>
              ) : null}
              <span className="muted">— matching is case-insensitive; unticking searches every file in the scope</span>
              {extOpen ? (
                <code className="ext-list" data-testid="ext-prefilter-full">
                  {allPreExts.join(", ")}
                </code>
              ) : null}
            </div>
          ) : null}
        </div>
      </form>

      <div className="search-status">
        {loading ? <Spinner label="searching…" /> : null}
        {result ? (
          <>
            <strong>{result.total.toLocaleString()}</strong>
            <span className="muted">
              {" "}
              match{result.total === 1 ? "" : "es"} in {result.tookMs} ms
            </span>
            {!result.indexFresh ? (
              <span className="pill pill-warn" title="A crawl is pending or in progress — results may be stale">
                index may be stale
              </span>
            ) : null}
          </>
        ) : null}
        {dirty ? (
          <span className="pill pill-warn" data-testid="search-dirty" title="These results are from the previous search">
            filters changed — press Search
          </span>
        ) : null}
      </div>

      {error ? <ErrorBanner error={error} /> : null}

      <div className="search-results">
        {loading && !result ? <SkeletonRows rows={10} cols={3} /> : null}

        {!run && !loading ? (
          <EmptyState title="Ready to search">
            Nothing is fetched until you press <strong>Search</strong> — enter query text, or add a metadata condition
            (a query is not required). Content matching needs the content index enabled in{" "}
            <a href={SETTINGS_HREF}>Settings</a>.
          </EmptyState>
        ) : null}

        {!loading && run && result && hits.length === 0 ? (
          <EmptyState title="No matches">
            {emptyIndex ? (
              <>
                The index looks empty or unavailable. Configure roots and run a scan in{" "}
                <a href={SETTINGS_HREF}>Settings</a>.
              </>
            ) : (
              <>Try a broader scope, fewer conditions, or unticking the extension pre-filter.</>
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
                <button type="button" className="hit-name" title={e.path} onClick={() => openHit(e)}>
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
          <button
            type="button"
            className="btn btn-sm"
            disabled={offset === 0}
            onClick={() => setOffset(Math.max(0, offset - PAGE))}
          >
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

/* ------------------------------------------------------------ condition row */

function ConditionRow({
  index,
  cond,
  cats,
  scope,
  onChange,
  onRemove,
}: {
  index: number;
  cond: Cond;
  cats: SearchCategory[];
  scope: string;
  onChange: (c: Cond) => void;
  onRemove: (id: number) => void;
}) {
  const cat = findCategory(cats, cond.cat);
  const field = findField(cats, cond.cat, cond.key);
  const [suggest, setSuggest] = useState<SearchValueCount[]>([]);
  const listId = `cond-vals-${cond.id}`;

  /**
   * The one request the form is allowed to make without the Search button:
   * filling in the dropdown the user just opened. Fired on focus, never on
   * every keystroke, so it costs one round trip per field the user touches.
   */
  const loadValues = useCallback(() => {
    if (!field) return;
    searchValues({ key: field.key, prefix: cond.value, path: scope || undefined, limit: 30 })
      .then(setSuggest)
      .catch(() => setSuggest([]));
  }, [field, cond.value, scope]);

  const pickCategory = (id: string) =>
    // Switching category invalidates the field, and with it the operator, the
    // value and its unit — none of them mean anything in the new one.
    onChange({ ...cond, cat: id, key: "", op: "", value: "", unit: DEFAULT_BYTE_UNIT });

  const pickField = (key: string) => {
    const f = findField(cats, cond.cat, key);
    setSuggest([]);
    if (!f) return onChange({ ...cond, key: "", op: "", value: "", unit: DEFAULT_BYTE_UNIT });
    onChange({
      ...cond,
      key,
      op: defaultOp(f.type),
      value: f.type === "bool" ? "true" : "",
      unit: DEFAULT_BYTE_UNIT,
    });
  };

  return (
    <div className="search-cond" data-testid="cond-row" data-index={index}>
      <span className="cond-join">{index === 0 ? "Where" : "and"}</span>

      <select
        className="input select"
        data-testid="cond-cat"
        aria-label="Category"
        value={cond.cat}
        onChange={(e) => pickCategory(e.target.value)}
      >
        {cats.map((c) => (
          <option key={c.id} value={c.id}>
            {c.label}
          </option>
        ))}
      </select>

      <select
        className="input select"
        data-testid="cond-field"
        aria-label="Field"
        value={cond.key}
        onChange={(e) => pickField(e.target.value)}
      >
        <option value="">Field…</option>
        {(cat?.fields ?? []).map((f) => (
          <option key={f.key} value={f.key}>
            {f.label}
          </option>
        ))}
      </select>

      {field ? (
        <select
          className="input select cond-op"
          data-testid="cond-op"
          aria-label="Operator"
          value={cond.op}
          onChange={(e) => onChange({ ...cond, op: e.target.value })}
        >
          {OPS[field.type].map((o) => (
            <option key={o.op} value={o.op}>
              {o.label}
            </option>
          ))}
        </select>
      ) : null}

      {field ? <ValueEditor field={field} cond={cond} suggest={suggest} listId={listId} onChange={onChange} onOpen={loadValues} /> : null}

      <button
        type="button"
        className="btn btn-icon btn-sm cond-remove"
        data-testid="cond-remove"
        title="Remove this condition"
        aria-label="Remove this condition"
        onClick={() => onRemove(cond.id)}
      >
        ×
      </button>
    </div>
  );
}

function ValueEditor({
  field,
  cond,
  suggest,
  listId,
  onChange,
  onOpen,
}: {
  field: SearchField;
  cond: Cond;
  suggest: SearchValueCount[];
  listId: string;
  onChange: (c: Cond) => void;
  onOpen: () => void;
}) {
  const set = (value: string) => onChange({ ...cond, value });

  // API.md ships `values` for bool fields as well as enums, so use them when
  // they are there and fall back to plain true/false when they are not.
  if (field.type === "bool") {
    const choices = field.values?.length
      ? field.values
      : [
          { value: "true", label: "Yes" },
          { value: "false", label: "No" },
        ];
    return (
      <select
        className="input select"
        data-testid="cond-value"
        aria-label="Value"
        value={cond.value || choices[0].value}
        onChange={(e) => set(e.target.value)}
      >
        {choices.map((c) => (
          <option key={c.value} value={c.value}>
            {c.label}
          </option>
        ))}
      </select>
    );
  }

  // An enum whose choices the daemon shipped: the user picks the label, the
  // wire gets the value ("Dolby Digital (AC-3)" → "ac3").
  if (field.type === "enum" && field.values?.length) {
    return (
      <select
        className="input select"
        data-testid="cond-value"
        aria-label="Value"
        value={cond.value}
        onChange={(e) => set(e.target.value)}
      >
        <option value="">Value…</option>
        {field.values.map((v) => (
          <option key={v.value} value={v.value}>
            {v.label}
          </option>
        ))}
      </select>
    );
  }

  if (field.type === "date") {
    return (
      <input
        className="input"
        data-testid="cond-value"
        aria-label="Value"
        type="date"
        value={cond.value}
        onChange={(e) => set(e.target.value)}
      />
    );
  }

  if (field.type === "bytes") {
    return (
      <span className="input-group">
        <input
          className="input num"
          data-testid="cond-value"
          aria-label="Value"
          type="number"
          min="0"
          value={cond.value}
          onChange={(e) => set(e.target.value)}
        />
        <select
          className="input select unit"
          data-testid="cond-unit"
          aria-label="Unit"
          value={cond.unit}
          onChange={(e) => onChange({ ...cond, unit: e.target.value })}
        >
          {BYTE_UNITS.map((u) => (
            <option key={u.id} value={u.id}>
              {u.label}
            </option>
          ))}
        </select>
      </span>
    );
  }

  if (field.type === "number") {
    return (
      <span className="input-group">
        <input
          className="input num"
          data-testid="cond-value"
          aria-label="Value"
          type="number"
          value={cond.value}
          onChange={(e) => set(e.target.value)}
        />
        {field.unit ? <span className="unit-label">{field.unit}</span> : null}
      </span>
    );
  }

  // text (and an enum the daemon left open): free text with type-ahead.
  return (
    <>
      <input
        className="input"
        data-testid="cond-value"
        aria-label="Value"
        list={listId}
        value={cond.value}
        placeholder={field.label}
        onFocus={onOpen}
        onChange={(e) => set(e.target.value)}
      />
      <datalist id={listId} data-testid="cond-datalist">
        {suggest.map((s) => (
          <option key={s.value} value={s.value}>
            {s.count ? `${s.count}` : ""}
          </option>
        ))}
      </datalist>
    </>
  );
}
