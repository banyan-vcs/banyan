// TokensPage.tsx — personal access tokens: mint (plaintext shown exactly
// once), list your own, and — when the account is an admin — list and
// revoke anyone's. Scopes are ceilings, never bypasses (docs/gateway/authorization.md §8).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useState } from "react";
import { api } from "../api/client";
import type { TokenInfo } from "../api/types";
import { useSession } from "../session";
import { ErrorBox, Spinner, whenStr } from "../components/ui";

interface MintResponse {
  token: string;
  hash: string;
  scope: string;
  expires: number;
}

export function TokensPage(): ReactNode {
  const { info } = useSession();
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [scope, setScope] = useState("write");
  const [days, setDays] = useState("30");
  const [error, setError] = useState("");
  const [minted, setMinted] = useState<MintResponse | null>(null);

  const mine = useQuery({
    queryKey: ["tokens", "mine"],
    queryFn: () => api<{ tokens: TokenInfo[] | null }>("/api/v1/auth/tokens"),
    retry: false,
  });
  const all = useQuery({
    queryKey: ["tokens", "all"],
    queryFn: () => api<{ tokens: TokenInfo[] | null }>("/api/v1/auth/tokens?all=1"),
    retry: false,
  });

  const mint = useMutation({
    mutationFn: () =>
      api<MintResponse>("/api/v1/auth/tokens", {
        method: "POST",
        body: JSON.stringify({ name, scope, expires_days: parseInt(days, 10) || 0 }),
      }),
    onSuccess: (res) => {
      setMinted(res);
      setName("");
      setError("");
      qc.invalidateQueries({ queryKey: ["tokens"] });
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });
  const revoke = useMutation({
    mutationFn: (hash: string) => api(`/api/v1/auth/tokens/${hash}`, { method: "DELETE" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["tokens"] }),
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  const table = (toks: TokenInfo[], title: string, showUser: boolean): ReactNode => (
    <section>
      <h2>{title}</h2>
      {toks.length === 0 && <p className="muted pad">none yet.</p>}
      {toks.length > 0 && (
        <table>
          <thead>
            <tr>
              {showUser && <th>user</th>}
              <th>name</th>
              <th>scope</th>
              <th>created</th>
              <th>expires</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {toks.map((t) => (
              <tr key={t.hash} className={t.revoked ? "muted" : ""}>
                {showUser && <td>{t.user}</td>}
                <td>{t.name || <span className="muted">(unnamed)</span>}</td>
                <td>
                  <span className={"pill" + (t.scope === "admin" ? " warn" : "")}>{t.scope}</span>
                </td>
                <td>{whenStr(t.created)}</td>
                <td>{t.expires ? whenStr(t.expires) : "never"}</td>
                <td>
                  {t.revoked ? (
                    <span className="muted">revoked</span>
                  ) : (
                    <button
                      className="btn ghost danger"
                      onClick={() => {
                        if (window.confirm(`revoke ${t.name || t.hash.slice(0, 8)} (${t.user})?`)) revoke.mutate(t.hash);
                      }}
                    >
                      revoke
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );

  return (
    <>
      <h1>personal access tokens</h1>
      <p className="muted">scopes are ceilings, never bypasses: a write-scoped token can read, an admin-scoped token can mint — none of them escapes the policies above.</p>
      {error && <div className="error-box">{error}</div>}
      <form
        className="mint-form"
        onSubmit={(e) => {
          e.preventDefault();
          setError("");
          mint.mutate();
        }}
      >
        <input placeholder="token name" value={name} onChange={(e) => setName(e.target.value)} />
        <select value={scope} onChange={(e) => setScope(e.target.value)}>
          <option value="read">read</option>
          <option value="write">write</option>
          <option value="admin">admin</option>
        </select>
        <input className="days" placeholder="days" value={days} onChange={(e) => setDays(e.target.value)} />
        <button className="btn" type="submit" disabled={mint.isPending}>
          mint
        </button>
      </form>
      {minted && (
        <div className="minted-box">
          <strong>copy it now — the plaintext is shown exactly once:</strong>
          <pre>{minted.token}</pre>
          <button
            className="btn ghost"
            onClick={() => {
              void navigator.clipboard?.writeText(minted.token);
            }}
          >
            copy
          </button>
        </div>
      )}
      {mine.isPending && <Spinner />}
      {mine.isError && <ErrorBox error={mine.error} />}
      {mine.isSuccess && table((mine.data.tokens || []).filter((t) => !t.revoked).concat((mine.data.tokens || []).filter((t) => t.revoked)), `your tokens · ${info?.user ?? ""}`, false)}
      {all.isSuccess && table(all.data.tokens || [], "all tokens (admin)", true)}
    </>
  );
}
