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
  blocking?: string[]; // reviewers whose latest verdict is request-changes
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

// --- the discussion ledger (docs/design/merge-requests.md §review) ---

export interface MRComment {
  id: number;
  repo: string;
  iid: number;
  parent_id?: number; // 0/absent = thread head
  author: string;
  body: string;
  kind: "comment" | "review" | string;
  verdict?: "approve" | "request-changes" | "comment" | string;
  path?: string; // anchor: repo-relative path
  line?: number; // anchor: line number on side
  side?: "old" | "new" | string; // anchor: which pane
  anchor_oid?: string; // anchor: the source tip the comment read
  resolved: boolean;
  resolved_by?: string;
  created_at: number;
}

export interface MRApprovalEvent {
  user: string;
  at: number;
}

export interface MRDiscussion {
  comments: MRComment[];
  approval_events: MRApprovalEvent[];
}

export const getDiscussion = (repo: string, iid: number | string): Promise<MRDiscussion> =>
  api<MRDiscussion>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/comments`);

export const addMRComment = (
  repo: string,
  iid: number | string,
  body: { body: string; path?: string; side?: string; line?: number; parent_id?: number },
): Promise<MRComment> =>
  api<MRComment>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/comments`, {
    method: "POST",
    body: JSON.stringify(body),
  });

export const resolveMRComment = (repo: string, iid: number | string, id: number, resolved: boolean): Promise<unknown> =>
  api(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/comments/${id}/resolve`, {
    method: "POST",
    body: JSON.stringify({ resolved }),
  });

export const submitMRReview = (
  repo: string,
  iid: number | string,
  verdict: "approve" | "request-changes" | "comment",
  body = "",
): Promise<MRComment> =>
  api<MRComment>(`/api/v1/repos/${encodeURIComponent(repo)}/merge-requests/${iid}/review`, {
    method: "POST",
    body: JSON.stringify({ verdict, body }),
  });
