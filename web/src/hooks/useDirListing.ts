import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, listDir } from "../api/client";
import type { Entry, SortDir, SortKey } from "../api/types";

export const PAGE_SIZE = 1000;

export interface DirListing {
  entries: Entry[];
  total: number;
  /** First page in flight (show skeleton). */
  loading: boolean;
  /** A follow-up page is in flight (show footer spinner). */
  loadingMore: boolean;
  error: ApiError | null;
  hasMore: boolean;
  loadMore: () => void;
  reload: () => void;
}

/**
 * Paged directory listing with infinite scroll. `fs/list` caps at limit=1000,
 * so a 100k-entry directory arrives as 100 appended pages while the virtualizer
 * only ever renders the visible window.
 */
export function useDirListing(path: string, sort: SortKey, dir: SortDir, dirsFirst: boolean): DirListing {
  const [entries, setEntries] = useState<Entry[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const [nonce, setNonce] = useState(0);

  // Guards against races when the user changes directory mid-flight.
  const genRef = useRef(0);
  const abortRef = useRef<AbortController | null>(null);
  const inFlightRef = useRef(false);
  const doneRef = useRef(false);

  const fetchPage = useCallback(
    async (offset: number, gen: number) => {
      if (inFlightRef.current) return;
      inFlightRef.current = true;
      abortRef.current?.abort();
      const ac = new AbortController();
      abortRef.current = ac;

      if (offset === 0) setLoading(true);
      else setLoadingMore(true);

      try {
        const res = await listDir(
          { path, offset, limit: PAGE_SIZE, sort, dir, dirsFirst },
          ac.signal,
        );
        if (gen !== genRef.current) return;
        setError(null);
        setTotal(res.total ?? res.entries.length);
        setEntries((prev) => {
          const next = offset === 0 ? res.entries : [...prev, ...res.entries];
          // A short page or a full tally means we've reached the end.
          if (res.entries.length === 0 || next.length >= (res.total ?? next.length)) doneRef.current = true;
          return next;
        });
      } catch (e) {
        if (gen !== genRef.current) return;
        if (e instanceof DOMException && e.name === "AbortError") return;
        doneRef.current = true;
        setError(e instanceof ApiError ? e : new ApiError("INTERNAL", String(e)));
        if (offset === 0) {
          setEntries([]);
          setTotal(0);
        }
      } finally {
        // Only the newest request clears the guard: a stale (aborted) response
        // must not unlock paging while the current page is still in flight.
        if (abortRef.current === ac) {
          inFlightRef.current = false;
          if (gen === genRef.current) {
            setLoading(false);
            setLoadingMore(false);
          }
        }
      }
    },
    [path, sort, dir, dirsFirst],
  );

  useEffect(() => {
    const gen = ++genRef.current;
    doneRef.current = false;
    inFlightRef.current = false;
    setEntries([]);
    setTotal(0);
    setError(null);
    void fetchPage(0, gen);
    return () => {
      abortRef.current?.abort();
    };
  }, [fetchPage, nonce]);

  const loadMore = useCallback(() => {
    if (doneRef.current || inFlightRef.current) return;
    void fetchPage(entries.length, genRef.current);
  }, [entries.length, fetchPage]);

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  return {
    entries,
    total,
    loading,
    loadingMore,
    error,
    hasMore: !doneRef.current && entries.length < total,
    loadMore,
    reload,
  };
}
