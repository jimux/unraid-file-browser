import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  ApiError,
  beaconCloseMediaSession,
  closeMediaSession,
  createMediaSession,
  listDir,
  mediaCapabilities,
  mediaProbe,
  rawUrl,
  stat,
} from "../api/client";
import type { Entry, MediaProbe, MediaSession, ProbeAudioStream, SortDir, SortKey } from "../api/types";
import { ErrorBanner, Spinner } from "../components/Feedback";
import { Icon } from "../components/Icon";
import { ContextMenu, useContextMenu, type MenuItem } from "../components/ContextMenu";
import { useAsync } from "../hooks/useAsync";
import { copyAndToast, triggerDownload } from "../lib/actions";
import { browserMediaSupport, useNativeHls } from "../lib/codecs";
import { formatBytesExact, formatSize } from "../lib/format";
import { attachHls, type HlsAttachment } from "../lib/hls";
import { canPlayDirectly, isPlaylistEntry, mediaKind, mediaMimeFor, type MediaKind } from "../lib/media";
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
const MAX_HEIGHT_KEY = "fb.player.maxHeight";

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
 * The quality control. `0` is "Original", which on a file this browser can
 * decode means the direct `fs/raw` bytes and on one it cannot means "transcode
 * without capping the height".
 *
 * The presets exist for the case the file plays *perfectly well* and is simply
 * too fat for the link — a 60 Mbps 4K remux over wifi — where trading
 * resolution for a stream that does not stutter is the whole point.
 */
const QUALITY_CAPS: { label: string; value: number }[] = [
  { label: "Original", value: 0 },
  { label: "1080p", value: 1080 },
  { label: "720p", value: 720 },
  { label: "480p", value: 480 },
];

/** Remembered across files *and* windows: a laptop on wifi wants 720p always. */
function readMaxHeight(): number {
  try {
    const v = Number(window.localStorage.getItem(MAX_HEIGHT_KEY));
    return QUALITY_CAPS.some((q) => q.value === v) ? v : 0;
  } catch {
    return 0;
  }
}

function writeMaxHeight(v: number): void {
  try {
    window.localStorage.setItem(MAX_HEIGHT_KEY, String(v));
  } catch {
    /* the preference just does not persist */
  }
}

/** What the media element is actually reading. */
type Source = { kind: "direct"; src: string } | { kind: "hls"; session: MediaSession };

/**
 * Everything the ladder decides for *one file*. Keyed by path rather than
 * reset in an effect: a ↓ into the next track must not inherit the previous
 * file's escalation, audio track or resume position, and an effect-based reset
 * would leave one render — and one session-creating effect pass — running on
 * the stale values.
 */
interface LadderState {
  forPath: string;
  /** The direct element failed (or was never possible): use HLS. */
  escalated: boolean;
  directFailure: "network" | "unsupported" | null;
  /** ffprobe stream index; undefined = the daemon's own default track. */
  audioIndex?: number;
  /** 0 = Original. */
  maxHeight: number;
  streamError: string | null;
  /** Resume point carried across a source switch. */
  startAt: number;
  startPaused: boolean;
  /** Bumped by "Try again" to force a new session. */
  retry: number;
}

function freshLadder(forPath: string): LadderState {
  return {
    forPath,
    escalated: false,
    directFailure: null,
    maxHeight: readMaxHeight(),
    streamError: null,
    startAt: 0,
    startPaused: false,
    retry: 0,
  };
}

/**
 * Standalone media player — the whole document, no sidebar/topbar, because it
 * is meant to live in its own popup window
 * (`#/play/<encoded path>?sort=&dir=`).
 *
 * ## The playback ladder
 *
 *  1. **Direct.** The daemon streams `video/*`/`audio/*` inline with Range
 *     support, so when this browser can decode the file we point a plain
 *     `<video>`/`<audio>` at `fs/raw` and no server-side work happens at all.
 *  2. **HLS.** Otherwise — an AVI, an HEVC mkv, an AC-3 track Chrome refuses,
 *     a file the daemon will not serve inline, an element that mounted and
 *     then fired `error`, or a quality cap the user picked by hand — we tell
 *     the daemon what this browser can decode (`lib/codecs.ts`) and it answers
 *     with an HLS session: a remux when only the container is wrong, a real
 *     transcode when a codec (or the requested height) demands one.
 *  3. **Neither.** ffmpeg missing on the server, or a file with nothing
 *     decodable in it: say exactly which, and offer the bytes.
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
   * that file gets its own turn up the ladder. Skipping ahead would be
   * guesswork and would silently drop files from the queue.
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
  const mime = entry ? mediaMimeFor(entry) : "";

  /* ------------------------------------------------------------- the ladder */

  const [ladderState, setLadderState] = useState<LadderState>(() => freshLadder(path));
  const ladder = ladderState.forPath === path ? ladderState : freshLadder(path);
  const patchLadder = useCallback(
    (p: Partial<LadderState>) =>
      setLadderState((s) => ({ ...(s.forPath === path ? s : freshLadder(path)), ...p, forPath: path })),
    [path],
  );

  /** Rung one: worth pointing an element straight at `fs/raw`? */
  const playsHere = useMemo(() => canPlayDirectly(entry), [entry]);
  const directOk = playsHere && !ladder.directFailure;
  /** The file itself forces HLS — nothing to do with the quality control. */
  const needsHls = !!entry && !!kind && !directOk;
  const wantHls = needsHls || (!!entry && !!kind && ladder.maxHeight > 0);

  /**
   * Capabilities are fetched for every media file, not just the ones that need
   * transcoding: the quality presets are always on screen and have to know
   * whether they can be offered. The client memoises the request, so it is one
   * small GET per player window.
   */
  const capsState = useAsync(() => mediaCapabilities(), [], !!entry && !!kind);
  const caps = capsState.data;
  const capsPending = capsState.loading && !caps && !capsState.error;
  const transcodeUsable = !!caps?.available;
  const unavailableReason = capsState.error ? capsState.error.message : caps?.reason || "ffmpeg is not installed";

  const buildSession = wantHls && transcodeUsable;

  /**
   * The stream list, for the audio-track picker. Pinned to the path for the
   * same reason `entryState` is, and skipped for archive-internal paths, which
   * `/media/probe` refuses outright (ffprobe needs a real file).
   */
  const probeState = useAsync<{ forPath: string; probe: MediaProbe }>(
    (signal) => mediaProbe(path, signal).then((p) => ({ forPath: path, probe: p })),
    [path],
    buildSession && !isVirtual(path),
  );
  const probe = probeState.data?.forPath === path ? probeState.data.probe : null;
  const audioTracks: ProbeAudioStream[] = probe?.audio ?? [];

  /**
   * Session state is keyed the same way the ladder is, so a session belonging
   * to the previous file (or to the previous track/quality choice) can never
   * be rendered for a beat before the effect replaces it.
   */
  const sessionKey = `${path}|${ladder.audioIndex ?? ""}|${ladder.maxHeight}|${ladder.retry}`;
  const [sessionState, setSessionState] = useState<{
    key: string;
    session: MediaSession | null;
    error: ApiError | Error | null;
  }>({ key: "", session: null, error: null });
  const session = sessionState.key === sessionKey ? sessionState.session : null;
  const sessionError = sessionState.key === sessionKey ? sessionState.error : null;

  /**
   * Session lifecycle. One session per (file, audio track, quality cap); the
   * cleanup closes it on unmount, on a ↑/↓ to another file, and on every
   * switch of those selectors.
   *
   * The creation POST is deliberately *not* aborted on cleanup: an aborted
   * fetch would hide the id of a session the daemon may well have created,
   * leaking it until the TTL sweeper notices. Letting it land and closing it
   * immediately is what keeps rapid ↓-stepping from piling up ffmpeg
   * processes.
   */
  useEffect(() => {
    if (!buildSession) return;
    let cancelled = false;
    let created: string | null = null;
    createMediaSession({
      path,
      can: browserMediaSupport().can,
      audioIndex: ladder.audioIndex,
      maxHeight: ladder.maxHeight || undefined,
    })
      .then((s) => {
        created = s.id;
        if (cancelled) {
          void closeMediaSession(s.id).catch(() => undefined);
          return;
        }
        setSessionState({ key: sessionKey, session: s, error: null });
      })
      .catch((e: unknown) => {
        if (!cancelled) {
          setSessionState({ key: sessionKey, session: null, error: e instanceof Error ? e : new Error(String(e)) });
        }
      });
    return () => {
      cancelled = true;
      if (created) void closeMediaSession(created).catch(() => undefined);
    };
    // `sessionKey` is exactly the tuple this effect is keyed on.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [buildSession, sessionKey]);

  /**
   * Belt and braces for the window simply going away, where React's cleanup
   * never runs. `sendBeacon` POSTs without the CSRF header the bridge's
   * auto_prepend wants, so this is best-effort only — the unmount close above
   * is the real mechanism and the daemon's TTL sweeper is the backstop.
   */
  const sessionId = session?.id ?? null;
  useEffect(() => {
    if (!sessionId) return;
    const bye = () => void beaconCloseMediaSession(sessionId);
    window.addEventListener("pagehide", bye);
    window.addEventListener("beforeunload", bye);
    return () => {
      window.removeEventListener("pagehide", bye);
      window.removeEventListener("beforeunload", bye);
    };
  }, [sessionId]);

  /** Written by the stage; read when a switch remounts it. */
  const positionRef = useRef({ time: 0, paused: false });
  const onProgress = useCallback((time: number, paused: boolean) => {
    positionRef.current = { time, paused };
  }, []);

  /** Reopen the stream with a different setting, where the user left off. */
  const reopenWith = useCallback(
    (p: Partial<LadderState>) => {
      const { time, paused } = positionRef.current;
      patchLadder({ startAt: time, startPaused: paused, streamError: null, ...p });
    },
    [patchLadder],
  );

  const onDirectError = useCallback(
    (reason: "network" | "unsupported") => {
      // Rung two. Even a network drop is worth retrying as HLS: if the daemon
      // is alive it answers, and if it is not the session request says so.
      reopenWith({ directFailure: reason, escalated: true });
    },
    [reopenWith],
  );

  /**
   * Session first, direct second. Keeping the direct element mounted while a
   * session is being built is what makes a quality switch seamless — and what
   * lets a failed switch stay on the stream that is already playing.
   */
  const source: Source | null =
    !entry || !kind
      ? null
      : session
        ? { kind: "hls", session }
        : directOk
          ? { kind: "direct", src: rawUrl(path) }
          : null;

  const switching = buildSession && !session && !sessionError && source?.kind === "direct";

  const badge = switching
    ? { label: "Switching…", tip: "Asking the server for a transcoded stream" }
    : source?.kind === "hls"
      ? {
          label:
            source.session.mode === "remux"
              ? "Remuxing"
              : `Transcoding${qualitySuffix(ladder.maxHeight, source.session)}`,
          tip:
            source.session.reason ||
            (source.session.mode === "remux" ? "Container rewrapped as HLS" : "Re-encoded for this browser"),
        }
      : source
        ? { label: "Direct", tip: "Playing the original bytes from fs/raw — no server-side work." }
        : null;

  const unplayableHere = (
    <>
      Your browser can’t play <code className="mono">{mime || "this file"}</code> natively — download the file or use an
      external player.
    </>
  );

  const downloadBtn = (
    <a className="btn btn-primary" href={rawUrl(path, true)} download={name}>
      Download file
    </a>
  );

  /* ------------------------------------------------------------- the stage */

  let stage: ReactNode;
  if (entryState.error && !entry) {
    // A stat failure is survivable when the directory listing already handed
    // us this entry, and the error from the *previous* file must not flash
    // over the new one.
    stage = (
      <div className="player-stage">
        <div className="player-card">
          <ErrorBanner error={entryState.error} onRetry={entryState.reload} />
          {downloadBtn}
        </div>
      </div>
    );
  } else if (!entry) {
    stage = (
      <div className="player-stage">
        <Spinner label="loading…" />
      </div>
    );
  } else if (!kind) {
    stage = (
      <div className="player-stage">
        <div className="player-card">
          <div className="player-card-title">Not a media file</div>
          <p className="muted">{entry.mime || "This file"} is not audio or video, so there is nothing to play here.</p>
          {downloadBtn}
        </div>
      </div>
    );
  } else if (needsHls && !transcodeUsable && !capsPending) {
    // Rung three, case one: the file needs help and the server cannot give it.
    stage = (
      <div className="player-stage">
        <div className="player-card" data-testid="transcode-unavailable">
          <div className="player-card-title">
            {ladder.directFailure === "network" ? "The stream stopped" : "Can’t play this format"}
          </div>
          <p className="player-card-body">
            {ladder.directFailure === "network" ? (
              <>The connection to the daemon dropped while streaming {name}.</>
            ) : (
              unplayableHere
            )}
          </p>
          <p className="muted small">Transcoding is unavailable: {unavailableReason}.</p>
          <div className="player-card-actions">
            {downloadBtn}
            <a className="btn" href={viewHref(path, "hex")}>
              View as hex
            </a>
          </div>
        </div>
      </div>
    );
  } else if (sessionError && !source) {
    // Rung three, case two: ffmpeg is there but this file defeated it.
    stage = (
      <div className="player-stage">
        <div className="player-card" data-testid="session-error">
          <div className="player-card-title">Can’t play this format</div>
          <p className="player-card-body">{unplayableHere}</p>
          <p className="muted small">The server could not transcode it either: {errorText(sessionError)}</p>
          <div className="player-card-actions">
            {downloadBtn}
            <button type="button" className="btn" onClick={() => patchLadder({ retry: ladder.retry + 1 })}>
              Try again
            </button>
          </div>
        </div>
      </div>
    );
  } else if (ladder.streamError) {
    stage = (
      <div className="player-stage">
        <div className="player-card" data-testid="stream-error">
          <div className="player-card-title">The stream stopped</div>
          <p className="player-card-body">{ladder.streamError}</p>
          <div className="player-card-actions">
            {downloadBtn}
            <button
              type="button"
              className="btn"
              onClick={() => reopenWith({ streamError: null, retry: ladder.retry + 1 })}
            >
              Try again
            </button>
          </div>
        </div>
      </div>
    );
  } else if (!source) {
    stage = (
      <div className="player-stage">
        <div className="player-card" data-testid="player-preparing">
          <Spinner label={capsPending ? "checking transcoder…" : "Preparing stream…"} />
          <p className="muted small">
            {ladder.directFailure
              ? "This browser could not decode the original, so the server is repackaging it."
              : "The server is repackaging this file into a stream your browser can play."}
          </p>
        </div>
      </div>
    );
  } else {
    stage = (
      // Keyed on the path *and* the source: a ↑/↓ swap, and every session
      // switch, get a fresh element with the spinner, failure and
      // autoplay-blocked state reset — which is exactly what they need.
      <MediaStage
        key={`${path}|${source.kind === "hls" ? source.session.id : "direct"}`}
        path={path}
        name={name}
        kind={kind}
        source={source}
        startAt={ladder.startAt}
        startPaused={ladder.startPaused}
        onPrev={prev ? () => goTo(prev) : null}
        onNext={next ? () => goTo(next) : null}
        canClose={canClose}
        onEnded={onEnded}
        onProgress={onProgress}
        onDirectError={onDirectError}
        onStreamError={(m) => patchLadder({ streamError: m })}
        hasPlaylist={playlist.length > 1}
      />
    );
  }

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
          {badge ? (
            <span className="player-badge" data-mode={badge.label.split(" ")[0]} title={badge.tip} data-testid="player-badge">
              {badge.label}
            </span>
          ) : null}
        </div>

        {/* The track picker only exists once a session does: it is one of the
            two knobs that session was built with. */}
        {session && audioTracks.length > 1 ? (
          <label className="player-select" title="Audio track — switching reopens the stream at the same position">
            <span className="player-select-label">Audio</span>
            <select
              value={ladder.audioIndex ?? defaultTrackIndex(audioTracks)}
              onChange={(e) => reopenWith({ audioIndex: Number(e.target.value) })}
              data-testid="player-audio-track"
            >
              {audioTracks.map((t) => (
                <option key={t.index} value={t.index}>
                  {audioTrackLabel(t)}
                </option>
              ))}
            </select>
          </label>
        ) : null}

        {/* Always on screen: "this plays, but not smoothly" is exactly the case
            the presets exist for, and it is invisible from here. */}
        {entry && kind ? (
          <label
            className="player-select"
            title={
              transcodeUsable || capsPending
                ? "Cap the height the server streams — lower is smaller on the wire"
                : `Transcoding is unavailable: ${unavailableReason}`
            }
          >
            <span className="player-select-label">Quality</span>
            <select
              value={ladder.maxHeight}
              onChange={(e) => {
                const v = Number(e.target.value);
                writeMaxHeight(v);
                reopenWith({ maxHeight: v });
              }}
              data-testid="player-quality"
            >
              {QUALITY_CAPS.map((q) => (
                <option
                  key={q.value}
                  value={q.value}
                  // Original always works; the presets need a transcoder.
                  disabled={q.value > 0 && !transcodeUsable && !capsPending}
                >
                  {q.label}
                </option>
              ))}
            </select>
          </label>
        ) : null}

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

      {/* A switch that failed with something still playing: say so, keep
          playing, and offer the one-click way back. */}
      {sessionError && source ? (
        <div className="player-banner" role="alert" data-testid="switch-error">
          <Icon name="warn" />
          <span>Could not switch stream: {errorText(sessionError)}</span>
          {ladder.maxHeight > 0 ? (
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => {
                writeMaxHeight(0);
                reopenWith({ maxHeight: 0 });
              }}
            >
              Back to Original
            </button>
          ) : null}
        </div>
      ) : null}

      {stage}
    </div>
  );
}

function errorText(e: ApiError | Error): string {
  return e instanceof ApiError ? `${e.code}: ${e.message}` : e.message;
}

/** " 720p" when a cap is in force, otherwise the height the daemon chose. */
function qualitySuffix(maxHeight: number, session: MediaSession): string {
  const h = maxHeight || session.video?.height || 0;
  return h > 0 ? ` ${h}p` : "";
}

/** ffprobe's stream index of the track the daemon would pick unprompted. */
function defaultTrackIndex(tracks: ProbeAudioStream[]): number {
  return (tracks.find((t) => t.default) ?? tracks[0])?.index ?? 0;
}

function audioTrackLabel(t: ProbeAudioStream): string {
  const bits = [t.lang || "und", t.title, t.codec, t.channels ? `${t.channels}ch` : ""].filter(Boolean);
  return bits.join(" · ");
}

/* -------------------------------------------------------------------- stage */

function MediaStage({
  path,
  name,
  kind,
  source,
  startAt,
  startPaused,
  canClose,
  onEnded,
  onProgress,
  onDirectError,
  onStreamError,
  onPrev,
  onNext,
  hasPlaylist,
}: {
  path: string;
  name: string;
  kind: MediaKind;
  source: Source;
  /** Seconds to resume at after a switch; 0 = from the start. */
  startAt: number;
  /** The user had it paused when they switched — do not start it playing. */
  startPaused: boolean;
  canClose: boolean;
  onEnded: () => void;
  onProgress: (seconds: number, paused: boolean) => void;
  /** The direct `fs/raw` element failed — the ladder escalates to HLS. */
  onDirectError: (reason: "network" | "unsupported") => void;
  /** An HLS stream failed fatally, after hls.js exhausted its own recovery. */
  onStreamError: (message: string) => void;
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

  const [ready, setReady] = useState(false);
  const [autoplayBlocked, setAutoplayBlocked] = useState(false);

  // Archive-internal entries stream without Range, so the browser cannot seek
  // the *direct* path. An HLS session is segment-addressed, so it always can.
  const seekable = source.kind === "hls" || !isVirtual(path);

  /**
   * Which code owns a media-element `error`. hls.js reports its own failures
   * (and recovers from most of them), so an element error under it must not be
   * double-handled; the native-HLS transport has no such owner.
   */
  const transportRef = useRef<"direct" | "native" | "hls.js">("direct");
  const cbRef = useRef({ onDirectError, onStreamError });
  cbRef.current = { onDirectError, onStreamError };

  useEffect(() => {
    if (source.kind !== "hls") return;
    const el = ref.current;
    if (!el) return;
    let dead = false;
    let attachment: HlsAttachment | null = null;
    const ac = new AbortController();
    attachHls(
      el,
      { session: source.session, startAt, signal: ac.signal, onFatal: (m) => cbRef.current.onStreamError(m) },
      useNativeHls(),
    )
      .then((a) => {
        if (dead) {
          a.destroy();
          return;
        }
        attachment = a;
        transportRef.current = a.transport;
      })
      .catch((e: unknown) => {
        if (dead || (e instanceof DOMException && e.name === "AbortError")) return;
        cbRef.current.onStreamError(e instanceof Error ? e.message : String(e));
      });
    return () => {
      dead = true;
      ac.abort();
      attachment?.destroy();
    };
    // `source` is stable for the life of this component (the key changes when
    // the session does) and `startAt` is read once, at attach time.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  /**
   * Resume where the previous source left off. hls.js is told through its
   * `startPosition`; a native or direct element has to be seeked once its
   * metadata is in.
   */
  useEffect(() => {
    const el = ref.current;
    if (!el || startAt <= 0) return;
    const apply = () => {
      try {
        if (Math.abs(el.currentTime - startAt) > 0.5) el.currentTime = startAt;
      } catch {
        /* not seekable yet; the next loadedmetadata will try again */
      }
    };
    if (el.readyState >= 1) apply();
    el.addEventListener("loadedmetadata", apply);
    return () => el.removeEventListener("loadedmetadata", apply);
  }, [startAt]);

  const onError = useCallback(() => {
    const err = ref.current?.error;
    const reason = err && err.code === MediaError.MEDIA_ERR_NETWORK ? "network" : "unsupported";
    if (source.kind === "direct") cbRef.current.onDirectError(reason);
    else if (transportRef.current === "native")
      cbRef.current.onStreamError(
        reason === "network"
          ? `The connection dropped while streaming ${name}.`
          : `The transcoded stream could not be decoded (${err?.message || "no detail"}).`,
      );
    // hls.js owns its own errors and gets to try recovering first.
  }, [name, source.kind]);

  // Autoplay: the popup itself was opened by a click, but that user activation
  // does not carry into the new document, so an audible autoplay may still be
  // refused. Say so rather than looking broken.
  useEffect(() => {
    const el = ref.current;
    if (!el || startPaused) return;
    const p = el.play();
    if (p && typeof p.catch === "function") {
      p.then(
        () => setAutoplayBlocked(false),
        (e: unknown) => {
          if (e instanceof DOMException && e.name === "NotAllowedError") setAutoplayBlocked(true);
        },
      );
    }
    // Mount only: a re-run would fight the user's own pause.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

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

  const report = () => onProgress(ref.current?.currentTime ?? 0, ref.current?.paused ?? false);

  const common = {
    ref,
    // HLS sources are fed by hls.js (MediaSource) or by a rewritten playlist
    // blob — either way the element must not be given a `src` here.
    src: source.kind === "direct" ? source.src : undefined,
    controls: true,
    autoPlay: !startPaused,
    /*
     * "Buffer as far ahead as you can": on the direct path this is the only
     * knob there is, and it tells the browser to keep fetching while paused.
     * The HLS path gets the same policy through hls.js's buffer config
     * (lib/hls.ts) — the ceiling in both cases is the browser's, not ours.
     */
    preload: "auto" as const,
    onCanPlay: () => setReady(true),
    onPlaying: () => {
      setReady(true);
      setAutoplayBlocked(false);
    },
    onTimeUpdate: report,
    onPause: report,
    onPlay: report,
    onError,
    onEnded,
    "data-testid": "player-media",
    "data-transport": source.kind,
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
