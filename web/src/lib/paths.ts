/**
 * Virtual-path helpers. Per API.md the client treats paths as *opaque strings*:
 * the server owns resolution (filesystem-first, longest literal match). We only
 * need enough structure to render breadcrumbs and walk "up".
 */

export const ARCHIVE_SEP = "!/";

/** Join a directory path with a child name using plain "/" semantics. */
export function joinPath(base: string, name: string): string {
  if (!base) return name;
  if (base.endsWith("/")) return base + name;
  return `${base}/${name}`;
}

/**
 * Parent of a path. Crossing an archive boundary lands on the archive file
 * itself (which lists like a directory), e.g.
 *   /a/b.zip!/x/y -> /a/b.zip!/x -> /a/b.zip -> /a
 */
export function parentPath(path: string): string | null {
  if (!path) return null;
  let s = path;
  // A trailing "!/" is the archive root; its parent is the archive file's dir.
  while (s.length > 1 && s.endsWith("/") && !s.endsWith(ARCHIVE_SEP)) s = s.slice(0, -1);
  if (s === "/" || s === "") return null;
  if (s.endsWith(ARCHIVE_SEP)) s = s.slice(0, -ARCHIVE_SEP.length);

  const i = s.lastIndexOf("/");
  if (i < 0) return null;
  if (i > 0 && s[i - 1] === "!") return s.slice(0, i - 1); // step out of the archive
  if (i === 0) return s.length > 1 ? "/" : null;
  return s.slice(0, i);
}

/** Last component of a path, archive-aware. */
export function basename(path: string): string {
  let s = path;
  while (s.length > 1 && s.endsWith("/")) s = s.slice(0, -1);
  const i = s.lastIndexOf("/");
  if (i < 0) return s;
  if (i > 0 && s[i - 1] === "!") return s.slice(0, i - 1).split("/").pop() || s;
  return s.slice(i + 1) || s;
}

/** True if the path descends into at least one archive. */
export function isVirtual(path: string): boolean {
  return path.includes(ARCHIVE_SEP);
}

export interface Crumb {
  label: string;
  path: string;
  /** This crumb is an archive file we are browsing *into*. */
  archive?: boolean;
  /** Render as the filesystem root "/" glyph. */
  root?: boolean;
}

/**
 * Breadcrumb model for a (possibly virtual) path.
 *
 *   /mnt/user/bk/site.tar.gz!/www/config.zip!/app/settings.php
 *   → / · mnt · user · bk · [site.tar.gz] · www · [config.zip] · app · settings.php
 *
 * Crumbs marked `archive` are the boundary segments and get a chip in the UI.
 */
export function buildCrumbs(path: string): Crumb[] {
  const crumbs: Crumb[] = [];
  const layers = path.split(ARCHIVE_SEP);

  let cursor = "";
  layers.forEach((layer, li) => {
    const last = li === layers.length - 1;
    const parts = layer.split("/").filter(Boolean);

    if (li === 0) {
      cursor = "";
      crumbs.push({ label: "/", path: "/", root: true });
      parts.forEach((part, pi) => {
        cursor = `${cursor}/${part}`;
        crumbs.push({
          label: part,
          path: cursor,
          archive: !last && pi === parts.length - 1,
        });
      });
    } else {
      // Inside an archive: the first part hangs off the previous cursor via "!/".
      parts.forEach((part, pi) => {
        cursor = pi === 0 ? `${cursor}${ARCHIVE_SEP}${part}` : `${cursor}/${part}`;
        crumbs.push({
          label: part,
          path: cursor,
          archive: !last && pi === parts.length - 1,
        });
      });
      if (parts.length === 0) {
        // Trailing "!/" — the archive root itself.
        cursor = `${cursor}${ARCHIVE_SEP}`;
      }
    }
  });

  return crumbs;
}

/** Directory portion of a file path (for search results). */
export function dirOf(path: string): string {
  return parentPath(path) ?? "/";
}

/**
 * True if `path` *is* `root` or lives underneath it. Boundary-aware, so
 * "/mnt/userdata" is not considered to be under "/mnt/user".
 */
export function isUnderRoot(path: string, root: string): boolean {
  if (!root || !path) return false;
  if (path === root) return true;
  return path.startsWith(root.endsWith("/") ? root : `${root}/`);
}

/** True if `path` is under at least one of `roots`. */
export function isUnderAnyRoot(path: string, roots: string[]): boolean {
  return roots.some((r) => isUnderRoot(path, r));
}

/** File extension without the dot, lowercased, "" if none. */
export function extOf(name: string): string {
  const i = name.lastIndexOf(".");
  if (i <= 0 || i === name.length - 1) return "";
  return name.slice(i + 1).toLowerCase();
}
