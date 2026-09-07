import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, listDir } from "../api/client";
import type { Entry } from "../api/types";
import { buildCrumbs, isUnderAnyRoot } from "../lib/paths";
import { browseHref } from "../lib/router";
import { Icon } from "./Icon";

interface NodeState {
  expanded: boolean;
  loading: boolean;
  error?: string;
  children?: Entry[];
  truncated?: boolean;
}

const TREE_PAGE = 500;

/**
 * Most ancestors we will auto-expand (and therefore auto-fetch) when revealing
 * a path. A crafted #/browse/ URL can name thousands of segments; without this
 * cap each one becomes a proxy.php request and starves php-fpm. 24 is deeper
 * than any real share path, and the tree still expands the tail — the part the
 * user is actually looking at.
 */
const MAX_REVEAL_ANCESTORS = 24;

/** Only containers appear in the tree. */
function isBranch(e: Entry): boolean {
  return e.type === "dir" || e.type === "archive";
}

export function TreeSidebar({
  roots,
  currentPath,
  onNavigate,
}: {
  roots: string[];
  currentPath: string;
  onNavigate: (p: string) => void;
}) {
  const [nodes, setNodes] = useState<Record<string, NodeState>>({});
  const loadingRef = useRef<Set<string>>(new Set());
  // Mirror of `nodes` for effect/callback reads without stale-closure games.
  const nodesRef = useRef(nodes);
  nodesRef.current = nodes;

  const load = useCallback(async (path: string) => {
    if (loadingRef.current.has(path)) return;
    loadingRef.current.add(path);
    setNodes((n) => ({ ...n, [path]: { ...(n[path] ?? { expanded: false }), loading: true, error: undefined } }));
    try {
      const res = await listDir({ path, limit: TREE_PAGE, sort: "name", dir: "asc", dirsFirst: true });
      const children = res.entries.filter(isBranch);
      setNodes((n) => ({
        ...n,
        [path]: {
          ...(n[path] ?? { expanded: false }),
          loading: false,
          children,
          truncated: res.total > res.entries.length,
        },
      }));
    } catch (e) {
      const msg = e instanceof ApiError ? `${e.code}: ${e.message}` : String(e);
      setNodes((n) => ({ ...n, [path]: { ...(n[path] ?? { expanded: false }), loading: false, error: msg } }));
    } finally {
      loadingRef.current.delete(path);
    }
  }, []);

  const toggle = useCallback(
    (path: string) => {
      const cur = nodesRef.current[path];
      const willExpand = !cur?.expanded;
      setNodes((n) => {
        const prev = n[path] ?? { expanded: false, loading: false };
        return { ...n, [path]: { ...prev, expanded: !prev.expanded } };
      });
      // Lazy-load children the first time a node opens (`load` de-dupes).
      if (willExpand && !cur?.children && !cur?.loading) void load(path);
    },
    [load],
  );

  // Reveal the current path: expand (and lazily fetch) every ancestor.
  const revealedRef = useRef("");
  useEffect(() => {
    if (!currentPath || revealedRef.current === currentPath) return;
    revealedRef.current = currentPath;
    // Only crumbs at or below a configured root are real tree nodes; "/" and
    // other above-root segments would just earn a FORBIDDEN from the daemon.
    const ancestors = buildCrumbs(currentPath)
      .map((c) => c.path)
      .filter((p) => isUnderAnyRoot(p, roots))
      .slice(-MAX_REVEAL_ANCESTORS);
    setNodes((n) => {
      const next = { ...n };
      for (const a of ancestors) next[a] = { ...(next[a] ?? { loading: false }), expanded: true };
      return next;
    });
    for (const a of ancestors) {
      if (!nodesRef.current[a]?.children) void load(a);
    }
  }, [currentPath, roots, load]);

  return (
    <aside className="sidebar" aria-label="Directory tree">
      <div className="sidebar-head">Roots</div>
      <div className="tree" role="tree">
        {roots.length === 0 ? <div className="tree-empty">No roots configured</div> : null}
        {roots.map((r) => (
          <TreeNode
            key={r}
            path={r}
            label={r}
            depth={0}
            type="dir"
            nodes={nodes}
            currentPath={currentPath}
            onToggle={toggle}
            onNavigate={onNavigate}
          />
        ))}
      </div>
    </aside>
  );
}

function TreeNode({
  path,
  label,
  depth,
  type,
  nodes,
  currentPath,
  onToggle,
  onNavigate,
}: {
  path: string;
  label: string;
  depth: number;
  type: Entry["type"];
  nodes: Record<string, NodeState>;
  currentPath: string;
  onToggle: (p: string) => void;
  onNavigate: (p: string) => void;
}) {
  const state = nodes[path];
  const expanded = !!state?.expanded;
  const selected = currentPath === path;

  return (
    <div className="tree-node" role="treeitem" aria-expanded={expanded} aria-selected={selected}>
      <div
        className={`tree-row${selected ? " is-selected" : ""}`}
        style={{ paddingLeft: `${4 + depth * 12}px` }}
      >
        <button
          type="button"
          className={`tree-twisty${expanded ? " is-open" : ""}`}
          onClick={() => onToggle(path)}
          aria-label={expanded ? `Collapse ${label}` : `Expand ${label}`}
          tabIndex={-1}
        >
          <Icon name={expanded ? "chevronDown" : "chevron"} />
        </button>
        <a
          className="tree-label"
          href={browseHref(path)}
          title={path}
          onClick={(e) => {
            if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
            e.preventDefault();
            onNavigate(path);
          }}
        >
          <Icon name={type === "archive" ? "archive" : "folder"} />
          <span className="tree-name">{label}</span>
        </a>
      </div>

      {expanded ? (
        <div className="tree-children">
          {state?.loading ? <div className="tree-msg" style={{ paddingLeft: `${16 + depth * 12}px` }}>Loading…</div> : null}
          {state?.error ? (
            <div className="tree-msg tree-msg-error" style={{ paddingLeft: `${16 + depth * 12}px` }}>
              {state.error}
            </div>
          ) : null}
          {state?.children?.map((c) => (
            <TreeNode
              key={c.path}
              path={c.path}
              label={c.name}
              depth={depth + 1}
              type={c.type}
              nodes={nodes}
              currentPath={currentPath}
              onToggle={onToggle}
              onNavigate={onNavigate}
            />
          ))}
          {state?.children && state.children.length === 0 && !state.loading && !state.error ? (
            <div className="tree-msg" style={{ paddingLeft: `${16 + depth * 12}px` }}>
              No subfolders
            </div>
          ) : null}
          {state?.truncated ? (
            <div className="tree-msg" style={{ paddingLeft: `${16 + depth * 12}px` }}>
              first {TREE_PAGE} shown
            </div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
