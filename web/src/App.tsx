// App.tsx — routes and providers. BASE_URL ("" or /-/banyan) is the router
// basename; every repo page lives under /r/:repo with a tab strip.
import { Navigate, Route, Routes, useNavigate } from "react-router-dom";
import { useEffect, type ReactNode } from "react";
import { Layout } from "./components/Layout";
import { BlamePage } from "./pages/BlamePage";
import { BranchesPage } from "./pages/BranchesPage";
import { CommitPage } from "./pages/CommitPage";
import { HistoryPage } from "./pages/HistoryPage";
import { LoginPage } from "./pages/LoginPage";
import { ReposPage } from "./pages/ReposPage";
import { RepoHomePage } from "./pages/RepoHomePage";
import { SettingsPage } from "./pages/SettingsPage";
import { SubtreesPage } from "./pages/SubtreesPage";
import { TreePage } from "./pages/TreePage";
import { TokensPage } from "./pages/TokensPage";

export function AppRoutes(): ReactNode {
  const nav = useNavigate();
  // API 401s route to the login page in-router (keeps the return target,
  // never reloads, and cannot loop on the login page itself).
  useEffect(() => {
    const on401 = (e: Event): void => {
      const back = (e as CustomEvent<{ back: string }>).detail?.back || "/";
      if (!back.startsWith("/login")) nav(`/login?back=${encodeURIComponent(back)}`);
    };
    window.addEventListener("banyan:unauthorized", on401);
    return () => window.removeEventListener("banyan:unauthorized", on401);
  }, [nav]);
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route element={<Layout />}>
        <Route path="/" element={<ReposPage />} />
        <Route path="/r/:repo" element={<RepoHomePage />} />
        <Route path="/r/:repo/tree" element={<TreePage />} />
        <Route path="/r/:repo/tree/*" element={<TreePage />} />
        <Route path="/r/:repo/history" element={<HistoryPage />} />
        <Route path="/r/:repo/history/:ref" element={<HistoryPage />} />
        <Route path="/r/:repo/commit/:oid" element={<CommitPage />} />
        <Route path="/r/:repo/blame/*" element={<BlamePage />} />
        <Route path="/r/:repo/branches" element={<BranchesPage />} />
        <Route path="/r/:repo/subtrees" element={<SubtreesPage />} />
        <Route path="/r/:repo/settings" element={<SettingsPage />} />
        <Route path="/settings/tokens" element={<TokensPage />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
