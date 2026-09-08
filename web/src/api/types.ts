/**
 * Mirrors the TypeScript interfaces in API.md verbatim. Any drift here is a
 * contract break — update API.md, the daemon and this file together.
 */

export type EntryType = "file" | "dir" | "symlink" | "archive";

export interface Entry {
  name: string;
  path: string; // full (possibly virtual) path
  type: EntryType; // archive = browsable-as-dir file
  size: number; // bytes; -1 if unknown (some archive entries)
  mtime: number; // unix seconds; 0 if unknown
  mime: string; // sniffed/derived, "" if unknown
  target?: string; // symlink target
}

export interface ViewResult {
  text: string; // decoded UTF-8 window
  encoding: string; // encoding actually used
  sniffed: string; // what detection suggested
  mime: string;
  size: number; // total file size in bytes
  offset: number; // byte offset of this window
  length: number; // byte length consumed from source
  truncated: boolean; // more bytes exist after this window
  lossy: boolean; // replacement chars were produced
}

export interface SearchHit {
  entry: Entry;
  score: number;
  snippet?: string; // HTML with <mark> tags, content hits only
  matchedIn: "name" | "content";
}

export interface IndexConfig {
  roots: string[]; // default ["/mnt/user"]
  schedule: string; // cron, default "0 3 * * *"
  parallelism: number; // default 2
  content: {
    enabled: boolean;
    includePaths: string[]; // prefixes that get content indexing
    extensions: string[]; // allowlist, e.g. ["txt","md","go","log",...]
    maxFileBytes: number; // default 10485760
  };
}

export type IndexState = "idle" | "crawling" | "extracting";

export interface IndexStatus {
  state: IndexState;
  filesIndexed: number;
  contentIndexed: number;
  dbBytes: number;
  lastFullScan: number | string;
  current?: string;
  progress?: number;
}

/* ------------------------------------------------------- media (HLS) types */

/**
 * `GET /media/capabilities` — is server-side transcoding possible at all?
 * Never errors; everything but `available` and `reason` is diagnostics.
 */
export interface MediaCapabilities {
  available: boolean;
  /** Versions, "" when the binary is missing. */
  ffmpeg: string;
  ffprobe: string;
  ffmpegPath?: string;
  ffprobePath?: string;
  hwaccels: string[];
  encoders: string[];
  /** Hardware encoder the daemon tries first; absent = software only. */
  hwEncoder?: string;
  /** Why `available` is false — quoted verbatim in the player's card. */
  reason?: string;
}

export interface ProbeVideoStream {
  index: number;
  codec: string;
  profile: string;
  width: number;
  height: number;
  fps: number;
  bitrate: number;
}

export interface ProbeAudioStream {
  index: number;
  codec: string;
  channels: number;
  lang: string;
  title: string;
  default: boolean;
}

export interface ProbeSubtitleStream {
  index: number;
  codec: string;
  lang: string;
  title: string;
}

/** `GET /media/probe?path=` — what ffprobe found inside the container. */
export interface MediaProbe {
  container: string;
  durationSec: number;
  bitrate: number;
  video: ProbeVideoStream | null;
  audio: ProbeAudioStream[];
  subtitles: ProbeSubtitleStream[];
}

/** remux = container rewrap only; transcode = at least one stream re-encoded. */
export type MediaSessionMode = "remux" | "transcode";

/** `POST /media/session` — a live HLS session on the daemon. */
export interface MediaSession {
  /** 32 hex chars. */
  id: string;
  mode: MediaSessionMode;
  /** Human sentence explaining the mode, e.g. "mpeg4 video is not supported". */
  reason: string;
  durationSec: number;
  segmentSec: number;
  segmentCount: number;
  /** Full API path, e.g. "/api/v1/media/hls/<id>/index.m3u8". */
  playlist: string;
  /** null for an audio-only session. `bitrate` is 0 when the stream is copied. */
  video: { codec: string; width: number; height: number; bitrate: number; copied: boolean } | null;
  audio: { codec: string; channels: number; lang: string; index: number; copied: boolean } | null;
}

export interface MediaSessionParams {
  path: string;
  /** Short codec names this browser can decode — see lib/codecs.ts. */
  can: string[];
  /** ffprobe stream index of the audio track to use; omitted = daemon default. */
  audioIndex?: number;
  /**
   * Cap the output height (1080/720/480); omitted = keep the source height.
   * Below the source height this forces a scaling transcode even when the
   * codec was already playable — the user asked for less. At or above it, it
   * never upscales.
   */
  maxHeight?: number;
}

/* ---- endpoint response payloads (the `data` side of the envelope) ---- */

export interface ListResult {
  entries: Entry[];
  total: number;
  path: string;
}

export interface StatResult {
  entry: Entry;
}

export interface HexRow {
  offset: number;
  hex: string;
  ascii: string;
}

export interface HexResult {
  rows: HexRow[];
  size: number;
  truncated: boolean;
}

export interface EncodingOption {
  id: string;
  label: string;
}

export interface EncodingsResult {
  encodings: EncodingOption[];
}

export interface SearchResult {
  hits: SearchHit[];
  total: number;
  indexFresh: boolean;
  tookMs: number;
}

/* ------------------------------------------------- searchable metadata schema */

/**
 * How a metadata field is edited and compared. The daemon owns this — the SPA
 * never hard-codes a field, only the editor for each of these six shapes.
 */
export type SearchFieldType = "text" | "number" | "bytes" | "date" | "enum" | "bool";

/** One choice of an `enum` field: the user sees `label`, the wire gets `value`. */
export interface SearchFieldValue {
  value: string;
  label: string;
}

export interface SearchField {
  /** Wire key, e.g. "image.cameraModel". Also the `key` for /search/values. */
  key: string;
  label: string;
  type: SearchFieldType;
  /** Suffix shown after a `number` input, e.g. "px", "kbps". */
  unit?: string;
  /** Present for `enum`; when absent the UI falls back to /search/values. */
  values?: SearchFieldValue[];
}

/**
 * A group of fields in the cascading picker.
 *
 * `extensions` is the default extension pre-filter for a *type-specific*
 * category (image/video/audio/package); it is `null` for the always-present
 * `common` category, which applies to every file.
 */
export interface SearchCategory {
  id: string;
  label: string;
  extensions: string[] | null;
  fields: SearchField[];
}

export interface SearchFieldsResult {
  categories: SearchCategory[];
}

/** One distinct value actually present in the index, with its hit count. */
export interface SearchValueCount {
  value: string;
  count: number;
}

export interface SearchValuesResult {
  values: SearchValueCount[];
}

export interface SearchValuesParams {
  key: string;
  prefix?: string;
  path?: string;
  limit?: number;
}

/**
 * GET and PUT /index/config both return this shape.
 *
 * `allowedRoots` are the browse roots the daemon was booted with (set in the
 * plugin's Unraid settings page, not editable from here). Every index root and
 * content include path must lie within one of them; the daemon enforces it and
 * answers BAD_REQUEST otherwise, the SPA pre-validates so the user finds out
 * before saving.
 */
export interface IndexConfigResult {
  config: IndexConfig;
  allowedRoots: string[];
}

export interface HealthResult {
  version: string;
  uptimeSec: number;
  indexDb: "ok" | "missing" | "error";
  /** Boot-time browse roots — same list as IndexConfigResult.allowedRoots. */
  roots: string[];
}

/* ---- request params ---- */

export type SortKey = "name" | "size" | "mtime" | "type";
export type SortDir = "asc" | "desc";
export type SearchMode = "name" | "content" | "both";

/** `relevance` is only meaningful when the request carries a `q`. */
export type SearchSort = "relevance" | "name" | "size" | "mtime";

export interface ListParams {
  path: string;
  offset?: number;
  limit?: number;
  sort?: SortKey;
  dir?: SortDir;
  dirsFirst?: boolean;
}

export interface ViewParams {
  path: string;
  encoding?: string;
  offset?: number;
  length?: number;
}

export interface HexParams {
  path: string;
  offset?: number;
  length?: number;
}

/**
 * `q` is optional: a request with no `q` but at least one filter
 * (path/ext/size/date/meta) is a valid metadata-only search. Both empty is a
 * BAD_REQUEST, which is why the SPA's Search button refuses to fire on it.
 */
export interface SearchParams {
  q?: string;
  mode?: SearchMode;
  path?: string;
  ext?: string;
  minSize?: number;
  maxSize?: number;
  after?: number;
  before?: number;
  /** Repeated `meta=<key><op><value>`; ops = != ~ < <= > >=. ANDed together. */
  meta?: string[];
  sort?: SearchSort;
  dir?: SortDir;
  limit?: number;
  offset?: number;
}

/** Error codes from API.md, plus a client-only NETWORK code for fetch failures. */
export type ApiErrorCode =
  | "BAD_REQUEST"
  | "NOT_FOUND"
  | "FORBIDDEN"
  | "TOO_LARGE"
  | "TIMEOUT"
  | "ARCHIVE_ERROR"
  | "ENCODING_ERROR"
  | "INDEXING"
  /** 503 from the media endpoints: ffmpeg/ffprobe are not installed. */
  | "UNAVAILABLE"
  | "INTERNAL"
  | "NETWORK";
