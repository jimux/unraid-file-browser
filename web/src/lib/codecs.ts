/**
 * What *this* browser can decode, expressed in the daemon's vocabulary.
 *
 * `POST /media/session` takes a `can` array of short codec names; the daemon
 * compares it against what ffprobe found in the file and picks the cheapest
 * thing that will play here — a pure remux when only the container is wrong
 * (an AVI holding h264/aac), a real transcode when a codec is (mpeg4, HEVC on
 * a browser without hardware support, an AC-3 track on Chrome).
 *
 * Two oracles:
 *
 *  1. `MediaSource.isTypeSupported()` wherever MSE exists — the *right*
 *     question, because that is what will decode the stream (hls.js appends to
 *     a SourceBuffer) and MSE support is narrower than element support (Chrome
 *     plays an AC-3 mp4 through the element on some platforms while refusing
 *     it in MSE).
 *  2. `<video>.canPlayType()` when there is no MediaSource at all — iOS
 *     Safari, where HLS is decoded by the element itself.
 *
 * Being conservative is the safe direction: a codec we fail to claim costs a
 * transcode, a codec we claim wrongly costs a black rectangle.
 */

export type CodecName =
  | "h264"
  | "hevc"
  | "vp8"
  | "vp9"
  | "av1"
  | "aac"
  | "ac3"
  | "eac3"
  | "mp3"
  | "opus"
  | "flac"
  | "vorbis";

interface CodecProbe {
  name: CodecName;
  /** Which element type `canPlayType` should be asked on. */
  el: "video" | "audio";
  /**
   * Candidate full MIME strings, most representative first. A codec counts as
   * supported when *any* candidate is accepted: the same decoder is reachable
   * through more than one container, and engines disagree about which spelling
   * they admit to (`hvc1` vs `hev1`, `audio/mpeg` vs `mp4a.40.34`).
   */
  mimes: string[];
}

/**
 * The fixed matrix. Profile/level strings are deliberately mainstream:
 * avc1.640029 is High@4.1 (1080p), hvc1.1.6.L93.B0 is Main@L3.1,
 * vp09.00.10.08 is Profile 0 8-bit, av01.0.04M.08 is Main Profile level 3.0
 * 8-bit. Asking about an exotic profile would under-report a decoder that
 * handles everything a NAS realistically holds.
 */
const MATRIX: readonly CodecProbe[] = [
  { name: "h264", el: "video", mimes: ['video/mp4; codecs="avc1.640029"', 'video/mp4; codecs="avc1.42E01E"'] },
  {
    name: "hevc",
    el: "video",
    mimes: ['video/mp4; codecs="hvc1.1.6.L93.B0"', 'video/mp4; codecs="hev1.1.6.L93.B0"'],
  },
  { name: "vp8", el: "video", mimes: ['video/webm; codecs="vp8"'] },
  {
    name: "vp9",
    el: "video",
    mimes: ['video/mp4; codecs="vp09.00.10.08"', 'video/webm; codecs="vp09.00.10.08"', 'video/webm; codecs="vp9"'],
  },
  {
    name: "av1",
    el: "video",
    mimes: ['video/mp4; codecs="av01.0.04M.08"', 'video/webm; codecs="av01.0.04M.08"'],
  },
  { name: "aac", el: "audio", mimes: ['audio/mp4; codecs="mp4a.40.2"'] },
  { name: "ac3", el: "audio", mimes: ['audio/mp4; codecs="ac-3"'] },
  { name: "eac3", el: "audio", mimes: ['audio/mp4; codecs="ec-3"'] },
  { name: "mp3", el: "audio", mimes: ["audio/mpeg", 'audio/mp4; codecs="mp4a.40.34"'] },
  { name: "opus", el: "audio", mimes: ['audio/webm; codecs="opus"', 'audio/mp4; codecs="opus"'] },
  { name: "flac", el: "audio", mimes: ['audio/mp4; codecs="flac"', "audio/flac"] },
  { name: "vorbis", el: "audio", mimes: ['audio/webm; codecs="vorbis"'] },
];

/**
 * The last-resort answer. Every shipping browser decodes h264 + AAC (that is
 * the whole reason the daemon's transcode target is h264/AAC), so claiming
 * nothing at all would be a worse lie than claiming these two.
 */
const BASELINE: CodecName[] = ["h264", "aac"];

export interface BrowserMediaSupport {
  /** Short codec names for `POST /media/session`'s `can` field. */
  can: CodecName[];
  /**
   * The engine admits to understanding an HLS playlist. Note this is *not* a
   * reason to use it: desktop Chrome answers "maybe" here and then cannot play
   * one. See `useNativeHls()`.
   */
  nativeHls: boolean;
  /** MediaSource Extensions present, i.e. hls.js can run here. */
  mse: boolean;
  /** Which oracle produced `can` — surfaced in the badge tooltip for support. */
  oracle: "mse" | "element" | "baseline";
}

function hasMse(): boolean {
  try {
    return typeof MediaSource !== "undefined" && typeof MediaSource.isTypeSupported === "function";
  } catch {
    return false;
  }
}

function mseSupports(mime: string): boolean {
  try {
    return MediaSource.isTypeSupported(mime);
  } catch {
    return false;
  }
}

/** `canPlayType` on a detached element; "" means no, anything else is a yes. */
function elementSupports(el: "video" | "audio", mime: string): boolean {
  try {
    const node = document.createElement(el);
    return typeof node.canPlayType === "function" && node.canPlayType(mime) !== "";
  } catch {
    return false;
  }
}

/**
 * True when the engine plays HLS playlists straight from `<video src>`. Both
 * spellings are asked because older WebKit answers only the vendor one.
 */
export function detectNativeHls(): boolean {
  return (
    elementSupports("video", "application/vnd.apple.mpegurl") ||
    elementSupports("video", "application/x-mpegurl") ||
    elementSupports("video", "audio/mpegurl")
  );
}

function probe(): BrowserMediaSupport {
  const nativeHls = detectNativeHls();
  const mse = hasMse();
  /*
   * MSE is the oracle wherever it exists, because that is what will decode the
   * stream (hls.js appends to a SourceBuffer). Only a browser without it —
   * iOS Safari — plays HLS through the element, and there the element's own
   * answers are the right ones.
   */
  const useMse = mse;

  const can: CodecName[] = [];
  for (const c of MATRIX) {
    const ok = useMse
      ? c.mimes.some(mseSupports)
      : c.mimes.some((m) => elementSupports(c.el, m));
    if (ok) can.push(c.name);
  }

  if (can.length === 0) return { can: [...BASELINE], nativeHls, mse, oracle: "baseline" };
  return { can, nativeHls, mse, oracle: useMse ? "mse" : "element" };
}

let cached: BrowserMediaSupport | null = null;

/**
 * Memoised — the answer cannot change inside a document, and each probe builds
 * a dozen throwaway elements.
 */
export function browserMediaSupport(): BrowserMediaSupport {
  if (!cached) {
    try {
      cached = probe();
    } catch {
      cached = { can: [...BASELINE], nativeHls: false, mse: false, oracle: "baseline" };
    }
  }
  return cached;
}

/**
 * True when HLS has to be handed straight to the element.
 *
 * NOT simply `nativeHls`: desktop Chrome answers "maybe" to
 * `canPlayType('application/vnd.apple.mpegurl')` and then cannot play a
 * playlist at all. The only browser that must use the native path is one with
 * no MediaSource for hls.js to drive — iOS Safari.
 */
export function useNativeHls(s: BrowserMediaSupport = browserMediaSupport()): boolean {
  return !s.mse && s.nativeHls;
}
