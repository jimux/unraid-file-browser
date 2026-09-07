import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { listDir, rawUrl, stat } from "../api/client";
import type { Entry, SortDir, SortKey } from "../api/types";
import { ErrorBanner, Spinner } from "../components/Feedback";
import { Icon } from "../components/Icon";
import { ContextMenu, useContextMenu, type MenuItem } from "../components/ContextMenu";
import { useAsync } from "../hooks/useAsync";
import { copyAndToast, triggerDownload } from "../lib/actions";
import { formatBytesExact, formatSize } from "../lib/format";
import {
  isPlaylistEntry,
  mediaKind,
  mediaMimeFor,
  playability,
  serverClassificationMessage,
  serverStreamsInline,
  type MediaKind,
} from "../lib/media";
import { basename, isVirtual, parentPath } from "../lib/paths";
import { navigate, playHref, viewHref } from "../lib/router";

const SEEK_STEP = 10; // seconds, ←/→

/**
 * One `fs/list` page is the whole playlist. 10 000 is the daemon's hard cap on
 * `limit`; a directory with more media files than that is not a listening
 * session, and paging it here would cost several proxy round-trips before the
 * first frame renders.
 */
const PLAYLIST_LIMIT = 10000;

const AUTO_ADVANCE_KEY = "fb.player.autoAdvance";

/** Defaults ON; a hostile/absent localStorage must not break the player. */
function readAutoAdvance(): boolean {
  try {
    return window.localStorage.getItem(AUTO_ADVANCE_KEY) !== "0";
  } catch {
    return true;
  }
}

function writeAutoAdvance(on: boolean): void {
  try {
    window.localStorage.setItem(AUTO_ADVANCE_KEY, on ? "1" : "0");
  } catch {
    /* private mode / storage disabled — the toggle just does not persist */
  }
}

const NO_SIBLINGS: Entry[] = [];

/**
 * Standalone media player — the whole document, no sidebar/topbar, because it
 * is meant to live in its own popup window
 * (`#/play/<encoded path>?sort=&dir=`).
 *
 * It plays `fs/raw` directly. There is no transcoding anywhere in this stack,
 * so the browser's own decoder is the entire compatibility story; when it
 * cannot decode the file we say so plainly and offer the bytes instead.
 *
 * `sort`/`dir` are the browser view's current ordering, carried in the hash:
 * ↑/↓ (and the ‹ › buttons) walk the media siblings of this file in that same
 * order, navigating this window in place rather than opening more of them.
 */
export function PlayerView({
  path,
  sort,
  dir,
  force,
}: {
  path: string;
  sort: SortKey;
  dir: SortDir;
  /** "Play as media…": mount the player for a file nothing detected as media. */
  force?: boolean;
}) {
  const name = basename(path);
  const parent = useMemo(() => parentPath(path), [path]);

  // `forPath` pins the answer to the request: while a ↑/↓ navigation is in
  // flight useAsync keeps the previous entry visible, and playing file B with
  // file A's mime would mount the wrong element for a beat.
  const entryState = useAsync<{ forPath: string; entry: Entry }>(
    (signal) => stat(path, signal).then((r) => ({ forPath: path, entry: r.entry })),
    [path],
  );

  /**
   * The playlist: this file's audio/video siblings in the caller's sort order.
   * `dirsFirst: false` because only files can be playlist members, so the
   * directory grouping the browser view applies is noise here.
   *
   * A failure is not surfaced: the file still plays, navigation is simply
   * unavailable and the "N of M" indicator does not render.
   */
  const siblingsState = useAsync<Entry[]>(
    (signal) =>
      listDir({ path: parent ?? "/", sort, dir, dirsFirst: false, limit: PLAYLIST_LIMIT }, signal).then((r) =>
        (r.entries ?? []).filter(isPlaylistEntry),
      ),
    [parent, sort, dir],
    parent !== null,
  );

  const playlist = siblingsState.data ?? NO_SIBLINGS;
  const index = useMemo(() => playlist.findIndex((e) => e.path === path), [playlist, path]);
  const prev = index > 0 ? playlist[index - 1] : null;
  const next = index >= 0 && index < playlist.length - 1 ? playlist[index + 1] : null;

  const goTo = useCallback(
    (target: Entry | null) => {
      if (target) navigate(playHref(target.path, { sort, dir }));
    },
    [sort, dir],
  );

  const [autoAdvance, setAutoAdvance] = useState(readAutoAdvance);
  const toggleAutoAdvance = useCallback((on: boolean) => {
    setAutoAdvance(on);
    writeAutoAdvance(on);
  }, []);

  /**
   * `ended` → the *immediate* next entry, even one this browser cannot decode:
   * the user then sees the fallback card and presses ↓ again. Skipping ahead to
   * the next decodable file would be guesswork (`canPlayType` is only a hint)
   * and would silently drop files from the queue.
   */
  const onEnded = useCallback(() => {
    if (autoAdvance) goTo(next);
  }, [autoAdvance, goTo, next]);

  /**
   * ↑/↓ live here rather than in <MediaStage> so they keep working on the
   * "can't play this format" and "not a media file" cards — skipping past a
   * file the browser refuses is exactly when you need them. ←/→ stay seek.
   */
  useEffect(() => {
    const onKey = (ev: KeyboardEvent) => {
      if (ev.key !== "ArrowUp" && ev.key !== "ArrowDown") return;
      if (ev.altKey || ev.ctrlKey || ev.metaKey) return;
      const target = ev.target as HTMLElement | null;
      if (target?.closest("input, textarea, select, [contenteditable=true]")) return;
      // Always swallow: a focused <video controls> maps ↑/↓ to volume, and at
      // the ends of the playlist the key should do nothing at all.
      ev.preventDefault();
      goTo(ev.key === "ArrowUp" ? prev : next);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [goTo, prev, next]);

  // Prefer the listing's copy of this entry while stat is in flight: it carries
  // the same mime and lets a ↑/↓ swap the source without a spinner in between.
  const listed = useMemo(() => (index >= 0 ? playlist[index] : null), [playlist, index]);
  const entry = (entryState.data?.forPath === path ? entryState.data.entry : null) ?? listed;

  // close() only works on script-opened windows; on a popup-blocked fallback we
  // are the main document and must not pretend otherwise.
  const canClose = useMemo(() => {
    try {
      return window.opener !== null && window.opener !== undefined;
    } catch {
      // Cross-origin opener still means *an* opener exists.
      return true;
    }
  }, []);

  useEffect(() => {
    const previous = document.title;
    document.title = `${name} — File Browser`;
    return () => {
      document.title = previous;
    };
  }, [name]);

  const kind = mediaKind(entry?.mime, entry?.name ?? name) ?? (force ? "video" : null);

  /**
   * The daemon, not the SPA, decides what `fs/raw` streams: only what *it*
   * classifies as `video/*` or `audio/*` comes back inline; everything else is
   * an `application/octet-stream` attachment a media element cannot read
   * (API.md, "Raw content policy"). So whenever our extension override — or
   * the user's "Play as media…" — is the only reason we are here, say so
   * before mounting an element that would just fire `error` seconds later.
   */
  const serverRefuses = !!entry && !serverStreamsInline(entry.mime);

  return (
    <div className={`player${kind === "video" ? " is-video" : ""}`}>
      <header className="player-head">
        <div className="player-title" title={path}>
          <Icon name="file" />
          <strong>{name}</strong>
          {entry ? (
            <span className="player-meta" title={formatBytesExact(entry.size)}>
              {formatSize(entry.size)} · {entry.mime || "unknown type"}
            </span>
          ) : null}
        </div>
        <div className="player-nav">
          <button
            type="button"
            className="btn btn-sm btn-step"
            onClick={() => goTo(prev)}
            disabled={!prev}
            aria-label="Previous media file"
            title="Previous (↑)"
            data-testid="player-prev"
          >
            ‹
          </button>
          {index >= 0 ? (
            <span className="player-count" data-testid="player-count">
              {index + 1} of {playlist.length}
            </span>
          ) : null}
          <button
            type="button"
            className="btn btn-sm btn-step"
            onClick={() => goTo(next)}
            disabled={!next}
            aria-label="Next media file"
            title="Next (↓)"
            data-testid="player-next"
          >
            ›
          </button>
        </div>

        <label className="player-auto" title="When a file ends, play the next one in this directory">
          <input
            type="checkbox"
            checked={autoAdvance}
            onChange={(e) => toggleAutoAdvance(e.target.checked)}
            data-testid="player-autoadvance"
          />
          Auto-advance
        </label>

        <a className="btn btn-sm" href={rawUrl(path, true)} download={name}>
          Download
        </a>
        {canClose ? (
          <button type="button" className="btn btn-sm" onClick={() => window.close()} title="Close window (Esc)">
            Close
          </button>
        ) : null}
      </header>

      {/* Only when we have nothing to play: a stat failure is survivable when
          the directory listing already handed us this entry, and the error
          from the *previous* file must not flash over the new one. */}
      {entryState.error && !entry ? (
        <div className="player-stage">
          <div className="player-card">
            <ErrorBanner error={entryState.error} onRetry={entryState.reload} />
            <a className="btn btn-primary" href={rawUrl(path, true)} download={name}>
              Download file
            </a>
          </div>
        </div>
      ) : !entry ? (
        <div className="player-stage">
          <Spinner label="loading…" />
        </div>
      ) : !kind ? (
        <div className="player-stage">
          <div className="player-card">
            <div className="player-card-title">Not a media file</div>
            <p className="muted">
              {entry.mime || "This file"} is not audio or video, so there is nothing to play here.
            </p>
            <a className="btn btn-primary" href={rawUrl(path, true)} download={name}>
              Download file
            </a>
          </div>
        </div>
      ) : serverRefuses ? (
        <div className="player-stage">
          <div className="player-card" data-testid="server-classification">
            <div className="player-card-title">The server won’t stream this file</div>
            <p className="player-card-body">{serverClassificationMessage(name, entry.mime)}</p>
            <p className="muted small">
              <code className="mono">fs/raw</code> serves only what the daemon itself classifies as{" "}
              <code className="mono">video/*</code> or <code className="mono">audio/*</code> inline; anything else
              arrives as an attachment, which no media element can play.
            </p>
            <div className="player-card-actions">
              <a className="btn btn-primary" href={rawUrl(path, true)} download={name}>
                Download file
              </a>
              <a className="btn" href={viewHref(path, "hex")}>
                View as hex
              </a>
            </div>
          </div>
        </div>
      ) : (
        // Keyed on the path: a ↑/↓ swap gets a fresh element with the spinner,
        // failure and autoplay-blocked state reset, which is exactly what a new
        // file needs.
        <MediaStage
          key={path}
          path={path}
          name={name}
          mime={mediaMimeFor(entry)}
          kind={kind}
          onPrev={prev ? () => goTo(prev) : null}
          onNext={next ? () => goTo(next) : null}
          canClose={canClose}
          onEnded={onEnded}
          hasPlaylist={playlist.length > 1}
        />
      )}
    </div>
  );
}

/* -------------------------------------------------------------------- stage */

function MediaStage({
  path,
  name,
  mime,
  kind,
  canClose,
  onEnded,
  onPrev,
  onNext,
  hasPlaylist,
}: {
  path: string;
  name: string;
  mime: string;
  kind: MediaKind;
  canClose: boolean;
  onEnded: () => void;
  /** null at the ends of the playlist — the menu item is then omitted. */
  onPrev: (() => void) | null;
  onNext: (() => void) | null;
  hasPlaylist: boolean;
}) {
  const ref = useRef<HTMLVideoElement & HTMLAudioElement>(null);
  const stageRef = useRef<HTMLDivElement>(null);
  // The menu's target is "was it paused when the menu opened" — the only thing
  // the Play/Pause label needs, and sampling it here keeps `paused` out of
  // React state (a re-render per play/pause event, on a control that is only
  // ever read at right-click time, is not a trade worth making).
  const { menu, openAt, close: closeMenu } = useContextMenu<boolean>();

  const verdict = useMemo(() => playability(kind, mime), [kind, mime]);
  // "no" refuses up front; mkv/mov answer "" but often play, so those attempt
  // and only fall back once the element actually errors.
  const [failure, setFailure] = useState<string | null>(verdict === "no" ? "unsupported" : null);
  const [ready, setReady] = useState(false);
  const [autoplayBlocked, setAutoplayBlocked] = useState(false);

  const src = useMemo(() => rawUrl(path), [path]);
  // Archive-internal entries stream without Range, so the browser cannot seek.
  const seekable = !isVirtual(path);

  const onError = useCallback(() => {
    const err = ref.current?.error;
    setFailure(err && err.code === MediaError.MEDIA_ERR_NETWORK ? "network" : "unsupported");
  }, []);

  // Autoplay: the popup itself was opened by a click, but that user activation
  // does not carry into the new document, so an audible autoplay may still be
  // refused. Say so rather than looking broken.
  useEffect(() => {
    const el = ref.current;
    if (!el || failure) return;
    const p = el.play();
    if (p && typeof p.catch === "function") {
      p.then(
        () => setAutoplayBlocked(false),
        (e: unknown) => {
          if (e instanceof DOMException && e.name === "NotAllowedError") setAutoplayBlocked(true);
        },
      );
    }
  }, [src, failure]);

  const toggleFullscreen = useCallback(() => {
    const target = kind === "video" ? (stageRef.current ?? ref.current) : stageRef.current;
    if (!target) return;
    if (document.fullscreenElement) void document.exitFullscreen?.();
    else void target.requestFullscreen?.();
  }, [kind]);

  const togglePlay = useCallback(() => {
    const el = ref.current;
    if (!el) return;
    if (el.paused) void el.play().catch(() => undefined);
    else el.pause();
    setAutoplayBlocked(false);
  }, []);

  /**
   * The stage's own menu. The native <video> menu is suppressed only where
   * ours opens, so a right-click anywhere else in the window (the header, the
   * hints) still gets the browser's.
   */
  const menuItems = useMemo<MenuItem[]>(() => {
    const items: MenuItem[] = [
      { id: "playpause", label: menu?.target === false ? "Pause" : "Play", onSelect: togglePlay },
    ];
    if (onPrev) items.push({ id: "prev", label: "Previous", onSelect: onPrev });
    if (onNext) items.push({ id: "next", label: "Next", onSelect: onNext });
    items.push(
      { id: "fullscreen", label: "Fullscreen", onSelect: () => toggleFullscreen() },
      { id: "download", label: "Download", separatorBefore: true, onSelect: () => triggerDownload(path, name) },
      { id: "copy-path", label: "Copy path", title: path, onSelect: () => void copyAndToast(path, "path") },
    );
    return items;
  }, [menu?.target, name, onNext, onPrev, path, togglePlay, toggleFullscreen]);

  useEffect(() => {
    const onKey = (ev: KeyboardEvent) => {
      const el = ref.current;
      const target = ev.target as HTMLElement | null;
      if (target?.closest("input, textarea, select, [contenteditable=true]")) return;

      switch (ev.key) {
        case " ":
        case "Spacebar": {
          if (!el) return;
          // Handle it ourselves so a focused <video controls> does not toggle twice.
          ev.preventDefault();
          togglePlay();
          break;
        }
        case "ArrowLeft":
        case "ArrowRight": {
          if (!el || !Number.isFinite(el.duration)) return;
          ev.preventDefault();
          const delta = ev.key === "ArrowRight" ? SEEK_STEP : -SEEK_STEP;
          el.currentTime = Math.max(0, Math.min(el.duration || 0, el.currentTime + delta));
          break;
        }
        case "f":
        case "F":
          ev.preventDefault();
          toggleFullscreen();
          break;
        case "Escape":
          if (document.fullscreenElement) return; // the browser exits fullscreen first
          if (canClose) window.close();
          // Popup-blocked fallback: this *is* the app window, so leave the user
          // somewhere useful instead of a dead end.
          else navigate(viewHref(path), true);
          break;
        default:
          break;
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [canClose, path, togglePlay, toggleFullscreen]);

  if (failure) {
    return (
      <div className="player-stage" ref={stageRef}>
        <div className="player-card">
          <div className="player-card-title">
            {failure === "network" ? "The stream stopped" : "Can’t play this format"}
          </div>
          <p className="player-card-body">
            {failure === "network" ? (
              <>The connection to the daemon dropped while streaming {name}.</>
            ) : (
              <>
                Your browser can’t play <code className="mono">{mime || "this file"}</code> natively — download the file
                or use an external player.
              </>
            )}
          </p>
          <p className="muted small">
            There is no server-side transcoding: the daemon streams the original bytes untouched.
          </p>
          <div className="player-card-actions">
            <a className="btn btn-primary" href={rawUrl(path, true)} download={name}>
              Download file
            </a>
            {failure === "network" ? (
              <button type="button" className="btn" onClick={() => setFailure(null)}>
                Try again
              </button>
            ) : null}
          </div>
        </div>
      </div>
    );
  }

  const common = {
    ref,
    src,
    controls: true,
    autoPlay: true,
    preload: "auto" as const,
    onCanPlay: () => setReady(true),
    onPlaying: () => {
      setReady(true);
      setAutoplayBlocked(false);
    },
    onError,
    onEnded,
    "data-testid": "player-media",
  };

  return (
    <div
      className="player-stage"
      ref={stageRef}
      onContextMenu={(ev) => {
        ev.preventDefault();
        openAt(ev, ref.current?.paused ?? true);
      }}
    >
      {kind === "video" ? (
        // eslint-disable-next-line jsx-a11y/media-has-caption
        <video {...common} playsInline className="player-video" />
      ) : (
        <div className="player-audio-wrap">
          <div className="player-audio-name">{name}</div>
          {/* eslint-disable-next-line jsx-a11y/media-has-caption */}
          <audio {...common} className="player-audio" />
        </div>
      )}

      {!ready ? (
        <div className="player-overlay" role="status">
          <Spinner label="buffering…" />
        </div>
      ) : null}

      {autoplayBlocked ? (
        <div className="player-note">Autoplay was blocked by the browser — press Play (or Space).</div>
      ) : null}

      <div className="player-hints">
        <span>Space play/pause</span>
        <span>←/→ ±{SEEK_STEP}s</span>
        {hasPlaylist ? <span>↑/↓ prev/next</span> : null}
        <span>F fullscreen</span>
        {canClose ? <span>Esc close</span> : null}
        {!seekable ? <span className="player-hint-warn">archive entry — no seeking</span> : null}
      </div>

      {menu ? (
        <ContextMenu x={menu.x} y={menu.y} items={menuItems} onClose={closeMenu} label={`Player actions for ${name}`} />
      ) : null}
    </div>
  );
}
