// Layout.tsx — the application chrome, after bytebase: a light top bar
// (brand, the logged-in user, logout) over a gray sidebar. The sidebar
// carries the workspace links and, inside /r/<repo>, that repository's
// navigation — narrowed by the deployment's advertised capabilities (the
// gitlab profile surfaces the subtree features only).
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useEffect, type ReactNode } from "react";
import { BASE_URL } from "../api/client";
import { useSession } from "../session";
import { can, useMode } from "../mode";
import { BranchesIcon, CodeIcon, HistoryIcon, ImportIcon, OverviewIcon, RepositoriesIcon, SettingsIcon, SubtreesIcon, TokensIcon } from "./icons";

export function Layout(): ReactNode {
  const { info } = useSession();
  const mode = useMode();
  // A GitLab-provider session wears GitLab's palette (styles.css body.gitlab)
  useEffect(() => {
    document.body.classList.toggle("gitlab", info?.provider?.kind === "gitlab");
  }, [info?.provider?.kind]);
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

  const repoNav = [
    { to: base, label: "overview", icon: OverviewIcon, enabled: true, active: at(base) },
    { to: `${base}/tree`, label: "code", icon: CodeIcon, enabled: can(mode, "code"), active: under(`${base}/tree`) || under(`${base}/blame`) },
    { to: `${base}/history`, label: "history", icon: HistoryIcon, enabled: can(mode, "history"), active: under(`${base}/history`) || under(`${base}/commit`) },
    { to: `${base}/branches`, label: "branches", icon: BranchesIcon, enabled: can(mode, "branches"), active: under(`${base}/branches`) },
    { to: `${base}/subtrees`, label: "subtrees", icon: SubtreesIcon, enabled: can(mode, "subtrees"), active: under(`${base}/subtrees`) },
    { to: `${base}/settings`, label: "settings", icon: SettingsIcon, enabled: can(mode, "settings"), active: under(`${base}/settings`) },
  ].filter((n) => n.enabled);

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
            {info.provider ? <span className="pill">{info.provider.id}</span> : null}
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
              {can(mode, "import") && item("/settings/imports", "imports", ImportIcon, under("/settings/imports"))}
              {can(mode, "tokens") && item("/settings/tokens", "tokens", TokensIcon, under("/settings/tokens"))}
            </div>
            {repo !== "" && (
              <div className="side-group">
                <div className="side-title">{repo}</div>
                {repoNav.map((n) => item(n.to, n.label, n.icon, n.active))}
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
