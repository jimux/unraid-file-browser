import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ApiError,
  getIndexConfig,
  healthz,
  pauseIndex,
  putIndexConfig,
  rescan,
  resumeIndex,
} from "../api/client";
import { subscribeIndexStatus, type IndexTransport } from "../api/events";
import type { HealthResult, IndexConfig, IndexConfigResult, IndexStatus } from "../api/types";
import { ErrorBanner, InfoBanner, SkeletonBlock, Spinner } from "../components/Feedback";
import { Icon } from "../components/Icon";
import { useAsync } from "../hooks/useAsync";
import { formatAbsolute, formatDuration, formatNumber, formatRelative, formatSize, toUnixSeconds } from "../lib/format";
import { isUnderAnyRoot } from "../lib/paths";

export function SettingsView({ onRootsChanged }: { onRootsChanged: (roots: string[]) => void }) {
  return (
    <section className="settings" aria-label="Settings">
      <StatusCard />
      <ConfigCard onRootsChanged={onRootsChanged} />
      <HealthCard />
    </section>
  );
}

/* ------------------------------------------------------------ status card */

function StatusCard() {
  const [status, setStatus] = useState<IndexStatus | null>(null);
  const [transport, setTransport] = useState<IndexTransport>("connecting");
  const [streamError, setStreamError] = useState<string | null>(null);
  const [action, setAction] = useState<string | null>(null);
  const [actionError, setActionError] = useState<ApiError | null>(null);
  const [paused, setPaused] = useState(false);

  useEffect(() => {
    return subscribeIndexStatus({
      onStatus: (s) => {
        setStatus(s);
        setStreamError(null);
      },
      onError: (m) => setStreamError(m),
      onTransport: setTransport,
    });
  }, []);

  const run = useCallback(async (name: string, fn: () => Promise<unknown>) => {
    setAction(name);
    setActionError(null);
    try {
      await fn();
      if (name === "pause") setPaused(true);
      if (name === "resume") setPaused(false);
    } catch (e) {
      setActionError(e instanceof ApiError ? e : new ApiError("INTERNAL", String(e)));
    } finally {
      setAction(null);
    }
  }, []);

  const lastScan = toUnixSeconds(status?.lastFullScan);
  const pct = status?.progress !== undefined ? Math.max(0, Math.min(1, status.progress)) : null;
  const busy = status?.state === "crawling" || status?.state === "extracting";

  return (
    <div className="card">
      <div className="card-head">
        <h2>Index status</h2>
        <span className={`pill pill-${transport === "sse" ? "ok" : transport === "poll" ? "muted" : "warn"}`}>
          {transport === "sse" ? "live (SSE)" : transport === "poll" ? "polling (2s)" : "connecting…"}
        </span>
      </div>

      {streamError ? <InfoBanner>{streamError}</InfoBanner> : null}
      {actionError ? <ErrorBanner error={actionError} /> : null}

      {!status ? (
        <SkeletonBlock lines={4} />
      ) : (
        <>
          <dl className="stat-grid">
            <Stat label="State">
              <span className={`state state-${status.state}`}>{status.state}</span>
              {paused ? <span className="pill pill-warn">paused</span> : null}
            </Stat>
            <Stat label="Files indexed">{formatNumber(status.filesIndexed)}</Stat>
            <Stat label="Content indexed">{formatNumber(status.contentIndexed)}</Stat>
            <Stat label="Database size">{formatSize(status.dbBytes)}</Stat>
            <Stat label="Last full scan">
              {lastScan ? (
                <span title={formatAbsolute(lastScan)}>{formatRelative(lastScan)}</span>
              ) : (
                <span className="muted">never</span>
              )}
            </Stat>
          </dl>

          {busy || pct !== null ? (
            <div className="progress-block">
              <div className="progress" role="progressbar" aria-valuenow={pct !== null ? Math.round(pct * 100) : undefined}>
                <div
                  className={`progress-bar${pct === null ? " is-indeterminate" : ""}`}
                  style={pct !== null ? { width: `${pct * 100}%` } : undefined}
                />
              </div>
              <div className="progress-meta">
                {pct !== null ? <span>{Math.round(pct * 100)}%</span> : <span>working…</span>}
                {status.current ? (
                  <code className="current-path" title={status.current}>
                    {status.current}
                  </code>
                ) : null}
              </div>
            </div>
          ) : null}
        </>
      )}

      <div className="card-actions">
        <button type="button" className="btn btn-primary" disabled={!!action} onClick={() => run("rescan", () => rescan())}>
          {action === "rescan" ? <Spinner /> : <Icon name="refresh" />} Rescan all
        </button>
        <button type="button" className="btn" disabled={!!action} onClick={() => run("pause", () => pauseIndex())}>
          Pause
        </button>
        <button type="button" className="btn" disabled={!!action} onClick={() => run("resume", () => resumeIndex())}>
          Resume
        </button>
        <RescanSubtree disabled={!!action} onRun={(p) => run("rescan", () => rescan(p))} />
      </div>
    </div>
  );
}

function RescanSubtree({ disabled, onRun }: { disabled: boolean; onRun: (p: string) => void }) {
  const [path, setPath] = useState("");
  return (
    <span className="input-group subtree">
      <input
        className="input"
        placeholder="/mnt/user/subtree…"
        value={path}
        onChange={(e) => setPath(e.target.value)}
      />
      <button type="button" className="btn" disabled={disabled || !path.trim()} onClick={() => onRun(path.trim())}>
        Rescan subtree
      </button>
    </span>
  );
}

function Stat({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="stat">
      <dt>{label}</dt>
      <dd>{children}</dd>
    </div>
  );
}

/* ------------------------------------------------------------ config card */

const DEFAULT_CONFIG: IndexConfig = {
  roots: ["/mnt/user"],
  schedule: "0 3 * * *",
  parallelism: 2,
  content: { enabled: false, includePaths: [], extensions: [], maxFileBytes: 10485760 },
};

function ConfigCard({ onRootsChanged }: { onRootsChanged: (roots: string[]) => void }) {
  const remote = useAsync<IndexConfigResult>((signal) => getIndexConfig(signal), []);
  const [draft, setDraft] = useState<IndexConfig | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<ApiError | null>(null);
  const [saved, setSaved] = useState(false);
  const [localErrors, setLocalErrors] = useState<string[]>([]);

  const remoteConfig = remote.data?.config ?? null;
  const allowedRoots = useMemo(() => remote.data?.allowedRoots ?? [], [remote.data]);

  const loadedRef = useRef<string>("");
  useEffect(() => {
    if (!remoteConfig) return;
    const sig = JSON.stringify(remoteConfig);
    if (loadedRef.current === sig) return;
    loadedRef.current = sig;
    setDraft(remoteConfig);
  }, [remoteConfig]);

  const cfg = draft ?? (remote.loading ? null : DEFAULT_CONFIG);

  const dirty = useMemo(() => {
    if (!draft || !remoteConfig) return false;
    return JSON.stringify(draft) !== JSON.stringify(remoteConfig);
  }, [draft, remoteConfig]);

  const patch = (p: Partial<IndexConfig>) => setDraft((d) => (d ? { ...d, ...p } : d));
  const patchContent = (p: Partial<IndexConfig["content"]>) =>
    setDraft((d) => (d ? { ...d, content: { ...d.content, ...p } } : d));

  function validate(c: IndexConfig): string[] {
    const errs: string[] = [];
    if (c.roots.length === 0) errs.push("At least one index root is required.");
    if (c.roots.some((r) => !r.startsWith("/"))) errs.push("Roots must be absolute paths.");
    // Mirror of the daemon's own check, so a bad path is caught before the PUT.
    // If the daemon reported no browse roots we skip it and let the server rule.
    if (allowedRoots.length) {
      const list = allowedRoots.join(", ");
      for (const r of c.roots) {
        if (r.trim() && !isUnderAnyRoot(r.trim(), allowedRoots))
          errs.push(`Index root "${r}" is outside the browse roots (${list}).`);
      }
      if (c.content.enabled) {
        for (const p of c.content.includePaths) {
          if (p.trim() && !isUnderAnyRoot(p.trim(), allowedRoots))
            errs.push(`Include path "${p}" is outside the browse roots (${list}).`);
        }
      }
    }
    if (!c.schedule.trim()) errs.push("A cron schedule is required (e.g. \"0 3 * * *\").");
    else if (c.schedule.trim().split(/\s+/).length < 5) errs.push("Cron schedule needs at least 5 fields.");
    if (!Number.isInteger(c.parallelism) || c.parallelism < 1 || c.parallelism > 32)
      errs.push("Parallelism must be an integer between 1 and 32.");
    if (!Number.isFinite(c.content.maxFileBytes) || c.content.maxFileBytes < 0)
      errs.push("Max file size must be a non-negative number of bytes.");
    return errs;
  }

  const save = async () => {
    if (!draft) return;
    const errs = validate(draft);
    setLocalErrors(errs);
    if (errs.length) return;
    setSaving(true);
    setSaveError(null);
    setSaved(false);
    try {
      const next = await putIndexConfig(draft);
      loadedRef.current = JSON.stringify(next.config);
      setDraft(next.config);
      setSaved(true);
      onRootsChanged(next.config.roots);
      remote.reload();
    } catch (e) {
      setSaveError(e instanceof ApiError ? e : new ApiError("INTERNAL", String(e)));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="card">
      <div className="card-head">
        <h2>Index configuration</h2>
        {dirty ? <span className="pill pill-warn">unsaved changes</span> : null}
      </div>

      {remote.error ? <ErrorBanner error={remote.error} onRetry={remote.reload} /> : null}
      {saveError ? <ErrorBanner error={saveError} /> : null}
      {localErrors.length ? (
        <div className="banner banner-error" role="alert">
          <Icon name="warn" className="banner-icon" />
          <ul className="banner-list">
            {localErrors.map((e) => (
              <li key={e}>{e}</li>
            ))}
          </ul>
        </div>
      ) : null}
      {saved && !dirty ? <InfoBanner>Configuration saved.</InfoBanner> : null}

      {!cfg ? (
        <SkeletonBlock lines={10} />
      ) : (
        <div className="form-grid">
          <BrowseRoots roots={allowedRoots} />

          <ListEditor
            label="Index roots"
            hint="Absolute paths crawled by the indexer. Everything outside these is FORBIDDEN to the API too."
            values={cfg.roots}
            placeholder={allowedRoots[0] ?? "/mnt/user"}
            onChange={(roots) => patch({ roots })}
          />

          <label className="field">
            <span className="field-label">Cron schedule</span>
            <input
              className="input mono"
              value={cfg.schedule}
              placeholder="0 3 * * *"
              onChange={(e) => patch({ schedule: e.target.value })}
            />
            <span className="field-hint">Standard 5-field cron. Default nightly at 03:00.</span>
          </label>

          <label className="field">
            <span className="field-label">Parallelism</span>
            <input
              className="input num"
              type="number"
              min={1}
              max={32}
              value={cfg.parallelism}
              onChange={(e) => patch({ parallelism: Number(e.target.value) })}
            />
            <span className="field-hint">Crawl workers. Keep low on a spinning array.</span>
          </label>

          <div className="subsection">
            <label className="field check-field">
              <input
                type="checkbox"
                checked={cfg.content.enabled}
                onChange={(e) => patchContent({ enabled: e.target.checked })}
              />
              <span>Content indexing (FTS5 full-text)</span>
            </label>

            <fieldset className="content-rules" disabled={!cfg.content.enabled}>
              <ListEditor
                label="Include paths"
                hint="Only files under these prefixes get their text extracted. Must also lie within the browse roots."
                values={cfg.content.includePaths}
                placeholder={allowedRoots[0] ? `${allowedRoots[0]}/appdata` : "/mnt/user/appdata"}
                onChange={(includePaths) => patchContent({ includePaths })}
              />

              <TagInput
                label="Extensions"
                hint="Allowlist of text-like extensions. Enter or comma to add."
                values={cfg.content.extensions}
                onChange={(extensions) => patchContent({ extensions })}
              />

              <BytesField
                label="Max file size"
                value={cfg.content.maxFileBytes}
                onChange={(maxFileBytes) => patchContent({ maxFileBytes })}
              />
            </fieldset>
          </div>
        </div>
      )}

      <div className="card-actions">
        <button type="button" className="btn btn-primary" disabled={!draft || saving || !dirty} onClick={save}>
          {saving ? <Spinner /> : null} Save configuration
        </button>
        <button
          type="button"
          className="btn"
          disabled={!dirty || saving}
          onClick={() => {
            setDraft(remoteConfig);
            setLocalErrors([]);
            setSaveError(null);
          }}
        >
          Revert
        </button>
      </div>
    </div>
  );
}

/**
 * The daemon's boot-time browse roots. Read-only here on purpose: they come
 * from the plugin's Unraid settings page and bound everything else in this
 * card (and every fs/* request the daemon will answer).
 */
function BrowseRoots({ roots }: { roots: string[] }) {
  return (
    <div className="field">
      <span className="field-label">Browse roots</span>
      {roots.length ? (
        <ul className="root-list">
          {roots.map((r) => (
            <li key={r} className="root-item mono">
              <Icon name="folder" />
              {r}
            </li>
          ))}
        </ul>
      ) : (
        <span className="muted small">The daemon reported no browse roots.</span>
      )}
      <span className="field-hint">
        Set in the plugin&rsquo;s Unraid settings page; index roots must lie within these.
      </span>
    </div>
  );
}

/* ------------------------------------------------------------ small inputs */

function ListEditor({
  label,
  hint,
  values,
  placeholder,
  onChange,
}: {
  label: string;
  hint?: string;
  values: string[];
  placeholder?: string;
  onChange: (v: string[]) => void;
}) {
  return (
    <div className="field">
      <span className="field-label">{label}</span>
      <div className="list-editor">
        {values.map((v, i) => (
          <div className="list-row" key={i}>
            <input
              className="input mono"
              value={v}
              placeholder={placeholder}
              onChange={(e) => {
                const next = values.slice();
                next[i] = e.target.value;
                onChange(next);
              }}
            />
            <button
              type="button"
              className="btn btn-icon"
              aria-label={`Remove ${v || "entry"}`}
              onClick={() => onChange(values.filter((_, j) => j !== i))}
            >
              <Icon name="close" />
            </button>
          </div>
        ))}
        <button type="button" className="btn btn-sm" onClick={() => onChange([...values, ""])}>
          + Add
        </button>
      </div>
      {hint ? <span className="field-hint">{hint}</span> : null}
    </div>
  );
}

function TagInput({
  label,
  hint,
  values,
  onChange,
}: {
  label: string;
  hint?: string;
  values: string[];
  onChange: (v: string[]) => void;
}) {
  const [text, setText] = useState("");

  const commit = (raw: string) => {
    const parts = raw
      .split(/[,\s]+/)
      .map((s) => s.trim().replace(/^\./, "").toLowerCase())
      .filter(Boolean);
    if (!parts.length) return;
    const set = new Set([...values, ...parts]);
    onChange([...set]);
    setText("");
  };

  return (
    <div className="field">
      <span className="field-label">{label}</span>
      <div className="tag-input">
        {values.map((v) => (
          <span className="tag" key={v}>
            {v}
            <button type="button" aria-label={`Remove ${v}`} onClick={() => onChange(values.filter((x) => x !== v))}>
              ×
            </button>
          </span>
        ))}
        <input
          className="tag-entry"
          value={text}
          placeholder="add extension…"
          onChange={(e) => {
            const v = e.target.value;
            if (v.endsWith(",")) commit(v);
            else setText(v);
          }}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              commit(text);
            } else if (e.key === "Backspace" && !text && values.length) {
              onChange(values.slice(0, -1));
            }
          }}
          onBlur={() => commit(text)}
        />
      </div>
      {hint ? <span className="field-hint">{hint}</span> : null}
    </div>
  );
}

const BYTE_UNITS = [
  { id: "B", mult: 1 },
  { id: "KiB", mult: 1024 },
  { id: "MiB", mult: 1024 ** 2 },
  { id: "GiB", mult: 1024 ** 3 },
];

function BytesField({ label, value, onChange }: { label: string; value: number; onChange: (n: number) => void }) {
  const unit = useMemo(() => {
    for (let i = BYTE_UNITS.length - 1; i >= 0; i--) {
      if (value >= BYTE_UNITS[i].mult && value % BYTE_UNITS[i].mult === 0) return BYTE_UNITS[i];
    }
    return BYTE_UNITS[0];
  }, [value]);
  const [unitId, setUnitId] = useState(unit.id);
  const active = BYTE_UNITS.find((u) => u.id === unitId) ?? BYTE_UNITS[0];

  return (
    <div className="field">
      <span className="field-label">{label}</span>
      <span className="input-group">
        <input
          className="input num"
          type="number"
          min={0}
          value={Math.round((value / active.mult) * 100) / 100}
          onChange={(e) => onChange(Math.round(Number(e.target.value) * active.mult))}
        />
        <select className="input select unit" value={unitId} onChange={(e) => setUnitId(e.target.value)}>
          {BYTE_UNITS.map((u) => (
            <option key={u.id} value={u.id}>
              {u.id}
            </option>
          ))}
        </select>
      </span>
      <span className="field-hint">{formatSize(value)} — files larger than this are never content-indexed.</span>
    </div>
  );
}

/* ------------------------------------------------------------ health card */

function HealthCard() {
  const health = useAsync<HealthResult>((signal) => healthz(signal), []);
  return (
    <div className="card card-compact">
      <div className="card-head">
        <h2>Daemon</h2>
        <button type="button" className="btn btn-sm" onClick={health.reload}>
          Refresh
        </button>
      </div>
      {health.error ? <ErrorBanner error={health.error} onRetry={health.reload} /> : null}
      {health.loading && !health.data ? (
        <SkeletonBlock lines={2} />
      ) : health.data ? (
        <dl className="stat-grid">
          <Stat label="Version">{health.data.version}</Stat>
          <Stat label="Uptime">{formatDuration(health.data.uptimeSec)}</Stat>
          <Stat label="Index DB">
            <span className={`state state-${health.data.indexDb === "ok" ? "idle" : "error"}`}>{health.data.indexDb}</span>
          </Stat>
          <Stat label="Browse roots">
            {health.data.roots?.length ? (
              <span className="mono small" title={health.data.roots.join("\n")}>
                {health.data.roots.join(", ")}
              </span>
            ) : (
              <span className="muted">none</span>
            )}
          </Stat>
        </dl>
      ) : null}
    </div>
  );
}
