/**
 * The right-click menu for a listing row, built once and used by both the
 * browser grid and the search results so the two can never drift.
 *
 * Ordering is deliberate: the default action first (so Enter on a freshly
 * opened menu does what a double-click would), then the alternative ways to
 * open the same file, then the inspect actions, then — behind a separator —
 * the things that leave the app or the page entirely.
 */

import type { Entry } from "../api/types";
import type { MenuItem } from "../components/ContextMenu";
import { copyAndToast, triggerDownload } from "./actions";
import { isMedia, openPlayer } from "./media";
import { navigate, viewHref, type PlaySort } from "./router";

export interface EntryMenuActions {
  /** The row's default action — the same one a double-click runs. */
  open: (e: Entry) => void;
  /** The viewer panel, whatever the file is. */
  details: (e: Entry) => void;
  /**
   * The order the caller is showing rows in, carried into the player URL so
   * its ↑/↓ walk the same sequence. Callers without a meaningful order (search
   * results) pass nothing and get the daemon's name/asc default.
   */
  playSort?: Partial<PlaySort> | null;
}

export function buildEntryMenu(e: Entry, a: EntryMenuActions): MenuItem[] {
  const items: MenuItem[] = [
    {
      id: "open",
      label: "Open",
      title: e.type === "dir" || e.type === "archive" ? `Browse ${e.path}` : undefined,
      onSelect: () => a.open(e),
    },
  ];

  // Directories and archives browse identically, and none of the file actions
  // below mean anything for them — fs/raw cannot hand you a directory.
  if (e.type === "dir" || e.type === "archive") {
    items.push(...copyItems(e));
    return items;
  }

  if (isMedia(e.mime, e.name)) {
    items.push({
      id: "play",
      label: "Play in new window",
      title: "Streams in a standalone player window (transcoded if this browser needs it)",
      onSelect: () => openPlayer(e.path, a.playSort),
    });
  } else {
    // The escape hatch for when both the reported mime and the extension are
    // wrong. It does not bypass the daemon: fs/raw still refuses to stream a
    // type it does not classify as media, and the player says exactly that.
    items.push({
      id: "play-force",
      label: "Play as media…",
      title: "Try the player anyway, even though this is not detected as audio or video",
      onSelect: () => openPlayer(e.path, { ...a.playSort, force: true }),
    });
  }

  items.push(
    {
      id: "view-text",
      label: "View as text",
      title: "Decode the bytes as text — any file, any encoding",
      onSelect: () => navigate(viewHref(e.path, "text")),
    },
    {
      id: "view-hex",
      label: "View as hex",
      onSelect: () => navigate(viewHref(e.path, "hex")),
    },
    {
      id: "details",
      label: "Details",
      title: "Open the viewer panel (⇧Enter)",
      onSelect: () => a.details(e),
    },
    {
      id: "download",
      label: "Download",
      separatorBefore: true,
      onSelect: () => triggerDownload(e.path, e.name),
    },
  );

  items.push(...copyItems(e));
  return items;
}

function copyItems(e: Entry): MenuItem[] {
  return [
    {
      id: "copy-path",
      label: "Copy path",
      separatorBefore: e.type === "dir" || e.type === "archive",
      title: e.path,
      onSelect: () => void copyAndToast(e.path, "path"),
    },
    {
      id: "copy-name",
      label: "Copy name",
      title: e.name,
      onSelect: () => void copyAndToast(e.name, "name"),
    },
  ];
}
