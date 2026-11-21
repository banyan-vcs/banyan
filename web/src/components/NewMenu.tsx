// NewMenu.tsx — the topbar's "+" : a small dropdown with the two creation
// paths (new repository, new group), narrowed by the deployment's
// capabilities. Click-outside and Escape close it; the menu is plain DOM —
// no portal, the topbar is sticky so the dropdown rides above the content.
import { useEffect, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { useT } from "../i18n";
import { can, useMode } from "../mode";

export function NewMenu(): ReactNode {
  const mode = useMode();
  const nav = useNavigate();
  const t = useT();
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);

  const canRepo = can(mode, "createRepos");
  const canGroup = can(mode, "groups");
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent): void => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  if (!canRepo && !canGroup) return null;
  const go = (url: string): void => {
    setOpen(false);
    nav(url);
  };
  return (
    <div className="new-menu" ref={root}>
      <button
        className={"new-menu-btn" + (open ? " open" : "")}
        title={t("create")}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <svg width="15" height="15" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" aria-hidden="true">
          <path d="M8 3v10M3 8h10" />
        </svg>
      </button>
      {open && (
        <div className="new-menu-pop" role="menu">
          {canRepo && (
            <button
              role="menuitem"
              onClick={() => go("/new?kind=repo")}
            >
              {t("new repository")}
            </button>
          )}
          {canGroup && (
            <button
              role="menuitem"
              onClick={() => go("/new?kind=group")}
            >
              {t("new group")}
            </button>
          )}
        </div>
      )}
    </div>
  );
}
