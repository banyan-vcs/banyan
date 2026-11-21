// mrs.ts — the merge-request API client (docs/design/merge-requests.md).
// The wire shapes mirror the gateway's /api/v1/repos/<repo>/merge-requests,
// /branch-rules, and the workspace-wide /api/v1/merge-requests faces.
import { api } from "./client";

export interface MRInfo {
  repo: string;
  iid: number;
  title: string;
  description?: string;
  source: string;
  target: string;
  state: "open" | "merged" | "closed" | string;
  author: string;
  source_head: string;
  merge_commit: string;
  merged_by: string;
  created_at: number;
  updated_at: number;
}

export interface MRView extends MRInfo {
  source_tip: string;
  mergeable: boolean;
  conflicts?: string[];
  merge_base: string;
  approvals: string[];
  required_approvals: number;
  rule_level?: string;
}

export interface BranchRule {
  repo: string;
  branch: string;
  level: "protected" | "mr-only" | string;
  approvals: number;
}

export const listMRs = (repo: string, state = ""): Promise<MRInfo[]> =>
  api<{ repo: string; merge_requests: MRInfo[] }>(
    `/api/v1/repos/${encodeURIComponent(repo)}/merge-requests${state ? `?state=${state}` : ""}`,
  ).then((r) => r.merge_requests ?? []);

// listAllMRs — the workspace-wide list (the sidebar's cross-repo view):
// every visible repository's ledger rows in one answer, newest first.
export const listAllMRs = (state = ""): Promise<MRInfo[]> =>
  api<{ merge_requests: MRInfo[] }>(`/api/v1/merge-requests${state ? `?state=${state}` : ""}`).then(
    (r) => r.merge_requests ?? [],
  );

export const getMR = (repo: string, iid: number | string): Promise<MRView> =>
  api<MRView>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}`);

export const openMR = (repo: string, body: { source: string; target: string; title: string; description?: string }): Promise<MRInfo> =>
  api<MRInfo>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests`, { method: "POST", body: JSON.stringify(body) });

export const approveMR = (repo: string, iid: number | string): Promise<unknown> =>
  api(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/approve`, { method: "POST", body: "{}" });

export const unapproveMR = (repo: string, iid: number | string): Promise<unknown> =>
  api(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/approve`, { method: "DELETE" });

export const mergeMR = (repo: string, iid: number | string, message = ""): Promise<MRInfo> =>
  api<MRInfo>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/merge`, {
    method: "POST",
    body: JSON.stringify({ message }),
  });

export const closeMR = (repo: string, iid: number | string): Promise<MRInfo> =>
  api<MRInfo>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/close`, { method: "POST", body: "{}" });

export const reopenMR = (repo: string, iid: number | string): Promise<MRInfo> =>
  api<MRInfo>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/reopen`, { method: "POST", body: "{}" });

export const branchRules = (repo: string): Promise<BranchRule[]> =>
  api<{ repo: string; rules: BranchRule[] }>(`/api/v1/repos/${encodeURIComponent(repo)}/branch-rules`).then(
    (r) => r.rules ?? [],
  );

export const putBranchRule = (repo: string, branch: string, level: string, approvals: number): Promise<BranchRule> =>
  api<BranchRule>(`/api/v1/repos/${encodeURIComponent(repo)}/branch-rules/${encodeURIComponent(branch)}`, {
    method: "PUT",
    body: JSON.stringify({ level, approvals }),
  });

export const deleteBranchRule = (repo: string, branch: string): Promise<unknown> =>
  api(`/api/v1/repos/${encodeURIComponent(repo)}/branch-rules/${encodeURIComponent(branch)}`, { method: "DELETE" });
