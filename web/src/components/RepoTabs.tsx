// RepoTabs.tsx — the per-repo tab strip (code / history / branches /
// subtrees / settings) shared by every repo page.
import { Link, useLocation } from "react-router-dom";
import type { ReactNode } from "react";

export function RepoTabs({ repo, current }: { repo: string; current: string }): ReactNode {
  const loc = useLocation();
  const base = `/r/${repo}`;
  const tabs: Array<[string, string]> = [
    ["", "code"],
    ["history", "history"],
    ["branches", "branches"],
    ["subtrees", "subtrees"],
    ["settings", "settings"],
  ];
  return (
    <div className="tabs">
      {tabs.map(([suffix, label]) => {
        const to = suffix === "" ? base : `${base}/${suffix}`;
        const active = current === suffix && (suffix !== "" || loc.pathname === base);
        return (
          <Link key={label} to={to} className={"tab" + (active ? " active" : "")}>
            {label}
          </Link>
        );
      })}
    </div>
  );
}
