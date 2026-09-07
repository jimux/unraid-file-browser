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

export interface SearchParams {
  q: string;
  mode?: SearchMode;
  path?: string;
  ext?: string;
  minSize?: number;
  maxSize?: number;
  after?: number;
  before?: number;
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
  | "INTERNAL"
  | "NETWORK";
