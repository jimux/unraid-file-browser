import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";

/**
 * A custom right-click menu.
 *
 * The native menu is suppressed wherever this one opens, because the browser's
 * own menu has nothing useful to say about a row in a virtualised grid: its
 * "Save link as…" would point at nothing, and everything the user actually
 * wants here (play, view as hex, copy the server-side path) has no native
 * equivalent.
 *
 * It is rendered inline by whichever view owns the menu state rather than
 * through a portal: `position: fixed` already escapes every scroll container
 * on the way up, and a portal would only add a mount point to lose track of.
 */

export interface MenuItem {
  id: string;
  label: string;
  onSelect: () => void;
  /** Draw a divider above this item. */
  separatorBefore?: boolean;
  title?: string;
}

export interface MenuAnchor {
  x: number;
  y: number;
}

/** Distance kept between the menu and the viewport edge when clamping. */
const EDGE_GAP = 6;

export function ContextMenu({
  x,
  y,
  items,
  onClose,
  label = "Actions",
}: MenuAnchor & { items: MenuItem[]; onClose: () => void; label?: string }) {
  const ref = useRef<HTMLDivElement>(null);
  const itemRefs = useRef<Array<HTMLButtonElement | null>>([]);
  const [active, setActive] = useState(0);
  const [pos, setPos] = useState<MenuAnchor>({ x, y });

  // Typeahead: consecutive letters within a short window jump to a label.
  const typed = useRef({ buffer: "", at: 0 });

  /**
   * Clamp after mount, when the real size is known: flipping to the other side
   * of the cursor keeps the pointer outside the menu (so the first item is not
   * under it), and only when there is no room does it slide flush to the edge.
   */
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const { width, height } = el.getBoundingClientRect();
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    let nx = x;
    let ny = y;
    if (nx + width > vw - EDGE_GAP) nx = Math.max(EDGE_GAP, x - width);
    if (ny + height > vh - EDGE_GAP) ny = Math.max(EDGE_GAP, y - height);
    // Still too tall for the viewport: pin to the top and let it scroll.
    if (ny + height > vh - EDGE_GAP) ny = EDGE_GAP;
    setPos({ x: nx, y: ny });
  }, [x, y, items.length]);

  // The first item takes focus so the keyboard works without a click.
  useEffect(() => {
    itemRefs.current[0]?.focus({ preventScroll: true });
  }, []);

  useEffect(() => {
    itemRefs.current[active]?.focus({ preventScroll: true });
  }, [active]);

  /**
   * Everything that means "the anchor moved or the user's attention left":
   * a click anywhere else, a scroll in any ancestor (capture phase, because a
   * scrolling <div> does not bubble its scroll event), a resize, a blur.
   */
  useEffect(() => {
    const el = ref.current;
    const onPointerDown = (ev: MouseEvent) => {
      if (el && ev.target instanceof Node && el.contains(ev.target)) return;
      onClose();
    };
    const onScroll = (ev: Event) => {
      if (el && ev.target instanceof Node && el.contains(ev.target)) return;
      onClose();
    };
    // `mousedown` rather than `click`: closing on the press feels immediate and
    // avoids the menu eating the click that dismissed it.
    document.addEventListener("mousedown", onPointerDown, true);
    document.addEventListener("contextmenu", onPointerDown, true);
    document.addEventListener("scroll", onScroll, true);
    window.addEventListener("resize", onClose);
    window.addEventListener("blur", onClose);
    return () => {
      document.removeEventListener("mousedown", onPointerDown, true);
      document.removeEventListener("contextmenu", onPointerDown, true);
      document.removeEventListener("scroll", onScroll, true);
      window.removeEventListener("resize", onClose);
      window.removeEventListener("blur", onClose);
    };
  }, [onClose]);

  const run = useCallback(
    (item: MenuItem) => {
      onClose();
      item.onSelect();
    },
    [onClose],
  );

  const onKeyDown = useCallback(
    (ev: React.KeyboardEvent<HTMLDivElement>) => {
      const last = items.length - 1;
      switch (ev.key) {
        case "Escape":
          ev.preventDefault();
          ev.stopPropagation();
          onClose();
          return;
        case "ArrowDown":
          ev.preventDefault();
          setActive((i) => (i >= last ? 0 : i + 1));
          return;
        case "ArrowUp":
          ev.preventDefault();
          setActive((i) => (i <= 0 ? last : i - 1));
          return;
        case "Home":
          ev.preventDefault();
          setActive(0);
          return;
        case "End":
          ev.preventDefault();
          setActive(last);
          return;
        case "Tab":
          // A menu is a dead end for Tab: leaving it silently would strand the
          // focus ring somewhere behind the (now stale) menu.
          ev.preventDefault();
          onClose();
          return;
        case "Enter":
        case " ":
        case "Spacebar": {
          ev.preventDefault();
          const item = items[active];
          if (item) run(item);
          return;
        }
        default:
          break;
      }

      // Typeahead — a single printable character, no modifiers.
      if (ev.key.length !== 1 || ev.ctrlKey || ev.altKey || ev.metaKey) return;
      const now = Date.now();
      const t = typed.current;
      const key = ev.key.toLowerCase();
      t.buffer = now - t.at > 700 ? key : t.buffer + key;
      t.at = now;

      /**
       * Try the accumulated prefix first ("co" → Copy path), then fall back to
       * the single character just typed. Without the fallback, typing two
       * letters that each name a *different* item ("d" for Details, then "c"
       * for Copy path) would search for "dc" and silently do nothing.
       */
      const hit = findFrom(items, t.buffer, t.buffer.length === 1 ? active + 1 : active);
      if (hit >= 0) {
        ev.preventDefault();
        setActive(hit);
        return;
      }
      if (t.buffer.length === 1) return;
      t.buffer = key;
      const retry = findFrom(items, key, active + 1);
      if (retry >= 0) {
        ev.preventDefault();
        setActive(retry);
      }
    },
    [active, items, onClose, run],
  );

  return (
    <div
      ref={ref}
      className="ctxmenu"
      role="menu"
      aria-label={label}
      data-testid="ctxmenu"
      style={{ left: `${pos.x}px`, top: `${pos.y}px` }}
      onKeyDown={onKeyDown}
      onContextMenu={(ev) => ev.preventDefault()}
    >
      {items.map((item, i) => (
        <button
          key={item.id}
          ref={(el) => {
            itemRefs.current[i] = el;
          }}
          type="button"
          role="menuitem"
          className={`ctxmenu-item${item.separatorBefore ? " has-sep" : ""}${i === active ? " is-active" : ""}`}
          tabIndex={i === active ? 0 : -1}
          title={item.title}
          data-menuitem={item.id}
          onMouseEnter={() => setActive(i)}
          onClick={() => run(item)}
        >
          {item.label}
        </button>
      ))}
    </div>
  );
}

/**
 * Menu state for one view: where it is, and what it acts on.
 *
 * `T` is whatever the view needs to build the items (an Entry, usually) — the
 * menu itself never looks inside it.
 */
export interface MenuState<T> {
  x: number;
  y: number;
  target: T;
}

/**
 * Open-at-cursor / open-at-element plumbing, shared by the grid and the search
 * results so both behave identically.
 */
export function useContextMenu<T>() {
  const [menu, setMenu] = useState<MenuState<T> | null>(null);

  const openAt = useCallback((ev: { clientX: number; clientY: number }, target: T) => {
    setMenu({ x: ev.clientX, y: ev.clientY, target });
  }, []);

  /**
   * Shift+F10 / the Menu key have no cursor, so the anchor is the element the
   * selection is on — bottom-left of it, the way native menus do.
   */
  const openAtElement = useCallback((el: Element | null | undefined, target: T) => {
    const r = el?.getBoundingClientRect();
    setMenu(r ? { x: Math.round(r.left + 8), y: Math.round(r.bottom), target } : { x: 24, y: 24, target });
  }, []);

  const close = useCallback(() => setMenu(null), []);

  return useMemo(() => ({ menu, openAt, openAtElement, close }), [menu, openAt, openAtElement, close]);
}

/** First item whose label starts with `prefix`, searching circularly. */
function findFrom(items: MenuItem[], prefix: string, from: number): number {
  for (let n = 0; n < items.length; n += 1) {
    const i = (from + n + items.length) % items.length;
    if (items[i].label.toLowerCase().startsWith(prefix)) return i;
  }
  return -1;
}

/** True for the two keystrokes that mean "open the context menu". */
export function isMenuKey(ev: { key: string; shiftKey: boolean }): boolean {
  return ev.key === "ContextMenu" || (ev.key === "F10" && ev.shiftKey);
}
