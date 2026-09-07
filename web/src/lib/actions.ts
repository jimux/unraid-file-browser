/**
 * Small imperative helpers the context menus need: copy to clipboard, a
 * transient confirmation toast, and a download that does not depend on a link
 * being present in the DOM.
 *
 * These are deliberately *not* React state: the same three actions are wanted
 * from the browser grid, the search results and the standalone player window,
 * and threading a toast reducer through three unrelated trees to say "Copied"
 * for a second and a half is not worth it.
 */

import { rawUrl } from "../api/client";

/**
 * `navigator.clipboard` is unavailable on insecure origins and can reject when
 * the document is not focused (a right-click menu is exactly the moment focus
 * gets interesting), so the ancient hidden-textarea + `execCommand` path is
 * kept as a real fallback rather than an apology.
 */
export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    /* fall through to the textarea */
  }
  return copyViaTextarea(text);
}

function copyViaTextarea(text: string): boolean {
  const ta = document.createElement("textarea");
  ta.value = text;
  // Off-screen but still focusable/selectable; `readOnly` stops mobile keyboards.
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.top = "-1000px";
  ta.style.left = "-1000px";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  let ok = false;
  try {
    ta.select();
    ta.setSelectionRange(0, ta.value.length);
    ok = document.execCommand("copy");
  } catch {
    ok = false;
  } finally {
    ta.remove();
  }
  return ok;
}

const TOAST_MS = 1600;
let toastTimer: number | undefined;

/** One transient toast at a time, bottom-centre, self-removing. */
export function toast(message: string): void {
  let el = document.querySelector<HTMLDivElement>(".toast");
  if (!el) {
    el = document.createElement("div");
    el.className = "toast";
    el.setAttribute("role", "status");
    el.setAttribute("aria-live", "polite");
    document.body.appendChild(el);
  }
  el.textContent = message;
  el.classList.add("is-shown");
  if (toastTimer !== undefined) window.clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => {
    el?.remove();
    toastTimer = undefined;
  }, TOAST_MS);
}

/** Copy + confirm, in one call, with an honest message when it fails. */
export async function copyAndToast(text: string, what: string): Promise<void> {
  const ok = await copyText(text);
  toast(ok ? "Copied" : `Could not copy the ${what}`);
}

/**
 * Download through a synthesised `<a download>` click.
 *
 * `rawUrl(path, true)` asks the daemon for `Content-Disposition: attachment`,
 * so the `download` attribute is only there to name the saved file. This runs
 * on the real Unraid page (not inside an artifact sandbox), so a
 * script-triggered download is honoured.
 */
export function triggerDownload(path: string, name: string): void {
  const a = document.createElement("a");
  a.href = rawUrl(path, true);
  a.download = name;
  a.rel = "noopener";
  a.style.display = "none";
  document.body.appendChild(a);
  a.click();
  a.remove();
}
