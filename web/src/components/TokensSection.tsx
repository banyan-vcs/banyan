// TokensSection.tsx — the personal access tokens block, lifted out of the
// old standalone page into personal settings: mint (plaintext shown
// exactly once), list your own, and — when the account is an admin — list
// and revoke anyone's. Scopes are ceilings, never bypasses
// (docs/gateway/authorization.md §8).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useState } from "react";
import { api } from "../api/client";
import type { TokenInfo } from "../api/types";
import { useT } from "../i18n";
import { useSession } from "../session";
import { ErrorBox, Spinner, whenStr } from "./ui";

interface MintResponse {
  token: string;
  hash: string;
  scope: string;
  expires: number;
}

export function TokensSection(): ReactNode {
  const t = useT();
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

  const table = (toks: TokenInfo[], heading: string, showUser: boolean): ReactNode => (
    <section>
      <h3>{heading}</h3>
      {toks.length === 0 && <p className="muted pad">{t("none yet.")}</p>}
      {toks.length > 0 && (
        <table>
          <thead>
            <tr>
              {showUser && <th>{t("user")}</th>}
              <th>{t("name")}</th>
              <th>{t("scope")}</th>
              <th>{t("created")}</th>
              <th>{t("expires")}</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {toks.map((tok) => (
              <tr key={tok.hash} className={tok.revoked ? "muted" : ""}>
                {showUser && <td>{tok.user}</td>}
                <td>{tok.name || <span className="muted">{t("(unnamed)")}</span>}</td>
                <td>
                  <span className={"pill" + (tok.scope === "admin" ? " warn" : "")}>{tok.scope}</span>
                </td>
                <td>{whenStr(tok.created)}</td>
                <td>{tok.expires ? whenStr(tok.expires) : t("never")}</td>
                <td>
                  {tok.revoked ? (
                    <span className="muted">{t("revoked")}</span>
                  ) : (
                    <button
                      className="btn ghost danger"
                      onClick={() => {
                        if (window.confirm(t("revoke {0} ({1})?", tok.name || tok.hash.slice(0, 8), tok.user))) revoke.mutate(tok.hash);
                      }}
                    >
                      {t("revoke")}
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
    <section>
      <h2>{t("access tokens")}</h2>
      <p className="muted">{t("scopes are ceilings, never bypasses: a write-scoped token can read, an admin-scoped token can mint — none of them escapes the policies above.")}</p>
      {error && <div className="error-box">{error}</div>}
      <form
        className="mint-form"
        onSubmit={(e) => {
          e.preventDefault();
          setError("");
          mint.mutate();
        }}
      >
        <input placeholder={t("token name")} value={name} onChange={(e) => setName(e.target.value)} />
        <select value={scope} onChange={(e) => setScope(e.target.value)}>
          <option value="read">read</option>
          <option value="write">write</option>
          <option value="admin">admin</option>
        </select>
        <input className="days" placeholder={t("days")} value={days} onChange={(e) => setDays(e.target.value)} />
        <button className="btn" type="submit" disabled={mint.isPending}>
          {t("mint")}
        </button>
      </form>
      {minted && (
        <div className="minted-box">
          <strong>{t("copy it now — the plaintext is shown exactly once:")}</strong>
          <pre>{minted.token}</pre>
          <button
            className="btn ghost"
            onClick={() => {
              void navigator.clipboard?.writeText(minted.token);
            }}
          >
            {t("copy")}
          </button>
        </div>
      )}
      {mine.isPending && <Spinner />}
      {mine.isError && <ErrorBox error={mine.error} />}
      {mine.isSuccess && table((mine.data.tokens || []).filter((tok) => !tok.revoked).concat((mine.data.tokens || []).filter((tok) => tok.revoked)), t("your tokens · {0}", info?.user ?? ""), false)}
      {all.isSuccess && table(all.data.tokens || [], t("all tokens (admin)"), true)}
    </section>
  );
}
