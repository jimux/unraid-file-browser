/**
 * SearchHit.snippet is server-produced HTML containing <mark> tags around the
 * matched terms. The file contents it quotes are entirely untrusted, so the
 * only markup that survives is <mark>/</mark>: every other tag is dropped and
 * everything that is not a tag is re-escaped as text.
 *
 * Entities the server introduced while escaping the file bytes (&amp;, &lt;,
 * &#39;, …) are decoded first and re-escaped after, so a code snippet reads as
 * `a && b` rather than `a &amp;&amp; b`. Decoding happens on plain strings and
 * the result is always escaped before it reaches innerHTML, so a decoded "<"
 * can never become a tag.
 *
 * Cost model: the scanner is a single left-to-right pass (indexOf only ever
 * moves forward), so a hostile snippet of N unterminated "<" is O(N), not the
 * O(N²) the old /<[^>]*>/g strip cost. Input is additionally hard-capped and
 * <mark> nesting is capped so neither the string nor the resulting DOM can be
 * blown up by a crafted file.
 */

/** Hard cap applied inside the sanitiser. Callers should clamp tighter. */
export const MAX_SNIPPET_CHARS = 65536;

/** Deepest <mark> nesting we will emit; extra opens (and their closes) vanish. */
const MAX_MARK_DEPTH = 16;

const NAMED: Record<string, string> = {
  amp: "&",
  lt: "<",
  gt: ">",
  quot: '"',
  apos: "'",
  nbsp: " ",
};

function decodeEntities(s: string): string {
  return s.replace(/&(#[0-9]+|#[xX][0-9a-fA-F]+|[a-zA-Z]+);/g, (m, g: string) => {
    if (g[0] === "#") {
      const cp = g[1] === "x" || g[1] === "X" ? parseInt(g.slice(2), 16) : parseInt(g.slice(1), 10);
      if (!Number.isFinite(cp) || cp <= 0 || cp > 0x10ffff) return m;
      try {
        return String.fromCodePoint(cp);
      } catch {
        return m;
      }
    }
    return NAMED[g.toLowerCase()] ?? m;
  });
}

function escapeHtml(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

/**
 * Anchored <mark> matcher. Group 1 = leading "/", group 2 = trailing "/".
 *   <mark>  <mark >   → open
 *   </mark> </mark >  → close
 *   <mark/> <mark />  → no-op (a self-closing <mark> opens nothing)
 *   </mark/>          → no-op
 * Sticky so it can be tested at an offset without slicing the string.
 */
const MARK_AT = /<(\/?)mark\s*(\/?)\s*>/iy;

export function sanitizeSnippet(html: string): string {
  const src = html.length > MAX_SNIPPET_CHARS ? html.slice(0, MAX_SNIPPET_CHARS) : html;

  const out: string[] = [];
  let text: string[] = [];
  // Depth of <mark> we actually emitted, plus opens suppressed by the cap.
  // Closes consume suppressed opens first, which is exactly LIFO nesting.
  let open = 0;
  let suppressed = 0;

  const flushText = () => {
    if (text.length === 0) return;
    out.push(escapeHtml(decodeEntities(text.join(""))));
    text = [];
  };

  let i = 0;
  while (i < src.length) {
    const lt = src.indexOf("<", i);
    if (lt < 0) {
      text.push(src.slice(i));
      break;
    }
    if (lt > i) text.push(src.slice(i, lt));

    MARK_AT.lastIndex = lt;
    const m = MARK_AT.exec(src);
    if (m) {
      const closing = m[1] === "/";
      const selfClosing = m[2] === "/";
      if (!closing && !selfClosing) {
        if (open < MAX_MARK_DEPTH) {
          flushText();
          out.push("<mark>");
          open += 1;
        } else {
          suppressed += 1;
        }
      } else if (closing && !selfClosing) {
        if (suppressed > 0) {
          suppressed -= 1;
        } else if (open > 0) {
          flushText();
          out.push("</mark>");
          open -= 1;
        }
      }
      // <mark/>, <mark />, </mark/> emit nothing at all.
      i = lt + m[0].length;
      continue;
    }

    // Some other tag. Drop it up to and including its ">"; if there is no ">"
    // left in the whole string this "<" (and everything after it) is text.
    const gt = src.indexOf(">", lt + 1);
    if (gt < 0) {
      text.push(src.slice(lt));
      break;
    }
    i = gt + 1;
  }

  flushText();
  return out.join("") + "</mark>".repeat(open);
}
