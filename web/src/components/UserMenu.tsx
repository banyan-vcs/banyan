// UserMenu.tsx — the avatar at the top bar's right end: click or
// right-click opens the account menu (the signed-in identity up top,
// settings and log out below). The bar itself carries no
// username text — the avatar is the whole account surface.
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router-dom";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { BASE_URL } from "../api/client";
import { useT } from "../i18n";
import { useSession } from "../session";

export function UserMenu(): ReactNode {
  const { info } = useSession();
  const t = useT();
  const nav = useNavigate();
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);

  const logout = useMutation({
    mutationFn: async () => {
      await fetch(`${BASE_URL}api/logout`, { method: "POST", credentials: "same-origin" });
    },
    onSettled: () => {
      qc.clear();
      nav("/login");
    },
  });

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

  if (!info) return null;
  const initial = info.user.slice(0, 1).toUpperCase();
  return (
    <div
      className="user-menu"
      ref={root}
      onClick={() => setOpen((v) => !v)}
      onContextMenu={(e) => {
        e.preventDefault();
        setOpen(true);
      }}
    >
      <button className="avatar-btn" title={t("{0} — Settings & Log Out", info.user)} aria-haspopup="menu" aria-expanded={open}>
        <span className="user-dot">{initial}</span>
      </button>
      {open && (
        <div className="user-menu-pop" role="menu" onClick={(e) => e.stopPropagation()}>
          <div className="user-menu-head">
            <span className="user-dot">{initial}</span>
            <span className="user-menu-name">{info.user}</span>
            {info.provider ? <span className="pill">{info.provider.id}</span> : null}
          </div>
          <button
            role="menuitem"
            onClick={() => {
              setOpen(false);
              nav("/settings/profile");
            }}
          >
            {t("Settings")}
          </button>
          <button role="menuitem" className="user-menu-logout" onClick={() => logout.mutate()} disabled={logout.isPending}>
            {logout.isPending ? t("logging out…") : t("Log Out")}
          </button>
        </div>
      )}
    </div>
  );
}
