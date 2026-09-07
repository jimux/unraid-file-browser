import { useCallback, useEffect, useMemo, useState } from "react";
import { encodings as fetchEncodings, hex as fetchHex, rawUrl, stat, view as fetchView } from "../api/client";
import type { Entry, EncodingOption, HexResult, ViewResult } from "../api/types";
import { ErrorBanner, SkeletonBlock, Spinner } from "../components/Feedback";
import { Icon } from "../components/Icon";
import { useAsync } from "../hooks/useAsync";
import { formatBytesExact, formatOffsetHex, formatSize } from "../lib/format";
import { isMedia, mediaKind, openPlayer, playability, playerUrl } from "../lib/media";
import { basename, isVirtual } from "../lib/paths";

const TEXT_WINDOW = 262144; // API.md default; max 1048576
const HEX_WINDOW = 4096; // API.md default; max 65536

/**
 * Image types the daemon still serves inline (everything else — SVG very much
 * included — now comes back as application/octet-stream + attachment +
 * nosniff, so an <img> pointing at it would only ever render broken). Keep this
 * in lockstep with the daemon's inline-media allowlist.
 */
const INLINE_IMAGE_MIMES = new Set([
  "image/png",
  "image/jpeg",
  "image/gif",
  "image/webp",
  "image/bmp",
  "image/avif",
]);

function isInlineImage(mime: string | undefined): boolean {
  if (!mime) return false;
  // Tolerate parameters, e.g. "image/jpeg; charset=binary".
  return INLINE_IMAGE_MIMES.has(mime.split(";")[0].trim().toLowerCase());
}

type Tab = "text" | "hex" | "image" | "media" | "download";

export function ViewerPanel({ path, onClose }: { path: string; onClose: () => void }) {
  const entryState = useAsync<Entry>((signal) => stat(path, signal).then((r) => r.entry), [path]);
  const entry = entryState.data;
  const isImage = isInlineImage(entry?.mime);
  const isPlayable = isMedia(entry?.mime);

  const [tab, setTab] = useState<Tab>("text");
  const [tabPinned, setTabPinned] = useState(false);

  // Default to the richest tab the file supports, unless the user chose one.
  useEffect(() => {
    if (tabPinned) return;
    if (isImage) setTab("image");
    else if (isPlayable) setTab("media");
  }, [isImage, isPlayable, tabPinned]);

  useEffect(() => {
    setTabPinned(false);
    setTab("text");
  }, [path]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const pick = (t: Tab) => {
    setTabPinned(true);
    setTab(t);
  };

  return (
    <section className="viewer" aria-label={`Viewer: ${basename(path)}`}>
      <header className="viewer-head">
        <div className="viewer-title" title={path}>
          <Icon name="file" />
          <strong>{basename(path)}</strong>
          {entry ? (
            <span className="viewer-meta" title={formatBytesExact(entry.size)}>
              {formatSize(entry.size)} · {entry.mime || "unknown type"}
            </span>
          ) : null}
        </div>
        <nav className="viewer-tabs" role="tablist">
          {(["text", "hex", "image", "media", "download"] as Tab[]).map((t) => {
            const disabled = (t === "image" && !isImage) || (t === "media" && !isPlayable);
            return (
              <button
                key={t}
                type="button"
                role="tab"
                aria-selected={tab === t}
                className={`btn btn-tab${tab === t ? " is-active" : ""}`}
                disabled={disabled}
                title={
                  disabled
                    ? t === "media"
                      ? "Not an audio or video file"
                      : "Not an inline-previewable image (png, jpeg, gif, webp, bmp, avif)"
                    : undefined
                }
                onClick={() => pick(t)}
              >
                {t[0].toUpperCase() + t.slice(1)}
              </button>
            );
          })}
        </nav>
        <button type="button" className="btn btn-icon" onClick={onClose} title="Close (Esc)" aria-label="Close viewer">
          <Icon name="close" />
        </button>
      </header>

      {entryState.error ? <ErrorBanner error={entryState.error} onRetry={entryState.reload} /> : null}

      <div className="viewer-body">
        {tab === "text" ? <TextTab path={path} /> : null}
        {tab === "hex" ? <HexTab path={path} /> : null}
        {tab === "image" ? <ImageTab path={path} /> : null}
        {tab === "media" ? <MediaTab path={path} mime={entry?.mime ?? ""} /> : null}
        {tab === "download" ? <DownloadTab path={path} entry={entry} /> : null}
      </div>
    </section>
  );
}

/* -------------------------------------------------------------------- text */

function TextTab({ path }: { path: string }) {
  const [encoding, setEncoding] = useState("auto");
  const [offset, setOffset] = useState(0);
  const [wrap, setWrap] = useState(false);

  useEffect(() => {
    setOffset(0);
    setEncoding("auto");
  }, [path]);

  const encState = useAsync<EncodingOption[]>((signal) => fetchEncodings(signal), []);
  const viewState = useAsync<ViewResult>(
    (signal) => fetchView({ path, encoding, offset, length: TEXT_WINDOW }, signal),
    [path, encoding, offset],
  );

  const v = viewState.data;
  const step = v?.length && v.length > 0 ? v.length : TEXT_WINDOW;
  const end = v ? v.offset + v.length : 0;
  const canPrev = offset > 0;
  const canNext = !!v && v.truncated;

  const options = useMemo(() => {
    const list = encState.data ?? [];
    return [{ id: "auto", label: "Auto-detect" }, ...list.filter((e) => e.id !== "auto")];
  }, [encState.data]);

  return (
    <div className="tab-pane tab-text">
      <div className="toolbar">
        <label className="field-inline">
          Encoding
          <select
            className="input select"
            value={encoding}
            onChange={(e) => {
              setEncoding(e.target.value);
              setOffset(0);
            }}
          >
            {options.map((o) => (
              <option key={o.id} value={o.id}>
                {o.label}
              </option>
            ))}
          </select>
        </label>

        {v ? (
          <span className="pills">
            <span className="pill" title="Encoding actually used">
              used: {v.encoding}
            </span>
            {v.sniffed ? (
              <span className="pill pill-muted" title="What detection suggested">
                sniffed: {v.sniffed}
              </span>
            ) : null}
            {v.lossy ? (
              <span className="pill pill-warn" title="Replacement characters were produced — try another encoding or the Hex tab">
                lossy
              </span>
            ) : null}
          </span>
        ) : null}

        <label className="field-inline check">
          <input type="checkbox" checked={wrap} onChange={(e) => setWrap(e.target.checked)} />
          Wrap
        </label>

        <div className="toolbar-spacer" />

        <WindowPager
          canPrev={canPrev}
          canNext={canNext}
          onPrev={() => setOffset((o) => Math.max(0, o - step))}
          onNext={() => setOffset(() => end)}
          label={
            v
              ? `showing bytes ${v.offset.toLocaleString()}–${Math.max(v.offset, end - 1).toLocaleString()} of ${v.size.toLocaleString()}`
              : ""
          }
          busy={viewState.loading}
        />
      </div>

      {viewState.error ? <ErrorBanner error={viewState.error} onRetry={viewState.reload} /> : null}
      {encState.error ? <ErrorBanner error={encState.error} onRetry={encState.reload} /> : null}

      {viewState.loading && !v ? (
        <SkeletonBlock lines={18} />
      ) : v && !viewState.error ? (
        <pre className={`text-view${wrap ? " is-wrapped" : ""}`}>{v.text}</pre>
      ) : null}
    </div>
  );
}

/* --------------------------------------------------------------------- hex */

function HexTab({ path }: { path: string }) {
  const [offset, setOffset] = useState(0);
  useEffect(() => setOffset(0), [path]);

  const hexState = useAsync<HexResult>((signal) => fetchHex({ path, offset, length: HEX_WINDOW }, signal), [path, offset]);
  const h = hexState.data;
  // Bytes actually returned = hex pairs across the rows (last row may be short).
  const consumed = h ? h.rows.reduce((n, r) => n + (r.hex.trim() ? r.hex.trim().split(/\s+/).length : 0), 0) : 0;
  const end = offset + (consumed || HEX_WINDOW);

  return (
    <div className="tab-pane tab-hex">
      <div className="toolbar">
        <span className="pill">4 KiB windows</span>
        <div className="toolbar-spacer" />
        <WindowPager
          canPrev={offset > 0}
          canNext={!!h?.truncated}
          onPrev={() => setOffset((o) => Math.max(0, o - HEX_WINDOW))}
          onNext={() => setOffset(end)}
          label={
            h ? `showing bytes ${offset.toLocaleString()}–${Math.max(offset, end - 1).toLocaleString()} of ${h.size.toLocaleString()}` : ""
          }
          busy={hexState.loading}
        />
      </div>

      {hexState.error ? <ErrorBanner error={hexState.error} onRetry={hexState.reload} /> : null}

      {hexState.loading && !h ? (
        <SkeletonBlock lines={20} />
      ) : h && !hexState.error ? (
        <div className="hex-view">
          {h.rows.map((r) => (
            <div className="hex-row" key={r.offset}>
              <span className="hex-off">{formatOffsetHex(r.offset)}</span>
              <span className="hex-bytes">{r.hex}</span>
              <span className="hex-ascii">{r.ascii}</span>
            </div>
          ))}
          {h.rows.length === 0 ? <div className="muted pad">Empty file.</div> : null}
        </div>
      ) : null}
    </div>
  );
}

/* ------------------------------------------------------------------- image */

function ImageTab({ path }: { path: string }) {
  const [failed, setFailed] = useState(false);
  useEffect(() => setFailed(false), [path]);
  return (
    <div className="tab-pane tab-image">
      {failed ? (
        <ErrorBanner error="The image could not be decoded by the browser. Try the Hex tab or download it." />
      ) : (
        <div className="image-wrap">
          <img src={rawUrl(path)} alt={basename(path)} onError={() => setFailed(true)} />
        </div>
      )}
    </div>
  );
}

/* ------------------------------------------------------------------- media */

/**
 * Audio/video files are *played* in their own window (`#/play/…`), not inside
 * this panel — a 40 GB movie has no business sharing a 300px pane with a hex
 * dump. This tab is the launcher, plus a small in-panel preview so the user can
 * confirm the file is what they think before opening a window for it.
 */
function MediaTab({ path, mime }: { path: string; mime: string }) {
  const kind = mediaKind(mime) ?? "video";
  const verdict = useMemo(() => playability(kind, mime), [kind, mime]);
  const [failed, setFailed] = useState(false);
  useEffect(() => setFailed(false), [path]);

  const unsupported = verdict === "no" || failed;

  return (
    <div className="tab-pane tab-media">
      <div className="media-launch">
        <button
          type="button"
          className="btn btn-primary btn-play"
          onClick={() => openPlayer(path)}
          title="Opens a separate window streaming from fs/raw"
        >
          <span className="play-glyph" aria-hidden="true">
            ▶
          </span>
          Play in new window
        </button>
        <a className="btn" href={rawUrl(path, true)} download={basename(path)}>
          Download
        </a>
      </div>

      <div className="muted small media-note">
        {unsupported ? (
          <>
            Your browser can’t play <code className="mono">{mime || "this type"}</code> natively — download the file or
            use an external player. There is no server-side transcoding.
          </>
        ) : isVirtual(path) ? (
          <>Streams from inside the archive: playback works, but seeking does not (no Range support on virtual paths).</>
        ) : (
          <>Streams with HTTP Range, so seeking works without downloading the whole file.</>
        )}
        <div className="media-url mono" title={playerUrl(path)}>
          {playerUrl(path)}
        </div>
      </div>

      {unsupported ? null : (
        <div className="media-preview">
          {kind === "video" ? (
            // eslint-disable-next-line jsx-a11y/media-has-caption
            <video src={rawUrl(path)} controls preload="metadata" playsInline onError={() => setFailed(true)} />
          ) : (
            // eslint-disable-next-line jsx-a11y/media-has-caption
            <audio src={rawUrl(path)} controls preload="metadata" onError={() => setFailed(true)} />
          )}
        </div>
      )}
    </div>
  );
}

/* ---------------------------------------------------------------- download */

function DownloadTab({ path, entry }: { path: string; entry: Entry | null }) {
  return (
    <div className="tab-pane tab-download">
      <div className="download-card">
        <div className="download-name">{basename(path)}</div>
        <div className="muted">{entry ? `${formatSize(entry.size)} · ${entry.mime || "unknown type"}` : "…"}</div>
        <p className="muted small">
          Streams from the daemon through the authenticated webGUI bridge. Archive entries are decompressed on the fly and
          do not support resume.
        </p>
        {/*
          Download only. There is deliberately no "open raw in a new tab" link:
          fs/raw would be rendered top-level on the webGUI origin, and the
          Download button already covers every legitimate use of the bytes.
        */}
        <a className="btn btn-primary" href={rawUrl(path, true)} download={basename(path)}>
          Download file
        </a>
      </div>
    </div>
  );
}

/* ------------------------------------------------------------------ pager */

function WindowPager({
  canPrev,
  canNext,
  onPrev,
  onNext,
  label,
  busy,
}: {
  canPrev: boolean;
  canNext: boolean;
  onPrev: () => void;
  onNext: () => void;
  label: string;
  busy: boolean;
}) {
  const prev = useCallback(() => canPrev && onPrev(), [canPrev, onPrev]);
  const next = useCallback(() => canNext && onNext(), [canNext, onNext]);
  return (
    <div className="pager">
      {busy ? <Spinner /> : null}
      <span className="pager-label">{label}</span>
      <button type="button" className="btn btn-sm" onClick={prev} disabled={!canPrev}>
        ‹ Prev
      </button>
      <button type="button" className="btn btn-sm" onClick={next} disabled={!canNext}>
        Next ›
      </button>
    </div>
  );
}
