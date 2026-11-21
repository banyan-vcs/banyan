// App.tsx — routes and providers. BASE_URL ("" or /-/banyan) is the router
// basename; every repo page lives under /r/:repo. Pages the deployment's
// capability list does not expose are gated to a note (the gitlab profile
// narrows the console to the subtree features).
import { Navigate, Route, Routes, useNavigate, useParams } from "react-router-dom";
import { useEffect, type ReactNode } from "react";
import { Layout } from "./components/Layout";
import { can, useMode } from "./mode";
import { BlamePage } from "./pages/BlamePage";
import { BranchesPage } from "./pages/BranchesPage";
import { CommitPage } from "./pages/CommitPage";
import { HistoryPage } from "./pages/HistoryPage";
import { LoginPage } from "./pages/LoginPage";
import { MergeRequestPage } from "./pages/MergeRequestPage";
import { MergeRequestsPage } from "./pages/MergeRequestsPage";
import { NewPage } from "./pages/NewPage";
import { ProfilePage } from "./pages/ProfilePage";
import { ReposPage } from "./pages/ReposPage";
import { RepoHomePage } from "./pages/RepoHomePage";
import { SettingsPage } from "./pages/SettingsPage";
import { SubtreesPage } from "./pages/SubtreesPage";
import { TreePage } from "./pages/TreePage";

/** LegacyHistoryRef forwards pre-?ref= links (branch names in the path
 * segment) to the ?ref= form — shareable, slash-safe. */
function LegacyHistoryRef(): ReactNode {
  const { repo = "", ref = "" } = useParams();
  return <Navigate to={"/r/" + repo + "/history?ref=" + encodeURIComponent(ref)} replace />;
}

/** Gate renders children only when the deployment exposes the capability. */
function Gate({ capability, children }: { capability: string; children: ReactNode }): ReactNode {
  const mode = useMode();
  if (can(mode, capability)) return children;
  return (
    <>
      <h1>not in this deployment</h1>
      <p className="muted">
        this page is a standalone-console capability; this deployment (profile {mode.profile}) does not expose it.
      </p>
    </>
  );
}

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
        <Route path="/new" element={<NewPage />} />
        <Route path="/r/:repo" element={<RepoHomePage />} />
        <Route path="/r/:repo/tree" element={<Gate capability="code"><TreePage /></Gate>} />
        <Route path="/r/:repo/tree/*" element={<Gate capability="code"><TreePage /></Gate>} />
        <Route path="/r/:repo/history" element={<Gate capability="history"><HistoryPage /></Gate>} />
        <Route
          path="/r/:repo/history/:ref"
          element={<LegacyHistoryRef />}
        />
        <Route path="/r/:repo/commit/:oid" element={<Gate capability="history"><CommitPage /></Gate>} />
        <Route path="/r/:repo/blame/*" element={<Gate capability="code"><BlamePage /></Gate>} />
        <Route path="/r/:repo/-/merge_requests" element={<Gate capability="mergeRequests"><MergeRequestsPage /></Gate>} />
        <Route path="/r/:repo/-/merge_requests/:iid" element={<Gate capability="mergeRequests"><MergeRequestPage /></Gate>} />
        <Route path="/r/:repo/branches" element={<Gate capability="branches"><BranchesPage /></Gate>} />
        <Route path="/r/:repo/subtrees" element={<SubtreesPage />} />
        <Route path="/r/:repo/settings" element={<Gate capability="settings"><SettingsPage /></Gate>} />
        <Route path="/settings/profile" element={<ProfilePage />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Route>
    </Routes>
  );
}
