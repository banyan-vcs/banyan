// ImportPage.tsx — repo import: pick a source URL (github.com, gitlab.com,
// a private GitLab), authenticate with the login provider's token (session
// mode) or a pasted PAT, name the target, and watch the job list. Local
// passwords are never sent to a source host — that's why session mode
// needs a provider login.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { api } from "../api/client";
import { suggestName, type ImportJob } from "../api/imports";
import { useSession } from "../session";
import { Empty, ErrorBox, Pill, Spinner } from "../components/ui";

export function ImportPage(): ReactNode {
  const { info } = useSession();
  const qc = useQueryClient();
  const [url, setUrl] = useState("");
  const [name, setName] = useState("");
  const [mode, setMode] = useState(info?.provider ? "session" : "token");
  const [token, setToken] = useState("");
  const [error, setError] = useState("");

  const jobs = useQuery({
    queryKey: ["imports"],
    queryFn: () => api<{ imports: ImportJob[] }>("/api/imports"),
    refetchInterval: (query) =>
      (query.state.data?.imports || []).some((j) => j.status === "running") ? 3000 : false,
  });

  const create = useMutation({
    mutationFn: () =>
      api<{ id: string }>("/api/imports", {
        method: "POST",
        body: JSON.stringify({
          url: url.trim(),
          name: name.trim() || suggestName(url),
          auth: mode === "token" ? { mode: "token", token } : { mode: "session" },
        }),
      }),
    onSuccess: () => {
      setError("");
      setUrl("");
      setToken("");
      setName("");
      qc.invalidateQueries({ queryKey: ["imports"] });
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });

  const onUrlChange = (v: string): void => {
    setUrl(v);
    if (name === "" || name === suggestName(url)) setName(suggestName(v));
  };

  return (
    <>
      <h1>import repositories</h1>
      <p className="muted">
        one URL, one credential: github.com, gitlab.com or a private GitLab — the gateway fetches every ref server-side
        and the imported repo is a first-class banyan repository.
      </p>
      {error && <ErrorBox error={new Error(error)} />}
      <form
        className="import-form"
        onSubmit={(e) => {
          e.preventDefault();
          if (url.trim()) create.mutate();
        }}
      >
        <input
          className="import-url"
          placeholder="https://github.com/owner/repo (or any git http URL)"
          value={url}
          onChange={(e) => onUrlChange(e.target.value)}
        />
        <select value={mode} onChange={(e) => setMode(e.target.value)}>
          {info?.provider ? <option value="session">my {info.provider.id} login</option> : null}
          <option value="token">private token</option>
        </select>
        {mode === "token" && (
          <input type="password" placeholder="github / gitlab token" value={token} onChange={(e) => setToken(e.target.value)} />
        )}
        <input placeholder="target name" value={name} onChange={(e) => setName(e.target.value)} />
        <button className="btn" type="submit" disabled={create.isPending || !url.trim()}>
          {create.isPending ? "starting…" : "import"}
        </button>
      </form>

      <h2>import history</h2>
      {jobs.isPending && <Spinner />}
      {jobs.isError && <ErrorBox error={jobs.error} />}
      {jobs.isSuccess && jobs.data.imports.length === 0 && <Empty>nothing imported yet.</Empty>}
      {jobs.isSuccess && jobs.data.imports.length > 0 && (
        <table>
          <thead>
            <tr>
              <th>repository</th>
              <th>source</th>
              <th>refs</th>
              <th>status</th>
              <th>started</th>
            </tr>
          </thead>
          <tbody>
            {jobs.data.imports.map((j) => (
              <tr key={j.id}>
                <td>
                  {j.status === "succeeded" ? <Link to={`/r/${j.repo}`}>{j.repo}</Link> : j.repo}
                  {j.defaultBranch && <Pill>{j.defaultBranch}</Pill>}
                </td>
                <td className="muted mono">{j.sourceUrl}</td>
                <td className="muted">{j.refs || "—"}</td>
                <td>
                  {j.status === "running" && <Pill>running</Pill>}
                  {j.status === "succeeded" && <Pill tone="ok">done</Pill>}
                  {j.status === "failed" && <span className="muted import-error">{j.error || "failed"}</span>}
                </td>
                <td className="muted">{new Date(j.startedAt * 1000).toISOString().slice(0, 16).replace("T", " ")}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
