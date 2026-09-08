/**
 * HLS playback plumbing for the transcoding player.
 *
 * ## Why every URL has to be rewritten
 *
 * In production the SPA reaches the daemon through the PHP auth bridge:
 *
 *     ../proxy.php?p=%2Fapi%2Fv1%2Fmedia%2Fhls%2F<id>%2Findex.m3u8
 *
 * The daemon's playlist lists its segments as *bare relative names*
 * (`seg-00001.ts`), which a player resolves against the URL the playlist came
 * from — i.e. against `/plugins/filebrowser/`, producing
 * `/plugins/filebrowser/seg-00001.ts`. That 404s. There is no `<base>` trick
 * that fixes it either, because the bridge encodes the whole API path into a
 * query parameter, so nothing about the URL is hierarchical.
 *
 * So both transports rewrite:
 *
 *  - **hls.js** gets a `loader` subclass whose `load()` maps every requested
 *    URL back through `apiUrl()` before the request goes out. Mapping is by
 *    *file name*, which is the one part that survives whatever base hls.js
 *    resolved against — and in dev mode the mapping is the identity, so the
 *    same code path is exercised in both modes.
 *  - **native HLS** (Safari) offers no loader hook at all, so the playlist is
 *    fetched here, its URIs rewritten to absolute bridge URLs, and the result
 *    handed to the element as a `blob:` URL. (That, plus hls.js's MediaSource
 *    object URL, is why index.html's CSP needs `media-src 'self' blob:`.)
 */

import { apiUrl, hlsPath } from "../api/client";
import type { MediaSession } from "../api/types";
import type HlsJs from "hls.js";
import type { ErrorData, HlsConfig, LoaderCallbacks, LoaderConfiguration, LoaderContext } from "hls.js";

/**
 * Names the daemon serves out of a session directory. The extension list is
 * deliberately broader than today's `.ts` segments so an fMP4 ladder
 * (`init.mp4` + `.m4s`) keeps working without a SPA change.
 */
const HLS_FILE = /^[A-Za-z0-9][A-Za-z0-9._-]*\.(m3u8|ts|m4s|mp4|aac|vtt|key)$/i;

/**
 * The bare file name a player is really asking for, or null when the URL is
 * not one of ours (the bridge's own `proxy.php`, a data:/blob: URL, …).
 *
 * Pure and dependency-free so it can be unit-tested outside a browser.
 */
export function hlsFileName(requested: string): string | null {
  if (!requested) return null;
  const noHash = requested.split("#")[0];
  const noQuery = noHash.split("?")[0];
  const name = noQuery.slice(noQuery.lastIndexOf("/") + 1);
  if (!name || name.includes("..")) return null;
  return HLS_FILE.test(name) ? name : null;
}

/**
 * Map whatever URL the player resolved to the one the bridge understands.
 *
 *  - dev:  `/api/v1/media/hls/<id>/seg-00001.ts` → itself (identity)
 *  - prod: `/plugins/filebrowser/seg-00001.ts`
 *          → `../proxy.php?p=%2Fapi%2Fv1%2Fmedia%2Fhls%2F<id>%2Fseg-00001.ts`
 *
 * Anything unrecognised is passed through untouched rather than guessed at.
 */
export function mapHlsUrl(sessionId: string, requested: string): string {
  const name = hlsFileName(requested);
  if (!name) return requested;
  const p = hlsPath(sessionId, name);
  return p ? apiUrl(p) : requested;
}

/** The bridge URL of a session's playlist. */
export function playlistUrl(session: MediaSession): string {
  return mapHlsUrl(session.id, session.playlist);
}

/**
 * Rewrite a VOD playlist so every URI in it is absolute *through the bridge*.
 * Used only on the native-HLS path, where we cannot intercept requests.
 */
export function rewritePlaylist(text: string, sessionId: string): string {
  return text
    .split("\n")
    .map((line) => {
      const t = line.trim();
      if (!t) return line;
      if (t.startsWith("#")) {
        // #EXT-X-MAP:URI="init.mp4", #EXT-X-KEY:...,URI="key.bin"
        return line.replace(/URI="([^"]+)"/g, (m, uri: string) => {
          const mapped = mapHlsUrl(sessionId, uri);
          return mapped === uri ? m : `URI="${mapped}"`;
        });
      }
      return mapHlsUrl(sessionId, t);
    })
    .join("\n");
}

/* --------------------------------------------------------------- attaching */

export interface HlsAttachOptions {
  session: MediaSession;
  /**
   * Seconds to start at — the position carried over from the source that was
   * playing before an audio-track or quality switch. hls.js takes it as
   * `startPosition`, which is cheaper and more accurate than letting it load
   * segment 1 and then seeking.
   */
  startAt?: number;
  /**
   * Aborts the attach. Essential rather than decorative: this function awaits a
   * dynamic import, and React StrictMode (dev) mounts an effect, tears it down
   * and mounts it again on the *same* element — so without this check the first
   * call would come back from its import after the second had already attached,
   * and its `destroy()` would rip the MediaSource off the element the second
   * one is feeding.
   */
  signal?: AbortSignal;
  /** Fatal, unrecoverable playback failure — the player shows a card. */
  onFatal: (message: string) => void;
  /** Diagnostics only (buffer trims, retried segments); never user-visible. */
  onNotice?: (message: string) => void;
}

export interface HlsAttachment {
  /** Which transport ended up being used, for the badge tooltip. */
  transport: "native" | "hls.js";
  destroy: () => void;
}

/**
 * Buffering policy: **as far ahead as the browser will let us**.
 *
 * hls.js ships conservative defaults (30 s ahead, 60 MB, 600 s ceiling) that
 * make a paused player stop fetching almost immediately. This deployment is a
 * NAS on a LAN with RAM to spare, so all three are lifted to "the whole file".
 *
 * The real ceiling is *browser-imposed*, not ours: a MediaSource SourceBuffer
 * has a quota (Chrome is around 150 MB of video), and `appendBuffer` throws
 * QuotaExceededError once it is reached however large these numbers are.
 * hls.js handles that itself — it evicts back buffer, and if that is not
 * enough it halves its own buffer target and carries on — so it must never be
 * surfaced as an error: only `fatal` errors reach `onFatal` below.
 */
function bufferConfig(durationSec: number): Partial<HlsConfig> {
  // Whole file plus slack; a bogus/zero duration still gets a generous window.
  const ahead = Math.max(Number.isFinite(durationSec) ? durationSec + 60 : 0, 3600);
  return {
    lowLatencyMode: false,
    maxBufferLength: ahead,
    maxMaxBufferLength: ahead,
    // 8 GiB: effectively "no byte cap", while staying a finite number so
    // hls.js's own arithmetic (it compares against this) stays sane.
    maxBufferSize: 8 * 1024 * 1024 * 1024,
    // Keep everything already played so a backward seek is instant.
    backBufferLength: Infinity,
    frontBufferFlushThreshold: Infinity,
    startFragPrefetch: true,
  };
}

/**
 * Point a media element at a session's HLS stream.
 *
 * Native HLS is preferred where it exists (Safari decodes it without a MB of
 * JavaScript); everywhere else hls.js is imported *lazily*, so a user who
 * never opens a file needing a transcode never downloads it.
 */
const NOOP_ATTACHMENT = (transport: HlsAttachment["transport"]): HlsAttachment => ({
  transport,
  destroy: () => undefined,
});

export async function attachHls(
  video: HTMLMediaElement,
  { session, startAt = 0, signal, onFatal, onNotice }: HlsAttachOptions,
  nativeHls: boolean,
): Promise<HlsAttachment> {
  if (nativeHls) {
    const url = playlistUrl(session);
    const res = await fetch(url, { credentials: "same-origin", signal });
    if (signal?.aborted) return NOOP_ATTACHMENT("native");
    if (!res.ok) throw new Error(`playlist request failed (HTTP ${res.status})`);
    const rewritten = rewritePlaylist(await res.text(), session.id);
    if (signal?.aborted) return NOOP_ATTACHMENT("native");
    const blobUrl = URL.createObjectURL(new Blob([rewritten], { type: "application/vnd.apple.mpegurl" }));
    video.src = blobUrl;
    return {
      transport: "native",
      destroy: () => {
        try {
          video.removeAttribute("src");
          video.load();
        } catch {
          /* the element is already gone */
        }
        URL.revokeObjectURL(blobUrl);
      },
    };
  }

  // The "light" bundle: same API, no alt-audio/subtitle/EME controllers we
  // have no use for (see src/types/hls-light.d.ts).
  const Hls = (await import("hls.js/light")).default;
  // Nothing has touched the element yet: bail out before it does.
  if (signal?.aborted) return NOOP_ATTACHMENT("hls.js");
  if (!Hls.isSupported()) throw new Error("this browser has no MediaSource support, so HLS cannot be played");

  const hls = new Hls({
    ...bufferConfig(session.durationSec),
    // -1 is hls.js's "start where the playlist says"; a positive value is the
    // position carried over from the source this one is replacing.
    startPosition: startAt > 0 ? startAt : -1,
    /*
     * hls.js would spawn its transmuxer worker from a blob: URL, which
     * index.html's CSP (script-src 'self') forbids — and widening script-src
     * to blob: to save a bit of main-thread work is a bad trade on a page that
     * renders untrusted file names. Transmuxing TS → fMP4 is a repackage, not
     * a decode, so the main thread copes.
     */
    enableWorker: false,
    loader: mappedLoader(Hls, session.id),
  });

  let recoveredMedia = false;
  let recoveredNetwork = false;
  hls.on(Hls.Events.ERROR, (_evt, data: ErrorData) => {
    if (!data.fatal) {
      // Buffer trims (including the QuotaExceededError path) land here.
      onNotice?.(`${data.details}`);
      return;
    }
    if (data.type === Hls.ErrorTypes.NETWORK_ERROR && !recoveredNetwork) {
      recoveredNetwork = true;
      hls.startLoad();
      return;
    }
    if (data.type === Hls.ErrorTypes.MEDIA_ERROR && !recoveredMedia) {
      recoveredMedia = true;
      hls.recoverMediaError();
      return;
    }
    onFatal(data.reason || data.details || "the transcoded stream failed");
  });

  hls.attachMedia(video as HTMLVideoElement);
  hls.loadSource(playlistUrl(session));

  return {
    transport: "hls.js",
    destroy: () => {
      try {
        hls.destroy();
      } catch {
        /* already torn down */
      }
    },
  };
}

/**
 * `Hls.DefaultConfig.loader` with one line changed. Subclassing (rather than
 * `xhrSetup`) is the hls.js-sanctioned way to rewrite a URL, and it covers the
 * playlist, the segments and any future key/init request in one place.
 */
function mappedLoader(Hls: typeof HlsJs, sessionId: string) {
  const Base = Hls.DefaultConfig.loader;
  return class BridgeLoader extends Base {
    load(context: LoaderContext, config: LoaderConfiguration, callbacks: LoaderCallbacks<LoaderContext>): void {
      context.url = mapHlsUrl(sessionId, context.url);
      super.load(context, config, callbacks);
    }
  };
}
