/**
 * hls.js ships a "light" bundle (no alternate-audio controller, no subtitle /
 * WebVTT controller, no EME) that is ~37% smaller than the full one, but no
 * .d.ts of its own — the API surface is identical, so the full package's types
 * describe it exactly.
 *
 * The light build is the right one here because the daemon hands us a
 * single-variant VOD playlist with one audio track already muxed in: switching
 * track or quality creates a *new session*, it is never an in-stream switch.
 */
declare module "hls.js/light" {
  export * from "hls.js";
  export { default } from "hls.js";
}
