/**
 * The search form ⇄ query-string mapping, kept out of the view so the exact
 * bytes we put on the wire are one readable function.
 *
 * Two rules shape everything here:
 *
 *  1. A search runs only when the user asks for one, so the query string a
 *     given form produces has to be **byte-stable** — the view compares the
 *     string the form currently describes against the string it last ran to
 *     decide whether the results on screen are stale.
 *
 *  2. `buildSearchParams(parseSearchForm(s)) === s` for every `s` this module
 *     produces. That round trip is what makes a search bookmarkable: the hash
 *     is rewritten from the built query on every run, and a reload rebuilds the
 *     same form (and therefore the same query) from it, with no false "you have
 *     unrun changes" indicator on arrival.
 *
 * Where a condition row maps onto a *pre-existing* API parameter it uses that
 * parameter rather than `meta=`: the common category's `size` becomes
 * `minSize`/`maxSize` and its `mtime` becomes `after`/`before`, because those
 * have been in the contract since v1. Everything else — including the common
 * category's `name`, `ext` and `mime` — travels as `meta=<key><op><value>`,
 * which is the parameter the daemon added for exactly this. `ext=` is reserved
 * for the extension pre-filter and the manual list, so it never has to mean
 * both "any of these" (its own semantics) and "and also this" at once.
 */

import type {
  SearchCategory,
  SearchField,
  SearchFieldType,
  SearchMode,
  SearchParams,
  SearchSort,
  SortDir,
} from "../api/types";

/** One `[ Category ▾ ][ Field ▾ ][ op ][ value ]` row. */
export interface Cond {
  /** Stable identity for React keys; never leaves the client. */
  id: number;
  cat: string;
  /** Field key, "" until the user picks one. */
  key: string;
  op: string;
  value: string;
  /** `bytes` fields only. */
  unit: string;
}

export interface SearchForm {
  q: string;
  mode: SearchMode;
  scope: string;
  everywhere: boolean;
  /** Manually typed extension list, independent of the pre-filter checkbox. */
  ext: string;
  /** The "only files with <type> extensions" pre-filter. Default on. */
  extFilter: boolean;
  conds: Cond[];
  sort: SearchSort;
  dir: SortDir;
}

/* ----------------------------------------------------------------- operators */

export interface OpChoice {
  op: string;
  label: string;
}

/**
 * The operator menu for each field type. `date` deliberately offers only
 * before/after: an equality test against a timestamp is never what anyone
 * means by a date filter.
 */
export const OPS: Record<SearchFieldType, OpChoice[]> = {
  text: [
    { op: "~", label: "contains" },
    { op: "=", label: "is" },
    { op: "!=", label: "is not" },
  ],
  number: [
    { op: "=", label: "=" },
    { op: "!=", label: "≠" },
    { op: "<", label: "<" },
    { op: "<=", label: "≤" },
    { op: ">", label: ">" },
    { op: ">=", label: "≥" },
  ],
  bytes: [
    { op: ">", label: ">" },
    { op: ">=", label: "≥" },
    { op: "<", label: "<" },
    { op: "<=", label: "≤" },
    { op: "=", label: "=" },
    { op: "!=", label: "≠" },
  ],
  date: [
    { op: ">", label: "after" },
    { op: "<", label: "before" },
  ],
  enum: [
    { op: "=", label: "is" },
    { op: "!=", label: "is not" },
  ],
  bool: [{ op: "=", label: "is" }],
};

export function defaultOp(type: SearchFieldType): string {
  return OPS[type][0].op;
}

/** Longest first, so "!=" is never read as "=" preceded by a "!" in the key. */
const OP_TOKENS = ["!=", ">=", "<=", "=", "~", "<", ">"];

/** Split a wire triple `key<op>value`. Returns null when there is no operator. */
export function splitMeta(s: string): { key: string; op: string; value: string } | null {
  let at = -1;
  let tok = "";
  for (const op of OP_TOKENS) {
    const i = s.indexOf(op);
    if (i <= 0) continue; // a leading operator would mean an empty key
    if (at < 0 || i < at || (i === at && op.length > tok.length)) {
      at = i;
      tok = op;
    }
  }
  if (at < 0) return null;
  return { key: s.slice(0, at), op: tok, value: s.slice(at + tok.length) };
}

/* --------------------------------------------------------------- byte units */

export interface ByteUnit {
  id: string;
  label: string;
  mult: number;
}

/**
 * Both families, because "20 GB" means 10⁹ to the people who write disk specs
 * and 2³⁰ to the people who read `ls -l`, and the wire carries plain bytes
 * either way — so there is no reason to make the user do the conversion.
 */
export const BYTE_UNITS: ByteUnit[] = [
  { id: "B", label: "B", mult: 1 },
  { id: "KB", label: "KB", mult: 1e3 },
  { id: "MB", label: "MB", mult: 1e6 },
  { id: "GB", label: "GB", mult: 1e9 },
  { id: "TB", label: "TB", mult: 1e12 },
  { id: "KiB", label: "KiB", mult: 1024 },
  { id: "MiB", label: "MiB", mult: 1024 ** 2 },
  { id: "GiB", label: "GiB", mult: 1024 ** 3 },
  { id: "TiB", label: "TiB", mult: 1024 ** 4 },
];

export const DEFAULT_BYTE_UNIT = "GB";

function unitMult(id: string): number {
  return BYTE_UNITS.find((u) => u.id === id)?.mult ?? 1;
}

export function bytesOf(value: string, unit: string): number | undefined {
  if (!value.trim()) return undefined;
  const n = Number(value);
  if (!Number.isFinite(n) || n < 0) return undefined;
  return Math.round(n * unitMult(unit));
}

/** Largest unit that divides `n` exactly — the inverse of `bytesOf`. */
export function splitBytes(n: number): { value: string; unit: string } {
  const byMult = [...BYTE_UNITS].sort((a, b) => b.mult - a.mult);
  for (const u of byMult) {
    if (n >= u.mult && n % u.mult === 0) return { value: String(n / u.mult), unit: u.id };
  }
  return { value: String(n), unit: "B" };
}

/* -------------------------------------------------------------------- dates */

export function unixToDateInput(v: string | number | null | undefined): string {
  if (v === null || v === undefined || v === "") return "";
  const n = Number(v);
  if (!Number.isFinite(n) || n <= 0) return "";
  const d = new Date(n * 1000);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

export function dateInputToUnix(v: string, endOfDay: boolean): number | undefined {
  if (!v) return undefined;
  const t = Date.parse(endOfDay ? `${v}T23:59:59` : `${v}T00:00:00`);
  return Number.isNaN(t) ? undefined : Math.floor(t / 1000);
}

/* --------------------------------------------------------------- extensions */

export function normaliseExtList(raw: string): string[] {
  return raw
    .split(/[,\s]+/)
    .map((s) => s.trim().replace(/^\./, "").toLowerCase())
    .filter(Boolean);
}

function unique(xs: string[]): string[] {
  return [...new Set(xs)];
}

/* ------------------------------------------------------------ schema lookup */

/**
 * What the picker offers when `/search/fields` is unavailable — an older daemon
 * answers NOT_FOUND for it. Deliberately just the two fields that map onto
 * `minSize`/`maxSize` and `after`/`before`: a daemon with no `/search/fields`
 * has no `meta=` either, so offering a field that could only be expressed as
 * `meta=` would be offering a filter that cannot work.
 */
export const FALLBACK_CATEGORIES: SearchCategory[] = [
  {
    id: "common",
    label: "Common",
    extensions: [],
    fields: [
      { key: "size", label: "Size", type: "bytes" },
      { key: "mtime", label: "Modified", type: "date" },
    ],
  },
];

/** API.md caps a request at 16 `meta` parameters; the UI stops one row short. */
export const MAX_CONDS = 16;

export function findCategory(cats: SearchCategory[], id: string): SearchCategory | undefined {
  return cats.find((c) => c.id === id);
}

export function findField(cats: SearchCategory[], catId: string, key: string): SearchField | undefined {
  if (!key) return undefined;
  return findCategory(cats, catId)?.fields.find((f) => f.key === key);
}

/** The category owning `key`, wherever it lives — used when parsing a URL. */
function categoryOfKey(cats: SearchCategory[], key: string): SearchCategory | undefined {
  return cats.find((c) => c.fields.some((f) => f.key === key));
}

/** A category with its own extension list is "type-specific" (image, audio, …). */
export function isTypeCategory(c: SearchCategory | undefined): boolean {
  return !!c && Array.isArray(c.extensions) && c.extensions.length > 0;
}

/** Type-specific categories the current rows refer to, in row order. */
export function typeCategoriesInUse(form: SearchForm, cats: SearchCategory[]): SearchCategory[] {
  const seen = new Set<string>();
  const out: SearchCategory[] = [];
  for (const c of form.conds) {
    if (seen.has(c.cat)) continue;
    seen.add(c.cat);
    const cat = findCategory(cats, c.cat);
    if (isTypeCategory(cat)) out.push(cat!);
  }
  return out;
}

/** Extensions the pre-filter contributes: the union of those categories' lists. */
export function preFilterExtensions(form: SearchForm, cats: SearchCategory[]): string[] {
  if (!form.extFilter) return [];
  return unique(
    typeCategoriesInUse(form, cats).flatMap((c) => (c.extensions ?? []).map((e) => e.replace(/^\./, "").toLowerCase())),
  );
}

/**
 * `size` and `mtime` on the common category predate `meta=` and keep their own
 * parameters. The leaf of the key is matched so both `size` and `common.size`
 * work, whichever the daemon ends up naming them.
 */
type Legacy = "size" | "mtime" | null;

function legacyKind(cats: SearchCategory[], c: Cond): Legacy {
  const cat = findCategory(cats, c.cat);
  if (!cat || isTypeCategory(cat)) return null;
  const leaf = (c.key.split(".").pop() ?? "").toLowerCase();
  if (leaf === "size" || leaf === "bytes") return "size";
  if (leaf === "mtime" || leaf === "modified") return "mtime";
  return null;
}

function commonField(cats: SearchCategory[], leaves: string[]): { cat: SearchCategory; field: SearchField } | null {
  for (const cat of cats) {
    if (isTypeCategory(cat)) continue;
    for (const f of cat.fields) {
      if (leaves.includes((f.key.split(".").pop() ?? "").toLowerCase())) return { cat, field: f };
    }
  }
  return null;
}

/* ------------------------------------------------------------------- values */

/** The value a row puts on the wire, or null when the row is not filled in. */
export function wireValue(field: SearchField, c: Cond): string | null {
  switch (field.type) {
    case "bytes": {
      const b = bytesOf(c.value, c.unit);
      return b === undefined ? null : String(b);
    }
    case "number": {
      if (!c.value.trim()) return null;
      const n = Number(c.value);
      return Number.isFinite(n) ? String(n) : null;
    }
    case "date": {
      const u = dateInputToUnix(c.value, c.op === "<");
      return u === undefined ? null : String(u);
    }
    case "bool":
      return c.value === "true" || c.value === "false" ? c.value : null;
    default: {
      const v = c.value.trim();
      return v ? v : null;
    }
  }
}

export function isCondComplete(cats: SearchCategory[], c: Cond): boolean {
  const f = findField(cats, c.cat, c.key);
  return !!f && wireValue(f, c) !== null;
}

/* ---------------------------------------------------------------- the sort */

/**
 * Relevance is meaningless with no `q`, so a form that has selected it falls
 * back to name ordering the moment the query box empties. The select shows the
 * fallback (and greys relevance out), so this is visible rather than silent.
 */
export function effectiveSort(form: SearchForm): SearchSort {
  return form.sort === "relevance" && !form.q.trim() ? "name" : form.sort;
}

/* ------------------------------------------------------------------- build */

export function buildSearchParams(form: SearchForm, cats: SearchCategory[]): SearchParams {
  const p: SearchParams = {};
  const q = form.q.trim();
  if (q) {
    p.q = q;
    p.mode = form.mode;
  }
  if (!form.everywhere && form.scope.trim()) p.path = form.scope.trim();

  const meta: string[] = [];
  let minSize: number | undefined;
  let maxSize: number | undefined;
  let after: number | undefined;
  let before: number | undefined;

  for (const c of form.conds) {
    const f = findField(cats, c.cat, c.key);
    if (!f) continue;
    const v = wireValue(f, c);
    if (v === null) continue;
    const kind = legacyKind(cats, c);
    const n = Number(v);

    if (kind === "size" && Number.isFinite(n)) {
      if (c.op === ">") minSize = Math.max(minSize ?? -Infinity, n + 1);
      else if (c.op === ">=") minSize = Math.max(minSize ?? -Infinity, n);
      else if (c.op === "<") maxSize = Math.min(maxSize ?? Infinity, Math.max(0, n - 1));
      else if (c.op === "<=") maxSize = Math.min(maxSize ?? Infinity, n);
      else if (c.op === "=") {
        minSize = Math.max(minSize ?? -Infinity, n);
        maxSize = Math.min(maxSize ?? Infinity, n);
      } else meta.push(`${f.key}${c.op}${v}`); // "!=" has no legacy equivalent
      continue;
    }
    if (kind === "mtime" && Number.isFinite(n)) {
      if (c.op === ">") after = Math.max(after ?? -Infinity, n);
      else if (c.op === "<") before = Math.min(before ?? Infinity, n);
      else meta.push(`${f.key}${c.op}${v}`);
      continue;
    }
    meta.push(`${f.key}${c.op}${v}`);
  }

  const ext = unique([...normaliseExtList(form.ext), ...preFilterExtensions(form, cats)]);
  if (ext.length) p.ext = ext.join(",");
  if (minSize !== undefined && Number.isFinite(minSize)) p.minSize = minSize;
  if (maxSize !== undefined && Number.isFinite(maxSize)) p.maxSize = maxSize;
  if (after !== undefined && Number.isFinite(after)) p.after = after;
  if (before !== undefined && Number.isFinite(before)) p.before = before;
  if (meta.length) p.meta = meta;

  p.sort = effectiveSort(form);
  p.dir = form.dir;
  return p;
}

/**
 * The server's rule is "q, or at least one filter". We are one notch stricter:
 * a bare path scope does not count, because "search everything under /mnt/user"
 * is not a search anybody meant to run — and the button says what is missing
 * rather than letting the daemon answer BAD_REQUEST.
 */
export function canSearch(form: SearchForm, cats: SearchCategory[]): boolean {
  if (form.q.trim()) return true;
  if (form.conds.some((c) => isCondComplete(cats, c))) return true;
  return normaliseExtList(form.ext).length > 0 || preFilterExtensions(form, cats).length > 0;
}

/* ------------------------------------------------------------------- parse */

const SORTS: readonly string[] = ["relevance", "name", "size", "mtime"];
const MODES: readonly string[] = ["name", "content", "both"];

let condSeq = 0;
export function nextCondId(): number {
  condSeq += 1;
  return condSeq;
}

export function newCond(catId: string): Cond {
  return { id: nextCondId(), cat: catId, key: "", op: "", value: "", unit: DEFAULT_BYTE_UNIT };
}

/** Turn a wire value back into the editor's representation for `field`. */
function editorValue(field: SearchField, op: string, raw: string): { value: string; unit: string } {
  if (field.type === "bytes") {
    const n = Number(raw);
    if (Number.isFinite(n)) return splitBytes(n);
    return { value: raw, unit: DEFAULT_BYTE_UNIT };
  }
  if (field.type === "date") return { value: unixToDateInput(raw), unit: DEFAULT_BYTE_UNIT };
  void op;
  return { value: raw, unit: DEFAULT_BYTE_UNIT };
}

export function parseSearchForm(params: URLSearchParams, cats: SearchCategory[], currentDir: string): SearchForm {
  const scopeParam = params.get("path");
  const mode = params.get("mode");
  const sort = params.get("sort");

  const conds: Cond[] = [];
  for (const raw of params.getAll("meta")) {
    const t = splitMeta(raw);
    if (!t) continue;
    const cat = categoryOfKey(cats, t.key);
    const field = cat?.fields.find((f) => f.key === t.key);
    if (!cat || !field) continue; // schema no longer has it — nothing to edit
    const { value, unit } = editorValue(field, t.op, t.value);
    conds.push({ id: nextCondId(), cat: cat.id, key: field.key, op: t.op, value, unit });
  }

  const size = commonField(cats, ["size", "bytes"]);
  const pushSize = (n: number, op: string) => {
    if (!size || !Number.isFinite(n)) return;
    const { value, unit } = splitBytes(n);
    conds.push({ id: nextCondId(), cat: size.cat.id, key: size.field.key, op, value, unit });
  };
  const minSize = params.get("minSize");
  const maxSize = params.get("maxSize");
  if (minSize) pushSize(Number(minSize), ">=");
  if (maxSize) pushSize(Number(maxSize), "<=");

  const time = commonField(cats, ["mtime", "modified"]);
  const pushTime = (v: string | null, op: string) => {
    if (!v || !time) return;
    const d = unixToDateInput(v);
    if (!d) return;
    conds.push({ id: nextCondId(), cat: time.cat.id, key: time.field.key, op, value: d, unit: DEFAULT_BYTE_UNIT });
  };
  pushTime(params.get("after"), ">");
  pushTime(params.get("before"), "<");

  // The pre-filter checkbox is not on the wire — it is inferred. Type-specific
  // rows with no `ext=` at all can only have come from unticking it; anything
  // left over after removing the union it would have produced is a manual list.
  const urlExts = normaliseExtList(params.get("ext") ?? "");
  const probe: SearchForm = {
    q: "",
    mode: "both",
    scope: "",
    everywhere: false,
    ext: "",
    extFilter: true,
    conds,
    sort: "relevance",
    dir: "asc",
  };
  const anyTypeCat = typeCategoriesInUse(probe, cats).length > 0;
  const extFilter = anyTypeCat ? urlExts.length > 0 : true;
  const preExts = extFilter ? preFilterExtensions({ ...probe, extFilter }, cats) : [];

  return {
    q: params.get("q") ?? "",
    mode: (mode && MODES.includes(mode) ? mode : "both") as SearchMode,
    scope: scopeParam ?? currentDir,
    everywhere: params.has("path") ? !scopeParam : !currentDir,
    ext: urlExts.filter((e) => !preExts.includes(e)).join(","),
    extFilter,
    conds,
    sort: (sort && SORTS.includes(sort) ? sort : "relevance") as SearchSort,
    dir: params.get("dir") === "desc" ? "desc" : "asc",
  };
}
