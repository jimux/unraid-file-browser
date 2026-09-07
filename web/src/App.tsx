import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { Entry } from "./api/types";
import { getIndexConfig } from "./api/client";
import { TopBar, type ViewMode } from "./components/TopBar";
import { TreeSidebar } from "./components/TreeSidebar";
import { BrowserView, type SortState } from "./views/BrowserView";
import { SearchView } from "./views/SearchView";
import { SettingsView } from "./views/SettingsView";
import { ViewerPanel } from "./views/ViewerPanel";
import { useRoute } from "./hooks/useRoute";
import { useTheme } from "./hooks/useTheme";
import { browseHref, navigate, viewHref } from "./lib/router";
import { parentPath } from "./lib/paths";

const FALLBACK_ROOTS = ["/mnt/user"];

export default function App() {
  useTheme();
  const route = useRoute();

  const [roots, setRoots] = useState<string[]>(FALLBACK_ROOTS);
  const [rootsReady, setRootsReady] = useState(false);
  const [sort, setSort] = useState<SortState>({ key: "name", dir: "asc", dirsFirst: true });
  const [reloadNonce, setReloadNonce] = useState(0);

  // Configured roots drive the tree and the default landing directory.
  useEffect(() => {
    let alive = true;
    const ac = new AbortController();
    getIndexConfig(ac.signal)
      .then((r) => {
        if (!alive) return;
        if (r.config?.roots?.length) setRoots(r.config.roots);
      })
      .catch(() => {
        /* Settings surfaces the real error; browsing still works from the fallback. */
      })
      .finally(() => alive && setRootsReady(true));
    return () => {
      alive = false;
      ac.abort();
    };
  }, []);

  const defaultPath = roots[0] ?? FALLBACK_ROOTS[0];

  // Land on the first root when there is no (or an unparsable) hash.
  useEffect(() => {
    if (route.name === "unknown" && rootsReady) navigate(browseHref(defaultPath), true);
  }, [route.name, rootsReady, defaultPath]);

  // Remember the last browsed directory so Search can prefill its scope.
  const lastDirRef = useRef(defaultPath);

  const browsePath = useMemo(() => {
    if (route.name === "browse") return route.path;
    if (route.name === "view") return parentPath(route.path) ?? defaultPath;
    return lastDirRef.current;
  }, [route, defaultPath]);

  useEffect(() => {
    if (route.name === "browse" || route.name === "view") lastDirRef.current = browsePath;
  }, [route.name, browsePath]);

  const go = useCallback((p: string) => navigate(browseHref(p)), []);
  const openFile = useCallback((p: string) => navigate(viewHref(p)), []);
  const openEntry = useCallback((e: Entry) => openFile(e.path), [openFile]);
  const closeViewer = useCallback(() => navigate(browseHref(browsePath)), [browsePath]);

  const mode: ViewMode = route.name === "search" ? "search" : route.name === "settings" ? "settings" : "browse";
  const showBrowser = route.name === "browse" || route.name === "view" || route.name === "unknown";

  return (
    <div className="app">
      <TopBar mode={mode} path={browsePath} onNavigate={go} onReload={() => setReloadNonce((n) => n + 1)} />

      <div className="app-body">
        <TreeSidebar roots={roots} currentPath={browsePath} onNavigate={go} />

        <main className="main">
          {showBrowser ? (
            <>
              <BrowserView
                path={browsePath}
                sort={sort}
                onSortChange={setSort}
                onNavigate={go}
                onOpenFile={openEntry}
                reloadNonce={reloadNonce}
                inert={route.name === "view"}
              />
              {route.name === "view" ? <ViewerPanel path={route.path} onClose={closeViewer} /> : null}
            </>
          ) : null}

          {route.name === "search" ? (
            <SearchView
              key={route.query.toString()}
              params={route.query}
              currentDir={lastDirRef.current}
              onNavigate={go}
              onOpenFile={openFile}
            />
          ) : null}

          {route.name === "settings" ? <SettingsView onRootsChanged={setRoots} /> : null}
        </main>
      </div>
    </div>
  );
}
