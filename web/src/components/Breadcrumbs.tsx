import { buildCrumbs } from "../lib/paths";
import { browseHref } from "../lib/router";
import { Icon } from "./Icon";

/**
 * Renders a (possibly virtual) path readably: real path segments as plain
 * crumbs, archive boundaries as a chip so it is obvious you have descended
 * into a container rather than a directory.
 */
export function Breadcrumbs({ path, onNavigate }: { path: string; onNavigate: (p: string) => void }) {
  const crumbs = buildCrumbs(path);

  return (
    <nav className="crumbs" aria-label="Path">
      {crumbs.map((c, i) => {
        const last = i === crumbs.length - 1;
        return (
          <span className="crumb-group" key={`${c.path}-${i}`}>
            {i > 0 && !c.root ? <span className="crumb-sep">/</span> : null}
            <a
              className={`crumb${last ? " crumb-current" : ""}${c.archive ? " crumb-archive" : ""}`}
              href={browseHref(c.path)}
              title={c.path}
              onClick={(e) => {
                if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
                e.preventDefault();
                onNavigate(c.path);
              }}
              aria-current={last ? "page" : undefined}
            >
              {c.archive ? <Icon name="archive" /> : null}
              {c.label}
            </a>
            {c.archive ? <span className="crumb-archive-sep" title="inside archive">!</span> : null}
          </span>
        );
      })}
    </nav>
  );
}
