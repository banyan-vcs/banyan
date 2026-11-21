// RepoHomePage.tsx — one repo's landing page: the branch list (THE USER'S
// filtered view, parsed from the same ref advertisement git sees), the
// default branch's recent history, and — when the repo sits in a project
// hierarchy (docs/design/data-model.md §14) — a breadcrumb of its ancestors and its
// direct sub-projects with an inline "create sub-project" form.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { api, qs } from "../api/client";
import { repoPath } from "../api/repos";
import { branchesOf, listRefs } from "../api/gitrefs";
import type { LogResponse, RepoInfo } from "../api/types";
import { can, useMode } from "../mode";
import { useT } from "../i18n";
import { Empty, ErrorBox, Pill, Spinner, shortOid, whenStr } from "../components/ui";
import { repoHref, useRepoBase, useRepoGroups, useRepoId } from "../repo";

export function RepoHomePage(): ReactNode {
  const t = useT();
  const repo = useRepoId();
  const base = useRepoBase();
  const groups = useRepoGroups();
  const [params] = useSearchParams();
  const mode = useMode();
  const nav = useNavigate();
  const qc = useQueryClient();
  const [subName, setSubName] = useState("");
  const [subError, setSubError] = useState("");
  const refs = useQuery({ queryKey: ["refs", repo], queryFn: () => listRefs(repo), retry: false });
  const branches = branchesOf(refs.data || { refs: [], capabilities: "", defaultBranch: "" });
  const defaultBranch = refs.data?.defaultBranch || branches[0]?.name || "";

  // The registry list rides the same cache as ReposPage — the hierarchy
  // view (breadcrumb + children) costs no extra request.
  const registry = useQuery({
    queryKey: ["repos"],
    queryFn: () => api<{ repos: RepoInfo[] }>("/api/v1/repos"),
  });
  const list = registry.data?.repos ?? [];
  const ancestors = repoPath(list, repo).slice(0, -1); // root … parent
  const children = list.filter((r) => r.parent === repo);

  const log = useQuery({
    queryKey: ["log", repo, defaultBranch, ""],
    queryFn: () => api<LogResponse>(`/api/v1/log${qs({ repo, rev: defaultBranch, limit: 10 })}`),
    enabled: defaultBranch !== "",
    retry: false,
  });

  const createSub = useMutation({
    mutationFn: (name: string) =>
      api("/api/v1/repos", { method: "POST", body: JSON.stringify({ name, parent: repo }) }),
    onSuccess: (_d, name) => {
      setSubName("");
      setSubError("");
      qc.invalidateQueries({ queryKey: ["repos"] });
      nav(repoHref(name));
    },
    onError: (e) => setSubError(e instanceof Error ? e.message : String(e)),
  });

  const cloneBase = params.get("clone") || "";

  return (
    <>
      <h1>
        <code>{repo}</code>
        {defaultBranch && <Pill>{t("default: {0}", defaultBranch)}</Pill>}
      </h1>
      {ancestors.length > 0 && (
        <p className="muted breadcrumb">
          {ancestors.map((a) => (
            <span key={a.id}>
              <Link to={repoHref(a.id, groups[a.id])}>{a.id}</Link> /{" "}
            </span>
          ))}
          <code>{repo}</code>
        </p>
      )}
      {cloneBase === "" && null}
      <div className="two-col">
        <section>
          <h2>{t("Branches · Your View")}</h2>
          {refs.isPending && <Spinner />}
          {refs.isError && <ErrorBox error={refs.error} />}
          {refs.isSuccess && branches.length === 0 && <p className="muted pad">{t("no branches yet — push one.")}</p>}
          {branches.length > 0 && (
            <table>
              <thead>
                <tr>
                  <th>{t("branch")}</th>
                  <th>{t("head")}</th>
                </tr>
              </thead>
              <tbody>
                {branches.map((b) => (
                  <tr key={b.name}>
                    <td>
                      <Link to={`${base}/tree?ref=${encodeURIComponent(b.name)}`}>{b.name}</Link>
                      {b.isDefault && <Pill>{t("default")}</Pill>}
                    </td>
                    <td>
                      <Link to={`${base}/commit/${b.oid}`} className="muted mono">
                        {shortOid(b.oid)}
                      </Link>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <p className="muted pad">
            {t(
              "personal branches of other users are simply not advertised here — the same filtered ref list your git client gets."
            )}
          </p>
        </section>
        <section>
          <h2>{t("Recent Commits")}{defaultBranch ? ` · ${defaultBranch}` : ""}</h2>
          {log.isPending && <Spinner />}
          {log.isError && <p className="muted pad">{t("no history yet.")}</p>}
          {log.isSuccess && (
            <table>
              <tbody>
                {log.data.entries.map((e) => (
                  <tr key={e.oid}>
                    <td>
                      <Link to={`${base}/commit/${e.oid}`}>{e.subject}</Link>
                      <div className="muted">
                        {e.author} · {whenStr(e.when)} · {shortOid(e.oid)}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {defaultBranch && (
            <p className="pad">
              <Link to={`${base}/history?ref=${encodeURIComponent(defaultBranch)}`}>{t("all commits →")}</Link>
            </p>
          )}
        </section>
      </div>
      <section>
        <h2>{t("Sub-Projects")}</h2>
        {children.length === 0 ? (
          <Empty>{t("this project has no sub-projects yet.")}</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th>{t("sub-project")}</th>
                <th>{t("default branch")}</th>
              </tr>
            </thead>
            <tbody>
              {children.map((c) => (
                <tr key={c.id}>
                  <td>
                    <Link to={repoHref(c.id, groups[c.id])}>{c.id}</Link>
                  </td>
                  <td className="muted">{c.defaultBranch || "main"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {can(mode, "createRepos") && (
          <form
            className="create-repo"
            onSubmit={(e) => {
              e.preventDefault();
              if (subName.trim()) createSub.mutate(subName.trim());
            }}
          >
            <input placeholder={t("new sub-project under this repo")} value={subName} onChange={(e) => setSubName(e.target.value)} />
            <button className="btn" type="submit" disabled={createSub.isPending}>
              {t("create sub-project")}
            </button>
            {subError && <span className="muted">{subError}</span>}
          </form>
        )}
      </section>
    </>
  );
}
