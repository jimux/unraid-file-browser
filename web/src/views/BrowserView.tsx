import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import type { Entry, SortDir, SortKey } from "../api/types";
import { useDirListing } from "../hooks/useDirListing";
import { EmptyState, ErrorBanner, SkeletonRows, Spinner } from "../components/Feedback";
import { EntryIcon } from "../components/Icon";
import { formatAbsolute, formatBytesExact, formatRelative, formatSize, typeLabel } from "../lib/format";
import { isMedia } from "../lib/media";
import { parentPath } from "../lib/paths";

const ROW_HEIGHT = 26;
/** Start fetching the next page this many rows before the end. */
const PREFETCH_ROWS = 200;

export interface SortState {
  key: SortKey;
  dir: SortDir;
  dirsFirst: boolean;
}

interface BrowserViewProps {
  path: string;
  sort: SortState;
  onSortChange: (s: SortState) => void;
  onNavigate: (p: string) => void;
  /** Default action for a file: viewer panel, or the player window for media. */
  onOpenFile: (e: Entry) => void;
  /** Secondary action (Shift+Enter / "Details"): always the viewer panel. */
  onOpenDetails: (e: Entry) => void;
  reloadNonce: number;
  /** Dimmed while the viewer panel is on top. */
  inert?: boolean;
}

const COLUMNS: Array<{ key: SortKey; label: string; className: string }> = [
  { key: "name", label: "Name", className: "col-name" },
  { key: "size", label: "Size", className: "col-size" },
  { key: "mtime", label: "Modified", className: "col-mtime" },
  { key: "type", label: "Type", className: "col-type" },
];

export function BrowserView({
  path,
  sort,
  onSortChange,
  onNavigate,
  onOpenFile,
  onOpenDetails,
  reloadNonce,
  inert,
}: BrowserViewProps) {
  const listing = useDirListing(path, sort.key, sort.dir, sort.dirsFirst);
  const { entries, total, loading, loadingMore, error, hasMore, loadMore, reload } = listing;

  const scrollRef = useRef<HTMLDivElement | null>(null);
  const [selected, setSelected] = useState(0);

  // External reload button.
  const reloadRef = useRef(reloadNonce);
  useEffect(() => {
    if (reloadRef.current !== reloadNonce) {
      reloadRef.current = reloadNonce;
      reload();
    }
  }, [reloadNonce, reload]);

  useEffect(() => {
    setSelected(0);
    scrollRef.current?.scrollTo({ top: 0 });
  }, [path, sort.key, sort.dir, sort.dirsFirst]);

  // Keyboard nav needs the grid focused; give it focus on arrival so arrows /
  // Enter / Backspace work without a click first (never while the viewer is up,
  // and never stealing focus from a field the user is typing in).
  useEffect(() => {
    if (inert) return;
    const el = scrollRef.current;
    if (!el) return;
    const active = document.activeElement as HTMLElement | null;
    if (active && active.closest("input, textarea, select, [contenteditable=true]")) return;
    el.focus({ preventScroll: true });
  }, [path, inert]);

  const virtualizer = useVirtualizer({
    count: entries.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => ROW_HEIGHT,
    overscan: 12,
  });

  const virtualItems = virtualizer.getVirtualItems();

  // Infinite paging: when the rendered window nears the tail, pull another page.
  const lastIndex = virtualItems.length ? virtualItems[virtualItems.length - 1].index : 0;
  useEffect(() => {
    if (!hasMore || loading || loadingMore) return;
    if (lastIndex >= entries.length - PREFETCH_ROWS) loadMore();
  }, [lastIndex, entries.length, hasMore, loading, loadingMore, loadMore]);

  const activate = useCallback(
    (e: Entry) => {
      // Dirs and archives browse identically — the path just grows a "!/" leg.
      if (e.type === "dir" || e.type === "archive") onNavigate(e.path);
      else onOpenFile(e);
    },
    [onNavigate, onOpenFile],
  );

  const move = useCallback(
    (delta: number) => {
      setSelected((cur) => {
        const next = Math.max(0, Math.min(entries.length - 1, cur + delta));
        virtualizer.scrollToIndex(next, { align: "auto" });
        return next;
      });
    },
    [entries.length, virtualizer],
  );

  const onKeyDown = useCallback(
    (ev: React.KeyboardEvent<HTMLDivElement>) => {
      if (inert) return;
      switch (ev.key) {
        case "ArrowDown":
          ev.preventDefault();
          move(1);
          break;
        case "ArrowUp":
          ev.preventDefault();
          move(-1);
          break;
        case "PageDown":
          ev.preventDefault();
          move(20);
          break;
        case "PageUp":
          ev.preventDefault();
          move(-20);
          break;
        case "Home":
          ev.preventDefault();
          move(-entries.length);
          break;
        case "End":
          ev.preventDefault();
          move(entries.length);
          break;
        case "Enter": {
          ev.preventDefault();
          const e = entries[selected];
          if (!e) break;
          // Shift+Enter always means "inspect", never "play".
          if (ev.shiftKey && e.type !== "dir" && e.type !== "archive") onOpenDetails(e);
          else activate(e);
          break;
        }
        case "ArrowRight": {
          const e = entries[selected];
          if (e && (e.type === "dir" || e.type === "archive")) {
            ev.preventDefault();
            onNavigate(e.path);
          }
          break;
        }
        case "Backspace":
        case "ArrowLeft": {
          const up = parentPath(path);
          if (up) {
            ev.preventDefault();
            onNavigate(up);
          }
          break;
        }
        default:
          break;
      }
    },
    [activate, entries, inert, move, onNavigate, onOpenDetails, path, selected],
  );

  const toggleSort = (key: SortKey) => {
    if (sort.key === key) onSortChange({ ...sort, dir: sort.dir === "asc" ? "desc" : "asc" });
    else onSortChange({ ...sort, key, dir: "asc" });
  };

  const nowSec = useMemo(() => Date.now() / 1000, [entries.length]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <section className={`browser${inert ? " is-inert" : ""}`} aria-label="Directory listing">
      {error ? <ErrorBanner error={error} onRetry={reload} /> : null}

      <div className="table-head" role="row">
        {COLUMNS.map((c) => (
          <button
            key={c.key}
            type="button"
            className={`th ${c.className}${sort.key === c.key ? " is-sorted" : ""}`}
            onClick={() => toggleSort(c.key)}
            title={`Sort by ${c.label.toLowerCase()}`}
          >
            {c.label}
            <span className="sort-caret">{sort.key === c.key ? (sort.dir === "asc" ? "▲" : "▼") : ""}</span>
          </button>
        ))}
        <label className="dirs-first" title="Group directories before files">
          <input
            type="checkbox"
            checked={sort.dirsFirst}
            onChange={(e) => onSortChange({ ...sort, dirsFirst: e.target.checked })}
          />
          Dirs first
        </label>
      </div>

      <div
        className="table-scroll"
        ref={scrollRef}
        tabIndex={0}
        role="grid"
        aria-rowcount={total || entries.length}
        onKeyDown={onKeyDown}
      >
        {loading && entries.length === 0 ? (
          <SkeletonRows rows={18} cols={4} />
        ) : entries.length === 0 && !error ? (
          <EmptyState title="Empty directory">Nothing to list at this path.</EmptyState>
        ) : (
          <div className="table-body" style={{ height: `${virtualizer.getTotalSize()}px` }}>
            {virtualItems.map((vi) => {
              const e = entries[vi.index];
              if (!e) return null;
              const isSel = vi.index === selected;
              const media = e.type === "file" && isMedia(e.mime);
              return (
                <div
                  key={`${e.path}-${vi.index}`}
                  className={`tr${vi.index % 2 ? " is-odd" : ""}${isSel ? " is-selected" : ""} tr-${e.type}${media ? " is-media" : ""}`}
                  role="row"
                  aria-rowindex={vi.index + 1}
                  style={{ transform: `translateY(${vi.start}px)`, height: `${ROW_HEIGHT}px` }}
                  onClick={() => setSelected(vi.index)}
                  onDoubleClick={() => activate(e)}
                  onKeyDown={undefined}
                >
                  <div className="td col-name" title={e.path}>
                    <EntryIcon type={e.type} />
                    <button
                      type="button"
                      className="name-btn"
                      onClick={(ev) => {
                        ev.stopPropagation();
                        setSelected(vi.index);
                        activate(e);
                      }}
                      tabIndex={-1}
                    >
                      {e.name}
                    </button>
                    {media ? (
                      <button
                        type="button"
                        className="row-action"
                        title="Open the viewer panel instead of the player (Shift+Enter)"
                        onClick={(ev) => {
                          ev.stopPropagation();
                          setSelected(vi.index);
                          onOpenDetails(e);
                        }}
                        tabIndex={-1}
                      >
                        Details
                      </button>
                    ) : null}
                    {e.type === "symlink" && e.target ? <span className="symlink-target">→ {e.target}</span> : null}
                  </div>
                  <div className="td col-size" title={formatBytesExact(e.size)}>
                    {e.type === "dir" ? "—" : formatSize(e.size)}
                  </div>
                  <div className="td col-mtime" title={formatAbsolute(e.mtime)}>
                    <span className="mtime-rel">{formatRelative(e.mtime, nowSec)}</span>
                    <span className="mtime-abs">{formatAbsolute(e.mtime)}</span>
                  </div>
                  <div className="td col-type" title={e.mime || undefined}>
                    {typeLabel(e.type, e.mime, e.name)}
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </div>

      <footer className="table-foot">
        <span>
          {entries.length.toLocaleString()}
          {total > entries.length ? ` of ${total.toLocaleString()}` : ""} item{total === 1 ? "" : "s"}
        </span>
        {loadingMore ? <Spinner label="loading more…" /> : null}
        <span className="foot-hint">↑↓ move · Enter open · ⇧Enter details · Backspace up</span>
      </footer>
    </section>
  );
}
