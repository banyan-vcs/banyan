// Layout.tsx — the application chrome, after bytebase: a light top bar
// (brand, the logged-in user, logout) over a gray sidebar. The sidebar
// carries the workspace links and, inside /r/<repo>, that repository's
// navigation — the whole demo is about per-user visibility, so the user
// stays visible in the chrome.
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useEffect, type ReactNode } from "react";
import { BASE_URL } from "../api/client";
import { useSession } from "../session";
import { BranchesIcon, CodeIcon, HistoryIcon, OverviewIcon, RepositoriesIcon, SettingsIcon, SubtreesIcon, TokensIcon } from "./icons";

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

  const repo = loc.pathname.match(/^\/r\/([^/]+)/)?.[1] || "";
  const base = `/r/${repo}`;
  const at = (path: string): boolean => loc.pathname === path;
  const under = (prefix: string): boolean => loc.pathname === prefix || loc.pathname.startsWith(prefix + "/");
  const item = (to: string, label: string, icon: () => ReactNode, active: boolean): ReactNode => (
    <Link to={to} className={"side-item" + (active ? " active" : "")}>
      {icon()}
      {label}
    </Link>
  );

  const repoNav: Array<[string, string, () => ReactNode, boolean]> = [
    [base, "overview", OverviewIcon, at(base)],
    [`${base}/tree`, "code", CodeIcon, under(`${base}/tree`) || under(`${base}/blame`)],
    [`${base}/history`, "history", HistoryIcon, under(`${base}/history`) || under(`${base}/commit`)],
    [`${base}/branches`, "branches", BranchesIcon, under(`${base}/branches`)],
    [`${base}/subtrees`, "subtrees", SubtreesIcon, under(`${base}/subtrees`)],
    [`${base}/settings`, "settings", SettingsIcon, under(`${base}/settings`)],
  ];

  return (
    <div className="shell">
      <header className="topbar">
        <Link to="/" className="brand">
          <img src={`${BASE_URL}avatar.png`} alt="" width="24" height="24" /> banyan
        </Link>
        <span className="spacer" />
        {info && (
          <span className="who">
            <span className="user-dot">{info.user.slice(0, 1).toUpperCase()}</span>
            {info.user}
            {info.sso ? <span className="pill">gitlab sso</span> : null}
            <button className="btn ghost" onClick={() => logout.mutate()}>
              log out
            </button>
          </span>
        )}
      </header>
      <div className="shell-body">
        <aside className="sidebar">
          <nav className="side-nav">
            <div className="side-group">
              <div className="side-title">workspace</div>
              {item("/", "repositories", RepositoriesIcon, at("/"))}
              {item("/settings/tokens", "tokens", TokensIcon, under("/settings/tokens"))}
            </div>
            {repo !== "" && (
              <div className="side-group">
                <div className="side-title">{repo}</div>
                {repoNav.map(([to, label, icon, active]) => item(to, label, icon, active))}
              </div>
            )}
          </nav>
          <div className="side-foot">banyan · the tree becomes a forest</div>
        </aside>
        <main>
          <div className="content-inner">
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  );
}
