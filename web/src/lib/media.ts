/**
 * Media (video/audio) helpers.
 *
 * `fs/raw` serves everything under `audio/*` and `video/*` **inline** with its
 * real Content-Type, `Accept-Ranges: bytes` and Range support for real files
 * (archive-internal virtual paths stream without Range — playback works, but
 * seeking does not). That is why a media file the browser understands can be
 * played straight from the daemon with a plain <video>/<audio> element, with
 * no server-side work at all.
 *
 * Everything else now goes up a ladder rather than into a dead end: the player
 * asks the daemon for an HLS session (`POST /media/session`, see PlayerView),
 * which remuxes when only the container is wrong and transcodes when a codec
 * is. The "download it instead" cards in this stack are therefore reserved for
 * the cases transcoding genuinely cannot rescue — ffmpeg missing on the
 * server, or a file with no decodable stream in it.
 *
 * Detection is *two* rules, in this order:
 *
 *  1. the mime the daemon reported (`video/…` | `audio/…`), and
 *  2. the file's extension.
 *
 * Rule 2 exists because the reported mime is not always the truth: it is ""
 * for a symlink whose target was never sniffed, and it is whatever the
 * daemon's extension table says for everything else — a table that maps `.ts`
 * to text/plain (TypeScript is far more common on a NAS than MPEG-TS) and
 * knows nothing about oddities. A file called `holiday.MP4` is a video whether
 * or not the server agrees.
 *
 * The catch is that the *server* still decides what `fs/raw` streams: only
 * what it classifies as `video/*` or `audio/*` is served inline. Anything else
 * arrives as an `application/octet-stream` attachment that a <video> element
 * cannot play. That no longer blocks playback — `/media/session` reads the
 * file from disk itself and is unaffected by the raw-content policy — but it
 * does mean the direct path is off the table, so such a file skips straight to
 * HLS. See `canPlayDirectly()`.
 */

import type { Entry } from "../api/types";
import { extOf } from "./paths";
import { playHref, navigate, type PlayOptions } from "./router";

export type MediaKind = "video" | "audio";

/**
 * Extensions that mean "video", whatever the reported mime says. Lower-case,
 * compared against the text after the last dot (so `.MP4` matches).
 */
export const VIDEO_EXTS: ReadonlySet<string> = new Set([
  "mp4",
  "m4v",
  "mkv",
  "webm",
  "mov",
  "avi",
  "wmv",
  "flv",
  "mpg",
  "mpeg",
  "mpe",
  "m2v",
  "ts",
  "m2ts",
  "mts",
  "ogv",
  "ogm",
  "3gp",
  "3g2",
  "vob",
  "divx",
  "asf",
  "rm",
  "rmvb",
  "f4v",
]);

/** Extensions that mean "audio", whatever the reported mime says. */
export const AUDIO_EXTS: ReadonlySet<string> = new Set([
  "mp3",
  "flac",
  "wav",
  "ogg",
  "oga",
  "opus",
  "m4a",
  "m4b",
  "aac",
  "wma",
  "aif",
  "aiff",
  "alac",
  "ape",
  "wv",
  "mka",
  "dsf",
]);

/**
 * Extension → the mime to hand `canPlayType()` and the <video>/<audio> src.
 *
 * Only used when the reported mime is *not* already `video/*` | `audio/*`;
 * an extension absent from here falls back to the bare `video/*` / `audio/*`
 * placeholder, which every engine answers "" for — so ATTEMPT_ANYWAY (below)
 * decides, and unknown containers are attempted rather than pre-refused.
 */
const EXT_MIME: Record<string, string> = {
  mp4: "video/mp4",
  m4v: "video/mp4",
  mkv: "video/x-matroska",
  webm: "video/webm",
  mov: "video/quicktime",
  avi: "video/x-msvideo",
  ts: "video/mp2t",
  m2ts: "video/mp2t",
  mts: "video/mp2t",
  mpg: "video/mpeg",
  mpeg: "video/mpeg",
  ogv: "video/ogg",
  "3gp": "video/3gpp",
  wmv: "video/x-ms-wmv",
  asf: "video/x-ms-wmv",
  flv: "video/x-flv",
  mp3: "audio/mpeg",
  flac: "audio/flac",
  wav: "audio/wav",
  ogg: "audio/ogg",
  oga: "audio/ogg",
  opus: "audio/opus",
  m4a: "audio/mp4",
  m4b: "audio/mp4",
  aac: "audio/aac",
  wma: "audio/x-ms-wma",
  aif: "audio/aiff",
  aiff: "audio/aiff",
  mka: "audio/x-matroska",
};

/** Strip parameters ("video/mp4; codecs=…") and normalise case. */
function baseType(mime: string | undefined | null): string {
  if (!mime) return "";
  return mime.split(";")[0].trim().toLowerCase();
}

/** "video" | "audio" | null from the *extension* alone. */
export function extKind(name: string | undefined | null): MediaKind | null {
  if (!name) return null;
  const ext = extOf(name);
  if (!ext) return null;
  if (VIDEO_EXTS.has(ext)) return "video";
  if (AUDIO_EXTS.has(ext)) return "audio";
  return null;
}

/**
 * "video" | "audio" | null — drives which element the player mounts.
 *
 * The mime rule wins when it applies; the extension is the override for
 * everything it does not cover (empty mime, `text/plain` on a `.ts`, an
 * `application/octet-stream` on a file the daemon could not place).
 */
export function mediaKind(mime: string | undefined | null, name?: string | null): MediaKind | null {
  const t = baseType(mime);
  if (t.startsWith("video/")) return "video";
  if (t.startsWith("audio/")) return "audio";
  return extKind(name);
}

/** True when the entry should open in the player window instead of the viewer. */
export function isMedia(mime: string | undefined | null, name?: string | null): boolean {
  return mediaKind(mime, name) !== null;
}

/**
 * Membership test for the player's prev/next playlist: the audio and video
 * siblings of the file being played, in listing order.
 *
 * It is deliberately the same mime-or-extension rule the browser view uses to
 * route a double-click into the player, so ↑/↓ can never land on something the
 * browser view would not have opened here — including the containers
 * `canPlayType()` lies about (mkv, mov, opus), which are attempted and show the
 * fallback card if the engine really cannot decode them, and the ones only the
 * extension recognises, which show the server-classification card. Directories
 * and archives are excluded even in the unlikely event they carry a media mime.
 */
export function isPlaylistEntry(e: Entry): boolean {
  return (e.type === "file" || e.type === "symlink") && isMedia(e.mime, e.name);
}

/**
 * The mime to hand `canPlayType()` and the media element.
 *
 * An honest `video/*`|`audio/*` from the daemon is used as-is. Otherwise the
 * extension synthesises one, because "" and "application/octet-stream" tell
 * the engine nothing at all.
 */
export function mediaMimeFor(entry: { name: string; mime?: string | null }): string {
  const t = baseType(entry.mime);
  if (t.startsWith("video/") || t.startsWith("audio/")) return entry.mime ?? "";
  const ext = extOf(entry.name);
  const synth = EXT_MIME[ext];
  if (synth) return synth;
  const kind = extKind(entry.name);
  // A placeholder no engine claims to support: canPlayType answers "", so the
  // ATTEMPT_ANYWAY / "no" decision below is what actually applies.
  return kind === "audio" ? "audio/*" : kind === "video" ? "video/*" : "";
}

/**
 * True when the *daemon* will stream this file inline, which is the only way a
 * media element can read `fs/raw` at all: it serves `video/*` and `audio/*`
 * with their real Content-Type and hands everything else back as an
 * `application/octet-stream` attachment (API.md, "Raw content policy").
 *
 * This is deliberately keyed on the mime the server *reported*, not on our
 * extension override — the server's own classification is what it will apply
 * when the bytes are requested.
 */
export function serverStreamsInline(mime: string | undefined | null): boolean {
  const t = baseType(mime);
  return t.startsWith("video/") || t.startsWith("audio/");
}

/**
 * The message for the case above: the SPA decided this file is media (from its
 * extension), but the daemon did not, so the stream would arrive as an
 * attachment and the element would fail. Say exactly that.
 */
export function serverClassificationMessage(name: string, mime: string | undefined | null): string {
  const reported = (mime ?? "").trim() || "no type";
  return `The server does not classify ${name} as media (reported ${reported}), so it cannot be streamed — download it instead.`;
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
 *  - The bare `video/*` / `audio/*` placeholders: they mean "an extension we
 *    recognise but have no concrete type for". Refusing them up front would
 *    turn the extension override into a dead end, so they are attempted too.
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
  "video/*",
  "audio/*",
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
 * Rung one of the playback ladder: is it worth pointing a media element
 * straight at `fs/raw`?
 *
 * Both halves have to hold — the daemon has to be willing to stream the bytes
 * inline, *and* the engine has to have some chance of decoding them. A "no"
 * here is not a refusal to play the file; it means "go build an HLS session",
 * which is also where an element that mounts and then fires `error` ends up.
 */
export function canPlayDirectly(entry: { name: string; mime?: string | null } | null): boolean {
  if (!entry) return false;
  if (!serverStreamsInline(entry.mime)) return false;
  const kind = mediaKind(entry.mime, entry.name);
  if (!kind) return false;
  return playability(kind, mediaMimeFor(entry)) !== "no";
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
 * file's siblings in the same order the caller is showing them in; `force`
 * rides there too, for "Play as media…" on a file nothing classifies as media.
 */
export function playerUrl(path: string, opts?: PlayOptions | null): string {
  return `${window.location.pathname}${window.location.search}${playHref(path, opts)}`;
}

/**
 * Open the player in its own window. Returns false when the popup was blocked,
 * in which case the current document navigates to the player route instead —
 * the user still gets to watch the file, just not side by side.
 */
export function openPlayer(path: string, opts?: PlayOptions | null): boolean {
  let win: Window | null = null;
  try {
    win = window.open(playerUrl(path, opts), "_blank", "popup,width=1280,height=760");
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
  navigate(playHref(path, opts));
  return false;
}
