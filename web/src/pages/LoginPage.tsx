// LoginPage.tsx — local form (banyan's own auth) or the GitLab SSO button;
// errors arrive via ?error= from the OAuth callback.
import { useMutation, useQuery } from "@tanstack/react-query";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { BASE_URL, api } from "../api/client";

export function LoginPage(): ReactNode {
  const nav = useNavigate();
  const [params] = useSearchParams();
  const [user, setUser] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState(params.get("error") || "");
  const mode = useQuery({ queryKey: ["mode"], queryFn: () => api<{ sso: boolean }>("/api/mode"), staleTime: Infinity });
  const login = useMutation({
    mutationFn: async () => {
      const resp = await fetch(`${BASE_URL}api/login`, {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ user, password }),
      });
      if (!resp.ok) {
        const body = (await resp.json().catch(() => ({}))) as { error?: string };
        throw new Error(body.error || "wrong user or password");
      }
    },
    onSuccess: () => nav(params.get("back") || "/", { replace: true }),
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  return (
    <div className="login-card">
      <h1>banyan console</h1>
      <p className="muted">every page shows the repositories and branches this account can see — the same view git gets.</p>
      {error && <div className="error-box">{error}</div>}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          setError("");
          login.mutate();
        }}
      >
        <input placeholder="user" value={user} onChange={(e) => setUser(e.target.value)} autoFocus />
        <input placeholder="password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} />
        <button className="btn" type="submit" disabled={login.isPending}>
          {login.isPending ? "checking…" : "log in"}
        </button>
      </form>
      {mode.data?.sso && (
        <a className="btn ghost sso" href={`${BASE_URL}api/auth/gitlab`}>
          continue with GitLab
        </a>
      )}
    </div>
  );
}
