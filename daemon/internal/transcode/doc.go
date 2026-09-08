// Package transcode serves media files the browser cannot decode natively as
// an HLS stream produced on the fly by ffmpeg.
//
// Design in one paragraph: a session is a small in-memory record (resolved
// path, probe result, chosen stream treatment, segment length). The playlist
// is a VOD playlist generated up front from the probed duration, so the
// player can seek anywhere immediately. Each segment is produced by its own
// short-lived ffmpeg process writing MPEG-TS to stdout, streamed straight to
// the HTTP client — nothing is ever written to the array, and there is no
// state to clean up beyond killing a process. The cheapest treatment wins:
// streams whose codec the browser already decodes are copied (`-c copy`) and
// only the offending stream is re-encoded (an MKV with H.264 video and AC-3
// audio costs one AAC encode, not a video transcode).
//
// Security: the input is opened by the caller-supplied Opener (fsops' race-
// safe walk) and inherited by ffmpeg as fd 3, addressed as /dev/fd/3 — a
// user-derived path is never an ffmpeg argument. argv is fixed, there is no
// shell, stdin is closed, and the protocol whitelist stops a crafted file
// from making ffmpeg open anything but that descriptor and its stdout.
package transcode
