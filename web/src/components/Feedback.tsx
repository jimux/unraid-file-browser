import type { ReactNode } from "react";
import { ApiError } from "../api/client";
import { Icon } from "./Icon";

/** Human copy for each error code in the contract. */
const HINTS: Record<string, string> = {
  NOT_FOUND: "That path no longer exists.",
  FORBIDDEN: "That path is outside the configured index roots.",
  TOO_LARGE: "The daemon refused this because it exceeds a size guardrail.",
  TIMEOUT: "The daemon timed out (archives are capped at 30s per request).",
  ARCHIVE_ERROR: "The archive could not be opened — it may be corrupt, encrypted, or nested too deep.",
  ENCODING_ERROR: "The bytes could not be decoded with that encoding. Try another one, or the Hex tab.",
  INDEXING: "The index is busy or unavailable. Check Settings.",
  BAD_REQUEST: "The daemon rejected the request parameters.",
  NETWORK: "Could not reach the daemon. Is filebrowserd running?",
  INTERNAL: "The daemon hit an internal error.",
};

export function ErrorBanner({
  error,
  onRetry,
  children,
}: {
  error: ApiError | Error | string | null;
  onRetry?: () => void;
  children?: ReactNode;
}) {
  if (!error) return null;
  const code = error instanceof ApiError ? error.code : null;
  const message = typeof error === "string" ? error : error.message;
  return (
    <div className="banner banner-error" role="alert">
      <Icon name="warn" className="banner-icon" />
      <div className="banner-body">
        <div className="banner-title">
          {code ? <span className="code-chip">{code}</span> : null}
          <span>{message}</span>
        </div>
        {code && HINTS[code] ? <div className="banner-hint">{HINTS[code]}</div> : null}
        {children}
      </div>
      {onRetry ? (
        <button type="button" className="btn btn-sm" onClick={onRetry}>
          Retry
        </button>
      ) : null}
    </div>
  );
}

export function InfoBanner({ children }: { children: ReactNode }) {
  return <div className="banner banner-info">{children}</div>;
}

/** Row-shaped shimmer used while a list/table loads. */
export function SkeletonRows({ rows = 12, cols = 4 }: { rows?: number; cols?: number }) {
  return (
    <div className="skeleton-rows" aria-hidden="true">
      {Array.from({ length: rows }, (_, r) => (
        <div className="skeleton-row" key={r}>
          {Array.from({ length: cols }, (_, c) => (
            <span className={`skeleton-cell skeleton-cell-${c}`} key={c} />
          ))}
        </div>
      ))}
    </div>
  );
}

export function SkeletonBlock({ lines = 8 }: { lines?: number }) {
  return (
    <div className="skeleton-block" aria-hidden="true">
      {Array.from({ length: lines }, (_, i) => (
        <span className="skeleton-line" key={i} style={{ width: `${45 + ((i * 37) % 50)}%` }} />
      ))}
    </div>
  );
}

export function EmptyState({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="empty-state">
      <div className="empty-title">{title}</div>
      {children ? <div className="empty-body">{children}</div> : null}
    </div>
  );
}

export function Spinner({ label }: { label?: string }) {
  return (
    <span className="spinner-wrap">
      <span className="spinner" aria-hidden="true" />
      {label ? <span className="spinner-label">{label}</span> : null}
    </span>
  );
}
