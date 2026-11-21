// Layout.tsx — the application chrome: a light top bar (brand, the "+"
// create menu, the avatar whose click/right-click opens the account
// menu) over the collapsible sidebar. The sidebar is contextual, one
// group at a time: on the workspace pages it carries the three global
// destinations (repositories, groups, merge requests — one-key
// shortcuts, narrowed by the deployment's capabilities); inside
// /r/<repo> it carries only that repository's navigation, and the way
// back out is the breadcrumb atop the content (repositories / group /
// repo). The fold control lives at the sidebar's bottom edge, next to
// the tagline; "[" folds it to an icon rail from anywhere.
import { Link, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useEffect, useState, type ReactNode } from "react";
import { BASE_URL } from "../api/client";
import { useT } from "../i18n";
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
  const t = useT();
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

  // The repo the page sits in, from the path: two-level /r/<group>/<project>
  // (the project is the physical id) or legacy one-level /r/<name>. The
  // known page segments distinguish the shapes — segment two is a page
  // name exactly when the path is one-level.
  const segs = loc.pathname.match(/^\/r\/([^/]+)(?:\/([^/]+))?/);
  const PAGE_SEGS = new Set(["tree", "history", "branches", "subtrees", "settings", "-"]);
  const twoLevel = !!segs?.[2] && !PAGE_SEGS.has(segs[2]);
  const group = twoLevel ? segs[1] : "";
  const repo = twoLevel ? segs[2] : segs?.[1] || "";
  const base = `/r/${group ? group + "/" : ""}${repo}`;
  const at = (path: string): boolean => loc.pathname === path;
  const under = (prefix: string): boolean => loc.pathname === prefix || loc.pathname.startsWith(prefix + "/");

  const workspaceNav = [
    { to: "/", label: t("repositories"), icon: RepositoriesIcon, hint: "R", active: at("/"), enabled: true },
    { to: "/groups", label: t("groups"), icon: GroupsIcon, hint: "G", active: under("/groups"), enabled: can(mode, "groups") },
    { to: "/merge_requests", label: t("merge requests"), icon: MergeIcon, hint: "M", active: under("/merge_requests"), enabled: can(mode, "mergeRequests") },
  ].filter((n) => n.enabled);

  const repoNav = [
    { to: base, label: t("overview"), icon: OverviewIcon, enabled: true, active: at(base) },
    { to: `${base}/tree`, label: t("code"), icon: CodeIcon, enabled: can(mode, "code"), active: under(`${base}/tree`) || under(`${base}/blame`) },
    { to: `${base}/history`, label: t("history"), icon: HistoryIcon, enabled: can(mode, "history"), active: under(`${base}/history`) || under(`${base}/commit`) },
    { to: `${base}/branches`, label: t("branches"), icon: BranchesIcon, enabled: can(mode, "branches"), active: under(`${base}/branches`) },
    { to: `${base}/-/merge_requests`, label: t("merge requests"), icon: MergeIcon, enabled: can(mode, "mergeRequests"), active: under(`${base}/-/merge_requests`) },
    { to: `${base}/subtrees`, label: t("subtrees"), icon: SubtreesIcon, enabled: can(mode, "subtrees"), active: under(`${base}/subtrees`) },
    { to: `${base}/settings`, label: t("settings"), icon: SettingsIcon, enabled: can(mode, "settings"), active: under(`${base}/settings`) },
  ].filter((n) => n.enabled);

  // One-key navigation + the fold: plain keys only, never while typing in
  // a form field or the editor, never with a modifier held (browser
  // shortcuts stay the browser's). The R/G/M keys stay global even on
  // repository pages, where the workspace menu is not shown — a hidden
  // capability shouldn't stop working because the page changed.
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

  const collapseBtn = (
    <button
      className="side-collapse"
      onClick={() => setCollapsed((v) => !v)}
      title={collapsed ? t("expand the sidebar ( [ )") : t("fold to icons ( [ )")}
      aria-label={collapsed ? t("expand sidebar") : t("collapse sidebar")}
    >
      <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" aria-hidden="true">
        {collapsed ? <path d="M6 3.5 10.5 8 6 12.5" /> : <path d="M10 3.5 5.5 8 10 12.5" />}
      </svg>
    </button>
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
            {repo === "" ? (
              <div className="side-group">{workspaceNav.map((n) => item(n.to, n.label, n.icon, n.active, n.hint))}</div>
            ) : (
              <div className="side-group">
                {!collapsed && <div className="side-title">{group ? `${group} / ${repo}` : repo}</div>}
                {repoNav.map((n) => item(n.to, n.label, n.icon, n.active))}
              </div>
            )}
          </nav>
          <div className="side-bottom">
            {!collapsed && <div className="side-foot">{t("banyan · the tree becomes a forest")}</div>}
            {collapseBtn}
          </div>
        </aside>
        <main>
          <div className="content-inner">
            {repo !== "" && (
              <div className="crumb-top">
                <Link to="/">{t("repositories")}</Link>
                {group && (
                  <>
                    {" / "}
                    <Link to="/groups">{group}</Link>
                  </>
                )}
                {" / "}
                <Link to={base} className="crumb-here">
                  {repo}
                </Link>
              </div>
            )}
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  );
}
