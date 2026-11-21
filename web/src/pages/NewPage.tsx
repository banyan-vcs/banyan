// NewPage.tsx — the single creation surface behind the topbar "+": a new
// repository (blank, or imported from a URL — the old imports page folded
// in here) or a new group. The kind rides ?kind= so both menu entries are
// shareable links.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useState, type ReactNode } from "react";
import { api } from "../api/client";
import { suggestName, type ImportJob } from "../api/imports";
import { repoPath } from "../api/repos";
import { repoHref, useRepoGroups } from "../repo";
import type { GroupInfo, GroupsResponse } from "../api/groups";
import type { RepoInfo } from "../api/types";
import { can, useMode } from "../mode";
import { useSession } from "../session";
import { useT } from "../i18n";
import { Empty, ErrorBox, Pill, Spinner } from "../components/ui";

/** BlankRepoForm — the create form (name, description, optional parent
 * nest, and the group — required in the two-level world: every new
 * repository lives at /<group>/<name>). All three ride POST
 * /api/v1/repos, so the whole create stays one all-or-nothing request. */
function BlankRepoForm({ groups }: { groups: GroupInfo[] }): ReactNode {
  const t = useT();
  const nav = useNavigate();
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [parent, setParent] = useState("");
  const [group, setGroup] = useState("");
  const [error, setError] = useState("");
  const repos = useQuery({
    queryKey: ["repos"],
    queryFn: () => api<{ repos: RepoInfo[] }>("/api/v1/repos"),
  });
  const list = repos.data?.repos ?? [];
  const create = useMutation({
    mutationFn: () => {
      const body: Record<string, string> = { name, group, description };
      if (parent) body.parent = parent;
      return api("/api/v1/repos", { method: "POST", body: JSON.stringify(body) });
    },
    onSuccess: () => {
      setName("");
      setError("");
      qc.invalidateQueries({ queryKey: ["repos"] });
      nav(repoHref(name, group || undefined));
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });
  return (
    <form
      className="create-repo"
      onSubmit={(e) => {
        e.preventDefault();
        if (name.trim() && group) create.mutate();
      }}
    >
      <select value={group} onChange={(e) => setGroup(e.target.value)} title={t("the repository lives at /<group>/<name>")}>
        <option value="">{t("group…")}</option>
        {groups.map((g) => (
          <option key={g.name} value={g.name}>
            {g.name}
          </option>
        ))}
      </select>
      <span className="muted">/</span>
      <input placeholder={t("repository name (single segment)")} value={name} onChange={(e) => setName(e.target.value)} />
      <input
        className="repo-desc-input"
        placeholder={t("description (shown in the repository list)")}
        value={description}
        onChange={(e) => setDescription(e.target.value)}
      />
      <select value={parent} onChange={(e) => setParent(e.target.value)} title={t("nest under a parent project")}>
        <option value="">{t("— root project —")}</option>
        {list.map((r) => (
          <option key={r.id} value={r.id}>
            {repoPath(list, r.id).map((p) => p.id).join(" / ")}
          </option>
        ))}
      </select>
      <button className="btn" type="submit" disabled={create.isPending || !name.trim() || !group}>
        {t("create")}
      </button>
      {error && <span className="muted import-error">{error}</span>}
      {groups.length === 0 && (
        <span className="muted">
          {t(
            "no groups yet — {0}; every repository lives under a group.",
            <Link to="/new?kind=group">{t("create one first")}</Link>
          )}
        </span>
      )}
    </form>
  );
}

/** ImportForm — the old imports page's form: one URL, one credential; the
 * gateway fetches every ref server-side and the imported repo is a
 * first-class banyan repository. */
function ImportForm(): ReactNode {
  const t = useT();
  const { info } = useSession();
  const qc = useQueryClient();
  const [url, setUrl] = useState("");
  const [name, setName] = useState("");
  const [mode, setMode] = useState(info?.provider ? "session" : "token");
  const [token, setToken] = useState("");
  const [error, setError] = useState("");

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
    <form
      className="import-form"
      onSubmit={(e) => {
        e.preventDefault();
        if (url.trim()) create.mutate();
      }}
    >
      <input
        className="import-url"
        placeholder={t("https://github.com/owner/repo (or any git http URL)")}
        value={url}
        onChange={(e) => onUrlChange(e.target.value)}
      />
      <select value={mode} onChange={(e) => setMode(e.target.value)}>
        {info?.provider ? <option value="session">{t("my {0} login", info.provider.id)}</option> : null}
        <option value="token">{t("private token")}</option>
      </select>
      {mode === "token" && (
        <input type="password" placeholder={t("github / gitlab token")} value={token} onChange={(e) => setToken(e.target.value)} />
      )}
      <input placeholder={t("target name")} value={name} onChange={(e) => setName(e.target.value)} />
      <button className="btn" type="submit" disabled={create.isPending || !url.trim()}>
        {create.isPending ? t("starting…") : t("import")}
      </button>
      {error && <span className="muted import-error">{error}</span>}
    </form>
  );
}

/** ImportJobs — the job list (running jobs poll). */
function ImportJobs(): ReactNode {
  const t = useT();
  const groups = useRepoGroups();
  const jobs = useQuery({
    queryKey: ["imports"],
    queryFn: () => api<{ imports: ImportJob[] }>("/api/imports"),
    refetchInterval: (query) =>
      (query.state.data?.imports || []).some((j) => j.status === "running") ? 3000 : false,
  });
  if (jobs.isPending) return <Spinner />;
  if (jobs.isError) return <ErrorBox error={jobs.error} />;
  if (jobs.data.imports.length === 0) return <Empty>{t("nothing imported yet.")}</Empty>;
  return (
    <table>
      <thead>
        <tr>
          <th>{t("repository")}</th>
          <th>{t("source")}</th>
          <th>{t("refs")}</th>
          <th>{t("status")}</th>
          <th>{t("started")}</th>
        </tr>
      </thead>
      <tbody>
        {jobs.data.imports.map((j) => (
          <tr key={j.id}>
            <td>
              {j.status === "succeeded" ? <Link to={repoHref(j.repo, groups[j.repo])}>{j.repo}</Link> : j.repo}
              {j.defaultBranch && <Pill>{j.defaultBranch}</Pill>}
            </td>
            <td className="muted mono">{j.sourceUrl}</td>
            <td className="muted">{j.refs || "—"}</td>
            <td>
              {j.status === "running" && <Pill>{t("running")}</Pill>}
              {j.status === "succeeded" && <Pill tone="ok">{t("done")}</Pill>}
              {j.status === "failed" && <span className="muted import-error">{j.error || t("failed")}</span>}
            </td>
            <td className="muted">{new Date(j.startedAt * 1000).toISOString().slice(0, 16).replace("T", " ")}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/** GroupForm — groups are organizational containers: a name and a
 * description; repos carry the label from the create form or their
 * settings page. */
function GroupForm(): ReactNode {
  const t = useT();
  const nav = useNavigate();
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [error, setError] = useState("");
  const create = useMutation({
    mutationFn: () => api("/api/v1/groups", { method: "POST", body: JSON.stringify({ name: name.trim(), description }) }),
    onSuccess: () => {
      setName("");
      setDescription("");
      setError("");
      qc.invalidateQueries({ queryKey: ["groups"] });
      qc.invalidateQueries({ queryKey: ["repos"] });
      nav("/");
    },
    onError: (e) => setError(e instanceof Error ? e.message : String(e)),
  });
  return (
    <form
      className="rule-form"
      onSubmit={(e) => {
        e.preventDefault();
        if (name.trim()) create.mutate();
      }}
    >
      <input placeholder={t("group name (single segment)")} value={name} onChange={(e) => setName(e.target.value)} />
      <input className="group-desc" placeholder={t("what belongs here (optional)")} value={description} onChange={(e) => setDescription(e.target.value)} />
      <button className="btn" type="submit" disabled={create.isPending}>
        {t("create group")}
      </button>
      {error && <span className="muted import-error">{error}</span>}
    </form>
  );
}

export function NewPage(): ReactNode {
  const t = useT();
  const mode = useMode();
  const [params, setParams] = useSearchParams();
  const kind = params.get("kind") === "group" ? "group" : "repo";
  const [repoMode, setRepoMode] = useState<"blank" | "import">("blank");
  const canRepo = can(mode, "createRepos");
  const canGroup = can(mode, "groups");
  const canImport = can(mode, "import");

  const groups = useQuery({
    queryKey: ["groups"],
    queryFn: () => api<GroupsResponse>("/api/v1/groups"),
    retry: false,
    enabled: canGroup || canRepo,
  });

  if (!canRepo && !canGroup) {
    return (
      <>
        <h1>{t("Not in This Deployment")}</h1>
        <p className="muted">{t("this page is a standalone-console capability; this deployment (profile {0}) does not expose it.", mode.profile)}</p>
      </>
    );
  }
  const groupList = groups.data?.groups ?? [];
  const setKind = (k: "repo" | "group"): void => {
    const n = new URLSearchParams(params);
    n.set("kind", k);
    setParams(n, { replace: true });
  };
  return (
    <>
      <h1>{t("Create")}</h1>
      <div className="tab-row">
        {canRepo && (
          <button className={"tab" + (kind === "repo" ? " active" : "")} onClick={() => setKind("repo")}>
            {t("repository")}
          </button>
        )}
        {canGroup && (
          <button className={"tab" + (kind === "group" ? " active" : "")} onClick={() => setKind("group")}>
            {t("group")}
          </button>
        )}
      </div>
      {kind === "repo" && canRepo ? (
        <>
          {canImport && (
            <div className="tab-row">
              <button className={"tab" + (repoMode === "blank" ? " active" : "")} onClick={() => setRepoMode("blank")}>
                {t("blank")}
              </button>
              <button className={"tab" + (repoMode === "import" ? " active" : "")} onClick={() => setRepoMode("import")}>
                {t("import from URL")}
              </button>
            </div>
          )}
          {repoMode === "blank" || !canImport ? (
            <>
              <p className="muted">
                {t(
                  "a new repository at {0} — the group folders the list and the address, the name stays a single flat segment underneath (the git URL keeps speaking it).",
                  <code>/&lt;group&gt;/&lt;name&gt;</code>
                )}
              </p>
              {groups.isError && <p className="muted">{t("groups unavailable on this deployment — creation needs one.")}</p>}
              <BlankRepoForm groups={groupList} />
            </>
          ) : (
            <>
              <p className="muted">
                {t(
                  "one URL, one credential: github.com, gitlab.com or a private GitLab — the gateway fetches every ref server-side and the imported repo is a first-class banyan repository. Local passwords are never sent to the source host."
                )}
              </p>
              <ImportForm />
              <h2>{t("Import History")}</h2>
              <ImportJobs />
            </>
          )}
        </>
      ) : (
        <>
          <p className="muted">
            {t(
              "a group is an organizational container: it owns no code and gates nothing — it folders the repository list and nothing else. Repositories take the label at creation or from their settings page."
            )}
          </p>
          <GroupForm />
        </>
      )}
    </>
  );
}
