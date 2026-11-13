// Layout.tsx — the application chrome: brand, navigation, the logged-in
// user (the whole demo is about per-user visibility) and logout.
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useEffect, type ReactNode } from "react";
import { BASE_URL } from "../api/client";
import { useSession } from "../session";

export function Layout(): ReactNode {
  const { info } = useSession();
  // GitLab SSO mode wears GitLab's palette (body.gitlab in styles.css)
  useEffect(() => {
    document.body.classList.toggle("gitlab", !!info?.sso);
  }, [info?.sso]);
  const nav = useNavigate();
  const loc = useLocation();
  const qc = useQueryClient();
  const logout = useMutation({
    mutationFn: async () => {
      await fetch(`${BASE_URL}api/logout`, { method: "POST", credentials: "same-origin" });
    },
    onSettled: () => {
      qc.clear();
      nav("/login");
    },
  });

  const tab = (to: string, label: string): ReactNode => (
    <Link to={to} className={"navlink" + (loc.pathname === to ? " active" : "")}>
      {label}
    </Link>
  );

  return (
    <>
      <header>
        <Link to="/" className="brand">
          <img src={`${BASE_URL}avatar.png`} alt="" width="26" height="26" /> banyan
        </Link>
        <nav>
          {tab("/", "repositories")}
          {tab("/settings/tokens", "tokens")}
        </nav>
        <span className="spacer" />
        {info && (
          <span className="who">
            {info.user}
            {info.sso ? <span className="pill">gitlab sso</span> : null}
            <button className="btn ghost" onClick={() => logout.mutate()}>
              log out
            </button>
          </span>
        )}
      </header>
      <main>
        <Outlet />
      </main>
      <footer className="site">
        <span className="muted">banyan · the tree becomes a forest</span>
      </footer>
    </>
  );
}
