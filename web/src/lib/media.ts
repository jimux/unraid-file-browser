/**
 * Media (video/audio) helpers.
 *
 * `fs/raw` serves everything under `audio/*` and `video/*` **inline** with its
 * real Content-Type, `Accept-Ranges: bytes` and Range support for real files
 * (archive-internal virtual paths stream without Range — playback works, but
 * seeking does not). That is the whole reason a media file can be played
 * straight from the daemon with a plain <video>/<audio> element: there is no
 * transcoder anywhere in this stack, so whatever the browser cannot decode
 * natively cannot be played at all — only downloaded.
 */

import type { Entry } from "../api/types";
import { playHref, navigate, type PlaySort } from "./router";

export type MediaKind = "video" | "audio";

/** Strip parameters ("video/mp4; codecs=…") and normalise case. */
function baseType(mime: string | undefined | null): string {
  if (!mime) return "";
  return mime.split(";")[0].trim().toLowerCase();
}

/** "video" | "audio" | null — drives which element the player mounts. */
export function mediaKind(mime: string | undefined | null): MediaKind | null {
  const t = baseType(mime);
  if (t.startsWith("video/")) return "video";
  if (t.startsWith("audio/")) return "audio";
  return null;
}

/** True when the entry should open in the player window instead of the viewer. */
export function isMedia(mime: string | undefined | null): boolean {
  return mediaKind(mime) !== null;
}

/**
 * Membership test for the player's prev/next playlist: the audio and video
 * siblings of the file being played, in listing order.
 *
 * It is deliberately the same `video/*` | `audio/*` rule the browser view uses
 * to route a double-click into the player, so ↑/↓ can never land on something
 * the browser view would not have opened here — including the containers
 * `canPlayType()` lies about (mkv, mov, opus), which are attempted and show the
 * fallback card if the engine really cannot decode them. Directories and
 * archives are excluded even in the unlikely event they carry a media mime.
 */
export function isPlaylistEntry(e: Entry): boolean {
  return (e.type === "file" || e.type === "symlink") && isMedia(e.mime);
}

/**
 * Types where `canPlayType()` says "" but the browser very often plays the
 * file anyway, because the answer is keyed on the *container* while support is
 * really keyed on the codecs inside it:
 *
 *  - Matroska: Chromium demuxes .mkv and plays h264/vp9 + aac/opus tracks, yet
 *    every engine returns "" for video/x-matroska.
 *  - QuickTime: .mov holding h264/aac is just an ISO-BMFF file Chromium plays,
 *    but video/quicktime is likewise reported unsupported.
 *  - Opus/Matroska audio: `audio/opus` names the *codec*, and a .opus file is
 *    an Ogg stream every engine that decodes Opus can play — yet Chrome answers
 *    "" for audio/opus while answering "maybe" for audio/ogg. Same story for
 *    .mka (audio/x-matroska).
 *
 * For these we *attempt* playback and only fall back when the element actually
 * fires `error`. Everything else that answers "" is refused up front, so the
 * user gets a straight answer instead of a black rectangle.
 */
const ATTEMPT_ANYWAY = new Set([
  "video/x-matroska",
  "video/matroska",
  "video/quicktime",
  "audio/opus",
  "audio/x-matroska",
  "audio/matroska",
]);

export type Playability =
  | "probably" // engine is confident
  | "maybe" // engine might manage it (the usual answer for mp4/webm)
  | "attempt" // engine says no, but it lies about this container — try it
  | "no"; // genuinely unsupported

/**
 * Ask the engine whether it can decode `mime`, with the container caveat above
 * applied. Never throws: a headless/odd environment without canPlayType is
 * treated as "attempt" rather than a hard refusal.
 */
export function playability(kind: MediaKind, mime: string | undefined | null): Playability {
  const t = baseType(mime);
  if (!t) return "no";
  let answer = "";
  try {
    const el = document.createElement(kind);
    answer = typeof el.canPlayType === "function" ? el.canPlayType(t) : "";
  } catch {
    answer = "";
  }
  if (answer === "probably") return "probably";
  if (answer === "maybe") return "maybe";
  return ATTEMPT_ANYWAY.has(t) ? "attempt" : "no";
}

/**
 * Absolute-ish URL that reloads the SPA standalone on the player route.
 *
 * The search string is carried through deliberately: it holds `theme` (so the
 * new window paints the right palette before React mounts) and `csrf` (GETs do
 * not need it, but keeping every SPA document's location shape identical means
 * one less special case if the player ever has to POST).
 *
 * `sort` (optional) rides *inside the hash* so the player's ↑/↓ can walk the
 * file's siblings in the same order the caller is showing them in.
 */
export function playerUrl(path: string, sort?: Partial<PlaySort> | null): string {
  return `${window.location.pathname}${window.location.search}${playHref(path, sort)}`;
}

/**
 * Open the player in its own window. Returns false when the popup was blocked,
 * in which case the current document navigates to the player route instead —
 * the user still gets to watch the file, just not side by side.
 */
export function openPlayer(path: string, sort?: Partial<PlaySort> | null): boolean {
  let win: Window | null = null;
  try {
    win = window.open(playerUrl(path, sort), "_blank", "popup,width=1280,height=760");
  } catch {
    win = null;
  }
  if (win) {
    try {
      win.focus();
    } catch {
      /* focus is best-effort; a blocked focus does not mean a blocked window */
    }
    return true;
  }
  navigate(playHref(path, sort));
  return false;
}
