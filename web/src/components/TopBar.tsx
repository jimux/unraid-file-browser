import { useEffect, useState } from "react";
import { Breadcrumbs } from "./Breadcrumbs";
import { Icon } from "./Icon";
import { SETTINGS_HREF, browseHref, navigate, searchHref } from "../lib/router";
import { parentPath } from "../lib/paths";

export type ViewMode = "browse" | "search" | "settings";

interface TopBarProps {
  mode: ViewMode;
  path: string;
  onNavigate: (p: string) => void;
  onReload: () => void;
}

export function TopBar({ mode, path, onNavigate, onReload }: TopBarProps) {
  const [q, setQ] = useState("");

  // Keep the box in sync when the app lands on a search route from elsewhere.
  useEffect(() => {
    if (mode !== "search") setQ("");
  }, [mode]);

  const up = parentPath(path);

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const term = q.trim();
    if (!term) return;
    const sp = new URLSearchParams({ q: term, mode: "both", path });
    navigate(searchHref(sp));
  };

  return (
    <header className="topbar">
      <div className="topbar-row">
        <div className="topbar-nav">
          <button
            type="button"
            className="btn btn-icon"
            title="Up one level (Backspace)"
            disabled={!up || mode !== "browse"}
            onClick={() => up && onNavigate(up)}
          >
            <Icon name="up" />
          </button>
          <button type="button" className="btn btn-icon" title="Reload" onClick={onReload}>
            <Icon name="refresh" />
          </button>
        </div>

        <div className="topbar-crumbs">
          {mode === "settings" ? (
            <div className="topbar-title">Settings</div>
          ) : mode === "search" ? (
            <div className="topbar-title">Search</div>
          ) : (
            <Breadcrumbs path={path} onNavigate={onNavigate} />
          )}
        </div>

        <form className="topbar-search" onSubmit={submit} role="search">
          <Icon name="search" className="search-icon" />
          <input
            type="search"
            className="input search-input"
            placeholder="Search files…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            aria-label="Search files"
          />
        </form>

        <div className="topbar-modes" role="tablist" aria-label="View">
          <a
            className={`btn btn-mode${mode === "browse" ? " is-active" : ""}`}
            href={browseHref(path)}
            role="tab"
            aria-selected={mode === "browse"}
          >
            Browse
          </a>
          <a
            className={`btn btn-mode${mode === "search" ? " is-active" : ""}`}
            href={searchHref(new URLSearchParams({ path }))}
            role="tab"
            aria-selected={mode === "search"}
          >
            Search
          </a>
          <a
            className={`btn btn-mode${mode === "settings" ? " is-active" : ""}`}
            href={SETTINGS_HREF}
            role="tab"
            aria-selected={mode === "settings"}
            title="Settings"
          >
            <Icon name="gear" />
          </a>
        </div>
      </div>
    </header>
  );
}
