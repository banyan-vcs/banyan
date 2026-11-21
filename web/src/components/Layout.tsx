// Layout.tsx — the application chrome: a light top bar (brand, the "+"
// create menu, the avatar whose click/right-click opens the account
// menu) over the collapsible sidebar. The sidebar's workspace section is
// the three global destinations (repositories, groups, merge requests —
// narrowed by the deployment's advertised capabilities) with one-key
// shortcuts; "[" folds it to an icon rail. Inside /r/<repo> the sidebar
// additionally carries that repository's navigation.
import { Link, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useEffect, useState, type ReactNode } from "react";
import { BASE_URL } from "../api/client";
import { useSession } from "../session";
import { can, useMode } from "../mode";
import { BranchesIcon, CodeIcon, GroupsIcon, HistoryIcon, MergeIcon, OverviewIcon, RepositoriesIcon, SettingsIcon, SubtreesIcon } from "./icons";
import { NewMenu } from "./NewMenu";
import { UserMenu } from "./UserMenu";

const SIDEBAR_KEY = "banyan.sidebar";

function storedCollapsed(): boolean {
  try {
    return localStorage.getItem(SIDEBAR_KEY) === "1";
  } catch {
    return false;
  }
}

export function Layout(): ReactNode {
  const { info } = useSession();
  const mode = useMode();
  const [collapsed, setCollapsed] = useState(storedCollapsed);
  // A GitLab-provider session wears GitLab's palette (styles.less body.gitlab)
  useEffect(() => {
    document.body.classList.toggle("gitlab", info?.provider?.kind === "gitlab");
  }, [info?.provider?.kind]);
  const nav = useNavigate();
  const loc = useLocation();

  useEffect(() => {
    try {
      if (collapsed) localStorage.setItem(SIDEBAR_KEY, "1");
      else localStorage.removeItem(SIDEBAR_KEY);
    } catch {
      /* storage unavailable — the fold still works for this view */
    }
  }, [collapsed]);

  const repo = loc.pathname.match(/^\/r\/([^/]+)/)?.[1] || "";
  const base = `/r/${repo}`;
  const at = (path: string): boolean => loc.pathname === path;
  const under = (prefix: string): boolean => loc.pathname === prefix || loc.pathname.startsWith(prefix + "/");

  const workspaceNav = [
    { to: "/", label: "repositories", icon: RepositoriesIcon, hint: "R", active: at("/"), enabled: true },
    { to: "/groups", label: "groups", icon: GroupsIcon, hint: "G", active: under("/groups"), enabled: can(mode, "groups") },
    { to: "/merge_requests", label: "merge requests", icon: MergeIcon, hint: "M", active: under("/merge_requests"), enabled: can(mode, "mergeRequests") },
  ].filter((n) => n.enabled);

  const repoNav = [
    { to: base, label: "overview", icon: OverviewIcon, enabled: true, active: at(base) },
    { to: `${base}/tree`, label: "code", icon: CodeIcon, enabled: can(mode, "code"), active: under(`${base}/tree`) || under(`${base}/blame`) },
    { to: `${base}/history`, label: "history", icon: HistoryIcon, enabled: can(mode, "history"), active: under(`${base}/history`) || under(`${base}/commit`) },
    { to: `${base}/branches`, label: "branches", icon: BranchesIcon, enabled: can(mode, "branches"), active: under(`${base}/branches`) },
    { to: `${base}/-/merge_requests`, label: "merge requests", icon: MergeIcon, enabled: can(mode, "mergeRequests"), active: under(`${base}/-/merge_requests`) },
    { to: `${base}/subtrees`, label: "subtrees", icon: SubtreesIcon, enabled: can(mode, "subtrees"), active: under(`${base}/subtrees`) },
    { to: `${base}/settings`, label: "settings", icon: SettingsIcon, enabled: can(mode, "settings"), active: under(`${base}/settings`) },
  ].filter((n) => n.enabled);

  // One-key navigation + the fold: plain keys only, never while typing in
  // a form field or the editor, never with a modifier held (browser
  // shortcuts stay the browser's).
  useEffect(() => {
    const onKey = (e: KeyboardEvent): void => {
      if (e.metaKey || e.ctrlKey || e.altKey || e.repeat) return;
      const t = e.target as HTMLElement | null;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT" || t.isContentEditable)) return;
      if (e.key === "[") {
        setCollapsed((v) => !v);
        return;
      }
      const hit = workspaceNav.find((n) => n.hint.toLowerCase() === e.key.toLowerCase());
      if (hit) nav(hit.to);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [nav, workspaceNav]);

  const item = (to: string, label: string, icon: () => ReactNode, active: boolean, hint?: string): ReactNode => (
    <Link to={to} className={"side-item" + (active ? " active" : "")} title={collapsed ? label : undefined}>
      {icon()}
      {!collapsed && label}
      {!collapsed && hint && <span className="kbd">{hint}</span>}
    </Link>
  );

  return (
    <div className="shell">
      <header className="topbar">
        <Link to="/" className="brand">
          <img src={`${BASE_URL}avatar.png`} alt="" width="24" height="24" /> banyan
        </Link>
        <span className="spacer" />
        <NewMenu />
        <UserMenu />
      </header>
      <div className="shell-body">
        <aside className={"sidebar" + (collapsed ? " collapsed" : "")}>
          <nav className="side-nav">
            <div className="side-group">
              <div className="side-title-row">
                {!collapsed && <div className="side-title">workspace</div>}
                <button
                  className="side-collapse"
                  onClick={() => setCollapsed((v) => !v)}
                  title={collapsed ? "expand the sidebar ( [ )" : "fold to icons ( [ )"}
                  aria-label={collapsed ? "expand sidebar" : "collapse sidebar"}
                >
                  <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" aria-hidden="true">
                    {collapsed ? <path d="M6 3.5 10.5 8 6 12.5" /> : <path d="M10 3.5 5.5 8 10 12.5" />}
                  </svg>
                </button>
              </div>
              {workspaceNav.map((n) => item(n.to, n.label, n.icon, n.active, n.hint))}
            </div>
            {repo !== "" && (
              <div className="side-group">
                {!collapsed && <div className="side-title">{repo}</div>}
                {repoNav.map((n) => item(n.to, n.label, n.icon, n.active))}
              </div>
            )}
          </nav>
          {!collapsed && <div className="side-foot">banyan · the tree becomes a forest</div>}
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
