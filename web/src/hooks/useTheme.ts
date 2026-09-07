import { useEffect, useState } from "react";

export const THEMES = ["white", "black", "azure", "gray"] as const;
export type Theme = (typeof THEMES)[number];

const CLASS_PREFIX = "theme-";

function isTheme(v: string | null): v is Theme {
  return !!v && (THEMES as readonly string[]).includes(v);
}

/** Theme comes from the iframe URL: app/index.html?theme=<black|white|azure|gray>. */
export function readThemeFromUrl(): Theme {
  if (typeof window === "undefined") return "white";
  const search = new URLSearchParams(window.location.search);
  const q = search.get("theme");
  if (isTheme(q)) return q;
  // Also honour a theme carried on the hash query, in case the host page only
  // controls the fragment (e.g. app/index.html#/browse/%2F?theme=black).
  const hash = window.location.hash;
  const i = hash.indexOf("?");
  if (i >= 0) {
    const h = new URLSearchParams(hash.slice(i + 1)).get("theme");
    if (isTheme(h)) return h;
  }
  return "white";
}

export function applyTheme(theme: Theme): void {
  const body = document.body;
  for (const t of THEMES) body.classList.remove(CLASS_PREFIX + t);
  body.classList.add(CLASS_PREFIX + theme);
  body.dataset.theme = theme;
}

export function useTheme(): [Theme, (t: Theme) => void] {
  const [theme, setTheme] = useState<Theme>(() => readThemeFromUrl());

  useEffect(() => {
    applyTheme(theme);
  }, [theme]);

  // The plugin page may swap themes without reloading the iframe.
  useEffect(() => {
    const onMessage = (ev: MessageEvent) => {
      // Only the hosting plugin page, on our own origin, may drive the theme.
      // Anything else (another frame, an opener, a cross-origin poster) is a
      // stranger scripting our UI and is dropped before `data` is even read.
      if (ev.origin !== window.location.origin) return;
      if (ev.source !== window.parent) return;
      const data = ev.data as { type?: string; theme?: string } | null;
      if (data && data.type === "filebrowser:theme" && isTheme(data.theme ?? null)) {
        setTheme(data.theme as Theme);
      }
    };
    window.addEventListener("message", onMessage);
    return () => window.removeEventListener("message", onMessage);
  }, []);

  return [theme, setTheme];
}
